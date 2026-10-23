package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// runPods plays the agent for job pods: each scheduled pod finishes with the
// given outcome.
func finishPods(st *store.State, phase api.PodPhase) {
	for _, p := range st.Pods {
		if p.Spec.NodeName != "" && !p.Finished() && p.Status.Phase != api.PodTerminating {
			p.Status.Phase = phase
			p.Status.FinishedAt = time.Now().Add(-time.Hour)
		}
	}
}

func jobTick(st *store.State) {
	now := time.Now()
	for _, k := range sortedKeys(st.Jobs) {
		if j := st.Jobs[k]; j != nil {
			reconcileJob(st, j, now)
		}
	}
	schedulePending(st, now)
	for k, p := range st.Pods {
		if p.Status.Phase == api.PodTerminating {
			delete(st.Pods, k)
		}
	}
}

func TestJobCompletesWithParallelism(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, `
kind: Job
metadata: {name: batch}
spec:
  completions: 5
  parallelism: 2
  template: {image: busybox, command: ["true"]}
`)
	for i := 0; i < 10 && st.Jobs["default/batch"].Status.Phase == "Running"; i++ {
		jobTick(st)
		active := 0
		for _, p := range st.PodsOwned("default", api.KindJob, "batch") {
			if !p.Finished() {
				active++
			}
		}
		if active > 2 {
			t.Fatalf("parallelism exceeded: %d active pods", active)
		}
		finishPods(st, api.PodSucceeded)
	}
	j := st.Jobs["default/batch"]
	if j.Status.Phase != "Complete" || j.Status.Succeeded != 5 {
		t.Fatalf("job = %s with %d succeeded", j.Status.Phase, j.Status.Succeeded)
	}
	if got := len(st.PodsOwned("default", api.KindJob, "batch")); got != 5 {
		t.Fatalf("want exactly 5 pods, got %d", got)
	}
}

func TestJobBackoffLimit(t *testing.T) {
	st := testState("n1")
	apply(t, st, `
kind: Job
metadata: {name: flaky}
spec:
  backoffLimit: 2
  template: {image: busybox}
`)
	for i := 0; i < 10; i++ {
		jobTick(st)
		finishPods(st, api.PodFailed)
	}
	j := st.Jobs["default/flaky"]
	if j.Status.Phase != "Failed" || j.Status.Failed != 3 {
		t.Fatalf("want Failed after 3 failures (backoffLimit 2), got %s with %d", j.Status.Phase, j.Status.Failed)
	}
}

func TestCronJobCreatesJobsAndKeepsHistory(t *testing.T) {
	st := testState("n1")
	apply(t, st, `
kind: CronJob
metadata: {name: report}
spec:
  schedule: "* * * * *"
  successfulJobsHistoryLimit: 2
  jobTemplate:
    template: {image: busybox}
`)
	c := st.CronJobs["default/report"]
	c.Metadata.CreatedAt = time.Now().Add(-10 * time.Minute)
	for i := 0; i < 4; i++ {
		due := time.Now().Add(time.Duration(i) * time.Minute) // one run per minute
		c.Status.NextScheduleTime = due
		reconcileCronJob(st, c, due.Add(time.Second))
		for _, j := range st.Jobs {
			j.Status.Phase, j.Status.CompletionTime = "Complete", time.Now()
		}
	}
	reconcileCronJob(st, c, time.Now())
	if n := len(cronJobsOf(st, c)); n != 2 {
		t.Fatalf("want 2 jobs kept by history limit, got %d", n)
	}
}

func TestCronJobForbidSkipsWhileRunning(t *testing.T) {
	st := testState("n1")
	apply(t, st, `
kind: CronJob
metadata: {name: sync}
spec:
  schedule: "* * * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    template: {image: busybox}
`)
	c := st.CronJobs["default/sync"]
	for i := 0; i < 3; i++ {
		c.Status.NextScheduleTime = time.Now().Add(-time.Second)
		reconcileCronJob(st, c, time.Now().Add(time.Duration(i)*time.Minute))
	}
	if n := len(st.Jobs); n != 1 {
		t.Fatalf("Forbid: want 1 job while the first runs, got %d", n)
	}
}

func stsTick(st *store.State) {
	now := time.Now()
	for _, k := range sortedKeys(st.StatefulSets) {
		reconcileStatefulSet(st, st.StatefulSets[k], now)
	}
	schedulePending(st, now)
}

