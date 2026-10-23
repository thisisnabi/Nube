// Package pki holds the cluster certificate authority helpers: creating the
// CA, issuing server/agent certificates, and TLS client configs that trust the
// CA by pinning its hash (so no certificate files need to be copied around).
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

func encodeKey(k *ecdsa.PrivateKey) string {
	der, _ := x509.MarshalECPrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func encodeCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// NewCA creates a self-signed certificate authority valid for 20 years.
func NewCA(name string) (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	return encodeCert(der), encodeKey(key), nil
}

// ParseCert decodes the first certificate in a PEM string.
func ParseCert(certPEM string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate found")
	}
	return x509.ParseCertificate(b.Bytes)
}

// Hash returns "sha256:<hex>" of a DER certificate; this is what clients pin.
func Hash(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HashPEM is Hash for a PEM certificate.
func HashPEM(certPEM string) string {
	c, err := ParseCert(certPEM)
	if err != nil {
		return ""
	}
	return Hash(c.Raw)
}

type authority struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func loadCA(certPEM, keyPEM string) (*authority, error) {
	c, err := ParseCert(certPEM)
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode([]byte(keyPEM))
	if b == nil {
		return nil, errors.New("no PEM key found")
	}
	k, err := x509.ParseECPrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	return &authority{c, k}, nil
}

// splitHosts separates IP addresses from DNS names.
func splitHosts(hosts []string) (ips []net.IP, dns []string) {
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, h)
		}
	}
	return
}

// IssueServer issues a TLS server certificate for hosts (IPs and DNS names),
// returned with the CA in its chain.
func IssueServer(caCertPEM, caKeyPEM, commonName string, hosts []string, validity time.Duration) (tls.Certificate, error) {
	ca, err := loadCA(caCertPEM, caKeyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	ips, dns := splitHosts(hosts)
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// ClientAuth too: servers present this certificate to agents (mutual TLS).
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: ips,
		DNSNames:    dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}, nil
}

// SelfSigned makes a throwaway certificate (used before the CA exists, and by
// the ingress for hosts without a configured certificate).
func SelfSigned(hosts []string) (tls.Certificate, error) {
	certPEM, keyPEM, err := NewCA("nube-self-signed")
	if err != nil {
		return tls.Certificate{}, err
	}
	return IssueServer(certPEM, keyPEM, firstOr(hosts, "nube"), hosts, 365*24*time.Hour)
}

func firstOr(s []string, def string) string {
	if len(s) > 0 && s[0] != "" {
		return s[0]
	}
	return def
}

// NewCSR creates a key and a certificate request for an agent at ip.
func NewCSR(name string, ips []string) (csrPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	ipList, dns := splitHosts(ips)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name}, IPAddresses: ipList, DNSNames: dns,
	}, key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), encodeKey(key), nil
}

// SignCSR signs an agent's certificate request. Only the given IP is allowed
// as a SAN, so a node cannot obtain a certificate for another address.
func SignCSR(caCertPEM, caKeyPEM, csrPEM, nodeName, nodeIP string) (string, error) {
	ca, err := loadCA(caCertPEM, caKeyPEM)
	if err != nil {
		return "", err
	}
	b, _ := pem.Decode([]byte(csrPEM))
	if b == nil {
		return "", errors.New("invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return "", fmt.Errorf("CSR signature: %w", err)
	}
	ip := net.ParseIP(nodeIP)
	if ip == nil {
		return "", fmt.Errorf("node has no valid IP")
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "node:" + nodeName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return "", err
	}
	return encodeCert(der) + caCertPEM, nil
}

// ClientConfig returns a TLS config that trusts the cluster CA. The CA is
// identified by caHash ("sha256:..."), caPEM, or both. Hostnames are not
// checked (servers move between IPs); the CA signature is what matters.
// With insecure=true nothing is verified (testing only).
func ClientConfig(caHash, caPEM string, insecure bool) *tls.Config {
	cfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	if insecure {
		return cfg
	}
	var pinned *x509.Certificate
	if caPEM != "" {
		if c, err := ParseCert(caPEM); err == nil {
			pinned = c
		}
	}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("server sent no certificate")
		}
		ca := pinned
		if caHash != "" {
			ca = nil
			for _, c := range cs.PeerCertificates {
				if Hash(c.Raw) == caHash {
					ca = c
					break
				}
			}
			if ca == nil && pinned != nil && Hash(pinned.Raw) == caHash {
				ca = pinned
			}
			if ca == nil {
				return fmt.Errorf("server certificate is not signed by the pinned cluster CA %s", short(caHash))
			}
		}
		if ca == nil {
			return errors.New("no cluster CA configured (pass --ca-hash)")
		}
		roots := x509.NewCertPool()
		roots.AddCert(ca)
		inter := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter})
		return err
	}
	return cfg
}

func short(h string) string {
	if len(h) > 23 {
		return h[:23] + "…"
	}
	return h
}

// PeerCAHash connects to addr without verification and returns the hash of
// the last certificate in the chain (the CA), for trust-on-first-use prompts.
func PeerCAHash(addr string) (string, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("no certificate")
	}
	return Hash(certs[len(certs)-1].Raw), nil
}
