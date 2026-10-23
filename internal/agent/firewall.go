package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/thisisnabi/Nube/internal/api"
)

// iptables chains owned by Nube.
const (
	chainServices = "NUBE-SERVICES"  // nat: service address -> pod DNAT
	chainMarkMasq = "NUBE-MARK-MASQ" // nat: mark hairpin traffic for masquerade
	chainNetpol   = "NUBE-NETPOL"    // filter: network policies
	masqMark      = "0x4000/0x4000"
)

// firewall programs service routing (like kube-proxy's iptables mode) and
// network policies. Both are written with iptables-restore so each update is
// atomic, and only when the desired rules changed.
type firewall struct {
	mu      sync.Mutex
	lastNAT string
	lastPol string
	hooks   bool
}

func chainName(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + strings.ToUpper(hex.EncodeToString(h[:])[:12])
}

func restore(table, rules string) error {
	cmd := exec.Command("iptables-restore", "-w", "--noflush")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore %s: %v: %s", table, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// existingChains lists user chains in a table with a prefix.
func existingChains(table, prefix string) []string {
	out, err := exec.Command("iptables", "-w", "-t", table, "-S").Output()
	if err != nil {
		return nil
	}
	var chains []string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "-N" && strings.HasPrefix(f[1], prefix) {
			chains = append(chains, f[1])
		}
	}
	return chains
}

// removeStale flushes and deletes chains that are no longer declared.
func removeStale(table string, prefixes []string, keep map[string]bool) {
	var stale []string
	for _, p := range prefixes {
		for _, c := range existingChains(table, p) {
			if !keep[c] {
				stale = append(stale, c)
			}
		}
	}
	for _, c := range stale {
		exec.Command("iptables", "-w", "-t", table, "-F", c).Run()
	}
	for _, c := range stale {
		exec.Command("iptables", "-w", "-t", table, "-X", c).Run()
	}
}

func (f *firewall) ensureHooks() error {
	if f.hooks {
		return nil
	}
	if _, err := exec.LookPath("iptables-restore"); err != nil {
		return fmt.Errorf("iptables-restore not found")
	}
	// Bridged pod-to-pod traffic must traverse iptables for policies and hairpin DNAT.
	exec.Command("modprobe", "br_netfilter").Run()
	os.WriteFile("/proc/sys/net/bridge/bridge-nf-call-iptables", []byte("1"), 0o644)

	if err := restore("nat", "*nat\n:"+chainServices+" - [0:0]\n:"+chainMarkMasq+" - [0:0]\n-A "+chainMarkMasq+" -j MARK --set-xmark "+masqMark+"\nCOMMIT\n"); err != nil {
		return err
	}
	if err := restore("filter", "*filter\n:"+chainNetpol+" - [0:0]\nCOMMIT\n"); err != nil {
		return err
	}
	for _, chain := range []string{"PREROUTING", "OUTPUT"} {
		if err := ensureRule("nat", chain, true, "-m", "comment", "--comment", "nube services", "-j", chainServices); err != nil {
			return err
		}
	}
	if err := ensureRule("nat", "POSTROUTING", true, "-m", "mark", "--mark", masqMark, "-m", "comment", "--comment", "nube hairpin", "-j", "MASQUERADE"); err != nil {
		return err
	}
	f.hooks = true
	return nil
}

// ensureNetpolFirst keeps the policy chain ahead of the pod-to-pod ACCEPT rule.
func ensureNetpolFirst() error {
	chain := "FORWARD"
	if chainExists("filter", "DOCKER-USER") {
		chain = "DOCKER-USER"
	}
	rule := []string{"-m", "comment", "--comment", "nube network policies", "-j", chainNetpol}
	out, _ := exec.Command("iptables", "-w", "-t", "filter", "-S", chain).Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 1 && strings.Contains(lines[1], "-j "+chainNetpol) {
		return nil // already first
	}
	exec.Command("iptables", append([]string{"-w", "-t", "filter", "-D", chain}, rule...)...).Run()
	_, err := run("iptables", append([]string{"-w", "-t", "filter", "-I", chain, "1"}, rule...)...)
	return err
}

// endpoint is one service backend.
type endpoint struct {
	ip   string
	port int
}

