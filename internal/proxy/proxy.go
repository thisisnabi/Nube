// Package proxy is a small round-robin TCP/UDP load balancer with
// hot-swappable backends. The server uses it for external service ports and
// every agent uses it for in-cluster service addresses.
package proxy

import (
	"io"
	"log"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// udpIdleTimeout ends a UDP "session" (client addr -> backend) after inactivity.
const udpIdleTimeout = 60 * time.Second

// Target is one listening address and the backends it balances across.
type Target struct {
	Name     string   // display name, e.g. "default/web:8080"
	Addr     string   // listen address, e.g. "0.0.0.0:8080"
	UDP      bool     // UDP instead of TCP
	Backends []string // "host:port", tried in round-robin order
}

func (t Target) key() string {
	if t.UDP {
		return "udp/" + t.Addr
	}
	return "tcp/" + t.Addr
}

type Status struct {
	Name      string `json:"name"`
	Addr      string `json:"addr"`
	UDP       bool   `json:"udp,omitempty"`
	Listening bool   `json:"listening"`
	Error     string `json:"error,omitempty"`
	Backends  int    `json:"backends"`
	Conns     int64  `json:"connections"` // total accepted (UDP: sessions)
	Active    int64  `json:"active"`
}

type Manager struct {
	mu        sync.Mutex
	listeners map[string]*listener
	logPrefix string
}

func NewManager(logPrefix string) *Manager {
	return &Manager{listeners: map[string]*listener{}, logPrefix: logPrefix}
}

type listener struct {
	name     string
	addr     string
	udp      bool
	ln       net.Listener
	pc       net.PacketConn
	err      string
	mu       sync.RWMutex
	backends []string
	rr       atomic.Uint64
	conns    atomic.Int64
	active   atomic.Int64
	lastTry  time.Time
}

func (l *listener) open() bool { return l.ln != nil || l.pc != nil }

func (l *listener) close() {
	if l.ln != nil {
		l.ln.Close()
	}
	if l.pc != nil {
		l.pc.Close()
	}
}

// Sync makes the set of listeners match targets exactly.
func (m *Manager) Sync(targets []Target) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]Target{}
	for _, t := range targets {
		want[t.key()] = t
	}
	for k, l := range m.listeners {
		if _, ok := want[k]; !ok {
			l.close()
			delete(m.listeners, k)
		}
	}
	for k, t := range want {
		l := m.listeners[k]
		if l == nil {
			l = &listener{name: t.Name, addr: t.Addr, udp: t.UDP}
			m.listeners[k] = l
		}
		l.name = t.Name
		l.mu.Lock()
		l.backends = append([]string(nil), t.Backends...)
		l.mu.Unlock()
		if l.open() || time.Since(l.lastTry) < 5*time.Second {
			continue
		}
		l.lastTry = time.Now()
		var err error
		if t.UDP {
			l.pc, err = net.ListenPacket("udp", t.Addr)
		} else {
			l.ln, err = net.Listen("tcp", t.Addr)
		}
		if err != nil {
			if l.err != err.Error() {
				log.Printf("%s: cannot listen on %s for %s: %v", m.logPrefix, k, t.Name, err)
			}
			l.err = err.Error()
			continue
		}
		l.err = ""
		if t.UDP {
			go l.serveUDP()
		} else {
			go l.serveTCP()
		}
	}
}

// Statuses returns the state of every listener, sorted by name.
func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.listeners))
	for _, l := range m.listeners {
		l.mu.RLock()
		nb := len(l.backends)
		l.mu.RUnlock()
		out = append(out, Status{
			Name: l.name, Addr: l.addr, UDP: l.udp, Listening: l.open(), Error: l.err,
			Backends: nb, Conns: l.conns.Load(), Active: l.active.Load(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) Close() {
	m.Sync(nil)
}

func (l *listener) pick() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.backends) == 0 {
		return nil
	}
	start := int(l.rr.Add(1))
	out := make([]string, len(l.backends))
	for i := range l.backends {
		out[i] = l.backends[(start+i)%len(l.backends)]
	}
	return out
}

// ---------------------------------------------------------------- TCP

func (l *listener) serveTCP() {
	ln := l.ln
	for {
		c, err := ln.Accept()
		if err != nil {
			return // closed
		}
		l.conns.Add(1)
		go l.handleTCP(c)
	}
}

func (l *listener) handleTCP(c net.Conn) {
	defer c.Close()
	var up net.Conn
	for _, addr := range l.pick() {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			up = conn
			break
		}
	}
	if up == nil {
		return
	}
	defer up.Close()
	l.active.Add(1)
	defer l.active.Add(-1)
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	<-done
	<-done
}

// ---------------------------------------------------------------- UDP

// serveUDP keeps one upstream socket per client address, so replies find
// their way back and a client sticks to one backend while it is active.
func (l *listener) serveUDP() {
	pc := l.pc
	var mu sync.Mutex
	sessions := map[string]*net.UDPConn{}
	buf := make([]byte, 64*1024)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			mu.Lock()
			for _, up := range sessions {
				up.Close()
			}
			mu.Unlock()
			return // closed
		}
		key := client.String()
		mu.Lock()
		up := sessions[key]
		mu.Unlock()
		if up == nil {
			for _, addr := range l.pick() {
				ra, err := net.ResolveUDPAddr("udp", addr)
				if err != nil {
					continue
				}
				if up, err = net.DialUDP("udp", nil, ra); err == nil {
					break
				}
			}
			if up == nil {
				continue
			}
			mu.Lock()
			sessions[key] = up
			mu.Unlock()
			l.conns.Add(1)
			l.active.Add(1)
			go func(up *net.UDPConn, client net.Addr) {
				defer func() {
					mu.Lock()
					delete(sessions, key)
					mu.Unlock()
					up.Close()
					l.active.Add(-1)
				}()
				rb := make([]byte, 64*1024)
				for {
					up.SetReadDeadline(time.Now().Add(udpIdleTimeout))
					m, err := up.Read(rb)
					if err != nil {
						return
					}
					pc.WriteTo(rb[:m], client)
				}
			}(up, client)
		}
		up.Write(buf[:n])
	}
}
