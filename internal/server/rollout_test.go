package server

import (
	"strings"
	"testing"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// tickAt is tick with a clock and an agent that crashes pods running image "bad".
func tickAt(st *store.State, now time.Time) {
	for _, k := range sortedKeys(st.Deployments) {
		reconcileDeployment(st, st.Deployments[k], now)
	}
	schedulePending(st, now)
	for k, p := range st.Pods {
		switch {
		case p.Status.Phase == api.PodTerminating:
			delete(st.Pods, k)
		case p.Spec.NodeName != "" && p.Spec.Template.MainImage() == "bad":
			p.Status.Phase, p.Status.Ready, p.Status.Message = api.PodCrashLoop, false, "exit code 1"
		case p.Spec.NodeName != "":
			p.Status.Phase, p.Status.Ready, p.Status.Message = api.PodRunning, true, ""
		}
	}
}

func podImages(st *store.State) map[string]int {
	out := map[string]int{}
	for _, p := range st.Pods {
		out[p.Spec.Template.MainImage()]++
	}
	return out
}

const configYAML = `
kind: ConfigMap
metadata: {name: app}
data: {MODE: blue}
---
kind: Deployment
metadata: {name: web}
spec:
  replicas: 2
  template:
    image: nginx
    envFrom: [{configMap: app}]
`

func TestConfigChangeRollsPods(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, configYAML)
	for range 3 {
		tick(st)
	}
	before := map[string]bool{}
	for k := range st.Pods {
		before[k] = true
	}
	d := st.Deployments["default/web"]
	rev := d.Status.Revision

	// Re-applying the same data changes nothing.
	apply(t, st, configYAML)
	tick(st)
	if d.Status.Revision != rev {
		t.Fatalf("unchanged ConfigMap started a rollout")
	}

	apply(t, st, strings.Replace(configYAML, "MODE: blue", "MODE: green", 1))
	for range 6 {
		tick(st)
	}
	if d.Status.Revision != rev+1 {
		t.Fatalf("revision %d, want %d", d.Status.Revision, rev+1)
	}
	if h := d.Status.History[len(d.Status.History)-1]; h.Cause != "configmap/app changed" {
		t.Fatalf("cause %q", h.Cause)
	}
	if len(st.Pods) != 2 {
		t.Fatalf("%d pods", len(st.Pods))
	}
	for k := range st.Pods {
		if before[k] {
			t.Fatalf("pod %s was not replaced", k)
		}
	}
	// Digests are keyed, not plain hashes of the data.
	if d := digestData(st, map[string]string{"MODE": "green"}); d == digestData(store.NewState(), map[string]string{"MODE": "green"}) {
		t.Fatalf("digest does not depend on the cluster key")
	}

	// Opting out keeps the pods.
	st2 := testState("n1")
	apply(t, st2, strings.Replace(configYAML, "envFrom:", "reloadOnConfigChange: false\n    envFrom:", 1))
	for range 3 {
		tick(st2)
	}
	rev = st2.Deployments["default/web"].Status.Revision
	apply(t, st2, strings.Replace(strings.Replace(configYAML, "envFrom:", "reloadOnConfigChange: false\n    envFrom:", 1), "blue", "green", 1))
	tick(st2)
	if st2.Deployments["default/web"].Status.Revision != rev {
		t.Fatalf("reloadOnConfigChange: false still rolled the pods")
	}
}

func TestFailedRolloutRollsBack(t *testing.T) {
	st := testState("n1", "n2")
	yaml := `
kind: Deployment
metadata: {name: web}
spec:
  replicas: 2
  progressDeadlineSeconds: 60
  template: {image: IMAGE}
`
	now := time.Now()
	apply(t, st, strings.Replace(yaml, "IMAGE", "nginx:1", 1))
	for range 3 {
		tickAt(st, now)
	}
	apply(t, st, strings.Replace(yaml, "IMAGE", "bad", 1))
	for i := range 5 {
		tickAt(st, now.Add(time.Duration(i)*10*time.Second))
	}
	d := st.Deployments["default/web"]
	if imgs := podImages(st); imgs["nginx:1"] != 2 {
		t.Fatalf("old pods not kept during the failing rollout: %v", imgs)
	}
	if d.Spec.Template.MainImage() != "bad" {
		t.Fatalf("rolled back before the deadline")
	}

	later := now.Add(2 * time.Minute)
	for i := range 4 {
		tickAt(st, later.Add(time.Duration(i)*time.Second))
	}
	if d.Spec.Template.MainImage() != "nginx:1" {
		t.Fatalf("not rolled back: %s", d.Spec.Template.MainImage())
	}
	if imgs := podImages(st); imgs["nginx:1"] != 2 || imgs["bad"] != 0 {
		t.Fatalf("pods after rollback: %v", imgs)
	}
	if !strings.Contains(d.Status.LastRollback, "rolled back to revision 1") || d.Status.Phase != "Healthy" {
		t.Fatalf("status: %s / %s", d.Status.Phase, d.Status.LastRollback)
	}
	if h := d.Status.History[len(d.Status.History)-1]; h.Cause != "automatic rollback" {
		t.Fatalf("cause %q", h.Cause)
	}
	if !d.Status.RolloutStartedAt.IsZero() {
		t.Fatalf("the rollback itself could be rolled back")
	}

	// autoRollback: false only reports.
	st2 := testState("n1")
	off := strings.Replace(yaml, "progressDeadlineSeconds: 60", "progressDeadlineSeconds: 60\n  autoRollback: false", 1)
	apply(t, st2, strings.Replace(off, "IMAGE", "nginx:1", 1))
	for range 3 {
		tickAt(st2, now)
	}
	apply(t, st2, strings.Replace(off, "IMAGE", "bad", 1))
	tickAt(st2, now)
	for i := range 3 {
		tickAt(st2, later.Add(time.Duration(i)*time.Second))
	}
	if st2.Deployments["default/web"].Spec.Template.MainImage() != "bad" {
		t.Fatalf("autoRollback: false was ignored")
	}
}

