package server

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thisisnabi/Nube/internal/pki"
	"github.com/thisisnabi/Nube/internal/store"
)

// certs serves this server's TLS certificate. The cluster CA lives in the
// replicated state; until it exists (a server that has not joined yet) a
// temporary self-signed certificate is used.
type certs struct {
	s       *Server
	mu      sync.Mutex
	caHash  string
	current *tls.Certificate
	temp    *tls.Certificate
	user    *tls.Certificate
	userMod time.Time
}

func (c *certs) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello != nil && c.s.acme != nil && c.s.cfg.ACMEDomain != "" && strings.EqualFold(hello.ServerName, c.s.cfg.ACMEDomain) {
		return c.s.acme.getCertificate(hello)
	}
	if hello != nil && c.s.cfg.TLSCertFile != "" {
		return c.userCert()
	}
	caCert, caKey := c.s.ca()
	c.mu.Lock()
	defer c.mu.Unlock()
	if caCert == "" {
		if c.temp == nil {
			t, err := pki.SelfSigned(c.s.certHosts())
			if err != nil {
				return nil, err
			}
			c.temp = &t
		}
		return c.temp, nil
	}
	if h := pki.HashPEM(caCert); h != c.caHash || c.current == nil {
		t, err := pki.IssueServer(caCert, caKey, "nube-server", c.s.certHosts(), 5*365*24*time.Hour)
		if err != nil {
			return nil, err
		}
		c.current, c.caHash = &t, h
	}
	return c.current, nil
}

// userCert serves --tls-cert/--tls-key, reloading them when they change
// (e.g. after certbot renews them).
func (c *certs) userCert() (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := os.Stat(c.s.cfg.TLSCertFile)
	if err != nil {
		return nil, err
	}
	if c.user == nil || st.ModTime().After(c.userMod) {
		cert, err := tls.LoadX509KeyPair(c.s.cfg.TLSCertFile, c.s.cfg.TLSKeyFile)
		if err != nil {
			return nil, err
		}
		c.user, c.userMod = &cert, st.ModTime()
	}
	return c.user, nil
}

// certHosts lists every name and address clients may use for this server.
func (s *Server) certHosts() []string {
	hosts := []string{"localhost", "127.0.0.1"}
	if h, err := os.Hostname(); err == nil {
		hosts = append(hosts, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				hosts = append(hosts, ipn.IP.String())
			}
		}
	}
	if u, err := url.Parse(s.cfg.APIAdvertise); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	return append(hosts, s.cfg.TLSSANs...)
}

// ca returns the cluster CA from the replicated state.
func (s *Server) ca() (certPEM, keyPEM string) {
	s.store.Read(func(st *store.State) { certPEM, keyPEM = st.CACert, st.CAKey })
	return
}

// ensureCA creates the cluster CA on the leader if there is none yet.
func (s *Server) ensureCA() {
	if c, _ := s.ca(); c != "" {
		return
	}
	certPEM, keyPEM, err := pki.NewCA("nube-cluster-ca")
	if err != nil {
		log.Printf("tls: create CA: %v", err)
		return
	}
	s.store.Write(func(st *store.State) error {
		if st.CACert == "" {
			st.CACert, st.CAKey = certPEM, keyPEM
			st.Event("Normal", "Cluster", "", "ca", "CACreated", "cluster certificate authority created ("+pki.HashPEM(certPEM)+")")
		}
		return nil
	})
}

// writeCAFiles keeps <data>/ca.crt and <data>/ca.hash in sync, so the installer
// and admins can read the values to give to agents and clients.
func (s *Server) writeCAFiles() {
	certPEM, _ := s.ca()
	if certPEM == "" {
		return
	}
	path := filepath.Join(s.cfg.DataDir, "ca.crt")
	if b, err := os.ReadFile(path); err == nil && string(b) == certPEM {
		return
	}
	os.WriteFile(path, []byte(certPEM), 0o644)
	os.WriteFile(filepath.Join(s.cfg.DataDir, "ca.hash"), []byte(pki.HashPEM(certPEM)+"\n"), 0o644)
}

// clusterClient is an HTTP client trusting the cluster CA (server->server and server->agent).
func (s *Server) clusterClient(timeout time.Duration) *http.Client {
	caCert, _ := s.ca()
	cfg := pki.ClientConfig(s.cfg.CAHash, caCert, false)
	if caCert == "" && s.cfg.CAHash == "" {
		cfg = pki.ClientConfig("", "", true) // plain-HTTP clusters
	}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: cfg, Proxy: nil}}
}

// scheme returns the URL scheme of this server's API.
func (s *Server) scheme() string {
	if s.cfg.TLS {
		return "https"
	}
	return "http"
}

func (s *Server) handleCA(w http.ResponseWriter, r *http.Request) {
	certPEM, _ := s.ca()
	if certPEM == "" {
		http.Error(w, "the cluster CA has not been created yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="nube-ca.crt"`)
	w.Header().Set("X-Nube-CA-Hash", pki.HashPEM(certPEM))
	w.Write([]byte(certPEM))
}

func trimScheme(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.TrimRight(u, "/")
}
