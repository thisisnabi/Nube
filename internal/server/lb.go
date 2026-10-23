package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/proxy"
	"github.com/thisisnabi/Nube/internal/store"
)

// loadBalancer exposes every service port on this server. It runs on every
// control-plane server (not only the leader), so any server can take traffic.
type loadBalancer struct {
	s   *Server
	mgr *proxy.Manager
}

func newLoadBalancer(s *Server) *loadBalancer {
	return &loadBalancer{s: s, mgr: proxy.NewManager("service-lb")}
}

func (lb *loadBalancer) run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		lb.sync()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// bindIPs returns the addresses service ports listen on. With the default
// 0.0.0.0 it binds every local IPv4 address *except* the pod network, so an
// agent on the same machine can still bind its in-cluster service proxy on
// the bridge gateway (a wildcard bind would make that port unavailable).
func (lb *loadBalancer) bindIPs() []string {
	if lb.s.cfg.ServiceBind != "0.0.0.0" && lb.s.cfg.ServiceBind != "" {
		return []string{lb.s.cfg.ServiceBind}
	}
	_, cluster, _ := net.ParseCIDR(lb.s.cfg.ClusterCIDR)
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{"0.0.0.0"}
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || (cluster != nil && cluster.Contains(ipn.IP)) {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	if len(out) == 0 {
		return []string{"0.0.0.0"}
	}
	return out
}

func (lb *loadBalancer) sync() {
	var targets []proxy.Target
	ips := lb.bindIPs()
	lb.s.store.Read(func(st *store.State) {
		for _, key := range sortedKeys(st.Services) {
			svc := st.Services[key]
			if svc.Spec.Internal || svc.Spec.Headless {
				continue // cluster-internal only
			}
			pods := serviceEndpoints(st, svc)
			for _, sp := range svc.Spec.Ports {
				var backends []string
				for _, p := range pods {
					if hp := hostPortFor(p, sp); hp > 0 && p.Status.HostIP != "" {
						backends = append(backends, net.JoinHostPort(p.Status.HostIP, strconv.Itoa(hp)))
					}
				}
				for _, ip := range ips {
					targets = append(targets, proxy.Target{
						Name:     portName(key, sp),
						Addr:     net.JoinHostPort(ip, strconv.Itoa(sp.Port)),
						UDP:      sp.IsUDP(),
						Backends: backends,
					})
				}
			}
		}
	})
	lb.mgr.Sync(targets)
}

func (lb *loadBalancer) close() { lb.mgr.Close() }

// decorate fills runtime listener info into copies of the services.
func (lb *loadBalancer) decorate(svcs []*api.Service) []*api.Service {
	// One service port may have several listeners (one per host address).
	stats := map[string]proxy.Status{}
	for _, s := range lb.mgr.Statuses() {
		agg, seen := stats[s.Name]
		if !seen {
			stats[s.Name] = s
			continue
		}
		agg.Conns += s.Conns
		agg.Listening = agg.Listening || s.Listening
		if agg.Error == "" {
			agg.Error = s.Error
		}
		stats[s.Name] = agg
	}
	out := make([]*api.Service, len(svcs))
	for i, svc := range svcs {
		c := *svc
		out[i] = &c
		if svc.Spec.Internal || svc.Spec.Headless {
			continue // no listener on the servers by design
		}
		c.Status.Listening = len(svc.Spec.Ports) > 0
		c.Status.Conns = 0
		for _, sp := range svc.Spec.Ports {
			s, ok := stats[portName(svc.Metadata.Key(), sp)]
			if !ok || !s.Listening {
				c.Status.Listening = false
				if ok && s.Error != "" {
					c.Status.Message = "listen error: " + s.Error
				}
			}
			c.Status.Conns += s.Conns
		}
		out[i] = &c
	}
	return out
}

func portName(key string, sp api.ServicePort) string {
	if sp.IsUDP() {
		return fmt.Sprintf("%s:%d/udp", key, sp.Port)
	}
	return fmt.Sprintf("%s:%d", key, sp.Port)
}

// hostPortFor returns the published host port of a pod for a service port.
func hostPortFor(p *api.Pod, sp api.ServicePort) int {
	if sp.IsUDP() {
		return p.Status.HostPortsUDP[sp.TargetPort]
	}
	return p.Status.HostPorts[sp.TargetPort]
}