func TestConfigOnlyFailureIsNotRolledBack(t *testing.T) {
	st := testState("n1")
	now := time.Now()
	apply(t, st, configYAML)
	for range 3 {
		tickAt(st, now)
	}
	apply(t, st, strings.Replace(configYAML, "MODE: blue", "MODE: broken", 1))
	tickAt(st, now)
	// The new pods crash (simulated).
	for _, p := range st.Pods {
		if p.Spec.TemplateHash == st.Deployments["default/web"].Status.CurrentHash && p.Spec.NodeName != "" {
			p.Status.Phase, p.Status.Ready = api.PodCrashLoop, false
		}
	}
	reconcileDeployment(st, st.Deployments["default/web"], now.Add(11*time.Minute))
	d := st.Deployments["default/web"]
	if !strings.Contains(d.Status.LastRollback, "old pods keep serving") {
		t.Fatalf("lastRollback: %q", d.Status.LastRollback)
	}
	old := 0
	for _, p := range st.Pods {
		if p.Status.Ready {
			old++
		}
	}
	if old != 2 {
		t.Fatalf("%d ready pods; the old ones must keep serving", old)
	}
}

func TestDependsOnWaitsForService(t *testing.T) {
	st := testState("n1")
	apply(t, st, `
kind: Deployment
metadata: {name: api}
spec:
  replicas: 1
  template: {image: api, dependsOn: [db]}
---
kind: Service
metadata: {name: db}
spec: {selector: {app: db}, ports: [{port: 5432}]}
`)
	tick(st)
	tick(st)
	var apiPod *api.Pod
	for _, p := range st.Pods {
		apiPod = p
	}
	if apiPod.Spec.NodeName != "" || !strings.Contains(apiPod.Status.Message, "waiting for service db") {
		t.Fatalf("api pod scheduled before db: node=%q msg=%q", apiPod.Spec.NodeName, apiPod.Status.Message)
	}
	apply(t, st, `
kind: Deployment
metadata: {name: db}
spec:
  replicas: 1
  template: {image: postgres, labels: {app: db}}
`)
	for range 3 {
		tick(st)
	}
	if apiPod.Spec.NodeName == "" {
		t.Fatalf("api pod still waiting after db became ready: %s", apiPod.Status.Message)
	}
}

func TestRecommendations(t *testing.T) {
	l := newLiveStats()
	tmpl := &api.PodTemplate{Container: api.Container{Image: "x", Resources: api.ResourceRequest{CPU: "1", Memory: "1Gi"}}}
	pods := []*api.Pod{
		{Metadata: api.Meta{Name: "web-a", Namespace: "default"}, Spec: api.PodSpec{Owner: "web", OwnerKind: api.KindDeployment}, Status: api.PodStatus{Phase: api.PodRunning}},
		{Metadata: api.Meta{Name: "web-b", Namespace: "default"}, Spec: api.PodSpec{Owner: "web", OwnerKind: api.KindDeployment}, Status: api.PodStatus{Phase: api.PodRunning}},
	}
	if r := l.recommend(api.KindDeployment, "default", "web", tmpl); r.CPU != 0 || !strings.Contains(r.Message, "collecting") {
		t.Fatalf("recommendation without data: %+v", r)
	}
	for i := range 60 {
		l.podUsage["default/web-a"] = api.Resources{CPU: 80, Memory: 100}
		l.podUsage["default/web-b"] = api.Resources{CPU: 40 + int64(i%3), Memory: 90}
		if i == 30 {
			l.podUsage["default/web-a"] = api.Resources{CPU: 900, Memory: 120} // one spike
		}
		l.sample(pods)
	}
	r := l.recommend(api.KindDeployment, "default", "web", tmpl)
	// CPU ignores the single spike (95th percentile), memory keeps the peak.
	if r.CPU != 95 || r.Memory != 144 {
		t.Fatalf("recommended %dm/%dMi, want 95m/144Mi", r.CPU, r.Memory)
	}
	if !strings.Contains(r.Message, "over-provisioned") || r.Requests.CPU != 1000 {
		t.Fatalf("message %q requests %+v", r.Message, r.Requests)
	}
	tmpl.Resources.Memory = "128Mi"
	if r := l.recommend(api.KindDeployment, "default", "web", tmpl); !strings.Contains(r.Message, "OOM") {
		t.Fatalf("message %q", r.Message)
	}
	// Deleted workloads are forgotten.
	l.sample(nil)
	if len(l.usage) != 0 {
		t.Fatalf("usage of deleted workload kept")
	}
}
