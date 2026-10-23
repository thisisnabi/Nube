package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/pki"
	"github.com/thisisnabi/Nube/internal/store"
)

// ================================================================ apply & status

func applyIngress(st *store.State, o api.Object) (string, error) {
	var ing api.Ingress
	if err := json.Unmarshal(o.Raw, &ing); err != nil {
		return "", err
	}
	if len(ing.Spec.Rules) == 0 {
		return "", fmt.Errorf("ingress/%s: at least one rule is required", o.Name)
	}
	for ri := range ing.Spec.Rules {
		r := &ing.Spec.Rules[ri]
		r.Host = strings.ToLower(strings.TrimSpace(r.Host))
		if strings.Contains(r.Host, ":") || strings.Contains(r.Host, "/") {
			return "", fmt.Errorf("ingress/%s: host %q must be a bare hostname", o.Name, r.Host)
		}
		if strings.Contains(r.Host, "*") && !strings.HasPrefix(r.Host, "*.") {
			return "", fmt.Errorf("ingress/%s: wildcard hosts must look like *.example.com", o.Name)
		}
		if len(r.Paths) == 0 {
			return "", fmt.Errorf("ingress/%s: rule %q needs at least one path", o.Name, r.Host)
		}
		for pi := range r.Paths {
			p := &r.Paths[pi]
			if p.Path == "" {
				p.Path = "/"
			}
			if !strings.HasPrefix(p.Path, "/") {
				return "", fmt.Errorf("ingress/%s: path %q must start with /", o.Name, p.Path)
			}
			switch p.PathType {
			case "":
				p.PathType = "Prefix"
			case "Prefix", "Exact":
			default:
				return "", fmt.Errorf("ingress/%s: pathType must be Prefix or Exact", o.Name)
			}
			if !api.ValidName(p.Backend.Service) || p.Backend.Port <= 0 {
				return "", fmt.Errorf("ingress/%s: path %s needs backend.service and backend.port", o.Name, p.Path)
			}
		}
	}
	for i := range ing.Spec.TLS {
		t := &ing.Spec.TLS[i]
		for k, h := range t.Hosts {
			t.Hosts[k] = strings.ToLower(h)
			if t.ACME && strings.HasPrefix(h, "*.") {
				return "", fmt.Errorf("ingress/%s: ACME (HTTP-01) cannot issue wildcard certificates; use a tls Secret for %s", o.Name, h)
			}
		}
		if t.ACME && t.SecretName != "" {
			return "", fmt.Errorf("ingress/%s: use either acme: true or secretName, not both", o.Name)
		}
	}
	ing.Kind = api.KindIngress
	ing.Metadata.Name, ing.Metadata.Namespace = o.Name, o.Namespace
	key := ing.Metadata.Key()
	if old := st.Ingresses[key]; old != nil {
		if jsonEqual(old.Spec, ing.Spec) {
			return "unchanged", nil
		}
		touchMeta(&ing.Metadata, &old.Metadata)
		ing.Status = old.Status
		st.Ingresses[key] = &ing
		return "configured", nil
	}
	touchMeta(&ing.Metadata, nil)
	st.Ingresses[key] = &ing
	st.Event("Normal", api.KindIngress, o.Namespace, o.Name, "Created", "ingress routes created")
	return "created", nil
}

