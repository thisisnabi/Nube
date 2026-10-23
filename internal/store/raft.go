package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/thisisnabi/Nube/internal/api"
)

// RaftStore replicates the state across control-plane servers with the Raft
// consensus protocol. Every committed log entry is a full state snapshot, which
// keeps the state machine trivially deterministic; for a personal cluster the
// state is small enough that this is cheap.
type RaftStore struct {
	id      string
	raft    *raft.Raft
	bolt    *raftboltdb.BoltStore
	trans   *raft.NetworkTransport
	writeMu sync.Mutex // serializes writers on the leader

	mu    sync.RWMutex
	state *State
	last  []byte // serialized form of state
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
	rs := &RaftStore{id: o.ID, state: NewState()}
	rs.last, _ = json.Marshal(rs.state)

	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(o.ID)
	cfg.SnapshotThreshold = 512
	cfg.SnapshotInterval = 30 * time.Second
	cfg.TrailingLogs = 256
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

	r.mu.RLock()
	clone, err := decodeState(r.last)
	r.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := fn(clone); err != nil {
		return err
	}
	b, err := json.Marshal(clone)
	if err != nil {
		return err
	}
	r.mu.RLock()
	same := bytes.Equal(b, r.last)
	r.mu.RUnlock()
	if same {
		return nil
	}
	f := r.raft.Apply(b, 5*time.Second)
	if err := f.Error(); err != nil {
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
	st, err := decodeState(l.Data)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.state, f.last = st, l.Data
	f.mu.Unlock()
	return nil
}

func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return snapshot(f.last), nil
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
	f.state, f.last = st, b
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
