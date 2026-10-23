package proxy

import (
	"net"
	"testing"
	"time"
)

func TestUDPProxy(t *testing.T) {
	// Echo server prefixing replies, so we know which backend answered.
	echo := func(prefix string) string {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			buf := make([]byte, 1500)
			for {
				n, from, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				pc.WriteTo(append([]byte(prefix), buf[:n]...), from)
			}
		}()
		t.Cleanup(func() { pc.Close() })
		return pc.LocalAddr().String()
	}
	a, b := echo("a:"), echo("b:")

	free, _ := net.ListenPacket("udp", "127.0.0.1:0")
	addr := free.LocalAddr().String()
	free.Close()

	m := NewManager("test")
	defer m.Close()
	m.Sync([]Target{{Name: "svc", Addr: addr, UDP: true, Backends: []string{a, b}}})

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		c, err := net.Dial("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		c.Write([]byte("ping"))
		buf := make([]byte, 100)
		n, err := c.Read(buf)
		c.Close()
		if err != nil {
			t.Fatalf("no reply through UDP proxy: %v", err)
		}
		reply := string(buf[:n])
		if reply != "a:ping" && reply != "b:ping" {
			t.Fatalf("unexpected reply %q", reply)
		}
		seen[reply[:1]] = true
	}
	if len(seen) != 2 {
		t.Fatalf("expected both backends to be used, saw %v", seen)
	}
}
