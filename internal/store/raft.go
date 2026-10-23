package store

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/thisisnabi/Nube/internal/api"
)

// RaftStore replicates the state across control-plane servers with Raft.
// Each log entry is a Patch holding only the objects a write changed, so the
// log grows with the size of changes, not with the size of the cluster.
//
// Writes run on the leader's live state; the result is diffed against the
// committed mirror, replicated, and rolled back from the mirror if the entry
// cannot be committed.
type RaftStore struct {
	id      string
	raft    *raft.Raft
	bolt    *raftboltdb.BoltStore
	trans   *raft.NetworkTransport
	writeMu sync.Mutex // serializes writers on the leader
	seq     atomic.Int64

	mu     sync.RWMutex
	state  *State
	mirror *committed
	// live holds origins of entries already applied to the live state but not
	// yet committed. Only those are skipped when they come back through Apply.
	live map[string]bool
}

type RaftOptions struct {
	ID        string // unique server id
	Dir       string // data directory
	Bind      string // raft listen address, e.g. 0.0.0.0:7072
	Advertise string // raft address reachable by peers, e.g. 10.0.0.1:7072
	Bootstrap bool   // create a new cluster with this server as its only voter
}

func NewRaftStore(o RaftOptions) (*RaftStore, error) {
	dir := filepath.Join(o.Dir, "raft")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	rs := &RaftStore{id: o.ID, state: NewState(), live: map[string]bool{}}
	rs.seq.Store(time.Now().UnixNano()) // origins are unique across restarts
	rs.mirror = snapshotOf(rs.state)

	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(o.ID)
	cfg.SnapshotThreshold = 2048
	cfg.SnapshotInterval = 60 * time.Second
	cfg.TrailingLogs = 1024
	cfg.Logger = hclog.New(&hclog.LoggerOptions{Name: "raft", Level: hclog.Warn, Output: os.Stderr})

	adv, err := net.ResolveTCPAddr("tcp", o.Advertise)
	if err != nil {
		return nil, fmt.Errorf("raft advertise address: %w", err)
	}
	rs.trans, err = raft.NewTCPTransport(o.Bind, adv, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft transport: %w", err)
	}
	snaps, err := raft.NewFileSnapshotStore(dir, 2, os.Stderr)
	if err != nil {
		return nil, err
	}
	rs.bolt, err = raftboltdb.New(raftboltdb.Options{Path: filepath.Join(dir, "raft.db")})
	if err != nil {
		return nil, fmt.Errorf("raft log store: %w", err)
	}
	rs.raft, err = raft.NewRaft(cfg, (*fsm)(rs), rs.bolt, rs.bolt, snaps, rs.trans)
	if err != nil {
		return nil, err
	}
	if o.Bootstrap {
		has, err := raft.HasExistingState(rs.bolt, rs.bolt, snaps)
		if err != nil {
			return nil, err
		}
		if !has {
			f := rs.raft.BootstrapCluster(raft.Configuration{Servers: []raft.Server{
				{ID: cfg.LocalID, Address: rs.trans.LocalAddr()},
			}})
			if err := f.Error(); err != nil {
				return nil, fmt.Errorf("bootstrap: %w", err)
			}
		}
	}
	return rs, nil
}

func (r *RaftStore) Read(fn func(*State)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn(r.state)
}

