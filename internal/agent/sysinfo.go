package agent

import (
	"bufio"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

// cpuSampler turns /proc/stat counters into millicores in use.
type cpuSampler struct {
	mu          sync.Mutex
	prevBusy    uint64
	prevTotal   uint64
	initialized bool
}

func (c *cpuSampler) sample(cores int) int64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}
	var total, idle uint64
	for i, s := range fields[1:] {
		v, _ := strconv.ParseUint(s, 10, 64)
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	busy := total - idle
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.prevBusy, c.prevTotal, c.initialized = busy, total, true }()
	if !c.initialized || total <= c.prevTotal {
		return 0
	}
	return int64(float64(busy-c.prevBusy) / float64(total-c.prevTotal) * float64(cores) * 1000)
}

// memUsedMiB returns MemTotal - MemAvailable from /proc/meminfo.
func memUsedMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	var total, avail int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	return (total - avail) / 1024
}

// outboundIP finds the local address used to reach the server.
func outboundIP(serverURL string) string {
	host := serverURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host = strings.SplitN(host, "/", 2)[0]
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := localAddrFor(host)
	if ip == nil || ip.IsLoopback() {
		// Server on the same machine: use the primary interface instead, so
		// other nodes can reach us. (UDP "dial" sends no packets.)
		if alt := localAddrFor("1.1.1.1"); alt != nil {
			ip = alt
		}
	}
	if ip == nil {
		return "127.0.0.1"
	}
	return ip.String()
}

func localAddrFor(host string) net.IP {
	c, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}
