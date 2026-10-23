package agent

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/thisisnabi/Nube/internal/api"
)

const (
	networkName = "nube"      // Docker network every pod joins
	bridgeName  = "nube0"     // Linux bridge device behind it
	wgDevice    = "nube-wg"   // WireGuard interface for the overlay
	modeNone    = "none"      // pods reachable across nodes only via published ports
	modeHostGW  = "host-gw"   // plain routes; nodes must share an L2 network
	modeWG      = "wireguard" // encrypted mesh; works across any IP network
)

// network owns everything that makes pod IPs routable across nodes:
//
//   - a Docker bridge network with this node's /24 (10.88.N.0/24)
//   - routes to every other node's /24, either via WireGuard or directly
//   - iptables rules: forward pod<->pod traffic, masquerade pod->internet
type network struct {
	docker  *Docker
	mode    string
	dataDir string
	wgPort  int

	mu      sync.Mutex
	podCIDR string
	cluster string
	gateway string
	privKey *ecdh.PrivateKey
	ready   bool
	err     string
	peers   string // fingerprint of the last applied peer set
}

func newNetwork(d *Docker, mode, dataDir string, wgPort int) *network {
	if runtime.GOOS != "linux" && mode != modeNone {
		log.Printf("network: %s mode needs Linux; using %q", mode, modeNone)
		mode = modeNone
	}
	n := &network{docker: d, mode: mode, dataDir: dataDir, wgPort: wgPort}
	if mode == modeWG {
		if err := n.loadKey(); err != nil {
			n.err = err.Error()
		}
	}
	return n
}

func (n *network) loadKey() error {
	path := filepath.Join(n.dataDir, "wg.key")
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err == nil {
			if k, err := ecdh.X25519().NewPrivateKey(raw); err == nil {
				n.privKey = k
				return nil
			}
		}
	}
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(k.Bytes())+"\n"), 0o600); err != nil {
		return err
	}
	n.privKey = k
	return nil
}

func (n *network) publicKey() string {
	if n.privKey == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(n.privKey.PublicKey().Bytes())
}

// status is reported to the server on every sync.
func (n *network) status() (mode, pub string, port int, ready bool, errMsg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	port = 0
	if n.mode == modeWG {
		port = n.wgPort
	}
	return n.mode, n.publicKey(), port, n.ready, n.err
}

func (n *network) Gateway() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.gateway
}

// OverlayReady reports whether remote pod IPs are routable from here.
func (n *network) OverlayReady() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.mode != modeNone && n.ready
}

// Apply converges local networking to cfg. Safe to call repeatedly.
func (n *network) Apply(ctx context.Context, cfg api.NetworkConfig) {
	if cfg.PodCIDR == "" {
		return
	}
	err := n.apply(ctx, cfg)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		if n.err != err.Error() {
			log.Printf("network: %v", err)
		}
		n.err, n.ready = err.Error(), false
		n.peers = "" // retry everything next time
		return
	}
	n.err, n.ready = "", true
}

func (n *network) apply(ctx context.Context, cfg api.NetworkConfig) error {
	_, sub, err := net.ParseCIDR(cfg.PodCIDR)
	if err != nil {
		return fmt.Errorf("bad pod CIDR %q", cfg.PodCIDR)
	}
	gw := sub.IP.To4()
	gw = net.IPv4(gw[0], gw[1], gw[2], 1)
	if err := n.ensureBridge(ctx, cfg.PodCIDR, gw.String()); err != nil {
		return err
	}
	n.mu.Lock()
	n.podCIDR, n.cluster, n.gateway = cfg.PodCIDR, cfg.ClusterCIDR, gw.String()
	n.mu.Unlock()
	if n.mode == modeNone {
		return nil
	}

	fp := fingerprint(cfg)
	n.mu.Lock()
	unchanged := fp == n.peers
	n.mu.Unlock()
	if unchanged {
		return nil
	}
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
	if err := n.ensureFirewall(cfg); err != nil {
		return err
	}
	switch n.mode {
	case modeWG:
		if err := n.applyWireGuard(cfg); err != nil {
			return err
		}
	case modeHostGW:
		if err := n.applyHostGW(cfg); err != nil {
			return err
		}
	}
	n.mu.Lock()
	n.peers = fp
	n.mu.Unlock()
	log.Printf("network: %s overlay configured with %d peers", n.mode, len(cfg.Peers))
	return nil
}

func fingerprint(cfg api.NetworkConfig) string {
	var parts []string
	for _, p := range cfg.Peers {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%d", p.Node, p.HostIP, p.PodCIDR, p.WGPublicKey, p.WGPort))
	}
	sort.Strings(parts)
	return cfg.PodCIDR + "#" + cfg.ClusterCIDR + "#" + strings.Join(parts, ",")
}

// ensureBridge creates (or recreates, if the subnet changed) the pod network.
func (n *network) ensureBridge(ctx context.Context, subnet, gateway string) error {
	info, err := n.docker.InspectNetwork(ctx, networkName)
	if err != nil {
		return err
	}
	if info != nil && info.Subnet == subnet {
		return nil
	}
	if info != nil {
		if info.Count > 0 {
			return fmt.Errorf("docker network %s has subnet %s but this node now owns %s; remove its containers to migrate", networkName, info.Subnet, subnet)
		}
		if err := n.docker.RemoveNetwork(ctx, networkName); err != nil {
			return err
		}
	}
	opts := map[string]string{"com.docker.network.bridge.name": bridgeName}
	if n.mode != modeNone {
		// We masquerade ourselves so pod->pod traffic keeps its source IP,
		// and let routed traffic from other nodes reach containers directly.
		opts["com.docker.network.bridge.enable_ip_masquerade"] = "false"
		withGW := map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "nat-unprotected"}
		for k, v := range opts {
			withGW[k] = v
		}
		if err := n.docker.CreateNetwork(ctx, networkName, subnet, gateway, withGW); err == nil {
			log.Printf("network: created %s (%s) on bridge %s", networkName, subnet, bridgeName)
			return nil
		}
		// Older Docker versions don't know gateway_mode_ipv4; fall back.
	}
	if err := n.docker.CreateNetwork(ctx, networkName, subnet, gateway, opts); err != nil {
		return fmt.Errorf("create docker network: %w", err)
	}
	log.Printf("network: created %s (%s) on bridge %s", networkName, subnet, bridgeName)
	return nil
}

