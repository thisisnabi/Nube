// Package agent runs on every worker node: it registers with the control
// plane, runs the pods assigned to it in Docker, keeps them healthy, and
// provides pod networking, cluster DNS and in-cluster service addresses.
package agent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/pki"
	"github.com/thisisnabi/Nube/internal/proxy"
	"github.com/thisisnabi/Nube/internal/version"
)

type Config struct {
	Servers     []string // control-plane API URLs; the first reachable one is used
	Token       string   // node token (role "node") or the cluster admin token
	CAHash      string   // pinned cluster CA ("sha256:..."), required for https servers
	Insecure    bool     // skip TLS verification (testing only)
	Name        string
	AdvertiseIP string // IP other nodes / servers use to reach this node
	Listen      string // agent API (logs), e.g. :7071
	DataDir     string
	DockerHost  string
	Labels      map[string]string
	NetworkMode string // wireguard | host-gw | none
	WGPort      int
	CPU         int64 // override capacity (millicores)
	Memory      int64 // override capacity (MiB)
}

type volumeInfo struct {
	path     string // filesystem to measure ("" = not measurable)
	enforced bool
	message  string
}

type Agent struct {
	cfg    Config
	docker *Docker
	net    *network
	dns    *dnsServer
	svc    *proxy.Manager
	sized  *sizedVolumes
	http   *http.Client
	cpu    cpuSampler
	info   RuntimeInfo
	tls    bool // the control plane uses HTTPS

	mu       sync.Mutex
	pods     map[string]*podState // by pod UID
	desired  map[string]api.AssignedPod
	domain   string
	synced   bool
	server   int // index into cfg.Servers currently used
	lastSync time.Time
	kick     chan struct{}
	volumes  map[string]volumeInfo // ns/name -> info
	caPEM    string
	cert     *tls.Certificate // agent API certificate, signed by the cluster CA
	certCh   chan struct{}    // closed once cert is available
	authOK   map[string]time.Time
}

func New(cfg Config) (*Agent, error) {
	if len(cfg.Servers) == 0 {
		return nil, errors.New("--server is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("--token is required (create one with: nube token create NAME --role node)")
	}
	if cfg.Name == "" {
		h, _ := os.Hostname()
		cfg.Name = strings.ToLower(h)
	}
	if !api.ValidName(cfg.Name) {
		return nil, fmt.Errorf("node name %q is not a valid name; pass --name", cfg.Name)
	}
	if cfg.AdvertiseIP == "" {
		cfg.AdvertiseIP = outboundIP(cfg.Servers[0])
	}
	if cfg.WGPort == 0 {
		cfg.WGPort = 51820
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	useTLS := false
	for _, s := range cfg.Servers {
		if strings.HasPrefix(s, "https://") {
			useTLS = true
		}
	}
	if useTLS && cfg.CAHash == "" && !cfg.Insecure {
		return nil, errors.New("--ca-hash is required for https servers (see /var/lib/nube/ca.hash on a server)")
	}
	d, err := NewDocker(cfg.DockerHost)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		cfg:     cfg,
		docker:  d,
		dns:     newDNSServer(),
		svc:     proxy.NewManager("service-proxy"),
		sized:   newSizedVolumes(cfg.DataDir),
		tls:     useTLS,
		pods:    map[string]*podState{},
		desired: map[string]api.AssignedPod{},
		kick:    make(chan struct{}, 1),
		volumes: map[string]volumeInfo{},
		certCh:  make(chan struct{}),
		authOK:  map[string]time.Time{},
	}
	if b, err := os.ReadFile(a.path("ca.crt")); err == nil {
		a.caPEM = string(b)
	}
	a.http = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		TLSClientConfig: pki.ClientConfig(cfg.CAHash, a.caPEM, cfg.Insecure),
	}}
	a.loadCert()
	a.net = newNetwork(d, cfg.NetworkMode, cfg.DataDir, cfg.WGPort)
	return a, nil
}

func (a *Agent) path(name string) string { return filepath.Join(a.cfg.DataDir, name) }