// syncServices writes the DNAT rules for every service port on this node.
func (f *firewall) syncServices(gw string, services []api.ServiceRoute) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ensureHooks(); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("*nat\n:" + chainServices + " - [0:0]\n")
	var body strings.Builder
	keep := map[string]bool{}
	sort.Slice(services, func(i, j int) bool {
		return services[i].Namespace+services[i].Name < services[j].Namespace+services[j].Name
	})
	for _, r := range services {
		if r.Headless {
			continue
		}
		for _, sp := range r.Ports {
			proto := "tcp"
			if sp.IsUDP() {
				proto = "udp"
			}
			var eps []endpoint
			for _, ep := range r.Endpoints {
				if ep.PodIP != "" {
					eps = append(eps, endpoint{ep.PodIP, sp.TargetPort})
				}
			}
			if len(eps) == 0 {
				continue // nothing listens on the gateway: clients get "connection refused"
			}
			svcChain := chainName("NUBE-SVC-", r.Namespace, r.Name, proto, strconv.Itoa(sp.Port))
			keep[svcChain] = true
			b.WriteString(":" + svcChain + " - [0:0]\n")
			fmt.Fprintf(&body, "-A %s -d %s/32 -p %s --dport %d -m comment --comment %q -j %s\n",
				chainServices, gw, proto, sp.Port, r.Namespace+"/"+r.Name, svcChain)
			for i, ep := range eps {
				sep := chainName("NUBE-SEP-", svcChain, ep.ip, strconv.Itoa(ep.port))
				keep[sep] = true
				b.WriteString(":" + sep + " - [0:0]\n")
				// Pick backends uniformly: the i-th rule matches 1/(n-i) of what reaches it.
				if n := len(eps); i < n-1 {
					fmt.Fprintf(&body, "-A %s -m statistic --mode random --probability %.5f -j %s\n", svcChain, 1/float64(n-i), sep)
				} else {
					fmt.Fprintf(&body, "-A %s -j %s\n", svcChain, sep)
				}
				fmt.Fprintf(&body, "-A %s -s %s/32 -j %s\n", sep, ep.ip, chainMarkMasq) // hairpin
				fmt.Fprintf(&body, "-A %s -p %s -j DNAT --to-destination %s:%d\n", sep, proto, ep.ip, ep.port)
			}
		}
	}
	b.WriteString(body.String())
	b.WriteString("COMMIT\n")
	rules := b.String()
	if rules == f.lastNAT {
		return nil
	}
	if err := restore("nat", rules); err != nil {
		return err
	}
	removeStale("nat", []string{"NUBE-SVC-", "NUBE-SEP-"}, keep)
	f.lastNAT = rules
	return nil
}

// syncPolicies writes the network-policy chains for this node's pods.
func (f *firewall) syncPolicies(pods []api.PodFirewall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ensureHooks(); err != nil {
		return err
	}
	var b, body strings.Builder
	b.WriteString("*filter\n:" + chainNetpol + " - [0:0]\n")
	body.WriteString("-A " + chainNetpol + " -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN\n")
	keep := map[string]bool{}
	sort.Slice(pods, func(i, j int) bool { return pods[i].PodIP < pods[j].PodIP })
	writeChain := func(chain, dir, ip string, rules []api.FirewallRule) {
		keep[chain] = true
		b.WriteString(":" + chain + " - [0:0]\n")
		match := "-d"
		peer := "-s"
		if dir == "egress" {
			match, peer = "-s", "-d"
		}
		fmt.Fprintf(&body, "-A %s %s %s/32 -j %s\n", chainNetpol, match, ip, chain)
		for _, r := range rules {
			cidrs := r.CIDRs
			if cidrs == nil {
				cidrs = []string{""} // any peer
			}
			ports := r.Ports
			if len(ports) == 0 {
				ports = []api.NetworkPolicyPort{{}}
			}
			for _, c := range cidrs {
				for _, p := range ports {
					line := "-A " + chain
					if c != "" {
						line += " " + peer + " " + c
					}
					if p.Port > 0 || p.Protocol != "" {
						proto := strings.ToLower(p.Protocol)
						if proto == "" {
							proto = "tcp"
						}
						line += " -p " + proto
						if p.Port > 0 {
							line += " --dport " + strconv.Itoa(p.Port)
						}
					}
					body.WriteString(line + " -j RETURN\n")
				}
			}
		}
		fmt.Fprintf(&body, "-A %s -m comment --comment %q -j DROP\n", chain, "nube "+dir+" policy for "+ip)
	}
	for _, p := range pods {
		if p.IsolateIngress {
			writeChain(chainName("NUBE-NPI-", p.PodIP), "ingress", p.PodIP, p.Ingress)
		}
		if p.IsolateEgress {
			writeChain(chainName("NUBE-NPE-", p.PodIP), "egress", p.PodIP, p.Egress)
		}
	}
	b.WriteString(body.String())
	b.WriteString("COMMIT\n")
	rules := b.String()
	if rules == f.lastPol {
		return nil
	}
	if err := restore("filter", rules); err != nil {
		return err
	}
	if err := ensureNetpolFirst(); err != nil {
		return err
	}
	removeStale("filter", []string{"NUBE-NPI-", "NUBE-NPE-"}, keep)
	f.lastPol = rules
	return nil
}

// reset forgets what was applied (e.g. after Docker flushed iptables).
func (f *firewall) reset() {
	f.mu.Lock()
	f.lastNAT, f.lastPol, f.hooks = "", "", false
	f.mu.Unlock()
}
