package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/thisisnabi/Nube/internal/store"
)

// acmeManager obtains and renews certificates from Let's Encrypt (or any
// ACME directory) for Ingress hosts marked "acme: true" and for the API's
// --acme-domain. The certificate cache lives in the replicated state, so all
// servers share certificates and challenge tokens. Only the Raft leader talks
// to the CA: concurrent orders for one domain from several servers share an
// authorization and spoil each other's challenges.
type acmeManager struct {
	s     *Server
	m     *autocert.Manager
	mu    sync.Mutex
	err   map[string]string // last error per host, for status
	wakec chan struct{}
}

// errACMEFollower is returned by the CA client on servers that are not the
// leader; they serve certificates from the shared cache only.
var errACMEFollower = errors.New("acme: certificates are requested by the leader")

func newACME(s *Server) (*acmeManager, error) {
	if s.cfg.ACMEEmail == "" {
		return nil, nil
	}
	client := &acme.Client{DirectoryURL: s.cfg.ACMEDirectory}
	if client.DirectoryURL == "" {
		client.DirectoryURL = autocert.DefaultACMEDirectory
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	if s.cfg.ACMECAFile != "" { // e.g. a private ACME server (step-ca, Pebble)
		pem, err := os.ReadFile(s.cfg.ACMECAFile)
		if err != nil {
			return nil, fmt.Errorf("--acme-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("--acme-ca: no certificates found")
		}
		base.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	client.HTTPClient = &http.Client{Timeout: 60 * time.Second, Transport: &orderLocator{s: s, next: base, orders: map[string]string{}}}
	a := &acmeManager{s: s, err: map[string]string{}, wakec: make(chan struct{}, 1)}
	a.m = &autocert.Manager{
		Prompt:      autocert.AcceptTOS,
		Email:       s.cfg.ACMEEmail,
		Cache:       stateCache{s},
		HostPolicy:  a.hostPolicy,
		Client:      client,
		RenewBefore: 30 * 24 * time.Hour,
	}
	return a, nil
}

// hostPolicy allows the API domain and hosts of Ingress TLS entries with acme: true.
func (a *acmeManager) hostPolicy(_ context.Context, host string) error {
	host = strings.ToLower(host)
	if host == strings.ToLower(a.s.cfg.ACMEDomain) {
		return nil
	}
	if a.s.ingress.acmeHost(host) {
		return nil
	}
	return fmt.Errorf("acme: %s is not an ingress host with acme: true", host)
}

func (a *acmeManager) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c, err := a.m.GetCertificate(hello)
	if err != nil && strings.Contains(err.Error(), errACMEFollower.Error()) {
		return nil, err // not yet in the cache; the leader is on it
	}
	a.mu.Lock()
	if err != nil {
		if a.err[hello.ServerName] != err.Error() {
			log.Printf("acme: %s: %v", hello.ServerName, err)
		}
		a.err[hello.ServerName] = err.Error()
	} else {
		delete(a.err, hello.ServerName)
	}
	a.mu.Unlock()
	return c, err
}

func (a *acmeManager) lastError(host string) string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err[host]
}

// run makes the leader obtain certificates for all ACME hosts up front, so
// the first visitor does not wait and errors show up in the ingress status
// early. Loading a certificate also schedules its renewal, and a new leader
// picks up where the old one left off.
func (a *acmeManager) run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if a.s.store.IsLeader() {
			hosts := a.s.ingress.acmeHosts()
			if a.s.cfg.ACMEDomain != "" {
				hosts = append(hosts, a.s.cfg.ACMEDomain)
			}
			for _, h := range hosts {
				if ctx.Err() != nil {
					return
				}
				a.getCertificate(&tls.ClientHelloInfo{ServerName: h, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.wakec:
		}
	}
}

// wake starts a round of run now (new ACME hosts).
func (a *acmeManager) wake() {
	select {
	case a.wakec <- struct{}{}:
	default:
	}
}

// stateCache implements autocert.Cache on top of the replicated state.
type stateCache struct{ s *Server }

func (c stateCache) Get(_ context.Context, key string) ([]byte, error) {
	var b []byte
	c.s.store.Read(func(st *store.State) {
		if v, ok := st.ACME[key]; ok {
			b = append([]byte(nil), v...)
		}
	})
	if b == nil {
		return nil, autocert.ErrCacheMiss
	}
	return b, nil
}

func (c stateCache) Put(_ context.Context, key string, data []byte) error {
	return c.s.acmeWrite(key, data, false)
}

func (c stateCache) Delete(_ context.Context, key string) error {
	return c.s.acmeWrite(key, nil, true)
}

// acmeWrite stores a cache entry through the leader (any server may be the
// one handling a TLS handshake or a challenge).
func (s *Server) acmeWrite(key string, data []byte, del bool) error {
	if s.store.IsLeader() {
		err := s.store.Write(func(st *store.State) error {
			if del {
				delete(st.ACME, key)
			} else {
				st.ACME[key] = data
			}
			return nil
		})
		// Challenge tokens: autocert asks the CA to validate right after this
		// returns, and the CA may reach any server. Give the followers a moment
		// to apply the entry first.
		if err == nil && !del && (strings.HasSuffix(key, "+token") || strings.HasSuffix(key, "+http-01")) && len(s.store.Cluster().Members) > 1 {
			time.Sleep(time.Second)
		}
		return err
	}
	body, _ := json.Marshal(map[string]any{"key": key, "data": data, "delete": del})
	return s.leaderPost("/api/v1/cluster/acme", body)
}

func (s *Server) handleACMECache(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Key    string `json:"key"`
		Data   []byte `json:"data"`
		Delete bool   `json:"delete"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Key == "" {
		writeErr(w, 400, "bad acme cache request")
		return
	}
	if err := s.acmeWrite(b.Key, b.Data, b.Delete); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// orderLocator fills in the Location header of "finalize" responses that lack
// it. x/crypto/acme polls the order at that URL when the CA finalizes
// asynchronously ("processing"), and some CAs (Pebble, for one) only send
// Location when the order is created. The order URL is remembered from that
// response, keyed by the order's finalize URL.
type orderLocator struct {
	s      *Server
	next   http.RoundTripper
	mu     sync.Mutex
	orders map[string]string // finalize URL -> order URL
}

func (o *orderLocator) RoundTrip(r *http.Request) (*http.Response, error) {
	if !o.s.store.IsLeader() {
		return nil, errACMEFollower
	}
	res, err := o.next.RoundTrip(r)
	if err != nil || r.Method != http.MethodPost || !strings.Contains(res.Header.Get("Content-Type"), "json") {
		return res, err
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	var order struct {
		Finalize string `json:"finalize"`
	}
	if json.Unmarshal(body, &order) != nil || order.Finalize == "" {
		return res, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if loc := res.Header.Get("Location"); loc != "" {
		if len(o.orders) > 1000 {
			clear(o.orders)
		}
		o.orders[order.Finalize] = loc
	} else if r.URL.String() == order.Finalize {
		if loc := o.orders[order.Finalize]; loc != "" {
			res.Header.Set("Location", loc)
		}
	}
	return res, nil
}
