package pki

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPinnedClient(t *testing.T) {
	caCert, caKey, err := NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := IssueServer(caCert, caKey, "srv", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	get := func(cfg *tls.Config) error {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := c.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(ClientConfig(HashPEM(caCert), "", false)); err != nil {
		t.Fatalf("pinned hash should be trusted: %v", err)
	}
	if err := get(ClientConfig("", caCert, false)); err != nil {
		t.Fatalf("CA PEM should be trusted: %v", err)
	}
	otherCA, _, _ := NewCA("other")
	if err := get(ClientConfig(HashPEM(otherCA), "", false)); err == nil {
		t.Fatal("a different CA hash must be rejected")
	}
	h, err := PeerCAHash(srv.Listener.Addr().String())
	if err != nil || h != HashPEM(caCert) {
		t.Fatalf("PeerCAHash = %s, %v", h, err)
	}
}

func TestSignCSRRestrictsIP(t *testing.T) {
	caCert, caKey, _ := NewCA("test")
	csr, _, err := NewCSR("node1", []string{"10.0.0.5", "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := SignCSR(caCert, caKey, csr, "node1", "10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := ParseCert(certPEM)
	if len(c.IPAddresses) != 1 || !c.IPAddresses[0].Equal(net.ParseIP("10.0.0.5")) {
		t.Fatalf("SANs = %v, want only the node IP", c.IPAddresses)
	}
}
