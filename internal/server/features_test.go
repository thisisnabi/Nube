package server

import (
	"encoding/json"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

func TestDaemonSetOnePodPerNode(t *testing.T) {
	st := testState("n1", "n2", "n3")
	st.Nodes["n3"].Labels["role"] = "edge"
	st.Nodes["n2"].Cordoned = true // daemonsets ignore cordoning
	apply(t, st, `
kind: DaemonSet
metadata: {name: agent}
spec:
  template: {image: busybox}
`)
	ds := st.DaemonSets["default/agent"]
	reconcileDaemonSet(st, ds, time.Now())
	perNode := map[string]int{}
	for _, p := range st.PodsOwned("default", api.KindDaemonSet, "agent") {
		perNode[p.Spec.NodeName]++
	}
	if len(perNode) != 3 || perNode["n1"] != 1 || perNode["n2"] != 1 || perNode["n3"] != 1 {
		t.Fatalf("want one pod on each of 3 nodes, got %v", perNode)
	}

	// Restrict to edge nodes: pods elsewhere go away.
	ds.Spec.NodeSelector = map[string]string{"role": "edge"}
	reconcileDaemonSet(st, ds, time.Now())
	for _, p := range st.PodsOwned("default", api.KindDaemonSet, "agent") {
		if p.Spec.NodeName != "n3" && p.Status.Phase != api.PodTerminating {
			t.Fatalf("pod on %s should be terminating", p.Spec.NodeName)
		}
	}

	// A new matching node gets a pod.
	st.Nodes["n4"] = &api.Node{Name: "n4", Status: api.NodeReady, Labels: map[string]string{"role": "edge"}, LastHeartbeat: time.Now()}
	reconcileDaemonSet(st, ds, time.Now())
	found := false
	for _, p := range st.PodsOwned("default", api.KindDaemonSet, "agent") {
		found = found || p.Spec.NodeName == "n4"
	}
	if !found {
		t.Fatal("no pod scheduled on the new node n4")
	}
}

func TestDaemonSetRollingUpdate(t *testing.T) {
	st := testState("n1", "n2", "n3")
	apply(t, st, `
kind: DaemonSet
metadata: {name: agent}
spec:
  template: {image: busybox:1}
`)
	ds := st.DaemonSets["default/agent"]
	tickDS := func() {
		reconcileDaemonSet(st, ds, time.Now())
		for k, p := range st.Pods {
			if p.Status.Phase == api.PodTerminating {
				delete(st.Pods, k)
			} else {
				p.Status.Phase, p.Status.Ready = api.PodRunning, true
			}
		}
	}
	tickDS()
	ds.Spec.Template.Image = "busybox:2"
	for i := 0; i < 12; i++ {
		ready := 0
		for _, p := range st.PodsOwned("default", api.KindDaemonSet, "agent") {
			if p.Status.Ready {
				ready++
			}
		}
		if ready < 2 {
			t.Fatalf("more than maxUnavailable=1 node down during rollout (ready=%d)", ready)
		}
		tickDS()
	}
	for _, p := range st.PodsOwned("default", api.KindDaemonSet, "agent") {
		if p.Spec.Template.Image != "busybox:2" {
			t.Fatalf("pod on %s still runs %s", p.Spec.NodeName, p.Spec.Template.Image)
		}
	}
}

func TestSubtractCIDRs(t *testing.T) {
	got := subtractCIDRs("10.0.0.0/24", []string{"10.0.0.0/26", "10.0.0.200/32"})
	sort.Strings(got)
	// /24 minus .0/26 minus .200 = .64/26, .128/26, and .192/26 without .200
	total := 0
	for _, c := range got {
		var bits int
		if i := strings.Index(c, "/"); i > 0 {
			json.Unmarshal([]byte(c[i+1:]), &bits)
		}
		total += 1 << (32 - bits)
	}
	if total != 256-64-1 {
		t.Fatalf("covered %d addresses (%v), want 191", total, got)
	}
	for _, c := range got {
		if c == "10.0.0.0/26" || strings.HasPrefix(c, "10.0.0.200/") {
			t.Fatalf("excepted range still present: %v", got)
		}
	}
}

func TestNetworkPolicyResolution(t *testing.T) {
	st := testState("n1")
	pod := func(name, ns, ip string, labels map[string]string) {
		st.Pods[ns+"/"+name] = &api.Pod{Kind: api.KindPod, Metadata: api.Meta{Name: name, Namespace: ns, Labels: labels},
			Spec: api.PodSpec{NodeName: "n1"}, Status: api.PodStatus{Phase: api.PodRunning, PodIP: ip}}
	}
	pod("db", "default", "10.88.1.2", map[string]string{"app": "db"})
	pod("web", "default", "10.88.1.3", map[string]string{"role": "frontend"})
	pod("other", "default", "10.88.1.4", map[string]string{"role": "batch"})
	pod("prom", "monitoring", "10.88.1.5", map[string]string{"app": "prometheus"})
	apply(t, st, `
kind: NetworkPolicy
metadata: {name: db}
spec:
  podSelector: {matchLabels: {app: db}}
  ingress:
    - from: [{podSelector: {matchLabels: {role: frontend}}}]
      ports: [{port: 5432}]
    - from: [{podSelector: {matchLabels: {app: prometheus}}, namespaces: [monitoring]}]
      ports: [{port: 9187}]
`)
	fw := firewallFor(st, "n1")
	if len(fw) != 1 || fw[0].PodIP != "10.88.1.2" || !fw[0].IsolateIngress || fw[0].IsolateEgress {
		t.Fatalf("only the db pod should be isolated (ingress): %+v", fw)
	}
	rules := fw[0].Ingress
	if len(rules) != 2 || strings.Join(rules[0].CIDRs, ",") != "10.88.1.3/32" || rules[0].Ports[0].Port != 5432 ||
		strings.Join(rules[1].CIDRs, ",") != "10.88.1.5/32" {
		t.Fatalf("unexpected rules: %+v", rules)
	}
}

func TestCRDValidationAndGarbageCollection(t *testing.T) {
	st := testState("n1")
	apply(t, st, `
kind: CustomResourceDefinition
metadata: {name: websites}
spec:
  kind: Website
  schema:
    type: object
    required: [image]
    properties:
      image: {type: string}
      replicas: {type: integer, minimum: 1, default: 2}
      tier: {type: string, enum: [free, pro]}
`)
	s := &Server{}
	bad := []string{
		"kind: Website\nmetadata: {name: a}\nspec: {replicas: 2}",                // missing image
		"kind: Website\nmetadata: {name: a}\nspec: {image: x, replicas: 0}",      // below minimum
		"kind: Website\nmetadata: {name: a}\nspec: {image: x, tier: gold}",       // not in enum
		"kind: Website\nmetadata: {name: a}\nspec: {image: x, colour: red}",      // unknown field
		"kind: Gadget\nmetadata: {name: a}\nspec: {}",                            // no CRD
		"kind: CustomResourceDefinition\nmetadata: {name: x}\nspec: {kind: Pod}", // clashes with built-in
	}
	for _, b := range bad {
		objs, err := api.ParseManifests([]byte(b))
		if err != nil {
			continue
		}
		if _, err := s.applyObject(st, objs[0]); err == nil {
			t.Errorf("should be rejected: %s", b)
		}
	}
	apply(t, st, "kind: Website\nmetadata: {name: blog}\nspec: {image: nginx}")
	o := st.Custom[store.CustomKey("Website", "default", "blog")]
	var spec map[string]any
	json.Unmarshal(o.Spec, &spec)
	if spec["replicas"] != float64(2) || o.Generation != 1 {
		t.Fatalf("default not applied or wrong generation: %s gen=%d", o.Spec, o.Generation)
	}
	apply(t, st, "kind: Website\nmetadata: {name: blog}\nspec: {image: nginx, replicas: 3}")
	if g := st.Custom[store.CustomKey("Website", "default", "blog")].Generation; g != 2 {
		t.Fatalf("generation = %d, want 2 after a spec change", g)
	}

	// The operator created a deployment owned by the website; deleting the website removes it.
	apply(t, st, `
kind: Deployment
metadata: {name: blog, labels: {nube/owned-by: Website/blog}}
spec: {template: {image: nginx}}
`)
	if err := s.deleteObject(st, "Website", "default", "blog", false); err != nil {
		t.Fatal(err)
	}
	if st.Deployments["default/blog"] != nil {
		t.Fatal("owned deployment was not garbage-collected")
	}
	if st.Custom["~"+store.CustomKey("Website", "default", "blog")] == nil {
		t.Fatal("no tombstone for watchers")
	}
}

func TestWatchReturnsChanges(t *testing.T) {
	fs, err := store.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	s := &Server{store: fs, live: newLiveStats()}
	fs.Write(func(st *store.State) error {
		objs, _ := api.ParseManifests([]byte("kind: CustomResourceDefinition\nmetadata: {name: things}\nspec: {kind: Thing, schema: {type: object}}\n---\nkind: Thing\nmetadata: {name: a}\nspec: {}"))
		for _, o := range objs {
			if _, err := s.applyObject(st, o); err != nil {
				return err
			}
		}
		return nil
	})
	watch := func(since string) (rev int64, events []api.WatchEvent) {
		r := httptest.NewRequest("GET", "/api/v1/watch/things?timeout=1&since="+since, nil)
		r.SetPathValue("kind", "things")
		w := httptest.NewRecorder()
		s.handleWatch(w, r.WithContext(r.Context()))
		var out struct {
			Revision int64            `json:"revision"`
			Events   []api.WatchEvent `json:"events"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.Revision, out.Events
	}
	rev, ev := watch("0")
	if len(ev) != 1 {
		t.Fatalf("initial watch: %d events", len(ev))
	}
	fs.Write(func(st *store.State) error { return deleteCustom(st, "Thing", "default", "a") })
	_, ev = watch(itoa(rev))
	if len(ev) != 1 || ev[0].Type != "DELETED" {
		t.Fatalf("want one DELETED event, got %+v", ev)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestVolumeMoveFlow(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, `
kind: Volume
metadata: {name: data}
spec: {type: local, size: 1Gi}
---
kind: Deployment
metadata: {name: db}
spec:
  template:
    image: redis
    volumes: [{volume: data, containerPath: /data}]
`)
	tick(st)
	tick(st)
	v := st.Volumes["default/data"]
	from := v.Status.NodeName
	to := map[string]string{"n1": "n2", "n2": "n1"}[from]
	v.Status.Migration = &api.VolumeMigration{Target: to, Phase: "Draining", Started: time.Now()}

	step := func() {
		now := time.Now()
		reconcileMigrations(st, now, nil2seen)
		reconcileDeployment(st, st.Deployments["default/db"], now)
		reconcileSnapshots(st, now)
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
	step() // pods stopped
	for _, p := range st.PodsOf("default", "db") {
		if p.Spec.NodeName != "" {
			t.Fatalf("pod %s scheduled while the volume is being moved", p.Metadata.Name)
		}
	}
	step() // snapshot requested
	if v.Status.Migration.Phase != "Snapshotting" {
		t.Fatalf("phase = %s, want Snapshotting", v.Status.Migration.Phase)
	}
	snap := st.Snapshots["default/"+v.Status.Migration.Snapshot]
	if snap == nil || snap.Status.Node != from {
		t.Fatalf("snapshot must be taken on %s: %+v", from, snap)
	}
	snap.Status.Phase = "Ready" // the agent uploaded it
	step()
	if v.Status.NodeName != to || v.Status.RestoreFrom != snap.Metadata.Name {
		t.Fatalf("volume should now be bound to %s and restore %s: %+v", to, snap.Metadata.Name, v.Status)
	}
	step()
	for _, p := range st.PodsOf("default", "db") {
		if p.Spec.NodeName != to {
			t.Fatalf("pod should run on %s now, got %q", to, p.Spec.NodeName)
		}
	}
	v.Status.Restored = v.Status.RestoreFrom // the agent restored it
	step()
	if v.Status.Migration != nil {
		t.Fatalf("migration not finished: %+v", v.Status.Migration)
	}
}

func nil2seen(n *api.Node) time.Time { return n.LastHeartbeat }

func TestAutoRecoverFromSnapshot(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, "kind: Volume\nmetadata: {name: data}\nspec: {type: local, autoRecover: true}")
	v := st.Volumes["default/data"]
	v.Status.NodeName, v.Status.Phase = "n1", "Bound"
	st.Snapshots["default/data-1"] = &api.VolumeSnapshot{Metadata: api.Meta{Name: "data-1", Namespace: "default"},
		Spec: api.VolumeSnapshotSpec{Volume: "data"}, Status: api.VolumeSnapshotStatus{Phase: "Ready", TakenAt: time.Now().Add(-time.Hour)}}
	n1 := st.Nodes["n1"]
	n1.Status, n1.LastHeartbeat = api.NodeNotReady, time.Now().Add(-10*time.Minute)
	reconcileMigrations(st, time.Now(), nil2seen)
	if v.Status.NodeName != "n2" || v.Status.RestoreFrom != "data-1" {
		t.Fatalf("volume should be recovered on n2 from data-1: %+v", v.Status)
	}
}

func TestScheduledBackupsAndPruning(t *testing.T) {
	st := testState("n1")
	apply(t, st, "kind: Volume\nmetadata: {name: data}\nspec: {type: local, backup: {schedule: '* * * * *', keep: 2}}")
	v := st.Volumes["default/data"]
	v.Status.NodeName = "n1"
	v.Metadata.CreatedAt = time.Now().Add(-time.Hour)
	for i := 0; i < 4; i++ {
		now := time.Now().Add(time.Duration(i) * time.Minute)
		reconcileBackups(st, now)
		for _, s := range st.Snapshots {
			if s.Status.Phase == "Pending" {
				s.Status.Phase, s.Status.TakenAt = "Ready", now
			}
		}
	}
	reconcileBackups(st, time.Now().Add(4*time.Minute))
	ready := 0
	for _, s := range st.Snapshots {
		if s.Status.Phase == "Ready" {
			ready++
		}
	}
	if ready != 2 {
		t.Fatalf("keep: 2 should leave 2 ready backups, got %d", ready)
	}
}

func TestSyncNeedsWriteOnlyOnChange(t *testing.T) {
	st := testState("n1")
	n := st.Nodes["n1"]
	n.PodCIDR = "10.88.1.0/24"
	req := &api.SyncRequest{Node: *n}
	req.Node.Usage = api.Resources{CPU: 999, Memory: 999} // usage alone must not cause a write
	if syncNeedsWrite(st, req, time.Now()) {
		t.Fatal("a heartbeat with only new usage numbers should not be replicated")
	}
	req.Node.NetworkReady = !n.NetworkReady
	if !syncNeedsWrite(st, req, time.Now()) {
		t.Fatal("a network status change must be replicated")
	}
}

// Regression: after a recovery, backups must not run against the not-yet-
// restored copy, and the snapshot being restored must never be pruned.
func TestBackupsWaitForPendingRestore(t *testing.T) {
	st := testState("n1")
	apply(t, st, "kind: Volume\nmetadata: {name: data}\nspec: {type: local, backup: {schedule: '* * * * *', keep: 1}}")
	v := st.Volumes["default/data"]
	v.Metadata.CreatedAt = time.Now().Add(-time.Hour)
	st.Snapshots["default/good"] = &api.VolumeSnapshot{Metadata: api.Meta{Name: "good", Namespace: "default", Labels: map[string]string{"nube/backup": "true"}},
		Spec: api.VolumeSnapshotSpec{Volume: "data"}, Status: api.VolumeSnapshotStatus{Phase: "Ready", TakenAt: time.Now().Add(-time.Hour)}}
	st.Snapshots["default/newer"] = &api.VolumeSnapshot{Metadata: api.Meta{Name: "newer", Namespace: "default", Labels: map[string]string{"nube/backup": "true"}},
		Spec: api.VolumeSnapshotSpec{Volume: "data"}, Status: api.VolumeSnapshotStatus{Phase: "Ready", TakenAt: time.Now()}}
	v.Status.NodeName, v.Status.RestoreFrom = "n1", "good" // recovering from "good"

	reconcileBackups(st, time.Now().Add(2*time.Minute))
	if st.Snapshots["default/good"] == nil {
		t.Fatal("the snapshot being restored was pruned")
	}
	for k, s := range st.Snapshots {
		if s.Status.Phase == "Pending" {
			t.Fatalf("backup %s taken while the restore is pending", k)
		}
	}
	objs, _ := api.ParseManifests([]byte("kind: VolumeSnapshot\nmetadata: {name: good}\nspec: {volume: data}"))
	if err := (&Server{}).deleteObject(st, api.KindVolumeSnapshot, "default", objs[0].Name, false); err == nil {
		t.Fatal("deleting a snapshot needed by a restore must be refused")
	}
}

// Regression: a ReadWriteOnce NFS volume must be released when its pod leaves
// a node, even though the replacement pod (not yet scheduled) still uses it.
func TestNFSReadWriteOnceFollowsPod(t *testing.T) {
	st := testState("n1", "n2")
	apply(t, st, `
kind: Volume
metadata: {name: db}
spec: {type: nfs, accessMode: ReadWriteOnce, size: 1Gi, nfs: {server: 10.0.0.9, path: /x}}
---
kind: Deployment
metadata: {name: db}
spec:
  strategy: {maxSurge: 0, maxUnavailable: 1}
  template: {image: redis, volumes: [{volume: db, containerPath: /data}]}
`)
	tick(st)
	tick(st)
	first := st.PodsOf("default", "db")[0].Spec.NodeName
	st.Nodes[first].Cordoned = true
	for _, p := range st.PodsOf("default", "db") {
		terminatePod(st, p, false)
	}
	for i := 0; i < 5; i++ {
		tick(st)
		reconcileVolumes(st)
	}
	pods := st.PodsOf("default", "db")
	if len(pods) != 1 || pods[0].Spec.NodeName == "" || pods[0].Spec.NodeName == first {
		t.Fatalf("pod should have moved off %s: %+v", first, pods)
	}
}
