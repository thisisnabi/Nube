package store

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stateJSON(s Store) string {
	var b []byte
	s.Read(func(st *State) { b, _ = json.Marshal(st) })
	return string(b)
}

func TestRaftReplicationAndFailover(t *testing.T) {
	var stores []*RaftStore
	for i := 0; i < 3; i++ {
		addr := freePort(t)
		rs, err := NewRaftStore(RaftOptions{ID: fmt.Sprint("s", i), Dir: t.TempDir(), Bind: addr, Advertise: addr, Bootstrap: i == 0})
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, rs)
		defer rs.Close()
	}
	waitFor(t, "s0 leader", stores[0].IsLeader)
	for _, s := range stores[1:] {
		if err := stores[0].AddServer(s.id, string(s.trans.LocalAddr()), "http://"+s.id); err != nil {
			t.Fatal(err)
		}
	}

	// Many writes, including one that fails and must roll back.
	for i := 0; i < 50; i++ {
		i := i
		if err := stores[0].Write(func(st *State) error {
			st.Pods[fmt.Sprintf("default/p%d", i)] = &api.Pod{Kind: api.KindPod, Metadata: api.Meta{Name: fmt.Sprintf("p%d", i), Namespace: "default"}}
			st.Event("Normal", "Pod", "default", fmt.Sprintf("p%d", i), "Created", "x")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := stores[0].Write(func(st *State) error {
		delete(st.Pods, "default/p1")
		return fmt.Errorf("validation failed")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	stores[0].Read(func(st *State) {
		if st.Pods["default/p1"] == nil {
			t.Fatal("failed write was not rolled back")
		}
	})
	want := stateJSON(stores[0])
	for _, s := range stores[1:] {
		waitFor(t, s.id+" converged", func() bool { return stateJSON(s) == want })
	}

	// Kill the leader; a new one takes writes and the survivors stay identical.
	stores[0].Close()
	var leader *RaftStore
	waitFor(t, "new leader", func() bool {
		for _, s := range stores[1:] {
			if s.IsLeader() {
				leader = s
				return true
			}
		}
		return false
	})
	if err := leader.Write(func(st *State) error {
		delete(st.Pods, "default/p7")
		st.CACert = "after-failover"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want = stateJSON(leader)
	for _, s := range stores[1:] {
		waitFor(t, s.id+" converged after failover", func() bool { return stateJSON(s) == want })
	}
	leader.Read(func(st *State) {
		if len(st.Pods) != 49 || st.CACert != "after-failover" || len(st.Events) != 52 { // 50 pod events + 2 "server joined"
			t.Fatalf("pods=%d ca=%q events=%d", len(st.Pods), st.CACert, len(st.Events))
		}
	})
}
