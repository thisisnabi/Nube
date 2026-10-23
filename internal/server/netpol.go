package server

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

func applyNetworkPolicy(st *store.State, o api.Object) (string, error) {
	var np api.NetworkPolicy
	if err := json.Unmarshal(o.Raw, &np); err != nil {
		return "", err
	}
	sp := &np.Spec
	if len(sp.PolicyTypes) == 0 {
		sp.PolicyTypes = []string{"Ingress"}
		if len(sp.Egress) > 0 {
			sp.PolicyTypes = append(sp.PolicyTypes, "Egress")
		}
	}
	for _, t := range sp.PolicyTypes {
		if t != "Ingress" && t != "Egress" {
			return "", fmt.Errorf("networkpolicy/%s: policyTypes must be Ingress and/or Egress", o.Name)
		}
	}
	check := func(rules []api.NetworkPolicyRule, peers func(api.NetworkPolicyRule) []api.NetworkPolicyPeer) error {
		for _, r := range rules {
			for _, p := range peers(r) {
				if p.IPBlock != nil {
					if _, err := netip.ParsePrefix(p.IPBlock.CIDR); err != nil {
						return fmt.Errorf("invalid ipBlock.cidr %q", p.IPBlock.CIDR)
					}
					for _, e := range p.IPBlock.Except {
						if _, err := netip.ParsePrefix(e); err != nil {
							return fmt.Errorf("invalid ipBlock.except %q", e)
						}
					}
					if p.PodSelector != nil {
						return fmt.Errorf("a peer has either ipBlock or podSelector, not both")
					}
				}
			}
			for _, pt := range r.Ports {
				if pt.Protocol != "" && !strings.EqualFold(pt.Protocol, "tcp") && !strings.EqualFold(pt.Protocol, "udp") {
					return fmt.Errorf("port protocol must be TCP or UDP")
				}
				if pt.Port < 0 || pt.Port > 65535 {
					return fmt.Errorf("invalid port %d", pt.Port)
				}
			}
		}
		return nil
	}
	if err := check(sp.Ingress, func(r api.NetworkPolicyRule) []api.NetworkPolicyPeer { return r.From }); err != nil {
		return "", fmt.Errorf("networkpolicy/%s: %w", o.Name, err)
	}
	if err := check(sp.Egress, func(r api.NetworkPolicyRule) []api.NetworkPolicyPeer { return r.To }); err != nil {
		return "", fmt.Errorf("networkpolicy/%s: %w", o.Name, err)
	}
	np.Kind = api.KindNetworkPolicy
	np.Metadata.Name, np.Metadata.Namespace = o.Name, o.Namespace
	key := np.Metadata.Key()
	if old := st.Policies[key]; old != nil {
		if jsonEqual(old.Spec, np.Spec) {
			return "unchanged", nil
		}
		touchMeta(&np.Metadata, &old.Metadata)
		st.Policies[key] = &np
		return "configured", nil
	}
	touchMeta(&np.Metadata, nil)
	st.Policies[key] = &np
	st.Event("Normal", api.KindNetworkPolicy, o.Namespace, o.Name, "Created", "network policy created")
	return "created", nil
}

func hasType(np *api.NetworkPolicy, t string) bool {
	for _, x := range np.Spec.PolicyTypes {
		if x == t {
			return true
		}
	}
	return false
}

// peerCIDRs resolves policy peers to address ranges.
func peerCIDRs(st *store.State, policyNS string, peers []api.NetworkPolicyPeer) []string {
	if len(peers) == 0 {
		return nil // anywhere
	}
	var out []string
	for _, peer := range peers {
		if peer.IPBlock != nil {
			out = append(out, subtractCIDRs(peer.IPBlock.CIDR, peer.IPBlock.Except)...)
			continue
		}
		namespaces := peer.Namespaces
		if len(namespaces) == 0 {
			namespaces = []string{policyNS}
		}
		for _, p := range st.Pods {
			if p.Status.PodIP == "" || p.Status.Phase == api.PodTerminating {
				continue
			}
			if !contains(namespaces, "*") && !contains(namespaces, p.Metadata.Namespace) {
				continue
			}
			if peer.PodSelector.Matches(p.Metadata.Labels) {
				out = append(out, p.Status.PodIP+"/32")
			}
		}
	}
	if out == nil {
		out = []string{} // peers given but nothing matches: allow nobody
	}
	sort.Strings(out)
	return dedupe(out)
}

// firewallFor computes the effective policy of every pod on a node.
func firewallFor(st *store.State, node string) []api.PodFirewall {
	var out []api.PodFirewall
	for _, key := range sortedKeys(st.Pods) {
		p := st.Pods[key]
		if p.Spec.NodeName != node || p.Status.PodIP == "" || p.Status.Phase == api.PodTerminating {
			continue
		}
		fw := api.PodFirewall{PodIP: p.Status.PodIP}
		for _, pk := range sortedKeys(st.Policies) {
			np := st.Policies[pk]
			if np.Metadata.Namespace != p.Metadata.Namespace || !np.Spec.PodSelector.Matches(p.Metadata.Labels) {
				continue
			}
			if hasType(np, "Ingress") {
				fw.IsolateIngress = true
				for _, r := range np.Spec.Ingress {
					fw.Ingress = append(fw.Ingress, api.FirewallRule{CIDRs: peerCIDRs(st, np.Metadata.Namespace, r.From), Ports: r.Ports})
				}
			}
			if hasType(np, "Egress") {
				fw.IsolateEgress = true
				for _, r := range np.Spec.Egress {
					fw.Egress = append(fw.Egress, api.FirewallRule{CIDRs: peerCIDRs(st, np.Metadata.Namespace, r.To), Ports: r.Ports})
				}
			}
		}
		if fw.IsolateIngress || fw.IsolateEgress {
			out = append(out, fw)
		}
	}
	return out
}

func countPolicyPods(st *store.State) {
	for _, np := range st.Policies {
		n := 0
		for _, p := range st.Pods {
			if p.Metadata.Namespace == np.Metadata.Namespace && p.Status.Phase != api.PodTerminating && np.Spec.PodSelector.Matches(p.Metadata.Labels) {
				n++
			}
		}
		np.Status.SelectedPods = n
	}
}

// subtractCIDRs returns base minus the except ranges, as a list of prefixes.
func subtractCIDRs(base string, except []string) []string {
	b, err := netip.ParsePrefix(base)
	if err != nil {
		return nil
	}
	parts := []netip.Prefix{b.Masked()}
	for _, e := range except {
		ep, err := netip.ParsePrefix(e)
		if err != nil {
			continue
		}
		ep = ep.Masked()
		var next []netip.Prefix
		for _, p := range parts {
			next = append(next, removePrefix(p, ep)...)
		}
		parts = next
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.String()
	}
	return out
}

// removePrefix returns p minus x by splitting p in halves around x.
func removePrefix(p, x netip.Prefix) []netip.Prefix {
	if !p.Overlaps(x) {
		return []netip.Prefix{p}
	}
	if x.Bits() <= p.Bits() { // x covers p entirely
		return nil
	}
	lo, hi := splitPrefix(p)
	return append(removePrefix(lo, x), removePrefix(hi, x)...)
}

func splitPrefix(p netip.Prefix) (netip.Prefix, netip.Prefix) {
	bits := p.Bits() + 1
	lo := netip.PrefixFrom(p.Addr(), bits)
	a := p.Addr().As4()
	idx := (bits - 1) / 8
	a[idx] |= 1 << (7 - uint((bits-1)%8))
	hi := netip.PrefixFrom(netip.AddrFrom4(a), bits)
	return lo, hi
}
