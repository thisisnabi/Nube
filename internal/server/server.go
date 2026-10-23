// Package server implements the Nube control plane: REST API, scheduler,
// controllers, service load balancer, ingress and the web dashboard.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/store"
	"github.com/thisisnabi/Nube/internal/version"
)

//go:embed web
var webFS embed.FS

type Config struct {
	Listen       string // API + dashboard, e.g. :7070
	DataDir      string
	Token        string // bootstrap admin token
	ServiceBind  string // address the service load balancer binds to
	ClusterCIDR  string // pod network, e.g. 10.244.0.0/16
	Domain       string // cluster DNS domain
	APIAdvertise string // URL reachable by other servers (HA), e.g. https://10.0.0.1:7070

	TLS     bool     // serve the API over HTTPS with the cluster CA
	TLSSANs []string // extra names/IPs for the server certificate
	CAHash  string   // pinned CA of the cluster to join (HA followers)

	IngressHTTP  string // ingress listen address, e.g. :80 ("" = disabled)
	IngressHTTPS string // e.g. :443

	TLSCertFile string // serve the API with this certificate instead of the cluster CA's
	TLSKeyFile  string

	ACMEEmail     string // enables ACME (Let's Encrypt) certificates
	ACMEDirectory string // default: Let's Encrypt production
	ACMECAFile    string // CA to trust for a private ACME directory
	ACMEDomain    string // public name of the API/dashboard, served with an ACME certificate

	HA            bool
	ServerID      string
	RaftBind      string
	RaftAdvertise string
	Bootstrap     bool
	Join          string // API URL of an existing server to join
}

type Server struct {
	cfg     Config
	store   store.Store
	lb      *loadBalancer
	ingress *ingress
	acme    *acmeManager
	certs   *certs
	live    *liveStats
	started time.Time
	http    *http.Server
}

func New(cfg Config) (*Server, error) {
	if cfg.ClusterCIDR == "" {
		cfg.ClusterCIDR = "10.244.0.0/16"
	}
	if cfg.Domain == "" {
		cfg.Domain = "nube"
	}
	if _, n, err := net.ParseCIDR(cfg.ClusterCIDR); err != nil || n.IP.To4() == nil {
		return nil, fmt.Errorf("invalid --cluster-cidr %q (need an IPv4 /16)", cfg.ClusterCIDR)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	if cfg.Token == "" {
		tok, err := loadOrCreateToken(filepath.Join(cfg.DataDir, "token"))
		if err != nil {
			return nil, err
		}
		cfg.Token = tok
	}

	var st store.Store
	var err error
	if cfg.HA {
		if cfg.ServerID == "" {
			cfg.ServerID, _ = os.Hostname()
		}
		if cfg.APIAdvertise == "" {
			return nil, errors.New("--advertise-api is required with --ha (e.g. https://10.0.0.1:7070)")
		}
		if cfg.RaftAdvertise == "" {
			return nil, errors.New("--raft-advertise is required with --ha (e.g. 10.0.0.1:7072)")
		}
		if cfg.Join != "" && strings.HasPrefix(cfg.Join, "https://") && cfg.CAHash == "" {
			return nil, errors.New("--ca-hash is required with an https --join (see <data>/ca.hash on the first server)")
		}
		st, err = store.NewRaftStore(store.RaftOptions{
			ID: cfg.ServerID, Dir: cfg.DataDir, Bind: cfg.RaftBind, Advertise: cfg.RaftAdvertise, Bootstrap: cfg.Bootstrap,
		})
	} else {
		st, err = store.NewFileStore(cfg.DataDir)
	}
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, started: time.Now(), live: newLiveStats()}
	s.lb = newLoadBalancer(s)
	s.ingress = newIngress(s)
	s.certs = &certs{s: s}
	if s.acme, err = newACME(s); err != nil {
		return nil, err
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return nil, errors.New("--tls-cert and --tls-key go together")
	}
	return s, nil
}

func loadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Server) Token() string { return s.cfg.Token }

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	s.routes(mux)
	s.http = &http.Server{
		Handler:           s.middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0), // TLS handshake noise from probes
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	if s.cfg.TLS {
		ln = tls.NewListener(ln, &tls.Config{GetCertificate: s.certs.get, MinVersion: tls.VersionTLS12})
	}
	log.Printf("nube server %s listening on %s://%s (mode=%s, data=%s)", version.Version, s.scheme(), s.cfg.Listen, s.store.Cluster().Mode, s.cfg.DataDir)

	go s.controllerLoop(ctx)
	go s.snapshotLoop(ctx)
	go s.lb.run(ctx)
	go s.ingress.run(ctx)
	if s.acme != nil {
		go s.acme.run(ctx)
	}
	if s.cfg.HA && s.cfg.Join != "" {
		go s.joinLoop(ctx)
	}
	go func() {
		for ctx.Err() == nil {
			s.writeCAFiles()
			time.Sleep(5 * time.Second)
		}
	}()

	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.http.Shutdown(shCtx)
	s.lb.close()
	s.ingress.close()
	return s.store.Close()
}

// joinLoop asks an existing server to add this one to the Raft cluster.
func (s *Server) joinLoop(ctx context.Context) {
	body, _ := json.Marshal(map[string]string{
		"id": s.cfg.ServerID, "raftAddr": s.cfg.RaftAdvertise, "apiAddr": s.cfg.APIAdvertise,
	})
	for attempt := 0; ; attempt++ {
		if s.store.Cluster().Leader != "" && attempt > 0 {
			// Already part of a cluster with a leader; make sure our membership record exists.
			found := false
			s.store.Read(func(st *store.State) { _, found = st.Servers[s.cfg.ServerID] })
			if found {
				log.Printf("ha: joined cluster (leader %s)", s.store.Cluster().Leader)
				return
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(s.cfg.Join, "/")+"/api/v1/cluster/join", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.clusterClient(30 * time.Second).Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				log.Printf("ha: join accepted by %s", s.cfg.Join)
			} else {
				log.Printf("ha: join via %s: %s %s", s.cfg.Join, resp.Status, strings.TrimSpace(string(b)))
			}
		} else {
			log.Printf("ha: join via %s: %v", s.cfg.Join, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// ---------------------------------------------------------------- HTTP plumbing

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		r, ok := s.authorize(w, r)
		if !ok {
			return
		}
		// Mutations go to the Raft leader.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.store.IsLeader() {
			s.forwardToLeader(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) forwardToLeader(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Nube-Forwarded") != "" {
		writeErr(w, http.StatusServiceUnavailable, "leader changed, retry")
		return
	}
	leader := s.store.LeaderAPI()
	if leader == "" {
		writeErr(w, http.StatusServiceUnavailable, "no leader elected yet, retry shortly")
		return
	}
	u, err := url.Parse(leader)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "bad leader address "+leader)
		return
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = s.clusterClient(0).Transport
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeErr(w, http.StatusBadGateway, "forward to leader: "+err.Error())
	}
	r.Header.Set("X-Nube-Forwarded", s.cfg.ServerID)
	p.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeStoreErr maps store errors to HTTP status codes.
func writeStoreErr(w http.ResponseWriter, err error) {
	var nf notFound
	var fb forbidden
	switch {
	case errors.Is(err, store.ErrNotLeader):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &nf):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.As(err, &fb):
		writeErr(w, http.StatusForbidden, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

type notFound string

func (n notFound) Error() string { return string(n) + " not found" }

type forbidden string

func (f forbidden) Error() string { return string(f) }

func (s *Server) dashboard() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	// Embedded files have no modification time, so give each a content-hash
	// ETag; browsers then revalidate and pick up new versions after upgrades.
	etags := map[string]string{}
	fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(sub, path)
			sum := sha256.Sum256(b)
			etags["/"+path] = `"` + hex.EncodeToString(sum[:8]) + `"`
		}
		return nil
	})
	etags["/"] = etags["/index.html"]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if tag, ok := etags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
		}
		files.ServeHTTP(w, r)
	})
}