func TestStatefulSetOrderedStartAndVolumes(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, `
kind: StatefulSet
metadata: {name: db}
spec:
  replicas: 3
  serviceName: db
  template:
    image: postgres
    volumes: [{volume: data, containerPath: /data}]
  volumeClaimTemplates:
    - name: data
      spec: {type: local, size: 1Gi}
`)
	stsTick(st)
	if len(st.Pods) != 1 || st.Pods["default/db-0"] == nil {
		t.Fatalf("OrderedReady: only db-0 should exist first, got %d pods", len(st.Pods))
	}
	stsTick(st) // db-0 not ready yet: nothing new
	if len(st.Pods) != 1 {
		t.Fatal("db-1 must wait for db-0 to be ready")
	}
	for i := 0; i < 6; i++ {
		for _, p := range st.Pods {
			if p.Spec.NodeName != "" {
				p.Status.Phase, p.Status.Ready = api.PodRunning, true
			}
		}
		stsTick(st)
	}
	for i := 0; i < 3; i++ {
		name := "db-" + string(rune('0'+i))
		p := st.Pods["default/"+name]
		if p == nil {
			t.Fatalf("%s missing", name)
		}
		vol := "data-db-" + string(rune('0'+i))
		if st.Volumes["default/"+vol] == nil {
			t.Fatalf("volume %s not created", vol)
		}
		if got := p.Spec.Template.Volumes[0].Volume; got != vol {
			t.Fatalf("%s mounts %s, want its own %s", name, got, vol)
		}
		if p.Spec.Subdomain != "db" {
			t.Fatalf("%s subdomain = %q", name, p.Spec.Subdomain)
		}
	}
	// Scale down removes the highest ordinal first.
	st.StatefulSets["default/db"].Spec.Replicas = 1
	stsTick(st)
	if p := st.Pods["default/db-2"]; p == nil || p.Status.Phase != api.PodTerminating {
		t.Fatal("db-2 should terminate first")
	}
	if p := st.Pods["default/db-1"]; p.Status.Phase == api.PodTerminating {
		t.Fatal("db-1 must wait until db-2 is gone")
	}
}

func TestRBAC(t *testing.T) {
	cases := []struct {
		method, path string
		role         string
		ns           []string
		ok           bool
	}{
		{"GET", "/api/v1/overview", api.RoleView, nil, true},
		{"POST", "/api/v1/apply", api.RoleView, nil, false},
		{"POST", "/api/v1/apply", api.RoleEdit, nil, true},
		{"GET", "/api/v1/namespaces/default/secrets/x?reveal=1", api.RoleView, nil, false},
		{"POST", "/api/v1/nodes/n1/drain", api.RoleEdit, nil, false},
		{"POST", "/api/v1/nodes/n1/drain", api.RoleAdmin, nil, true},
		{"POST", "/api/v1/tokens", api.RoleEdit, nil, false},
		{"POST", "/api/v1/agent/sync", api.RoleNode, nil, true},
		{"GET", "/api/v1/overview", api.RoleNode, nil, false},
		{"DELETE", "/api/v1/namespaces/prod/pods/x", api.RoleEdit, []string{"dev"}, false},
		{"DELETE", "/api/v1/namespaces/dev/pods/x", api.RoleEdit, []string{"dev"}, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		id := &api.Identity{Role: c.role, Namespaces: c.ns}
		ok := allowed(id, requiredRole(r))
		if ok && len(c.ns) > 0 {
			ok = nsAllowed(id, "dev") && (c.path == "/api/v1/namespaces/dev/pods/x")
		}
		if ok != c.ok {
			t.Errorf("%s %s as %s%v: allowed=%v, want %v", c.method, c.path, c.role, c.ns, ok, c.ok)
		}
	}
}

func TestIngressMatching(t *testing.T) {
	in := &ingress{}
	in.routes = []ingressRoute{
		{host: "api.example.com", path: "/v1", service: "api-v1"},
		{host: "api.example.com", path: "/", service: "api"},
		{host: "*.example.com", path: "/", service: "wild"},
		{host: "", path: "/health", exact: true, service: "health"},
		{host: "", path: "/", service: "default"},
	}
	cases := map[string]string{
		"api.example.com /v1/users": "api-v1",
		"api.example.com /v10":      "api",
		"x.example.com /":           "wild",
		"a.b.example.com /":         "default", // wildcard covers one label only
		"other.org /health":         "health",
		"other.org /health/x":       "default",
	}
	for in2, want := range cases {
		var host, path string
		for i, c := range in2 {
			if c == ' ' {
				host, path = in2[:i], in2[i+1:]
			}
		}
		r := in.match(host, path)
		if r == nil || r.service != want {
			got := "<nil>"
			if r != nil {
				got = r.service
			}
			t.Errorf("%s -> %s, want %s", in2, got, want)
		}
	}
}

func TestMultiContainerTemplate(t *testing.T) {
	st := testState("n1")
	apply(t, st, `
kind: Deployment
metadata: {name: app}
spec:
  template:
    containers:
      - name: web
        image: nginx
        ports: [{containerPort: 80}]
        resources: {cpu: 200m, memory: 64Mi}
      - name: sidecar
        image: busybox
        resources: {cpu: 100m, memory: 32Mi}
    initContainers:
      - name: migrate
        image: busybox
        resources: {cpu: 500m}
`)
	d := st.Deployments["default/app"]
	r := d.Spec.Template.Requests()
	if r.CPU != 500 || r.Memory != 96 {
		t.Fatalf("requests = %+v, want cpu 500 (largest init) and memory 96", r)
	}
	bad := `
kind: Deployment
metadata: {name: bad}
spec:
  template:
    image: nginx
    containers: [{name: a, image: x}]
`
	objs, _ := api.ParseManifests([]byte(bad))
	if _, err := (&Server{}).applyObject(st, objs[0]); err == nil {
		t.Fatal("inline image together with containers must be rejected")
	}
}
