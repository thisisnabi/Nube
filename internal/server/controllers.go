package server

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

const (
	nodeHeartbeatTimeout = 15 * time.Second // Ready -> NotReady
	podEvictionTimeout   = 40 * time.Second // pods on a NotReady node are rescheduled
	terminatingTimeout   = 90 * time.Second // force-remove pods stuck in Terminating
	autoscaleInterval    = 15 * time.Second
	scaleDownCooldown    = 2 * time.Minute
	maxHistory           = 10
)

// controllerLoop runs the reconcilers once per second on the leader.
func (s *Server) controllerLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastAutoscale, lastSample time.Time
	wasLeader := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !s.store.IsLeader() {
			wasLeader = false
			continue
		}
		if !wasLeader {
			wasLeader = true
			log.Printf("controllers: this server is the leader")
			var names []string
			s.store.Read(func(st *store.State) {
				for n := range st.Nodes {
					names = append(names, n)
				}
			})
			s.live.grace(names, time.Now()) // no live heartbeats yet: don't evict everything
			s.ensureMembership()
			if s.cfg.TLS {
				s.ensureCA()
			}
		}
		doAutoscale := time.Since(lastAutoscale) >= autoscaleInterval
		if doAutoscale {
			lastAutoscale = time.Now()
		}
		err := s.store.Write(func(st *store.State) error {
			now := time.Now()
			reconcileNodes(st, now, s.live.lastSeen)
			reconcileTerminating(st, now)
			for _, key := range sortedKeys(st.Deployments) {
				d := st.Deployments[key]
				if doAutoscale {
					autoscale(st, d, now, func(p *api.Pod) int64 { return s.live.podCPU(p.Metadata.Key()) })
				}
				reconcileDeployment(st, d, now)
			}
			for _, key := range sortedKeys(st.CronJobs) {
				reconcileCronJob(st, st.CronJobs[key], now)
			}
			for _, key := range sortedKeys(st.Jobs) {
				if j := st.Jobs[key]; j != nil {
					reconcileJob(st, j, now)
				}
			}
			for _, key := range sortedKeys(st.StatefulSets) {
				reconcileStatefulSet(st, st.StatefulSets[key], now)
			}
			for _, key := range sortedKeys(st.DaemonSets) {
				reconcileDaemonSet(st, st.DaemonSets[key], now)
			}
			reconcileBackups(st, now)
			reconcileSnapshots(st, now)
			reconcileMigrations(st, now, s.live.lastSeen)
			schedulePending(st, now)
			computeAllocations(st)
			reconcileVolumes(st)
			reconcileServiceStatus(st)
			reconcileIngressStatus(st)
			countCRDObjects(st)
			countPolicyPods(st)
			return nil
		})
		if err != nil && err != store.ErrNotLeader {
			log.Printf("controllers: %v", err)
		}
		if time.Since(lastSample) >= time.Minute {
			lastSample = time.Now()
			s.store.Read(func(st *store.State) {
				pods := make([]*api.Pod, 0, len(st.Pods))
				for _, p := range st.Pods {
					pods = append(pods, p)
				}
				s.live.sample(pods)
			})
		}
	}
}