// ---------------------------------------------------------------- iptables

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ensureRule inserts (or appends) an iptables rule unless it already exists.
func ensureRule(table, chain string, insert bool, rule ...string) error {
	check := append([]string{"-w", "-t", table, "-C", chain}, rule...)
	if _, err := run("iptables", check...); err == nil {
		return nil
	}
	op := "-A"
	if insert {
		op = "-I"
	}
	_, err := run("iptables", append([]string{"-w", "-t", table, op, chain}, rule...)...)
	return err
}

func chainExists(table, chain string) bool {
	_, err := run("iptables", "-w", "-t", table, "-nL", chain)
	return err == nil
}

func (n *network) ensureFirewall(cfg api.NetworkConfig) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return errors.New("iptables not found (apt install iptables)")
	}
	// Pods reaching the outside world are masqueraded; pod<->pod is not.
	if err := ensureRule("nat", "POSTROUTING", false,
		"-s", cfg.PodCIDR, "!", "-d", cfg.ClusterCIDR, "-m", "comment", "--comment", "nube pod egress", "-j", "MASQUERADE"); err != nil {
		return err
	}
	chain := "FORWARD"
	if chainExists("filter", "DOCKER-USER") {
		chain = "DOCKER-USER"
	}
	if err := ensureRule("filter", chain, true,
		"-s", cfg.ClusterCIDR, "-d", cfg.ClusterCIDR, "-m", "comment", "--comment", "nube pod-to-pod", "-j", "ACCEPT"); err != nil {
		return err
	}
	if n.mode == modeWG {
		if err := ensureRule("filter", "INPUT", true,
			"-p", "udp", "--dport", fmt.Sprint(n.wgPort), "-m", "comment", "--comment", "nube wireguard", "-j", "ACCEPT"); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- WireGuard

func (n *network) applyWireGuard(cfg api.NetworkConfig) error {
	if n.privKey == nil {
		return errors.New("wireguard key not available")
	}
	if _, err := exec.LookPath("wg"); err != nil {
		return errors.New("wireguard tools not found (apt install wireguard-tools)")
	}
	if _, err := run("ip", "link", "show", wgDevice); err != nil {
		if _, err := run("ip", "link", "add", wgDevice, "type", "wireguard"); err != nil {
			return fmt.Errorf("create wireguard device (kernel module missing?): %w", err)
		}
	}
	keyFile := filepath.Join(n.dataDir, "wg.key")
	if _, err := run("wg", "set", wgDevice, "listen-port", fmt.Sprint(n.wgPort), "private-key", keyFile); err != nil {
		return err
	}
	if _, err := run("ip", "link", "set", wgDevice, "up"); err != nil {
		return err
	}
	want := map[string]api.NetworkPeer{}
	for _, p := range cfg.Peers {
		if p.WGPublicKey == "" || p.WGPort == 0 {
			continue // peer is not running in wireguard mode
		}
		want[p.WGPublicKey] = p
		if _, err := run("wg", "set", wgDevice, "peer", p.WGPublicKey,
			"endpoint", net.JoinHostPort(p.HostIP, fmt.Sprint(p.WGPort)),
			"allowed-ips", p.PodCIDR, "persistent-keepalive", "25"); err != nil {
			return err
		}
		// src = our bridge gateway, so traffic the node itself sends to remote pods
		// (service proxy, health checks) carries an address the peer's allowed-ips accept.
		if _, err := run("ip", "route", "replace", p.PodCIDR, "dev", wgDevice, "src", n.Gateway()); err != nil {
			return err
		}
	}
	// Remove peers and routes that are gone.
	if out, err := run("wg", "show", wgDevice, "peers"); err == nil {
		for _, pub := range strings.Fields(out) {
			if _, ok := want[pub]; !ok {
				run("wg", "set", wgDevice, "peer", pub, "remove")
			}
		}
	}
	wantCIDR := map[string]bool{}
	for _, p := range want {
		wantCIDR[p.PodCIDR] = true
	}
	if out, err := run("ip", "-4", "route", "show", "dev", wgDevice); err == nil {
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) > 0 && !wantCIDR[f[0]] {
				run("ip", "route", "del", f[0], "dev", wgDevice)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- host-gw

func (n *network) applyHostGW(cfg api.NetworkConfig) error {
	_, cluster, err := net.ParseCIDR(cfg.ClusterCIDR)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, p := range cfg.Peers {
		want[p.PodCIDR] = true
		if _, err := run("ip", "route", "replace", p.PodCIDR, "via", p.HostIP); err != nil {
			return fmt.Errorf("route to %s via %s (nodes must share a network for host-gw; use wireguard otherwise): %w", p.PodCIDR, p.HostIP, err)
		}
	}
	out, err := run("ip", "-4", "route", "show")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "via" {
			continue
		}
		ip, sub, err := net.ParseCIDR(f[0])
		if err != nil || !cluster.Contains(ip) || f[0] == cfg.PodCIDR || want[sub.String()] {
			continue
		}
		run("ip", "route", "del", f[0])
	}
	return nil
}
