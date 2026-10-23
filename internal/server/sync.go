package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/pki"
	"github.com/thisisnabi/Nube/internal/store"
)

// handleSync is the agent heartbeat: the agent reports its node and pods and
// gets back the pods it should run plus network/service routing information.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req api.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid sync request: "+err.Error())
		return
	}
	if !api.ValidName(req.Node.Name) {
		writeErr(w, 400, "invalid node name "+req.Node.Name)
		return
	}
	name := req.Node.Name
	err := s.store.Write(func(st *store.State) error {
		now := time.Now()
		n := st.Nodes[name]
		if n == nil {
			n = &api.Node{Name: name, JoinedAt: now, Labels: map[string]string{}}
			st.Nodes[name] = n
			st.Event("Normal", api.KindNode, "", name, "NodeJoined", fmt.Sprintf("node joined from %s (%d cores, %d MiB)", req.Node.HostIP, req.Node.Capacity.CPU/1000, req.Node.Capacity.Memory))
		}
		if n.Status != api.NodeReady {
			if !n.LastHeartbeat.IsZero() {
				st.Event("Normal", api.KindNode, "", name, "NodeReady", "node is reporting again")
			}
			n.Status = api.NodeReady
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		for k, v := range req.Node.Labels {
			n.Labels[k] = v
		}
		n.Labels["nube/hostname"] = name
		n.Address, n.HostIP = req.Node.Address, req.Node.HostIP
		n.Capacity, n.Usage, n.Info = req.Node.Capacity, req.Node.Usage, req.Node.Info
		n.NetworkMode, n.WGPublicKey, n.WGPort = req.Node.NetworkMode, req.Node.WGPublicKey, req.Node.WGPort
		n.NetworkReady, n.NetworkError, n.TLS = req.Node.NetworkReady, req.Node.NetworkError, req.Node.TLS
		n.LastHeartbeat = now
		if n.PodCIDR == "" {
			cidr, err := allocatePodCIDR(st, s.cfg.ClusterCIDR)
			if err != nil {
				return err
			}
			n.PodCIDR = cidr
			st.Event("Normal", api.KindNode, "", name, "PodCIDR", "allocated pod subnet "+cidr)
		}
		for _, rep := range req.Pods {
			applyReport(st, n, rep, now)
		}
		for _, vr := range req.Volumes {
			if v := st.Volumes[vr.Namespace+"/"+vr.Name]; v != nil && (v.Status.NodeName == name || v.Spec.Type != "local") {
				v.Status.UsedMiB, v.Status.CapacityMiB, v.Status.Enforced, v.Status.Message = vr.UsedMiB, vr.CapacityMiB, vr.Enforced, vr.Message
			}
		}
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var resp api.SyncResponse
	s.store.Read(func(st *store.State) { resp = s.buildSyncResponse(st, name) })
	if req.CSR != "" {
		caCert, caKey := s.ca()
		if caCert != "" {
			cert, err := pki.SignCSR(caCert, caKey, req.CSR, name, req.Node.HostIP)
			if err != nil {
				log.Printf("tls: cannot sign certificate for node %s: %v", name, err)
			} else {
				resp.Certificate = cert
			}
		}
	}
	if caCert, _ := s.ca(); caCert != "" {
		resp.CA = caCert
	}
	writeJSON(w, 200, resp)
}

// allocatePodCIDR hands out the lowest free /24 inside the cluster /16.
func allocatePodCIDR(st *store.State, clusterCIDR string) (string, error) {
	_, cn, _ := net.ParseCIDR(clusterCIDR)
	base := cn.IP.To4()
	used := map[string]bool{}
	for _, n := range st.Nodes {
		used[n.PodCIDR] = true
	}
	for i := 1; i < 255; i++ {
		c := fmt.Sprintf("%d.%d.%d.0/24", base[0], base[1], i)
		if !used[c] {
			return c, nil
		}
	}
	return "", fmt.Errorf("pod network %s is full", clusterCIDR)
}

func applyReport(st *store.State, n *api.Node, rep api.PodReport, now time.Time) {
	p := st.Pods[rep.Namespace+"/"+rep.Name]
	if p == nil || p.Metadata.UID != rep.UID || p.Spec.NodeName != n.Name {
		return // stale: the agent removes containers that are no longer assigned
	}
	if p.Status.Phase == api.PodTerminating {
		if rep.Phase == api.PodTerminated {
			delete(st.Pods, p.Metadata.Key())
			st.Event("Normal", api.KindPod, p.Metadata.Namespace, p.Metadata.Name, "Deleted", "pod stopped and removed")
			return
		}
		p.Status.CPU, p.Status.Memory = rep.CPU, rep.Memory
		return
	}
	ps := &p.Status
	prevPhase, prevReady, prevRestarts := ps.Phase, ps.Ready, ps.Restarts
	ps.Phase, ps.Ready, ps.Message = rep.Phase, rep.Ready, rep.Message
	ps.ContainerID, ps.Containers = rep.ContainerID, rep.Containers
	ps.HostPorts, ps.HostPortsUDP, ps.Restarts, ps.ExitCode = rep.HostPorts, rep.HostPortsUDP, rep.Restarts, rep.ExitCode
	ps.CPU, ps.Memory, ps.StartedAt, ps.FinishedAt = rep.CPU, rep.Memory, rep.StartedAt, rep.FinishedAt
	ps.HostIP = n.HostIP
	if rep.PodIP != "" {
		ps.PodIP = rep.PodIP
	}
	if prevPhase != ps.Phase {
		p.Metadata.UpdatedAt = now
	}
	ns, name := p.Metadata.Namespace, p.Metadata.Name
	switch {
	case ps.Phase == api.PodCrashLoop && prevPhase != api.PodCrashLoop:
		st.Event("Warning", api.KindPod, ns, name, "BackOff", firstNonEmpty(rep.Message, "container keeps exiting"))
	case ps.Phase == api.PodFailed && prevPhase != api.PodFailed:
		st.Event("Warning", api.KindPod, ns, name, "Failed", rep.Message)
	case ps.Phase == api.PodSucceeded && prevPhase != api.PodSucceeded:
		st.Event("Normal", api.KindPod, ns, name, "Completed", "all containers exited successfully")
	case ps.Ready && !prevReady:
		st.Event("Normal", api.KindPod, ns, name, "Ready", "pod is running and ready on "+n.Name)
	case !ps.Ready && prevReady && ps.Phase == api.PodRunning:
		st.Event("Warning", api.KindPod, ns, name, "Unhealthy", firstNonEmpty(rep.Message, "health check failing"))
	}
	if ps.Restarts > prevRestarts && ps.Phase != api.PodCrashLoop {
		st.Event("Warning", api.KindPod, ns, name, "Restarted", fmt.Sprintf("container restarted (%d total)", ps.Restarts))
	}
}

func (s *Server) buildSyncResponse(st *store.State, nodeName string) api.SyncResponse {
	resp := api.SyncResponse{PodIPs: map[string]string{}, Records: map[string]string{}}
	node := st.Nodes[nodeName]

	// Pods for this node.
	for _, key := range sortedKeys(st.Pods) {
		p := st.Pods[key]
		if p.Spec.NodeName != nodeName {
			continue
		}
		ap := api.AssignedPod{Pod: *p, Env: map[string]map[string]string{}}
		for _, c := range append(p.Spec.Template.AllContainers(), p.Spec.Template.InitContainers...) {
			ap.Env[c.Name] = s.resolveEnv(st, p, c)
		}
		if missing := missingRefs(st, p); len(missing) > 0 {
			ap.Blocked = "waiting for " + strings.Join(missing, ", ")
		}
		for _, vm := range p.Spec.Template.AllVolumeMounts() {
			if vm.Volume == "" {
				continue
			}
			if v := st.Volumes[p.Metadata.Namespace+"/"+vm.Volume]; v != nil {
				if ap.Volumes == nil {
					ap.Volumes = map[string]api.Volume{}
				}
				ap.Volumes[vm.Volume] = *v
			}
		}
		resp.Pods = append(resp.Pods, ap)
	}

	// Network: our subnet and every other node as a peer.
	resp.Network = api.NetworkConfig{ClusterCIDR: s.cfg.ClusterCIDR, Domain: s.cfg.Domain}
	if node != nil {
		resp.Network.PodCIDR = node.PodCIDR
	}
	for _, name := range sortedKeys(st.Nodes) {
		n := st.Nodes[name]
		if name == nodeName || n.PodCIDR == "" || n.HostIP == "" {
			continue
		}
		resp.Network.Peers = append(resp.Network.Peers, api.NetworkPeer{
			Node: name, HostIP: n.HostIP, PodCIDR: n.PodCIDR, WGPublicKey: n.WGPublicKey, WGPort: n.WGPort,
		})
	}

	// Services and pod DNS records.
	for _, key := range sortedKeys(st.Services) {
		svc := st.Services[key]
		route := api.ServiceRoute{Namespace: svc.Metadata.Namespace, Name: svc.Metadata.Name, Ports: svc.Spec.Ports, Headless: svc.Spec.Headless}
		for _, p := range serviceEndpoints(st, svc) {
			route.Endpoints = append(route.Endpoints, api.PodEndpoint{
				Pod: p.Metadata.Name, Node: p.Spec.NodeName, PodIP: p.Status.PodIP, HostIP: p.Status.HostIP,
				HostPorts: p.Status.HostPorts, HostPortsUDP: p.Status.HostPortsUDP,
			})
		}
		resp.Services = append(resp.Services, route)
	}
	for _, p := range st.Pods {
		if p.Status.PodIP == "" || p.Status.Phase == api.PodTerminating || p.Finished() {
			continue
		}
		resp.PodIPs[p.Metadata.Name+"."+p.Metadata.Namespace] = p.Status.PodIP
		if p.Spec.Subdomain != "" {
			// StatefulSet identity: db-0.db.default.svc.<domain>
			resp.Records[p.Metadata.Name+"."+p.Spec.Subdomain+"."+p.Metadata.Namespace+".svc"] = p.Status.PodIP
		}
	}
	return resp
}

// resolveEnv merges service-discovery variables, envFrom sources and the
// container's own env into its final environment.
func (s *Server) resolveEnv(st *store.State, p *api.Pod, c api.Container) map[string]string {
	ns := p.Metadata.Namespace
	env := map[string]string{}
	for _, svc := range st.Services {
		if svc.Metadata.Namespace != ns || len(svc.Spec.Ports) == 0 {
			continue
		}
		prefix := envName(svc.Metadata.Name)
		env[prefix+"_SERVICE_HOST"] = svc.Metadata.Name + "." + ns + ".svc." + s.cfg.Domain
		env[prefix+"_SERVICE_PORT"] = fmt.Sprint(svc.Spec.Ports[0].Port)
	}
	for _, src := range c.EnvFrom {
		if src.ConfigMap != "" {
			if cm := st.ConfigMaps[ns+"/"+src.ConfigMap]; cm != nil {
				for k, v := range cm.Data {
					env[k] = v
				}
			}
		}
		if src.Secret != "" {
			if sec := st.Secrets[ns+"/"+src.Secret]; sec != nil {
				for k, v := range sec.Data {
					env[k] = v
				}
			}
		}
	}
	for k, v := range c.Env {
		env[k] = v
	}
	env["NUBE_POD_NAME"] = p.Metadata.Name
	env["NUBE_NAMESPACE"] = ns
	env["NUBE_NODE_NAME"] = p.Spec.NodeName
	env["NUBE_CONTAINER_NAME"] = c.Name
	return env
}

func envName(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, "-", "_"))
}

// missingRefs lists envFrom references that do not exist (shown on the pod).
func missingRefs(st *store.State, p *api.Pod) []string {
	seen := map[string]bool{}
	ns := p.Metadata.Namespace
	for _, c := range append(p.Spec.Template.AllContainers(), p.Spec.Template.InitContainers...) {
		for _, src := range c.EnvFrom {
			if src.ConfigMap != "" && st.ConfigMaps[ns+"/"+src.ConfigMap] == nil {
				seen["configmap/"+src.ConfigMap] = true
			}
			if src.Secret != "" && st.Secrets[ns+"/"+src.Secret] == nil {
				seen["secret/"+src.Secret] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