// ensureMembership records this server in the replicated member list (HA mode).
func (s *Server) ensureMembership() {
	if !s.cfg.HA {
		return
	}
	s.store.Write(func(st *store.State) error {
		m := st.Servers[s.cfg.ServerID]
		if m == nil {
			m = &api.ServerMember{ID: s.cfg.ServerID, JoinedAt: time.Now()}
			st.Servers[s.cfg.ServerID] = m
		}
		m.RaftAddr, m.APIAddr = s.cfg.RaftAdvertise, s.cfg.APIAdvertise
		st.Event("Normal", "Server", "", s.cfg.ServerID, "LeaderElected", "became control-plane leader")
		return nil
	})
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------- nodes

// reconcileNodes marks silent nodes NotReady and evicts their pods. seen
// returns a node's last heartbeat (nil: the persisted one).
func reconcileNodes(st *store.State, now time.Time, seen func(*api.Node) time.Time) {
	for _, n := range st.Nodes {
		last := n.LastHeartbeat
		if seen != nil {
			last = seen(n)
		}
		silent := now.Sub(last)
		if n.Status == api.NodeReady && silent > nodeHeartbeatTimeout {
			n.Status = api.NodeNotReady
			st.Event("Warning", api.KindNode, "", n.Name, "NodeNotReady", fmt.Sprintf("no heartbeat for %s", silent.Round(time.Second)))
		}
		if n.Status != api.NodeReady && silent > podEvictionTimeout {
			evicted := 0
			for key, p := range st.Pods {
				if p.Spec.NodeName == n.Name {
					delete(st.Pods, key)
					evicted++
				}
			}
			if evicted > 0 {
				st.Event("Warning", api.KindNode, "", n.Name, "PodsEvicted", fmt.Sprintf("node lost: %d pods will be rescheduled on other nodes", evicted))
			}
		}
	}
}

func reconcileTerminating(st *store.State, now time.Time) {
	for key, p := range st.Pods {
		if p.Status.Phase == api.PodTerminating && now.Sub(p.Status.DeletionAt) > terminatingTimeout {
			delete(st.Pods, key)
		}
	}
}

// computeAllocations sums the resource requests and pod counts per node.
func computeAllocations(st *store.State) {
	for _, n := range st.Nodes {
		n.Allocated = api.Resources{}
		n.PodCount = 0
	}
	for _, p := range st.Pods {
		if p.Finished() {
			continue // completed job pods hold no resources
		}
		if n := st.Nodes[p.Spec.NodeName]; n != nil {
			r := p.Spec.Template.Requests()
			n.Allocated.CPU += r.CPU
			n.Allocated.Memory += r.Memory
			n.PodCount++
		}
	}
}

// ---------------------------------------------------------------- scheduler

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func schedulePending(st *store.State, now time.Time) {
	computeAllocations(st)
	var pending []*api.Pod
	for _, p := range st.Pods {
		if p.Spec.NodeName == "" && p.Status.Phase != api.PodTerminating {
			pending = append(pending, p)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Metadata.CreatedAt.Before(pending[j].Metadata.CreatedAt) })
	for _, p := range pending {
		if wait := dependencyWait(st, p); wait != "" {
			if p.Status.Message != wait {
				p.Status.Message = wait
				st.Event("Normal", api.KindPod, p.Metadata.Namespace, p.Metadata.Name, "WaitingForDependency", wait)
			}
			continue
		}
		node, reason := pickNode(st, p)
		if node == nil {
			if p.Status.Message != reason {
				p.Status.Message = reason
				st.Event("Warning", api.KindPod, p.Metadata.Namespace, p.Metadata.Name, "FailedScheduling", reason)
			}
			continue
		}
		bindPod(st, p, node, now)
	}
}

// pickNode filters nodes by readiness, selectors, volumes, ports and free
// resources, then scores the survivors (more free capacity, fewer replicas of
// the same deployment = better).
func pickNode(st *store.State, p *api.Pod) (*api.Node, string) {
	req := p.Spec.Template.Requests()
	requiredNode := ""
	for _, vm := range p.Spec.Template.AllVolumeMounts() {
		if vm.Volume == "" {
			continue
		}
		v := st.Volumes[p.Metadata.Namespace+"/"+vm.Volume]
		if v == nil {
			return nil, fmt.Sprintf("volume %q not found", vm.Volume)
		}
		if m := v.Status.Migration; m != nil && (m.Phase == "Draining" || m.Phase == "Snapshotting") {
			return nil, fmt.Sprintf("volume %q is being moved to %s", vm.Volume, m.Target)
		}
		if !nodeBound(v) {
			continue
		}
		bound := v.Status.NodeName
		if bound == "" {
			bound = v.Spec.NodeName
		}
		if bound != "" {
			if requiredNode != "" && requiredNode != bound {
				return nil, "pod uses local volumes bound to different nodes"
			}
			requiredNode = bound
		}
	}

	var best *api.Node
	bestScore := math.Inf(-1)
	var reasons []string
	for _, name := range sortedKeys(st.Nodes) {
		n := st.Nodes[name]
		switch {
		case n.Status != api.NodeReady:
			reasons = append(reasons, name+": not ready")
			continue
		case n.Cordoned:
			reasons = append(reasons, name+": cordoned")
			continue
		case requiredNode != "" && name != requiredNode:
			reasons = append(reasons, name+": volume is on "+requiredNode)
			continue
		case !labelsMatch(n.Labels, p.Spec.NodeSelector):
			reasons = append(reasons, name+": nodeSelector mismatch")
			continue
		}
		freeCPU := n.Capacity.CPU - n.Allocated.CPU
		freeMem := n.Capacity.Memory - n.Allocated.Memory
		if n.Capacity.CPU > 0 && req.CPU > freeCPU {
			reasons = append(reasons, fmt.Sprintf("%s: insufficient cpu (%dm free)", name, freeCPU))
			continue
		}
		if n.Capacity.Memory > 0 && req.Memory > freeMem {
			reasons = append(reasons, fmt.Sprintf("%s: insufficient memory (%dMi free)", name, freeMem))
			continue
		}
		if conflict := hostPortConflict(st, p, name); conflict != "" {
			reasons = append(reasons, name+": "+conflict)
			continue
		}
		score := 0.0
		if n.Capacity.CPU > 0 {
			score += float64(freeCPU-req.CPU) / float64(n.Capacity.CPU)
		}
		if n.Capacity.Memory > 0 {
			score += float64(freeMem-req.Memory) / float64(n.Capacity.Memory)
		}
		siblings := 0
		for _, o := range st.Pods {
			if o.Spec.NodeName == name && o.Spec.Owner != "" && o.Spec.Owner == p.Spec.Owner && o.Spec.OwnerKind == p.Spec.OwnerKind && o.Metadata.Namespace == p.Metadata.Namespace {
				siblings++
			}
		}
		score -= float64(siblings) * 0.75 // spread replicas across nodes
		if score > bestScore {
			best, bestScore = n, score
		}
	}
	if best == nil {
		if len(st.Nodes) == 0 {
			return nil, "no nodes have joined the cluster yet"
		}
		return nil, "0/" + fmt.Sprint(len(st.Nodes)) + " nodes available: " + strings.Join(reasons, "; ")
	}
	return best, ""
}

func hostPortConflict(st *store.State, p *api.Pod, node string) string {
	for _, cp := range p.Spec.Template.AllPorts() {
		if cp.HostPort == 0 {
			continue
		}
		for _, o := range st.Pods {
			if o.Spec.NodeName != node || o == p || o.Finished() {
				continue
			}
			for _, op := range o.Spec.Template.AllPorts() {
				if op.HostPort == cp.HostPort && op.IsUDP() == cp.IsUDP() {
					return fmt.Sprintf("host port %d in use", cp.HostPort)
				}
			}
		}
	}
	return ""
}

func bindPod(st *store.State, p *api.Pod, n *api.Node, now time.Time) {
	p.Spec.NodeName = n.Name
	p.Status.Phase = api.PodScheduled
	p.Status.HostIP = n.HostIP
	p.Status.ScheduledAt = now
	p.Status.Message = ""
	r := p.Spec.Template.Requests()
	n.Allocated.CPU += r.CPU
	n.Allocated.Memory += r.Memory
	for _, vm := range p.Spec.Template.AllVolumeMounts() {
		if v := st.Volumes[p.Metadata.Namespace+"/"+vm.Volume]; v != nil && nodeBound(v) && v.Status.NodeName == "" {
			v.Status.NodeName, v.Status.Phase = n.Name, "Bound"
			st.Event("Normal", api.KindVolume, v.Metadata.Namespace, v.Metadata.Name, "Bound", "local volume bound to node "+n.Name)
		}
	}
	st.Event("Normal", api.KindPod, p.Metadata.Namespace, p.Metadata.Name, "Scheduled", "assigned to node "+n.Name)
}

// ---------------------------------------------------------------- deployments

func newPodFor(d *api.Deployment, hash string, now time.Time) *api.Pod {
	tmpl := deepCopy(d.Spec.Template)
	labels := map[string]string{}
	for k, v := range tmpl.Labels {
		labels[k] = v
	}
	labels["nube/deployment"] = d.Metadata.Name
	p := &api.Pod{
		Kind: api.KindPod,
		Metadata: api.Meta{
			Name: d.Metadata.Name + "-" + randSuffix(5), Namespace: d.Metadata.Namespace,
			UID: newUID(), Labels: labels, CreatedAt: now, UpdatedAt: now,
		},
		Spec: api.PodSpec{
			Owner: d.Metadata.Name, OwnerKind: api.KindDeployment, TemplateHash: hash, NodeSelector: d.Spec.NodeSelector, Template: tmpl,
		},
		Status: api.PodStatus{Phase: api.PodPending},
	}
	return p
}

// reconcileDeployment drives a deployment towards spec.replicas pods of the
// current template, doing a surge-based rolling update when the template changes.
func reconcileDeployment(st *store.State, d *api.Deployment, now time.Time) {
	ns, name := d.Metadata.Namespace, d.Metadata.Name
	config := configDigests(st, ns, &d.Spec.Template)
	hash := rolloutHash(&d.Spec.Template, config)
	if d.Status.CurrentHash != hash {
		rev := 0
		var hist []api.Revision
		var cur *api.Revision
		for i, h := range d.Status.History {
			rev = max(rev, h.Revision)
			if h.Hash == d.Status.CurrentHash {
				cur = &d.Status.History[i]
			}
			if h.Hash != hash {
				hist = append(hist, h)
			}
		}
		rev++
		cause := "created"
		if cur != nil {
			cause = rolloutCause(&cur.Template, cur.Config, &d.Spec.Template, config)
		}
		if d.Status.RollingBack {
			cause = "automatic rollback"
		}
		hist = append(hist, api.Revision{Revision: rev, Hash: hash, Template: deepCopy(d.Spec.Template), CreatedAt: now, Config: config, Cause: cause})
		if len(hist) > maxHistory {
			hist = hist[len(hist)-maxHistory:]
		}
		first := d.Status.CurrentHash == ""
		d.Status.History, d.Status.Revision, d.Status.CurrentHash = hist, rev, hash
		// A rollback is never rolled back itself.
		d.Status.RolloutStartedAt = time.Time{}
		if !first && !d.Status.RollingBack {
			d.Status.RolloutStartedAt = now
		}
		d.Status.RollingBack = false
		if !first {
			st.Event("Normal", api.KindDeployment, ns, name, "RolloutStarted", fmt.Sprintf("rolling out revision %d: %s", rev, cause))
		}
	}

	var newPods, oldPods []*api.Pod
	terminating := 0
	for _, p := range st.PodsOf(ns, name) {
		switch {
		case p.Status.Phase == api.PodTerminating:
			terminating++
		case p.Finished() && d.Spec.Template.RestartPolicy != "Never":
			// A replica that can no longer run is replaced.
			st.Event("Warning", api.KindPod, ns, p.Metadata.Name, "Replaced", "pod "+strings.ToLower(string(p.Status.Phase))+": "+p.Status.Message)
			terminatePod(st, p, false)
			terminating++
		case p.Spec.TemplateHash == hash:
			newPods = append(newPods, p)
		default:
			oldPods = append(oldPods, p)
		}
	}
	desired := d.Spec.Replicas
	strat := d.Spec.Strategy
	if strat == nil {
		strat = &api.Strategy{MaxSurge: 1}
	}
	countReady := func(ps []*api.Pod) int {
		n := 0
		for _, p := range ps {
			if p.Status.Ready {
				n++
			}
		}
		return n
	}

	if !d.Spec.Paused {
		maxTotal := desired
		if len(oldPods) > 0 {
			maxTotal = desired + strat.MaxSurge
		}
		// Scale up the new revision.
		for len(newPods) < desired && len(newPods)+len(oldPods) < maxTotal {
			p := newPodFor(d, hash, now)
			st.Pods[p.Metadata.Key()] = p
			newPods = append(newPods, p)
		}
		// Retire old pods without dropping below the availability floor.
		minAvailable := desired - strat.MaxUnavailable
		available := countReady(newPods) + countReady(oldPods)
		sort.Slice(oldPods, func(i, j int) bool { return !oldPods[i].Status.Ready && oldPods[j].Status.Ready })
		var keep []*api.Pod
		for _, p := range oldPods {
			if !p.Status.Ready {
				terminatePod(st, p, false)
				continue
			}
			if available-1 >= minAvailable || len(newPods) == 0 && desired == 0 {
				terminatePod(st, p, false)
				available--
				continue
			}
			keep = append(keep, p)
		}
		oldPods = keep
		// Scale down surplus pods of the current revision (unscheduled / not ready first).
		if len(newPods) > desired {
			sort.SliceStable(newPods, func(i, j int) bool { return podRank(newPods[i]) < podRank(newPods[j]) })
			for _, p := range newPods[desired:] {
				terminatePod(st, p, false)
			}
			newPods = newPods[:desired]
		}
	}

	autoRollback(st, d, newPods, now)

	readyNew, readyOld := countReady(newPods), countReady(oldPods)
	stat := &d.Status
	stat.Replicas = len(newPods) + len(oldPods)
	stat.UpdatedReplicas = len(newPods)
	stat.ReadyReplicas = readyNew + readyOld
	stat.AvailableReplicas = stat.ReadyReplicas
	prev := stat.Phase
	troubled := ""
	for _, p := range append(append([]*api.Pod{}, newPods...), oldPods...) {
		// Broken, or impossible to schedule; pulling an image or waiting for a
		// dependency is progress, not trouble.
		if r := failing(p); r != "" {
			troubled = p.Metadata.Name + ": " + r
			break
		}
		if p.Spec.NodeName == "" && p.Status.Message != "" && !strings.HasPrefix(p.Status.Message, "waiting for service") {
			troubled = p.Metadata.Name + ": " + p.Status.Message
			break
		}
	}
	switch {
	case d.Spec.Paused:
		stat.Phase, stat.Message = "Paused", "rollout paused"
	case readyNew == desired && len(oldPods) == 0 && len(newPods) == desired:
		stat.Phase, stat.Message = "Healthy", ""
		stat.RolloutStartedAt = time.Time{}
	case troubled != "":
		stat.Phase, stat.Message = "Degraded", troubled
	default:
		stat.Phase = "Progressing"
		stat.Message = fmt.Sprintf("%d/%d updated, %d ready", len(newPods), desired, stat.ReadyReplicas)
		if terminating > 0 {
			stat.Message += fmt.Sprintf(", %d terminating", terminating)
		}
	}
	if prev != stat.Phase {
		typ := "Normal"
		if stat.Phase == "Degraded" {
			typ = "Warning"
		}
		msg := "deployment is " + strings.ToLower(stat.Phase)
		if stat.Message != "" {
			msg += ": " + stat.Message
		}
		st.Event(typ, api.KindDeployment, ns, name, stat.Phase, msg)
	}
}

// podRank orders pods for removal: unscheduled < not ready < ready; newer first.
func podRank(p *api.Pod) int {
	switch {
	case p.Spec.NodeName == "":
		return 0
	case !p.Status.Ready:
		return 1
	}
	return 2
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------------------------------------------------------------- autoscaler

// autoscale adjusts replicas so that average CPU use per pod approaches
// targetCPUPercent of its CPU request (the same formula as the Kubernetes HPA).
func autoscale(st *store.State, d *api.Deployment, now time.Time, cpu func(*api.Pod) int64) {
	a := d.Spec.Autoscale
	if a == nil {
		return
	}
	req := d.Spec.Template.Requests().CPU
	if req <= 0 {
		req = 1000 // without a request, treat one core as 100%
	}
	var used int64
	ready := 0
	for _, p := range st.PodsOf(d.Metadata.Namespace, d.Metadata.Name) {
		if p.Status.Ready && p.Status.Phase != api.PodTerminating {
			if cpu != nil {
				used += cpu(p)
			} else {
				used += p.Status.CPU
			}
			ready++
		}
	}
	if ready == 0 {
		d.Status.CPUPercent = 0
		if d.Spec.Replicas < a.Min {
			d.Spec.Replicas = a.Min
		}
		return
	}
	util := float64(used) / float64(int64(ready)*req) * 100
	d.Status.CPUPercent = int(math.Round(util))
	ratio := util / float64(a.TargetCPUPercent)
	if math.Abs(ratio-1) < 0.1 { // tolerance band, avoids flapping
		return
	}
	want := clamp(int(math.Ceil(float64(ready)*ratio)), a.Min, a.Max)
	cur := d.Spec.Replicas
	if want == cur {
		return
	}
	if want < cur && now.Sub(d.Status.LastScaleTime) < scaleDownCooldown {
		return
	}
	d.Spec.Replicas = want
	d.Status.LastScaleTime = now
	st.Event("Normal", api.KindDeployment, d.Metadata.Namespace, d.Metadata.Name, "Autoscaled",
		fmt.Sprintf("cpu at %d%% of request (target %d%%): replicas %d -> %d", d.Status.CPUPercent, a.TargetCPUPercent, cur, want))
}

// ---------------------------------------------------------------- volumes & services

// nodeBound reports whether a volume can only be used on one node at a time.
func nodeBound(v *api.Volume) bool {
	return v.Spec.Type == "local" || (v.Spec.Type == "nfs" && v.Spec.AccessMode == "ReadWriteOnce")
}

func reconcileVolumes(st *store.State) {
	for _, v := range st.Volumes {
		users := volumeUsers(st, v.Metadata.Namespace, v.Metadata.Name)
		sort.Strings(users)
		v.Status.UsedBy = users
		switch {
		case v.Spec.Type == "nfs" && v.Spec.AccessMode == "ReadWriteOnce":
			// Released once unused, so the next pod may run on any node; ext4
			// multi-mount protection guards against two nodes mounting it.
			// Pending replacements don't hold it: only pods placed on a node do.
			if !volumeBusy(st, v.Metadata.Namespace, v.Metadata.Name) {
				v.Status.NodeName, v.Status.Phase = "", "Available"
			} else if v.Status.NodeName != "" {
				v.Status.Phase = "Bound"
			}
		case v.Spec.Type != "local":
			if len(users) > 0 {
				v.Status.Phase = "Bound"
			} else {
				v.Status.Phase = "Available"
			}
		}
	}
}

// serviceEndpoints returns the ready pods selected by a service.
func serviceEndpoints(st *store.State, svc *api.Service) []*api.Pod {
	var out []*api.Pod
	for _, p := range st.Pods {
		if p.Metadata.Namespace == svc.Metadata.Namespace && p.Status.Ready &&
			p.Status.Phase == api.PodRunning && labelsMatch(p.Metadata.Labels, svc.Spec.Selector) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func reconcileServiceStatus(st *store.State) {
	for _, svc := range st.Services {
		pods := serviceEndpoints(st, svc)
		var eps []string
		unpublished := false
		sp := svc.Spec.Ports[0]
		for _, p := range pods {
			if hp := hostPortFor(p, sp); hp > 0 && !svc.Spec.Internal && !svc.Spec.Headless {
				eps = append(eps, fmt.Sprintf("%s:%d", p.Status.HostIP, hp))
			} else if p.Status.PodIP != "" {
				eps = append(eps, fmt.Sprintf("%s:%d", p.Status.PodIP, sp.TargetPort))
				unpublished = !svc.Spec.Internal && !svc.Spec.Headless
			}
		}
		svc.Status.Endpoints = eps
		switch {
		case len(pods) == 0:
			svc.Status.Message = "no ready pods match the selector"
		case unpublished:
			svc.Status.Message = fmt.Sprintf("targetPort %d is not in the pods' ports list: reachable inside the cluster only", svc.Spec.Ports[0].TargetPort)
		default:
			svc.Status.Message = ""
		}
	}
}