func (r *RaftStore) Write(fn func(*State) error) error {
	if r.raft.State() != raft.Leader {
		return ErrNotLeader
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	r.mu.Lock()
	if err := fn(r.state); err != nil {
		r.rollbackLocked()
		r.mu.Unlock()
		return err
	}
	p := diff(r.state, r.mirror)
	if p.empty() {
		r.mu.Unlock()
		return nil
	}
	p.Origin = fmt.Sprintf("%s#%d", r.id, r.seq.Add(1))
	data, err := json.Marshal(p)
	if err != nil {
		r.rollbackLocked()
		r.mu.Unlock()
		return err
	}
	r.live[p.Origin] = true
	r.mu.Unlock()

	f := r.raft.Apply(data, 10*time.Second)
	if err := f.Error(); err != nil {
		r.mu.Lock()
		r.rollbackLocked()
		r.mu.Unlock()
		if err == raft.ErrNotLeader || err == raft.ErrLeadershipLost {
			return ErrNotLeader
		}
		return err
	}
	if resp, ok := f.Response().(error); ok && resp != nil {
		return resp
	}
	return nil
}

// rollbackLocked discards uncommitted changes by rebuilding from the mirror.
// Pending entries may still commit later; they are then applied normally.
func (r *RaftStore) rollbackLocked() {
	st, err := r.mirror.state()
	if err != nil {
		log.Printf("store: rollback failed: %v", err)
		return
	}
	r.state = st
	r.live = map[string]bool{}
}

func (r *RaftStore) IsLeader() bool { return r.raft.State() == raft.Leader }

func (r *RaftStore) LeaderAPI() string {
	_, id := r.raft.LeaderWithID()
	if id == "" || string(id) == r.id {
		return ""
	}
	var addr string
	r.Read(func(s *State) {
		if m := s.Servers[string(id)]; m != nil {
			addr = m.APIAddr
		}
	})
	return addr
}

func (r *RaftStore) Cluster() api.ClusterInfo {
	leaderAddr, leaderID := r.raft.LeaderWithID()
	ci := api.ClusterInfo{
		Mode:     "ha",
		ServerID: r.id,
		Leader:   string(leaderID),
		IsLeader: r.raft.State() == raft.Leader,
		State:    r.raft.State().String(),
	}
	stats := r.raft.Stats()
	fmt.Sscan(stats["term"], &ci.Term)
	known := map[string]*api.ServerMember{}
	r.Read(func(s *State) {
		for id, m := range s.Servers {
			c := *m
			known[id] = &c
		}
	})
	if f := r.raft.GetConfiguration(); f.Error() == nil {
		for _, srv := range f.Configuration().Servers {
			m := known[string(srv.ID)]
			if m == nil {
				m = &api.ServerMember{ID: string(srv.ID)}
			}
			m.RaftAddr = string(srv.Address)
			m.Voter = srv.Suffrage == raft.Voter
			m.Leader = srv.Address == leaderAddr
			ci.Members = append(ci.Members, *m)
		}
	}
	sort.Slice(ci.Members, func(i, j int) bool { return ci.Members[i].ID < ci.Members[j].ID })
	return ci
}

func (r *RaftStore) AddServer(id, raftAddr, apiAddr string) error {
	if !r.IsLeader() {
		return ErrNotLeader
	}
	if f := r.raft.GetConfiguration(); f.Error() == nil {
		for _, srv := range f.Configuration().Servers {
			if string(srv.ID) == id && string(srv.Address) == raftAddr {
				goto record
			}
			if string(srv.ID) == id || string(srv.Address) == raftAddr {
				if err := r.raft.RemoveServer(srv.ID, 0, 0).Error(); err != nil {
					return err
				}
			}
		}
	}
	if err := r.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(raftAddr), 0, 10*time.Second).Error(); err != nil {
		return err
	}
record:
	return r.Write(func(s *State) error {
		m := s.Servers[id]
		if m == nil {
			m = &api.ServerMember{ID: id, JoinedAt: time.Now()}
			s.Servers[id] = m
			s.Event("Normal", "Server", "", id, "Joined", "control-plane server joined at "+apiAddr)
		}
		m.RaftAddr, m.APIAddr = raftAddr, apiAddr
		return nil
	})
}

func (r *RaftStore) RemoveServer(id string) error {
	if !r.IsLeader() {
		return ErrNotLeader
	}
	if err := r.raft.RemoveServer(raft.ServerID(id), 0, 0).Error(); err != nil {
		return err
	}
	return r.Write(func(s *State) error {
		delete(s.Servers, id)
		s.Event("Normal", "Server", "", id, "Removed", "control-plane server removed")
		return nil
	})
}

func (r *RaftStore) Close() error {
	err := r.raft.Shutdown().Error()
	r.bolt.Close()
	return err
}

// ---------------------------------------------------------------- FSM

type fsm RaftStore

func (f *fsm) Apply(l *raft.Log) any {
	var p Patch
	if err := json.Unmarshal(l.Data, &p); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// The leader already applied its own pending write to the live state;
	// everything else (followers, replays, entries rolled back locally but
	// committed anyway) is applied here.
	if f.live[p.Origin] {
		delete(f.live, p.Origin)
	} else if err := p.apply(f.state); err != nil {
		return err
	}
	f.mirror.record(&p)
	return nil
}

func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return snapshot(f.mirror.encode()), nil // committed data only
}

func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	st, err := decodeState(b)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.state, f.mirror, f.live = st, snapshotOf(st), map[string]bool{}
	f.mu.Unlock()
	return nil
}

type snapshot []byte

func (s snapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s snapshot) Release() {}
