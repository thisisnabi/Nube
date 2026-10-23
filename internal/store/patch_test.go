package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

func TestPatchRoundTrip(t *testing.T) {
	a := NewState()
	a.Nodes["n1"] = &api.Node{Name: "n1", Status: api.NodeReady}
	a.Pods["default/p1"] = &api.Pod{Kind: api.KindPod, Metadata: api.Meta{Name: "p1", Namespace: "default"}}
	a.CACert = "x"
	mirror := snapshotOf(a)

	// Change things on the "leader".
	a.Pods["default/p1"].Status.Phase = api.PodRunning
	a.Pods["default/p2"] = &api.Pod{Kind: api.KindPod, Metadata: api.Meta{Name: "p2", Namespace: "default"}}
	delete(a.Nodes, "n1")
	a.CACert = "y"
	a.Event("Normal", "Pod", "default", "p2", "Created", "hi")

	p := diff(a, mirror)
	if len(p.Sets["pods"]) != 2 || len(p.Dels["nodes"]) != 1 || p.Scalars["caCert"] == nil || len(p.Events) != 1 {
		t.Fatalf("unexpected patch: %+v", p)
	}
	if len(p.Sets["deployments"]) != 0 {
		t.Fatal("unchanged collections must not be in the patch")
	}

	// A follower starting from the old state ends up identical.
	b, _ := mirror.state()
	wire, _ := json.Marshal(p)
	var got Patch
	json.Unmarshal(wire, &got)
	if err := got.apply(b); err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("follower diverged:\nleader   %s\nfollower %s", ja, jb)
	}
	mirror.record(p)
	if q := diff(a, mirror); !q.empty() {
		t.Fatalf("after recording, nothing should differ: %+v", q)
	}
}

// BenchmarkWrite measures one controller-style write on a large cluster:
// 100 nodes, 5000 pods, one pod status change.
func TestLargeClusterWriteCost(t *testing.T) {
	s := NewState()
	for n := 0; n < 100; n++ {
		s.Nodes[fmt.Sprint("node", n)] = &api.Node{Name: fmt.Sprint("node", n), Status: api.NodeReady}
	}
	for i := 0; i < 5000; i++ {
		key := fmt.Sprintf("default/web-%d", i)
		s.Pods[key] = &api.Pod{Kind: api.KindPod, Metadata: api.Meta{Name: fmt.Sprintf("web-%d", i), Namespace: "default",
			Labels: map[string]string{"app": "web"}, CreatedAt: time.Now()},
			Spec:   api.PodSpec{Owner: "web", OwnerKind: api.KindDeployment, NodeName: fmt.Sprint("node", i%100), Template: api.PodTemplate{Container: api.Container{Image: "nginx:alpine"}}},
			Status: api.PodStatus{Phase: api.PodRunning, Ready: true, PodIP: "10.88.1.2"}}
	}
	mirror := snapshotOf(s)
	full, _ := json.Marshal(s)

	start := time.Now()
	s.Pods["default/web-42"].Status.Ready = false
	p := diff(s, mirror)
	elapsed := time.Since(start)
	wire, _ := json.Marshal(p)
	t.Logf("state %d KB, one change: patch %d bytes, diff took %s", len(full)/1024, len(wire), elapsed)
	if len(wire) > 4096 {
		t.Fatalf("patch for one pod should be small, got %d bytes", len(wire))
	}
}
