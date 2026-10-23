// Package store holds the cluster state and persists or replicates it.
package store

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

// ErrNotLeader is returned by Write on a follower in an HA cluster.
var ErrNotLeader = errors.New("not the leader")

const maxEvents = 500

// State is the complete desired + observed state of the cluster.
// It is always accessed through Store.Read / Store.Write.
type State struct {
	Nodes        map[string]*api.Node         `json:"nodes"`
	Deployments  map[string]*api.Deployment   `json:"deployments"`
	Pods         map[string]*api.Pod          `json:"pods"`
	Services     map[string]*api.Service      `json:"services"`
	ConfigMaps   map[string]*api.ConfigMap    `json:"configMaps"`
	Secrets      map[string]*api.Secret       `json:"secrets"`
	Volumes      map[string]*api.Volume       `json:"volumes"`
	Jobs         map[string]*api.Job          `json:"jobs"`
	CronJobs     map[string]*api.CronJob      `json:"cronJobs"`
	StatefulSets map[string]*api.StatefulSet  `json:"statefulSets"`
	Ingresses    map[string]*api.Ingress      `json:"ingresses"`
	Tokens       map[string]*api.Token        `json:"tokens"` // by token id
	Servers      map[string]*api.ServerMember `json:"servers"`
	// Cluster certificate authority (PEM), created once by the first leader.
	// It signs every server and agent certificate; clients pin its hash.
	CACert   string      `json:"caCert,omitempty"`
	CAKey    string      `json:"caKey,omitempty"`
	Events   []api.Event `json:"events"`
	EventSeq int64       `json:"eventSeq"`
}

func NewState() *State {
	s := &State{}
	s.init()
	return s
}

func (s *State) init() {
	if s.Nodes == nil {
		s.Nodes = map[string]*api.Node{}
	}
	if s.Deployments == nil {
		s.Deployments = map[string]*api.Deployment{}
	}
	if s.Pods == nil {
		s.Pods = map[string]*api.Pod{}
	}
	if s.Services == nil {
		s.Services = map[string]*api.Service{}
	}
	if s.ConfigMaps == nil {
		s.ConfigMaps = map[string]*api.ConfigMap{}
	}
	if s.Secrets == nil {
		s.Secrets = map[string]*api.Secret{}
	}
	if s.Volumes == nil {
		s.Volumes = map[string]*api.Volume{}
	}
	if s.Servers == nil {
		s.Servers = map[string]*api.ServerMember{}
	}
	if s.Jobs == nil {
		s.Jobs = map[string]*api.Job{}
	}
	if s.CronJobs == nil {
		s.CronJobs = map[string]*api.CronJob{}
	}
	if s.StatefulSets == nil {
		s.StatefulSets = map[string]*api.StatefulSet{}
	}
	if s.Ingresses == nil {
		s.Ingresses = map[string]*api.Ingress{}
	}
	if s.Tokens == nil {
		s.Tokens = map[string]*api.Token{}
	}
}

func decodeState(b []byte) (*State, error) {
	s := &State{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	s.init()
	return s, nil
}

// Event records a cluster event. Identical events within a minute are coalesced.
func (s *State) Event(typ, kind, ns, name, reason, msg string) {
	now := time.Now()
	for i := len(s.Events) - 1; i >= 0 && i >= len(s.Events)-30; i-- {
		e := s.Events[i]
		if e.Kind == kind && e.Namespace == ns && e.Name == name && e.Reason == reason && e.Message == msg &&
			now.Sub(e.Time) < time.Minute {
			return
		}
	}
	s.EventSeq++
	s.Events = append(s.Events, api.Event{
		ID: s.EventSeq, Time: now, Type: typ, Kind: kind, Namespace: ns, Name: name, Reason: reason, Message: msg,
	})
	if len(s.Events) > maxEvents {
		s.Events = append([]api.Event(nil), s.Events[len(s.Events)-maxEvents:]...)
	}
}

// PodsOf returns the pods owned by a deployment, sorted by creation time.
func (s *State) PodsOf(ns, deployment string) []*api.Pod {
	return s.PodsOwned(ns, api.KindDeployment, deployment)
}

// PodsOwned returns the pods owned by a workload, sorted by creation time.
func (s *State) PodsOwned(ns, kind, name string) []*api.Pod {
	var out []*api.Pod
	for _, p := range s.Pods {
		if p.Metadata.Namespace == ns && p.OwnedBy(kind, name) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.CreatedAt.Before(out[j].Metadata.CreatedAt) })
	return out
}

// Namespaces lists every namespace that has at least one object.
func (s *State) Namespaces() []string {
	set := map[string]bool{api.DefaultNamespace: true}
	for _, d := range s.Deployments {
		set[d.Metadata.Namespace] = true
	}
	for _, p := range s.Pods {
		set[p.Metadata.Namespace] = true
	}
	for _, v := range s.Services {
		set[v.Metadata.Namespace] = true
	}
	for _, v := range s.ConfigMaps {
		set[v.Metadata.Namespace] = true
	}
	for _, v := range s.Secrets {
		set[v.Metadata.Namespace] = true
	}
	for _, v := range s.Volumes {
		set[v.Metadata.Namespace] = true
	}
	for _, v := range s.Jobs {
		set[v.Metadata.Namespace] = true
	}
	for _, v := range s.CronJobs {
		set[v.Metadata.Namespace] = true
	}
	for _, v := range s.StatefulSets {
		set[v.Metadata.Namespace] = true
	}
	for _, v := range s.Ingresses {
		set[v.Metadata.Namespace] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Store is the interface shared by the single-node and HA backends.
type Store interface {
	// Read runs fn with a consistent view of the state. fn must not modify it.
	Read(fn func(*State))
	// Write runs fn and persists/replicates the result if fn returns nil.
	Write(fn func(*State) error) error
	IsLeader() bool
	// LeaderAPI returns the API address of the current leader ("" if unknown or self).
	LeaderAPI() string
	Cluster() api.ClusterInfo
	AddServer(id, raftAddr, apiAddr string) error
	RemoveServer(id string) error
	Close() error
}
