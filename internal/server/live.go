package server

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

// liveStats holds fast-changing data that is NOT replicated through Raft:
// heartbeats and usage metrics. Replicating them would turn every agent
// heartbeat into a log entry; instead the leader keeps them in memory and
// persists only meaningful changes (and a heartbeat once a minute).
type liveStats struct {
	mu        sync.RWMutex
	heartbeat map[string]time.Time
	nodeUsage map[string]api.Resources
	podUsage  map[string]api.Resources // ns/name -> usage
	volUsed   map[string]int64         // ns/name -> MiB used
	// usage holds one sample per minute for each workload ("Kind|ns/name"):
	// the highest usage of any of its pods in that minute.
	usage map[string][]api.Resources
}

func newLiveStats() *liveStats {
	return &liveStats{
		heartbeat: map[string]time.Time{}, nodeUsage: map[string]api.Resources{},
		podUsage: map[string]api.Resources{}, volUsed: map[string]int64{},
		usage: map[string][]api.Resources{},
	}
}

func (l *liveStats) record(req *api.SyncRequest, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.heartbeat[req.Node.Name] = now
	l.nodeUsage[req.Node.Name] = req.Node.Usage
	for _, p := range req.Pods {
		l.podUsage[p.Namespace+"/"+p.Name] = api.Resources{CPU: p.CPU, Memory: p.Memory}
	}
	for _, v := range req.Volumes {
		l.volUsed[v.Namespace+"/"+v.Name] = v.UsedMiB
	}
}

// lastSeen returns the most recent heartbeat of a node (live or persisted).
func (l *liveStats) lastSeen(n *api.Node) time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if t, ok := l.heartbeat[n.Name]; ok && t.After(n.LastHeartbeat) {
		return t
	}
	return n.LastHeartbeat
}

// grace gives every node a fresh heartbeat (a new leader has no live data yet).
func (l *liveStats) grace(nodes []string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, n := range nodes {
		if _, ok := l.heartbeat[n]; !ok {
			l.heartbeat[n] = now
		}
	}
}

func (l *liveStats) podCPU(key string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.podUsage[key].CPU
}

// node/pod/volume return copies with live numbers filled in.
func (l *liveStats) node(n *api.Node) *api.Node {
	c := *n
	l.mu.RLock()
	defer l.mu.RUnlock()
	if u, ok := l.nodeUsage[n.Name]; ok {
		c.Usage = u
	}
	if t, ok := l.heartbeat[n.Name]; ok && t.After(c.LastHeartbeat) {
		c.LastHeartbeat = t
	}
	return &c
}

func (l *liveStats) pod(p *api.Pod) *api.Pod {
	c := *p
	l.mu.RLock()
	defer l.mu.RUnlock()
	if u, ok := l.podUsage[p.Metadata.Key()]; ok {
		c.Status.CPU, c.Status.Memory = u.CPU, u.Memory
	}
	return &c
}

func (l *liveStats) volume(v *api.Volume) *api.Volume {
	c := *v
	l.mu.RLock()
	defer l.mu.RUnlock()
	if u, ok := l.volUsed[v.Metadata.Key()]; ok {
		c.Status.UsedMiB = u
	}
	return &c
}

func (l *liveStats) forget(podKey string) {
	l.mu.Lock()
	delete(l.podUsage, podKey)
	l.mu.Unlock()
}

// Recommendation tuning: a day of one-minute samples; advice after 15 minutes.
const (
	usageSamples    = 24 * 60
	minUsageSamples = 15
)

// sample records the peak per-pod usage of every workload (called once a
// minute on the leader). Workloads without running pods get no sample.
func (l *liveStats) sample(pods []*api.Pod) {
	l.mu.Lock()
	defer l.mu.Unlock()
	peak := map[string]api.Resources{}
	for _, p := range pods {
		if p.Spec.Owner == "" || p.Status.Phase != api.PodRunning {
			continue
		}
		u, ok := l.podUsage[p.Metadata.Key()]
		if !ok {
			continue
		}
		key := p.Spec.OwnerKind + "|" + p.Metadata.Namespace + "/" + p.Spec.Owner
		cur := peak[key]
		peak[key] = api.Resources{CPU: max(cur.CPU, u.CPU), Memory: max(cur.Memory, u.Memory)}
	}
	for key, u := range peak {
		h := append(l.usage[key], u)
		if len(h) > usageSamples {
			h = h[len(h)-usageSamples:]
		}
		l.usage[key] = h
	}
	for key := range l.usage {
		if _, ok := peak[key]; !ok && !ownerExists(pods, key) {
			delete(l.usage, key)
		}
	}
}

func ownerExists(pods []*api.Pod, key string) bool {
	for _, p := range pods {
		if p.Spec.OwnerKind+"|"+p.Metadata.Namespace+"/"+p.Spec.Owner == key {
			return true
		}
	}
	return false
}

// recommend suggests per-pod requests for a workload: CPU at the 95th
// percentile of its minute peaks plus 15%, memory at the highest peak plus
// 20% (memory is a hard limit, so the peak matters). Kubernetes needs the
// Vertical Pod Autoscaler add-on for this.
func (l *liveStats) recommend(kind, ns, name string, t *api.PodTemplate) *api.Recommendation {
	l.mu.RLock()
	h := slices.Clone(l.usage[kind+"|"+ns+"/"+name])
	l.mu.RUnlock()
	r := &api.Recommendation{Requests: t.Requests(), Minutes: len(h)}
	if len(h) < minUsageSamples {
		r.Message = fmt.Sprintf("collecting usage (%d of %d minutes)", len(h), minUsageSamples)
		return r
	}
	cpus := make([]int64, len(h))
	for i, u := range h {
		cpus[i] = u.CPU
		r.Memory = max(r.Memory, u.Memory)
	}
	slices.Sort(cpus)
	p95 := cpus[(len(cpus)*95+99)/100-1]
	r.CPU = max(10, roundUp(p95*115/100, 5))
	r.Memory = max(16, roundUp(r.Memory*120/100, 8))
	req := r.Requests
	switch {
	case req.Memory > 0 && r.Memory > req.Memory:
		r.Message = fmt.Sprintf("memory peaks near the %dMi limit; raise it to %dMi to avoid OOM kills", req.Memory, r.Memory)
	case req.CPU > 0 && r.CPU > req.CPU:
		r.Message = fmt.Sprintf("uses more CPU than requested; request %dm so the scheduler reserves it", r.CPU)
	case req.CPU == 0 && req.Memory == 0:
		r.Message = fmt.Sprintf("no requests set; suggested cpu: %dm, memory: %dMi", r.CPU, r.Memory)
	case req.CPU >= 2*r.CPU && req.CPU-r.CPU >= 100 || req.Memory >= 2*r.Memory && req.Memory-r.Memory >= 64:
		r.Message = fmt.Sprintf("over-provisioned; cpu: %dm, memory: %dMi per pod would do", r.CPU, r.Memory)
	default:
		r.Message = "requests fit the observed usage"
	}
	return r
}

func roundUp(v, step int64) int64 { return (v + step - 1) / step * step }