func reconcileIngressStatus(st *store.State) {
	for _, ing := range st.Ingresses {
		ns := ing.Metadata.Namespace
		var problems []string
		for _, r := range ing.Spec.Rules {
			for _, p := range r.Paths {
				svc := st.Services[ns+"/"+p.Backend.Service]
				switch {
				case svc == nil:
					problems = append(problems, "service "+p.Backend.Service+" not found")
				case servicePort(svc, p.Backend.Port) == nil:
					problems = append(problems, fmt.Sprintf("service %s has no port %d", p.Backend.Service, p.Backend.Port))
				case len(serviceEndpoints(st, svc)) == 0:
					problems = append(problems, "service "+p.Backend.Service+" has no ready pods")
				}
			}
		}
		for _, t := range ing.Spec.TLS {
			if t.ACME || t.SecretName == "" {
				continue
			}
			if sec := st.Secrets[ns+"/"+t.SecretName]; sec == nil || sec.Type != "tls" {
				problems = append(problems, "tls secret "+t.SecretName+" missing (serving self-signed)")
			}
		}
		ing.Status.Message = strings.Join(dedupe(problems), "; ")
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func servicePort(svc *api.Service, port int) *api.ServicePort {
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == port && !svc.Spec.Ports[i].IsUDP() {
			return &svc.Spec.Ports[i]
		}
	}
	return nil
}

// ================================================================ proxy

type ingressRoute struct {
	ingress  string // ns/name
	host     string // exact, "*.suffix" or ""
	path     string
	exact    bool
	backends []string
	service  string
	tlsHost  bool
	redirect bool
}

type ingress struct {
	s          *Server
	mu         sync.RWMutex
	routes     []ingressRoute
	certs      map[string]*tls.Certificate // by SNI host
	selfSign   map[string]*tls.Certificate
	tlsHosts   []api.IngressTLS
	secrets    map[string]*api.Secret // ns/name -> tls secret
	prefetched map[string]bool
	requests   sync.Map // ingress key -> *atomic.Int64
	rr         atomic.Uint64

	lnMu      sync.Mutex
	listeners map[string]*http.Server // "http|ip:port" -> server
	lastErr   map[string]string
}

func newIngress(s *Server) *ingress {
	return &ingress{
		s: s, certs: map[string]*tls.Certificate{}, selfSign: map[string]*tls.Certificate{},
		listeners: map[string]*http.Server{}, lastErr: map[string]string{},
	}
}

func (in *ingress) run(ctx context.Context) {
	if in.s.cfg.IngressHTTP == "" && in.s.cfg.IngressHTTPS == "" {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		in.refresh()
		in.syncListeners()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (in *ingress) close() {
	in.lnMu.Lock()
	defer in.lnMu.Unlock()
	for k, srv := range in.listeners {
		srv.Close()
		delete(in.listeners, k)
	}
}

// refresh rebuilds the routing table from the state.
func (in *ingress) refresh() {
	var routes []ingressRoute
	var tlsHosts []api.IngressTLS
	secrets := map[string]*api.Secret{}
	in.s.store.Read(func(st *store.State) {
		for _, key := range sortedKeys(st.Ingresses) {
			ing := st.Ingresses[key]
			ns := ing.Metadata.Namespace
			tlsSet := map[string]bool{}
			for _, t := range ing.Spec.TLS {
				for _, h := range t.Hosts {
					tlsSet[h] = true
				}
				tlsHosts = append(tlsHosts, api.IngressTLS{Hosts: t.Hosts, SecretName: ns + "/" + t.SecretName, ACME: t.ACME})
				if sec := st.Secrets[ns+"/"+t.SecretName]; sec != nil && sec.Type == "tls" && t.SecretName != "" {
					c := *sec
					secrets[ns+"/"+t.SecretName] = &c
				}
			}
			for _, r := range ing.Spec.Rules {
				for _, p := range r.Paths {
					route := ingressRoute{
						ingress: key, host: r.Host, path: p.Path, exact: p.PathType == "Exact",
						service: p.Backend.Service, tlsHost: tlsSet[r.Host], redirect: ing.Spec.RedirectHTTP && tlsSet[r.Host],
					}
					if svc := st.Services[ns+"/"+p.Backend.Service]; svc != nil {
						if sp := servicePort(svc, p.Backend.Port); sp != nil {
							for _, pod := range serviceEndpoints(st, svc) {
								if hp := pod.Status.HostPorts[sp.TargetPort]; hp > 0 && pod.Status.HostIP != "" {
									route.backends = append(route.backends, net.JoinHostPort(pod.Status.HostIP, strconv.Itoa(hp)))
								} else if pod.Status.PodIP != "" {
									route.backends = append(route.backends, net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(sp.TargetPort)))
								}
							}
						}
					}
					routes = append(routes, route)
				}
			}
		}
	})
	// Most specific first: exact host, wildcard, catch-all; then exact paths, then longest prefix.
	hostRank := func(h string) int {
		switch {
		case h == "":
			return 2
		case strings.HasPrefix(h, "*."):
			return 1
		}
		return 0
	}
	sort.SliceStable(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if hostRank(a.host) != hostRank(b.host) {
			return hostRank(a.host) < hostRank(b.host)
		}
		if a.exact != b.exact {
			return a.exact
		}
		return len(a.path) > len(b.path)
	})
	in.mu.Lock()
	in.routes, in.tlsHosts, in.secrets = routes, tlsHosts, secrets
	in.certs = map[string]*tls.Certificate{} // re-resolve (secrets may have changed)
	var fetch []string
	if in.prefetched == nil {
		in.prefetched = map[string]bool{}
	}
	for _, t := range tlsHosts {
		for _, h := range t.Hosts {
			if t.ACME && !in.prefetched[h] {
				in.prefetched[h] = true
				fetch = append(fetch, h)
			}
		}
	}
	in.mu.Unlock()
	// New hosts get their certificates right away instead of at the leader's next round.
	if len(fetch) > 0 && in.s.acme != nil {
		in.s.acme.wake()
	}
}

func hostMatches(pattern, host string) bool {
	switch {
	case pattern == "":
		return true
	case strings.HasPrefix(pattern, "*."):
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && !strings.Contains(strings.TrimSuffix(host, suffix), ".")
	}
	return pattern == host
}

func pathMatches(r ingressRoute, path string) bool {
	if r.exact {
		return path == r.path
	}
	if r.path == "/" {
		return true
	}
	return path == r.path || strings.HasPrefix(path, strings.TrimSuffix(r.path, "/")+"/")
}

func (in *ingress) match(host, path string) *ingressRoute {
	in.mu.RLock()
	defer in.mu.RUnlock()
	for i := range in.routes {
		if hostMatches(in.routes[i].host, host) && pathMatches(in.routes[i], path) {
			r := in.routes[i]
			return &r
		}
	}
	return nil
}

func (in *ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	route := in.match(host, r.URL.Path)
	if route == nil {
		http.Error(w, "404 no ingress rule for "+host+r.URL.Path, http.StatusNotFound)
		return
	}
	if r.TLS == nil && route.redirect {
		target := "https://" + r.Host + r.URL.RequestURI()
		if _, port, err := net.SplitHostPort(in.s.cfg.IngressHTTPS); err == nil && port != "443" {
			target = "https://" + net.JoinHostPort(host, port) + r.URL.RequestURI()
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
		return
	}
	v, _ := in.requests.LoadOrStore(route.ingress, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
	if len(route.backends) == 0 {
		http.Error(w, "503 service "+route.service+" has no ready pods", http.StatusServiceUnavailable)
		return
	}
	backend := route.backends[int(in.rr.Add(1))%len(route.backends)]
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: backend})
			pr.Out.Host = pr.In.Host // keep the original Host header
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "502 backend "+backend+": "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// getCertificate picks the certificate for an SNI name: the ingress's TLS
// secret if configured, otherwise a cached self-signed one.
func (in *ingress) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(hello.ServerName)
	if in.s.acme != nil && (in.acmeHost(name) || isALPNChallenge(hello)) {
		if c, err := in.s.acme.getCertificate(hello); err == nil {
			return c, nil
		}
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if c := in.certs[name]; c != nil {
		return c, nil
	}
	for _, t := range in.tlsHosts {
		for _, h := range t.Hosts {
			if !hostMatches(h, name) {
				continue
			}
			if sec := in.secrets[t.SecretName]; sec != nil {
				c, err := tls.X509KeyPair([]byte(sec.Data["tls.crt"]), []byte(sec.Data["tls.key"]))
				if err == nil {
					in.certs[name] = &c
					return &c, nil
				}
				log.Printf("ingress: secret %s: %v", t.SecretName, err)
			}
		}
	}
	if name == "" {
		name = "nube-ingress"
	}
	if c := in.selfSign[name]; c != nil {
		return c, nil
	}
	c, err := pki.SelfSigned([]string{name})
	if err != nil {
		return nil, err
	}
	in.selfSign[name] = &c
	return &c, nil
}

// syncListeners binds the ingress ports on every host address (outside the pod network).
func (in *ingress) syncListeners() {
	want := map[string]bool{}
	for _, spec := range []struct{ kind, addr string }{{"http", in.s.cfg.IngressHTTP}, {"https", in.s.cfg.IngressHTTPS}} {
		if spec.addr == "" {
			continue
		}
		host, port, err := net.SplitHostPort(spec.addr)
		if err != nil {
			continue
		}
		ips := []string{host}
		if host == "" || host == "0.0.0.0" {
			ips = in.s.lb.bindIPs()
		}
		for _, ip := range ips {
			want[spec.kind+"|"+net.JoinHostPort(ip, port)] = true
		}
	}
	in.lnMu.Lock()
	defer in.lnMu.Unlock()
	for k, srv := range in.listeners {
		if !want[k] {
			srv.Close()
			delete(in.listeners, k)
		}
	}
	for k := range want {
		if in.listeners[k] != nil {
			continue
		}
		kind, addr, _ := strings.Cut(k, "|")
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			if in.lastErr[k] != err.Error() {
				log.Printf("ingress: cannot listen on %s (%s): %v", addr, kind, err)
				in.lastErr[k] = err.Error()
			}
			continue
		}
		delete(in.lastErr, k)
		var handler http.Handler = in
		if kind == "http" && in.s.acme != nil {
			handler = in.s.acme.m.HTTPHandler(in) // answers /.well-known/acme-challenge/
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second, ErrorLog: log.New(discard{}, "", 0)}
		if kind == "https" {
			ln = tls.NewListener(ln, &tls.Config{GetCertificate: in.getCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1", acme.ALPNProto}})
		}
		in.listeners[k] = srv
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("ingress: %s: %v", addr, err)
			}
		}()
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// acmeHost reports whether a host is listed in an Ingress TLS entry with acme: true.
func (in *ingress) acmeHost(host string) bool {
	in.mu.RLock()
	defer in.mu.RUnlock()
	for _, t := range in.tlsHosts {
		if !t.ACME {
			continue
		}
		for _, h := range t.Hosts {
			if strings.EqualFold(h, host) {
				return true
			}
		}
	}
	return false
}

// acmeHosts lists the hosts of Ingress TLS entries with acme: true.
func (in *ingress) acmeHosts() []string {
	in.mu.RLock()
	defer in.mu.RUnlock()
	var hosts []string
	for _, t := range in.tlsHosts {
		if t.ACME {
			hosts = append(hosts, t.Hosts...)
		}
	}
	return hosts
}

func isALPNChallenge(hello *tls.ClientHelloInfo) bool {
	for _, p := range hello.SupportedProtos {
		if p == acme.ALPNProto {
			return true
		}
	}
	return false
}

// decorate adds the live request counters and ACME errors to copies of the ingresses.
func (in *ingress) acmeStatus(ing *api.Ingress) string {
	if in.s.acme == nil {
		for _, t := range ing.Spec.TLS {
			if t.ACME {
				return "acme: true but the server has no --acme-email; serving self-signed"
			}
		}
		return ""
	}
	var msgs []string
	for _, t := range ing.Spec.TLS {
		if !t.ACME {
			continue
		}
		for _, h := range t.Hosts {
			if e := in.s.acme.lastError(h); e != "" {
				msgs = append(msgs, h+": "+e)
			}
		}
	}
	return strings.Join(msgs, "; ")
}

// decorate adds the live request counters to copies of the ingresses.
func (in *ingress) decorate(list []*api.Ingress) []*api.Ingress {
	out := make([]*api.Ingress, len(list))
	for i, ing := range list {
		c := *ing
		if v, ok := in.requests.Load(ing.Metadata.Key()); ok {
			c.Status.Requests = v.(*atomic.Int64).Load()
		}
		if m := in.acmeStatus(ing); m != "" {
			c.Status.Message = strings.TrimPrefix(c.Status.Message+"; "+m, "; ")
		}
		out[i] = &c
	}
	return out
}

// certFingerprint is used in tests and status output.
func certFingerprint(c *tls.Certificate) string {
	if c == nil || len(c.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(c.Certificate[0])
	return fmt.Sprintf("%x", sum[:6])
}
