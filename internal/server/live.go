package server

import (
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
}

func newLiveStats() *liveStats {
	return &liveStats{
		heartbeat: map[string]time.Time{}, nodeUsage: map[string]api.Resources{},
		podUsage: map[string]api.Resources{}, volUsed: map[string]int64{},
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
