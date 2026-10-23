package agent

import (
	"bufio"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// dnsServer answers cluster names and forwards everything else upstream.
//
//	<service>.<namespace>.svc.<domain>  -> this node's service proxy address
//	<pod>.<namespace>.pod.<domain>      -> the pod IP
//
// Containers get it as their only nameserver, with search domains so that a
// bare "web" or "web.prod" also resolves.
type dnsServer struct {
	mu       sync.RWMutex
	domain   string
	records  map[string][]net.IP // lower-case fqdn without trailing dot
	upstream []string

	conn *net.UDPConn
	addr string
}

func newDNSServer() *dnsServer {
	return &dnsServer{records: map[string][]net.IP{}, upstream: hostResolvers()}
}

// hostResolvers reads the host's nameservers (systemd-resolved's stub is fine:
// the agent runs on the host).
func hostResolvers() []string {
	var out []string
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "nameserver" && net.ParseIP(fields[1]).To4() != nil {
				out = append(out, net.JoinHostPort(fields[1], "53"))
			}
		}
		f.Close()
		if len(out) > 0 {
			return out
		}
	}
	return []string{"1.1.1.1:53", "8.8.8.8:53"}
}

// Listen (re)binds the server to ip:53.
func (d *dnsServer) Listen(ip string) error {
	addr := net.JoinHostPort(ip, "53")
	d.mu.Lock()
	if d.addr == addr && d.conn != nil {
		d.mu.Unlock()
		return nil
	}
	if d.conn != nil {
		d.conn.Close()
	}
	d.mu.Unlock()
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.conn, d.addr = conn, addr
	d.mu.Unlock()
	log.Printf("dns: serving cluster names on %s", addr)
	go d.serve(conn)
	return nil
}

func (d *dnsServer) SetRecords(domain string, recs map[string][]net.IP) {
	d.mu.Lock()
	d.domain, d.records = strings.ToLower(domain), recs
	d.mu.Unlock()
}

func (d *dnsServer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

func (d *dnsServer) serve(conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := d.handle(q); resp != nil {
				conn.WriteToUDP(resp, from)
			}
		}()
	}
}

const (
	typeA    = 1
	typeAAAA = 28
	classIN  = 1
)

// handle builds the reply for one query packet.
func (d *dnsServer) handle(q []byte) []byte {
	if len(q) < 12 || q[2]&0x80 != 0 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return nil
	}
	name, end, ok := readName(q, 12)
	if !ok || end+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[end:])
	question := q[12 : end+4]
	lname := strings.ToLower(name)

	d.mu.RLock()
	domain := d.domain
	ips, found := d.records[lname]
	d.mu.RUnlock()

	if domain == "" || (lname != domain && !strings.HasSuffix(lname, "."+domain)) {
		return d.forward(q)
	}
	// Authoritative answer for our zone.
	resp := make([]byte, 12, 12+len(question)+16)
	copy(resp, q[:2])
	flags := uint16(0x8000 | 0x0400 | 0x0080) // QR, AA, RA
	flags |= binary.BigEndian.Uint16(q[2:4]) & 0x0100
	if !found {
		flags |= 3 // NXDOMAIN
	}
	binary.BigEndian.PutUint16(resp[2:], flags)
	binary.BigEndian.PutUint16(resp[4:], 1)
	resp = append(resp, question...)
	if found && qtype == typeA {
		// Rotate the answer order so clients spread over headless backends.
		start := 0
		if len(ips) > 1 {
			start = int(binary.BigEndian.Uint16(q[:2])) % len(ips)
		}
		binary.BigEndian.PutUint16(resp[6:], uint16(len(ips)))
		for i := range ips {
			rr := []byte{0xC0, 0x0C, 0, typeA, 0, classIN, 0, 0, 0, 5, 0, 4}
			resp = append(resp, rr...)
			resp = append(resp, ips[(start+i)%len(ips)].To4()...)
		}
	}
	return resp
}

func readName(b []byte, off int) (string, int, bool) {
	var labels []string
	for off < len(b) {
		l := int(b[off])
		if l == 0 {
			return strings.Join(labels, "."), off + 1, true
		}
		if l&0xC0 != 0 || off+1+l > len(b) {
			return "", 0, false // compression is not used in questions
		}
		labels = append(labels, string(b[off+1:off+1+l]))
		off += 1 + l
	}
	return "", 0, false
}

func (d *dnsServer) forward(q []byte) []byte {
	d.mu.RLock()
	ups := d.upstream
	d.mu.RUnlock()
	buf := make([]byte, 4096)
	for _, up := range ups {
		c, err := net.DialTimeout("udp", up, 2*time.Second)
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write(q); err == nil {
			if n, err := c.Read(buf); err == nil {
				c.Close()
				return buf[:n]
			}
		}
		c.Close()
	}
	// SERVFAIL
	if len(q) < 12 {
		return nil
	}
	resp := append([]byte(nil), q...)
	resp[2] |= 0x80
	resp[3] = (resp[3] & 0xF0) | 2
	return resp
}
