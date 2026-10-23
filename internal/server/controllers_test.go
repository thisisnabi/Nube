package server

import (
	"testing"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

func testState(nodes ...string) *store.State {
	st := store.NewState()
	for _, n := range nodes {
		st.Nodes[n] = &api.Node{
			Name: n, Status: api.NodeReady, HostIP: "10.0.0." + n[len(n)-1:], LastHeartbeat: time.Now(),
			Capacity: api.Resources{CPU: 2000, Memory: 2048}, Labels: map[string]string{"nube/hostname": n},
		}
	}
	return st
}

func apply(t *testing.T, st *store.State, yaml string) {
	t.Helper()
	objs, err := api.ParseManifests([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	for _, o := range objs {
		if _, err := s.applyObject(st, o); err != nil {
			t.Fatal(err)
		}
	}
}

// tick runs one controller pass and then plays the agent: every scheduled pod
// becomes ready, every terminating pod disappears.
func tick(st *store.State) {
	now := time.Now()
	reconcileNodes(st, now)
	for _, k := range sortedKeys(st.Deployments) {
		reconcileDeployment(st, st.Deployments[k], now)
	}
	schedulePending(st, now)
	for k, p := range st.Pods {
		switch {
		case p.Status.Phase == api.PodTerminating:
			delete(st.Pods, k)
		case p.Spec.NodeName != "":
			p.Status.Phase, p.Status.Ready = api.PodRunning, true
		}
	}
}

const webYAML = `
kind: Deployment
metadata: {name: web}
spec:
  replicas: 3
  template:
    image: nginx:1
    resources: {cpu: 100m, memory: 64Mi}
`

func TestDeploymentScalesAndSpreads(t *testing.T) {
	st := testState("n1", "n2", "n3")
	apply(t, st, webYAML)
	for i := 0; i < 3; i++ {
		tick(st)
	}
	pods := st.PodsOf("default", "web")
	if len(pods) != 3 {
		t.Fatalf("want 3 pods, got %d", len(pods))
	}
	perNode := map[string]int{}
	for _, p := range pods {
		perNode[p.Spec.NodeName]++
	}
	if len(perNode) != 3 {
		t.Fatalf("replicas not spread across nodes: %v", perNode)
	}
	reconcileDeployment(st, st.Deployments["default/web"], time.Now())
	if ph := st.Deployments["default/web"].Status.Phase; ph != "Healthy" {
		t.Fatalf("phase = %s", ph)
	}
}

func TestRollingUpdateKeepsAvailability(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, webYAML)
	for i := 0; i < 3; i++ {
		tick(st)
	}
	d := st.Deployments["default/web"]
	d.Spec.Template.Image = "nginx:2"
	newHash := d.Spec.Template.Hash()
	for i := 0; i < 20; i++ {
		// Availability never drops below replicas - maxUnavailable (= 3).
		ready := 0
		for _, p := range st.PodsOf("default", "web") {
			if p.Status.Ready {
				ready++
			}
		}
		if ready < 3 {
			t.Fatalf("tick %d: only %d ready pods during rollout", i, ready)
		}
		tick(st)
	}
	pods := st.PodsOf("default", "web")
	if len(pods) != 3 {
		t.Fatalf("want 3 pods after rollout, got %d", len(pods))
	}
	for _, p := range pods {
		if p.Spec.TemplateHash != newHash || p.Spec.Template.Image != "nginx:2" {
			t.Fatalf("pod %s still on old template", p.Metadata.Name)
		}
	}
	if d.Status.Revision != 2 || len(d.Status.History) != 2 {
		t.Fatalf("revision=%d history=%d", d.Status.Revision, len(d.Status.History))
	}
}

func TestSchedulerRespectsResourcesAndSelectors(t *testing.T) {
	st := testState("n1", "n2")
	st.Nodes["n2"].Labels["disk"] = "ssd"
	apply(t, st, `
kind: Deployment
metadata: {name: db}
spec:
  replicas: 2
  nodeSelector: {disk: ssd}
  template:
    image: postgres
    resources: {cpu: "1.5", memory: 1Gi}
`)
	tick(st)
	tick(st)
	var scheduled, pending int
	for _, p := range st.PodsOf("default", "db") {
		if p.Spec.NodeName == "n2" {
			scheduled++
		} else if p.Spec.NodeName == "" {
			pending++
			if p.Status.Message == "" {
				t.Fatal("pending pod has no FailedScheduling message")
			}
		} else {
			t.Fatalf("pod placed on %s despite nodeSelector", p.Spec.NodeName)
		}
	}
	if scheduled != 1 || pending != 1 {
		t.Fatalf("scheduled=%d pending=%d (n2 only fits one 1.5-core pod)", scheduled, pending)
	}
}

func TestLocalVolumePinsPodsToNode(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, `
kind: Volume
metadata: {name: data}
spec: {type: local}
---
kind: Deployment
metadata: {name: db}
spec:
  replicas: 1
  template:
    image: redis
    volumes: [{volume: data, containerPath: /data}]
`)
	tick(st)
	tick(st)
	v := st.Volumes["default/data"]
	if v.Status.Phase != "Bound" || v.Status.NodeName == "" {
		t.Fatalf("volume not bound: %+v", v.Status)
	}
	bound := v.Status.NodeName
	// Kill the pod; the replacement must land on the same node.
	for k := range st.Pods {
		delete(st.Pods, k)
	}
	tick(st)
	tick(st)
	for _, p := range st.PodsOf("default", "db") {
		if p.Spec.NodeName != bound {
			t.Fatalf("replacement pod on %s, volume is on %s", p.Spec.NodeName, bound)
		}
	}
}

func TestNodeFailureReschedules(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, webYAML)
	tick(st)
	tick(st)
	st.Nodes["n1"].LastHeartbeat = time.Now().Add(-time.Minute)
	for i := 0; i < 4; i++ {
		tick(st)
	}
	if st.Nodes["n1"].Status != api.NodeNotReady {
		t.Fatal("n1 should be NotReady")
	}
	pods := st.PodsOf("default", "web")
	if len(pods) != 3 {
		t.Fatalf("want 3 pods, got %d", len(pods))
	}
	for _, p := range pods {
		if p.Spec.NodeName != "n2" {
			t.Fatalf("pod %s still on failed node %s", p.Metadata.Name, p.Spec.NodeName)
		}
	}
}

func TestAutoscale(t *testing.T) {
	st := testState("n1", "n2", "n3")
	apply(t, st, `
kind: Deployment
metadata: {name: api}
spec:
  replicas: 2
  autoscale: {min: 2, max: 6, targetCPUPercent: 50}
  template:
    image: x
    resources: {cpu: 200m}
`)
	tick(st)
	tick(st)
	for _, p := range st.PodsOf("default", "api") {
		p.Status.CPU = 200 // 100% of request
	}
	d := st.Deployments["default/api"]
	autoscale(st, d, time.Now())
	if d.Spec.Replicas != 4 {
		t.Fatalf("want 4 replicas at 2x target load, got %d", d.Spec.Replicas)
	}
}

func TestQuantities(t *testing.T) {
	cases := map[string]int64{"250m": 250, "0.5": 500, "2": 2000}
	for in, want := range cases {
		if got, _ := api.ParseCPU(in); got != want {
			t.Errorf("cpu %s = %d, want %d", in, got, want)
		}
	}
	mem := map[string]int64{"128Mi": 128, "1Gi": 1024, "512M": 488}
	for in, want := range mem {
		if got, _ := api.ParseMemory(in); got != want {
			t.Errorf("mem %s = %d, want %d", in, got, want)
		}
	}
}