func (a *Agent) Run(ctx context.Context) error {
	var err error
	for i := 0; ; i++ {
		a.info, err = a.docker.Info(ctx)
		if err == nil {
			break
		}
		if i == 0 {
			log.Printf("agent: waiting for Docker: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	log.Printf("nube agent %s: node %s (%s), docker %s, %d cpus, %d MiB, network=%s",
		version.Version, a.cfg.Name, a.cfg.AdvertiseIP, a.info.Version, a.info.NCPU, a.info.MemMiB, a.cfg.NetworkMode)

	go a.serveAPI(ctx)
	go a.statsLoop(ctx)
	go a.reconcileLoop(ctx)

	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		if err := a.sync(ctx); err != nil {
			log.Printf("agent: sync: %v", err)
		}
		select {
		case <-ctx.Done():
			a.svc.Close()
			a.dns.Close()
			return nil
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------- certificates

func (a *Agent) loadCert() {
	c, err := tls.LoadX509KeyPair(a.path("agent.crt"), a.path("agent.key"))
	if err != nil || len(c.Certificate) == 0 {
		return
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil || time.Until(leaf.NotAfter) < 30*24*time.Hour {
		return
	}
	ipOK := false
	for _, ip := range leaf.IPAddresses {
		if ip.String() == a.cfg.AdvertiseIP {
			ipOK = true
		}
	}
	if !ipOK {
		return // the node's IP changed: request a new certificate
	}
	a.cert = &c
	close(a.certCh)
}

// csr returns a certificate request while the agent has no certificate.
func (a *Agent) csr() string {
	if !a.tls {
		return ""
	}
	a.mu.Lock()
	has := a.cert != nil
	a.mu.Unlock()
	if has {
		return ""
	}
	keyPath := a.path("agent.key")
	b, err := os.ReadFile(a.path("agent.csr"))
	if err == nil {
		if _, kerr := os.Stat(keyPath); kerr == nil {
			return string(b)
		}
	}
	csrPEM, keyPEM, err := pki.NewCSR("node:"+a.cfg.Name, []string{a.cfg.AdvertiseIP})
	if err != nil {
		log.Printf("tls: %v", err)
		return ""
	}
	os.WriteFile(keyPath, []byte(keyPEM), 0o600)
	os.WriteFile(a.path("agent.csr"), []byte(csrPEM), 0o600)
	return csrPEM
}

func (a *Agent) storeCert(certPEM string) {
	if err := os.WriteFile(a.path("agent.crt"), []byte(certPEM), 0o600); err != nil {
		log.Printf("tls: %v", err)
		return
	}
	c, err := tls.LoadX509KeyPair(a.path("agent.crt"), a.path("agent.key"))
	if err != nil {
		log.Printf("tls: received certificate does not match key: %v", err)
		return
	}
	os.Remove(a.path("agent.csr"))
	a.mu.Lock()
	first := a.cert == nil
	a.cert = &c
	a.mu.Unlock()
	if first {
		close(a.certCh)
		log.Printf("tls: received node certificate from the cluster CA")
	}
}

// ---------------------------------------------------------------- sync with server

func (a *Agent) nodeReport() api.Node {
	mode, pub, port, ready, nerr := a.net.status()
	capacity := api.Resources{CPU: int64(a.info.NCPU) * 1000, Memory: a.info.MemMiB}
	if a.cfg.CPU > 0 {
		capacity.CPU = a.cfg.CPU
	}
	if a.cfg.Memory > 0 {
		capacity.Memory = a.cfg.Memory
	}
	a.mu.Lock()
	hasCert := a.cert != nil
	a.mu.Unlock()
	return api.Node{
		Name:     a.cfg.Name,
		Address:  net.JoinHostPort(a.cfg.AdvertiseIP, portOf(a.cfg.Listen)),
		HostIP:   a.cfg.AdvertiseIP,
		Labels:   a.cfg.Labels,
		Capacity: capacity,
		Usage:    api.Resources{CPU: a.cpu.sample(a.info.NCPU), Memory: memUsedMiB()},
		Info: api.NodeInfo{
			OS: a.info.OS, Kernel: a.info.Kernel, Arch: a.info.Arch, Runtime: "docker",
			RuntimeVer: a.info.Version, AgentVersion: version.Version + " " + runtime.GOOS + "/" + runtime.GOARCH,
		},
		NetworkMode: mode, WGPublicKey: pub, WGPort: port, NetworkReady: ready, NetworkError: nerr,
		TLS: a.tls && hasCert,
	}
}

func portOf(listen string) string {
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "7071"
	}
	return p
}

func (a *Agent) sync(ctx context.Context) error {
	req := api.SyncRequest{Node: a.nodeReport(), Pods: a.reports(), Volumes: a.volumeReports(), CSR: a.csr()}
	body, _ := json.Marshal(req)
	var lastErr error
	for i := 0; i < len(a.cfg.Servers); i++ {
		idx := (a.server + i) % len(a.cfg.Servers)
		resp, err := a.post(ctx, a.cfg.Servers[idx]+"/api/v1/agent/sync", body)
		if err != nil {
			lastErr = err
			continue
		}
		if a.server != idx {
			log.Printf("agent: now syncing with %s", a.cfg.Servers[idx])
		}
		a.server = idx
		a.applySync(ctx, resp)
		return nil
	}
	return lastErr
}

func (a *Agent) post(ctx context.Context, url string, body []byte) (*api.SyncResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out api.SyncResponse
	return &out, json.NewDecoder(resp.Body).Decode(&out)
}

func (a *Agent) applySync(ctx context.Context, resp *api.SyncResponse) {
	if resp.CA != "" && resp.CA != a.caPEM && (a.cfg.CAHash == "" || pki.HashPEM(resp.CA) == a.cfg.CAHash) {
		a.caPEM = resp.CA
		os.WriteFile(a.path("ca.crt"), []byte(resp.CA), 0o644)
	}
	if resp.Certificate != "" {
		a.storeCert(resp.Certificate)
	}
	desired := map[string]api.AssignedPod{}
	for _, ap := range resp.Pods {
		desired[ap.Pod.Metadata.UID] = ap
	}
	a.mu.Lock()
	a.desired, a.domain = desired, resp.Network.Domain
	first := !a.synced
	a.synced, a.lastSync = true, time.Now()
	a.mu.Unlock()
	if first {
		log.Printf("agent: registered with control plane, pod subnet %s", resp.Network.PodCIDR)
	}

	a.net.Apply(ctx, resp.Network)
	if gw := a.net.Gateway(); gw != "" {
		if err := a.dns.Listen(gw); err != nil {
			log.Printf("dns: %v", err)
		}
		a.updateServiceRouting(gw, resp)
	}
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// updateServiceRouting refreshes DNS records and the node-local service proxy.
// A service name resolves to the bridge gateway, where this agent proxies the
// service port to ready pods — the equivalent of a Kubernetes ClusterIP.
// Headless services resolve straight to the pod IPs instead.
func (a *Agent) updateServiceRouting(gw string, resp *api.SyncResponse) {
	domain := resp.Network.Domain
	gwIP := net.ParseIP(gw)
	recs := map[string][]net.IP{}
	overlay := a.net.OverlayReady()
	var targets []proxy.Target
	for _, r := range resp.Services {
		fqdn := strings.ToLower(r.Name + "." + r.Namespace + ".svc." + domain)
		if r.Headless {
			for _, ep := range r.Endpoints {
				if ip := net.ParseIP(ep.PodIP); ip != nil {
					recs[fqdn] = append(recs[fqdn], ip)
				}
			}
			continue
		}
		recs[fqdn] = []net.IP{gwIP}
		for _, sp := range r.Ports {
			t := proxy.Target{
				Name: fmt.Sprintf("%s/%s:%d", r.Namespace, r.Name, sp.Port),
				Addr: net.JoinHostPort(gw, strconv.Itoa(sp.Port)),
				UDP:  sp.IsUDP(),
			}
			for _, ep := range r.Endpoints {
				local := ep.Node == a.cfg.Name
				hostPorts := ep.HostPorts
				if sp.IsUDP() {
					hostPorts = ep.HostPortsUDP
				}
				switch {
				case ep.PodIP != "" && (local || overlay):
					t.Backends = append(t.Backends, net.JoinHostPort(ep.PodIP, strconv.Itoa(sp.TargetPort)))
				case hostPorts[sp.TargetPort] > 0:
					t.Backends = append(t.Backends, net.JoinHostPort(ep.HostIP, strconv.Itoa(hostPorts[sp.TargetPort])))
				}
			}
			targets = append(targets, t)
		}
	}
	for podNS, ip := range resp.PodIPs {
		if p := net.ParseIP(ip); p != nil {
			recs[strings.ToLower(podNS)+".pod."+domain] = []net.IP{p}
		}
	}
	for name, ip := range resp.Records {
		if p := net.ParseIP(ip); p != nil {
			recs[strings.ToLower(name)+"."+domain] = []net.IP{p}
		}
	}
	a.dns.SetRecords(domain, recs)
	a.svc.Sync(targets)
}

// ---------------------------------------------------------------- reports

func (a *Agent) reports() []api.PodReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]api.PodReport, 0, len(a.pods))
	for _, ps := range a.pods {
		out = append(out, ps.report())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *Agent) recordVolume(key, path string, enforced bool, msg string) {
	a.mu.Lock()
	a.volumes[key] = volumeInfo{path: path, enforced: enforced, message: msg}
	a.mu.Unlock()
}

func (a *Agent) volumeReports() []api.VolumeReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []api.VolumeReport
	for key, vi := range a.volumes {
		ns, name, _ := strings.Cut(key, "/")
		r := api.VolumeReport{Namespace: ns, Name: name, Enforced: vi.enforced, Message: vi.message}
		if vi.path != "" {
			if used, total, ok := diskUsage(vi.path); ok {
				r.UsedMiB = used
				if vi.enforced {
					r.CapacityMiB = total
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------- reconcile loop

func (a *Agent) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
		}
		a.mu.Lock()
		ready := a.synced
		a.mu.Unlock()
		if ready {
			if err := a.reconcile(ctx); err != nil {
				log.Printf("agent: reconcile: %v", err)
			}
		}
	}
}

// ---------------------------------------------------------------- metrics

func (a *Agent) statsLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.mu.Lock()
		var items []*ctrState
		ids := map[*ctrState]string{}
		for _, ps := range a.pods {
			for _, st := range ps.ctrs {
				if st.id != "" && st.state == "Running" && !st.init {
					items = append(items, st)
					ids[st] = st.id
				}
			}
		}
		a.mu.Unlock()
		for _, st := range items {
			cpuNS, mem, err := a.docker.Stats(ctx, ids[st])
			if err != nil {
				continue
			}
			now := time.Now()
			a.mu.Lock()
			if !st.lastCPUAt.IsZero() && cpuNS >= st.lastCPU {
				st.cpu = int64(float64(cpuNS-st.lastCPU) / float64(now.Sub(st.lastCPUAt).Nanoseconds()) * 1000)
			}
			st.lastCPU, st.lastCPUAt, st.mem = cpuNS, now, int64(mem>>20)
			a.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------- agent API

// serveAPI serves container logs to the control plane. With TLS it uses
// mutual TLS: only servers holding a certificate from the cluster CA are
// accepted. Without TLS the caller's token is checked against the server.
func (a *Agent) serveAPI(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		last := a.lastSync
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "node": a.cfg.Name, "lastSync": last})
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil && !a.tokenAllowed(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			http.Error(w, "unauthorized", 401)
			return
		}
		id := r.URL.Query().Get("id")
		cs, err := a.docker.Inspect(r.Context(), id)
		if err != nil || cs.Labels["nube.managed"] != "true" {
			http.Error(w, "container not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		a.docker.Logs(r.Context(), id, r.URL.Query().Get("tail"), r.URL.Query().Get("follow") == "1", w)
	})
	srv := &http.Server{Addr: a.cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	var err error
	if a.tls {
		select {
		case <-a.certCh:
		case <-ctx.Done():
			return
		}
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				a.mu.Lock()
				defer a.mu.Unlock()
				return a.cert, nil
			},
			ClientAuth: tls.RequireAnyClientCert,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				return a.verifyServerClient(raw)
			},
		}
		log.Printf("agent: API listening on https://%s (mutual TLS)", a.cfg.Listen)
		err = srv.ListenAndServeTLS("", "")
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("agent: api: %v", err)
	}
}

// verifyServerClient accepts only control-plane servers' certificates signed by the cluster CA.
func (a *Agent) verifyServerClient(raw [][]byte) error {
	if len(raw) == 0 {
		return errors.New("client certificate required")
	}
	ca, err := pki.ParseCert(a.caPEM)
	if err != nil {
		return errors.New("cluster CA unknown")
	}
	leaf, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return err
	}
	if leaf.Subject.CommonName != "nube-server" {
		return fmt.Errorf("certificate %q is not a control-plane server", leaf.Subject.CommonName)
	}
	return nil
}

// tokenAllowed (plain-HTTP clusters) asks the control plane whether the
// caller's token is an admin token; answers are cached for a minute.
func (a *Agent) tokenAllowed(ctx context.Context, tok string) bool {
	if tok == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.Token)) == 1 {
		return true
	}
	a.mu.Lock()
	exp, ok := a.authOK[tok]
	server := a.cfg.Servers[a.server]
	a.mu.Unlock()
	if ok && time.Now().Before(exp) {
		return true
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", server+"/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var id api.Identity
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&id) != nil || id.Role != api.RoleAdmin {
		return false
	}
	a.mu.Lock()
	a.authOK[tok] = time.Now().Add(time.Minute)
	a.mu.Unlock()
	return true
}
