package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/pki"
	"github.com/thisisnabi/Nube/internal/store"
	"github.com/thisisnabi/Nube/internal/version"
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ci := s.store.Cluster()
		writeJSON(w, 200, map[string]any{"ok": true, "version": version.Version, "leader": ci.IsLeader, "state": ci.State})
	})
	mux.HandleFunc("GET /ca.crt", s.handleCA)
	mux.Handle("GET /", s.dashboard())

	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"version": version.Version})
	})
	mux.HandleFunc("GET /api/v1/whoami", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, identityFrom(r)) })
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)

	mux.HandleFunc("GET /api/v1/cluster", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.clusterInfo()) })
	mux.HandleFunc("POST /api/v1/cluster/join", s.handleJoin)
	mux.HandleFunc("DELETE /api/v1/cluster/servers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.RemoveServer(r.PathValue("id")); err != nil {
			writeStoreErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "removed"})
	})

	mux.HandleFunc("GET /api/v1/tokens", s.handleTokenList)
	mux.HandleFunc("POST /api/v1/tokens", s.handleTokenCreate)
	mux.HandleFunc("DELETE /api/v1/tokens/{name}", s.handleTokenDelete)

	mux.HandleFunc("POST /api/v1/apply", s.handleApply)
	mux.HandleFunc("POST /api/v1/delete", s.handleDeleteManifests)

	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) { s.handleList(w, r, api.KindNode, "") })
	mux.HandleFunc("GET /api/v1/nodes/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleGet(w, r, api.KindNode, "", r.PathValue("name"))
	})
	mux.HandleFunc("DELETE /api/v1/nodes/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleDelete(w, r, api.KindNode, "", r.PathValue("name"))
	})
	mux.HandleFunc("POST /api/v1/nodes/{name}/{action}", s.handleNodeAction)

	mux.HandleFunc("GET /api/v1/{kind}", func(w http.ResponseWriter, r *http.Request) {
		s.handleList(w, r, s.resolveKind(r.PathValue("kind")), "")
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/{kind}", func(w http.ResponseWriter, r *http.Request) {
		s.handleList(w, r, s.resolveKind(r.PathValue("kind")), r.PathValue("ns"))
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/{kind}/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleGet(w, r, s.resolveKind(r.PathValue("kind")), r.PathValue("ns"), r.PathValue("name"))
	})
	mux.HandleFunc("DELETE /api/v1/namespaces/{ns}/{kind}/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleDelete(w, r, s.resolveKind(r.PathValue("kind")), r.PathValue("ns"), r.PathValue("name"))
	})
	mux.HandleFunc("PUT /api/v1/namespaces/{ns}/{kind}/{name}/status", s.handleCustomStatus)
	mux.HandleFunc("GET /api/v1/watch/{kind}", s.handleWatch)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/volumes/{name}/move", s.handleVolumeMove)
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/volumesnapshots/{name}/data", s.handleSnapshotDownload)
	mux.HandleFunc("POST /api/v1/agent/snapshots/{ns}/{name}", s.handleSnapshotUpload)
	mux.HandleFunc("GET /api/v1/agent/snapshots/{ns}/{name}", s.handleSnapshotDownload)
	mux.HandleFunc("POST /api/v1/cluster/snapshots/stored", s.handleSnapshotStored)
	mux.HandleFunc("POST /api/v1/cluster/acme", s.handleACMECache)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/deployments/{name}/{action}", s.handleDeploymentAction)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/statefulsets/{name}/{action}", s.handleStatefulSetAction)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/cronjobs/{name}/{action}", s.handleCronJobAction)
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods/{name}/logs", s.handleLogs)

	mux.HandleFunc("POST /api/v1/agent/sync", s.handleSync)
}

// resolveKind maps a URL segment to a built-in kind or a custom resource kind.
func (s *Server) resolveKind(raw string) string {
	if k := api.NormalizeKind(raw); k != "" {
		return k
	}
	kind := ""
	s.store.Read(func(st *store.State) {
		if c := st.CRDByName(raw); c != nil {
			kind = c.Spec.Kind
		}
	})
	return kind
}

func (s *Server) clusterInfo() api.ClusterInfo {
	ci := s.store.Cluster()
	ci.TLS = s.cfg.TLS
	if c, _ := s.ca(); c != "" {
		ci.CAHash = pki.HashPEM(c)
	}
	return ci
}

// ---------------------------------------------------------------- read

// readJSON encodes the value fn returns while holding the read lock (the value
// may share memory with the store), then writes it after releasing the lock so
// slow clients never block writers.
func (s *Server) readJSON(w http.ResponseWriter, fn func(*store.State) (int, any)) {
	var code int
	var body []byte
	s.store.Read(func(st *store.State) {
		var v any
		code, v = fn(st)
		body, _ = json.Marshal(v)
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(body)
}

// visible filters a sorted list down to the namespaces an identity may see.
func visible[T any](id *api.Identity, list []T, ns func(T) string) []T {
	if len(id.Namespaces) == 0 {
		return list
	}
	out := make([]T, 0, len(list))
	for _, v := range list {
		if nsAllowed(id, ns(v)) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	// Usage metrics live in the leader's memory: ask it.
	if !s.store.IsLeader() && s.store.LeaderAPI() != "" && r.Header.Get("X-Nube-Forwarded") == "" {
		s.forwardToLeader(w, r)
		return
	}
	id := identityFrom(r)
	ov := api.Overview{Version: version.Version, StartedAt: s.started, Cluster: s.clusterInfo(), Me: *id}
	s.readJSON(w, func(st *store.State) (int, any) {
		for _, n := range sortedValues(st.Nodes, func(n *api.Node) string { return n.Name }) {
			ov.Nodes = append(ov.Nodes, s.live.node(n))
		}
		ov.Deployments = visible(id, sortedValues(st.Deployments, func(d *api.Deployment) string { return d.Metadata.Key() }), func(d *api.Deployment) string { return d.Metadata.Namespace })
		ov.StatefulSets = visible(id, sortedValues(st.StatefulSets, func(d *api.StatefulSet) string { return d.Metadata.Key() }), func(d *api.StatefulSet) string { return d.Metadata.Namespace })
		ov.Jobs = visible(id, sortedValues(st.Jobs, func(d *api.Job) string { return d.Metadata.Key() }), func(d *api.Job) string { return d.Metadata.Namespace })
		ov.CronJobs = visible(id, sortedValues(st.CronJobs, func(d *api.CronJob) string { return d.Metadata.Key() }), func(d *api.CronJob) string { return d.Metadata.Namespace })
		for _, p := range visible(id, sortedValues(st.Pods, func(p *api.Pod) string { return p.Metadata.Key() }), func(d *api.Pod) string { return d.Metadata.Namespace }) {
			ov.Pods = append(ov.Pods, s.live.pod(p))
		}
		ov.Services = s.lb.decorate(visible(id, sortedValues(st.Services, func(v *api.Service) string { return v.Metadata.Key() }), func(d *api.Service) string { return d.Metadata.Namespace }))
		ov.Ingresses = s.ingress.decorate(visible(id, sortedValues(st.Ingresses, func(v *api.Ingress) string { return v.Metadata.Key() }), func(d *api.Ingress) string { return d.Metadata.Namespace }))
		ov.ConfigMaps = visible(id, sortedValues(st.ConfigMaps, func(v *api.ConfigMap) string { return v.Metadata.Key() }), func(d *api.ConfigMap) string { return d.Metadata.Namespace })
		for _, v := range visible(id, sortedValues(st.Volumes, func(v *api.Volume) string { return v.Metadata.Key() }), func(d *api.Volume) string { return d.Metadata.Namespace }) {
			ov.Volumes = append(ov.Volumes, s.live.volume(v))
		}
		ov.Secrets = []*api.Secret{}
		for _, sec := range visible(id, sortedValues(st.Secrets, func(v *api.Secret) string { return v.Metadata.Key() }), func(d *api.Secret) string { return d.Metadata.Namespace }) {
			ov.Secrets = append(ov.Secrets, maskSecret(sec))
		}
		ov.Events = []api.Event{}
		for _, e := range st.Events[max(0, len(st.Events)-300):] {
			if e.Namespace == "" || nsAllowed(id, e.Namespace) {
				ov.Events = append(ov.Events, e)
			}
		}
		ov.DaemonSets = visible(id, sortedValues(st.DaemonSets, func(v *api.DaemonSet) string { return v.Metadata.Key() }), func(d *api.DaemonSet) string { return d.Metadata.Namespace })
		ov.Policies = visible(id, sortedValues(st.Policies, func(v *api.NetworkPolicy) string { return v.Metadata.Key() }), func(d *api.NetworkPolicy) string { return d.Metadata.Namespace })
		ov.Snapshots = visible(id, sortedValues(st.Snapshots, func(v *api.VolumeSnapshot) string { return v.Metadata.Key() }), func(d *api.VolumeSnapshot) string { return d.Metadata.Namespace })
		ov.CRDs = sortedValues(st.CRDs, func(v *api.CustomResourceDefinition) string { return v.Spec.Kind })
		ov.Custom = []*api.CustomObject{}
		for _, k := range sortedKeys(st.Custom) {
			if o := st.Custom[k]; !strings.HasPrefix(k, "~") && nsAllowed(id, o.Metadata.Namespace) {
				ov.Custom = append(ov.Custom, o)
			}
		}
		ov.Namespaces = visible(id, st.Namespaces(), func(n string) string { return n })
		return 200, ov
	})
}

func maskSecret(sec *api.Secret) *api.Secret {
	c := *sec
	c.Data = map[string]string{}
	for k := range sec.Data {
		c.Data[k] = "••••••"
	}
	return &c
}

func sortedValues[T any](m map[string]T, key func(T) string) []T {
	out := make([]T, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

// namespaced returns every object of a namespaced kind keyed "ns/name".
func namespaced(st *store.State, kind string) map[string]any {
	out := map[string]any{}
	add := func(k string, v any) { out[k] = v }
	switch kind {
	case api.KindDeployment:
		for k, v := range st.Deployments {
			add(k, v)
		}
	case api.KindStatefulSet:
		for k, v := range st.StatefulSets {
			add(k, v)
		}
	case api.KindJob:
		for k, v := range st.Jobs {
			add(k, v)
		}
	case api.KindCronJob:
		for k, v := range st.CronJobs {
			add(k, v)
		}
	case api.KindPod:
		for k, v := range st.Pods {
			add(k, v)
		}
	case api.KindService:
		for k, v := range st.Services {
			add(k, v)
		}
	case api.KindIngress:
		for k, v := range st.Ingresses {
			add(k, v)
		}
	case api.KindConfigMap:
		for k, v := range st.ConfigMaps {
			add(k, v)
		}
	case api.KindSecret:
		for k, v := range st.Secrets {
			add(k, v)
		}
	case api.KindVolume:
		for k, v := range st.Volumes {
			add(k, v)
		}
	case api.KindDaemonSet:
		for k, v := range st.DaemonSets {
			add(k, v)
		}
	case api.KindNetworkPolicy:
		for k, v := range st.Policies {
			add(k, v)
		}
	case api.KindVolumeSnapshot:
		for k, v := range st.Snapshots {
			add(k, v)
		}
	case api.KindCRD:
		for _, v := range st.CRDs {
			add("/"+v.Metadata.Name, v) // cluster-scoped
		}
	default:
		if st.CRDs[kind] != nil {
			return customList(st, kind)
		}
	}
	return out
}

func (s *Server) present(kind string, v any, reveal bool) any {
	switch o := v.(type) {
	case *api.Node:
		return s.live.node(o)
	case *api.Pod:
		return s.live.pod(o)
	case *api.Volume:
		return s.live.volume(o)
	case *api.Secret:
		if !reveal {
			return maskSecret(o)
		}
	case *api.Service:
		return s.lb.decorate([]*api.Service{o})[0]
	case *api.Ingress:
		return s.ingress.decorate([]*api.Ingress{o})[0]
	}
	return v
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, kind, ns string) {
	if kind == "" || kind == api.KindToken {
		writeErr(w, 404, "unknown resource type "+r.PathValue("kind"))
		return
	}
	id := identityFrom(r)
	reveal := r.URL.Query().Get("reveal") == "1"
	s.readJSON(w, func(st *store.State) (int, any) {
		out := []any{}
		if kind == api.KindNode {
			for _, v := range sortedValues(st.Nodes, func(n *api.Node) string { return n.Name }) {
				out = append(out, s.live.node(v))
			}
			return 200, map[string]any{"kind": kind, "items": out}
		}
		objs := namespaced(st, kind)
		for _, key := range sortedKeys(objs) {
			objNS, _, _ := strings.Cut(key, "/")
			if (ns != "" && ns != "all" && objNS != ns) || !nsAllowed(id, objNS) {
				continue
			}
			out = append(out, s.present(kind, objs[key], reveal))
		}
		return 200, map[string]any{"kind": kind, "items": out}
	})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, kind, ns, name string) {
	reveal := r.URL.Query().Get("reveal") == "1"
	s.readJSON(w, func(st *store.State) (int, any) {
		var v any
		if kind == api.KindNode {
			if n := st.Nodes[name]; n != nil {
				v = s.live.node(n)
			}
		} else if kind == api.KindCRD {
			if o, ok := namespaced(st, kind)["/"+name]; ok {
				v = o
			}
		} else if o, ok := namespaced(st, kind)[ns+"/"+name]; ok {
			v = s.present(kind, o, reveal)
		}
		if v == nil {
			return 404, map[string]string{"error": fmt.Sprintf("%s %q not found", strings.ToLower(kind), name)}
		}
		return 200, v
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	s.readJSON(w, func(st *store.State) (int, any) {
		out := []api.Event{}
		for _, e := range st.Events {
			if e.ID > since && (e.Namespace == "" || nsAllowed(id, e.Namespace)) {
				out = append(out, e)
			}
		}
		return 200, out
	})
}

// ---------------------------------------------------------------- write

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 8<<20))
}

func (s *Server) parseRequestManifests(w http.ResponseWriter, r *http.Request) ([]api.Object, bool) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return nil, false
	}
	objs, err := api.ParseManifests(body)
	if err != nil {
		writeErr(w, 400, err.Error())
		return nil, false
	}
	if ns := r.URL.Query().Get("namespace"); ns != "" {
		for i := range objs {
			var probe struct {
				Metadata api.Meta `json:"metadata"`
			}
			json.Unmarshal(objs[i].Raw, &probe)
			if probe.Metadata.Namespace == "" {
				objs[i].Namespace = ns
			}
		}
	}
	id := identityFrom(r)
	for _, o := range objs {
		if !nsAllowed(id, o.Namespace) {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("%s may not change namespace %s", id.Name, o.Namespace))
			return nil, false
		}
		if o.Kind == api.KindCRD && id.Role != api.RoleAdmin {
			writeErr(w, http.StatusForbidden, "custom resource definitions are cluster-wide: only admins may change them")
			return nil, false
		}
		if o.Kind == api.KindNode || o.Kind == api.KindToken {
			writeErr(w, 400, strings.ToLower(o.Kind)+" objects cannot be applied from manifests")
			return nil, false
		}
	}
	return objs, true
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	objs, ok := s.parseRequestManifests(w, r)
	if !ok {
		return
	}
	// Dependencies first so everything in one file validates in one apply.
	order := map[string]int{
		api.KindVolume: 0, api.KindConfigMap: 1, api.KindSecret: 1,
		api.KindDeployment: 2, api.KindStatefulSet: 2, api.KindJob: 2, api.KindCronJob: 2, api.KindPod: 2,
		api.KindService: 3, api.KindIngress: 4,
	}
	sort.SliceStable(objs, func(i, j int) bool { return order[objs[i].Kind] < order[objs[j].Kind] })

	var results []string
	err := s.store.Write(func(st *store.State) error {
		results = results[:0]
		for _, o := range objs {
			if !api.ValidName(o.Namespace) {
				return fmt.Errorf("invalid namespace %q", o.Namespace)
			}
			res, err := s.applyObject(st, o)
			if err != nil {
				return err
			}
			results = append(results, fmt.Sprintf("%s/%s %s", strings.ToLower(o.Kind), o.Name, res))
		}
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

func (s *Server) handleDeleteManifests(w http.ResponseWriter, r *http.Request) {
	objs, ok := s.parseRequestManifests(w, r)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	var results []string
	err := s.store.Write(func(st *store.State) error {
		results = results[:0]
		for _, o := range objs {
			err := s.deleteObject(st, o.Kind, o.Namespace, o.Name, force)
			var nf notFound
			switch {
			case err == nil:
				results = append(results, fmt.Sprintf("%s/%s deleted", strings.ToLower(o.Kind), o.Name))
			case asNotFound(err, &nf):
				results = append(results, fmt.Sprintf("%s/%s not found", strings.ToLower(o.Kind), o.Name))
			default:
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

func asNotFound(err error, nf *notFound) bool {
	v, ok := err.(notFound)
	if ok {
		*nf = v
	}
	return ok
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, kind, ns, name string) {
	if kind == "" || kind == api.KindToken {
		writeErr(w, 404, "unknown resource type")
		return
	}
	if kind == api.KindCRD && identityFrom(r).Role != api.RoleAdmin {
		writeErr(w, http.StatusForbidden, "custom resource definitions are cluster-wide: only admins may delete them")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	err := s.store.Write(func(st *store.State) error { return s.deleteObject(st, kind, ns, name, force) })
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": strings.ToLower(kind) + "/" + name + " deleted"})
}

func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	var body struct {
		Labels map[string]string `json:"labels"`
	}
	if action == "labels" {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, "body must be {\"labels\": {...}}")
			return
		}
	}
	msg := ""
	err := s.store.Write(func(st *store.State) error {
		n := st.Nodes[name]
		if n == nil {
			return notFound("node/" + name)
		}
		switch action {
		case "cordon":
			n.Cordoned = true
			msg = "node cordoned: no new pods will be scheduled"
		case "uncordon":
			n.Cordoned = false
			msg = "node uncordoned"
		case "drain":
			n.Cordoned = true
			count := 0
			for _, p := range st.Pods {
				if p.Spec.NodeName == name && p.Status.Phase != api.PodTerminating && !p.Finished() {
					terminatePod(st, p, false)
					count++
				}
			}
			msg = fmt.Sprintf("node drained: %d pods evicted", count)
		case "labels":
			if n.Labels == nil {
				n.Labels = map[string]string{}
			}
			for k, v := range body.Labels {
				if v == "" {
					delete(n.Labels, k)
				} else {
					n.Labels[k] = v
				}
			}
			msg = "labels updated"
		default:
			return fmt.Errorf("unknown node action %q (cordon, uncordon, drain, labels)", action)
		}
		st.Event("Normal", api.KindNode, "", name, title(action), msg)
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": msg})
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

type actionBody struct {
	Replicas  *int   `json:"replicas"`
	Revision  int    `json:"revision"`
	Image     string `json:"image"`
	Container string `json:"container"`
}

// setImage changes the image of one container of a template ("" = the only/first one).
func setImage(t *api.PodTemplate, container, image string) error {
	if len(t.Containers) == 0 {
		if container != "" && container != "main" && container != t.Name {
			return fmt.Errorf("no container named %q", container)
		}
		t.Image = image
		return nil
	}
	for i := range t.Containers {
		if container == "" && i == 0 || t.Containers[i].Name == container {
			t.Containers[i].Image = image
			return nil
		}
	}
	for i := range t.InitContainers {
		if t.InitContainers[i].Name == container {
			t.InitContainers[i].Image = image
			return nil
		}
	}
	return fmt.Errorf("no container named %q", container)
}

func restartAnnotation(t *api.PodTemplate) {
	if t.Annotations == nil {
		t.Annotations = map[string]string{}
	}
	t.Annotations["nube/restartedAt"] = time.Now().UTC().Format(time.RFC3339)
}

func (s *Server) handleDeploymentAction(w http.ResponseWriter, r *http.Request) {
	ns, name, action := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	var body actionBody
	json.NewDecoder(r.Body).Decode(&body)
	msg := ""
	err := s.store.Write(func(st *store.State) error {
		d := st.Deployments[ns+"/"+name]
		if d == nil {
			return notFound("deployment/" + name)
		}
		switch action {
		case "scale":
			if body.Replicas == nil || *body.Replicas < 0 {
				return fmt.Errorf("body must be {\"replicas\": N} with N >= 0")
			}
			if a := d.Spec.Autoscale; a != nil {
				return fmt.Errorf("deployment has autoscale (min %d, max %d); edit autoscale instead", a.Min, a.Max)
			}
			d.Spec.Replicas = *body.Replicas
			msg = fmt.Sprintf("scaled to %d replicas", d.Spec.Replicas)
		case "restart":
			restartAnnotation(&d.Spec.Template)
			msg = "rolling restart started"
		case "set-image":
			if body.Image == "" {
				return fmt.Errorf("body must be {\"image\": \"...\", \"container\": \"optional\"}")
			}
			if err := setImage(&d.Spec.Template, body.Container, body.Image); err != nil {
				return err
			}
			msg = "image set to " + body.Image
		case "rollback":
			target := body.Revision
			var rev *api.Revision
			for i := len(d.Status.History) - 1; i >= 0; i-- {
				h := &d.Status.History[i]
				if (target == 0 && h.Hash != d.Status.CurrentHash) || (target != 0 && h.Revision == target) {
					rev = h
					break
				}
			}
			if rev == nil {
				return fmt.Errorf("no revision to roll back to")
			}
			d.Spec.Template = deepCopy(rev.Template)
			msg = fmt.Sprintf("rolling back to revision %d", rev.Revision)
		case "pause":
			d.Spec.Paused = true
			msg = "rollout paused"
		case "resume":
			d.Spec.Paused = false
			msg = "rollout resumed"
		default:
			return fmt.Errorf("unknown action %q (scale, restart, set-image, rollback, pause, resume)", action)
		}
		d.Metadata.UpdatedAt = time.Now()
		st.Event("Normal", api.KindDeployment, ns, name, title(action), msg)
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": msg})
}

func (s *Server) handleStatefulSetAction(w http.ResponseWriter, r *http.Request) {
	ns, name, action := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	var body actionBody
	json.NewDecoder(r.Body).Decode(&body)
	msg := ""
	err := s.store.Write(func(st *store.State) error {
		ss := st.StatefulSets[ns+"/"+name]
		if ss == nil {
			return notFound("statefulset/" + name)
		}
		switch action {
		case "scale":
			if body.Replicas == nil || *body.Replicas < 0 {
				return fmt.Errorf("body must be {\"replicas\": N} with N >= 0")
			}
			ss.Spec.Replicas = *body.Replicas
			msg = fmt.Sprintf("scaled to %d replicas", ss.Spec.Replicas)
		case "restart":
			restartAnnotation(&ss.Spec.Template)
			msg = "rolling restart started (highest ordinal first)"
		case "set-image":
			if err := setImage(&ss.Spec.Template, body.Container, body.Image); err != nil {
				return err
			}
			msg = "image set to " + body.Image
		default:
			return fmt.Errorf("unknown action %q (scale, restart, set-image)", action)
		}
		ss.Metadata.UpdatedAt = time.Now()
		st.Event("Normal", api.KindStatefulSet, ns, name, title(action), msg)
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": msg})
}

func (s *Server) handleCronJobAction(w http.ResponseWriter, r *http.Request) {
	ns, name, action := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	msg := ""
	err := s.store.Write(func(st *store.State) error {
		c := st.CronJobs[ns+"/"+name]
		if c == nil {
			return notFound("cronjob/" + name)
		}
		switch action {
		case "trigger":
			now := time.Now()
			j := startJobFromCron(st, c, now, now, true)
			if j == nil {
				return fmt.Errorf("could not create a job, try again")
			}
			msg = "started job " + j.Metadata.Name
		case "suspend":
			c.Spec.Suspend = true
			msg = "cronjob suspended"
		case "resume":
			c.Spec.Suspend = false
			c.Status.NextScheduleTime = time.Time{}
			msg = "cronjob resumed"
		default:
			return fmt.Errorf("unknown action %q (trigger, suspend, resume)", action)
		}
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": msg})
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		RaftAddr string `json:"raftAddr"`
		APIAddr  string `json:"apiAddr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" || req.RaftAddr == "" {
		writeErr(w, 400, "body must include id, raftAddr and apiAddr")
		return
	}
	if err := s.store.AddServer(req.ID, req.RaftAddr, req.APIAddr); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "joined"})
}

// agentClient talks to node agents: over mutual TLS when the cluster has a CA
// (the server proves itself with a CA-signed client certificate), otherwise
// plain HTTP with the bootstrap token.
func (s *Server) agentClient(n *api.Node) (*http.Client, string, error) {
	if !n.TLS {
		return &http.Client{}, "http://" + n.Address, nil
	}
	caCert, _ := s.ca()
	if caCert == "" {
		return nil, "", fmt.Errorf("cluster CA not ready")
	}
	cert, err := s.certs.get(nil)
	if err != nil {
		return nil, "", err
	}
	cfg := pki.ClientConfig("", caCert, false)
	cfg.Certificates = []tls.Certificate{*cert}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}, "https://" + n.Address, nil
}

// handleLogs proxies a log request to the agent that runs the pod.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	container := r.URL.Query().Get("container")
	var node *api.Node
	var cid string
	var names []string
	s.store.Read(func(st *store.State) {
		p := st.Pods[ns+"/"+name]
		if p == nil {
			return
		}
		cid = p.Status.ContainerID
		for _, c := range p.Status.Containers {
			names = append(names, c.Name)
			if c.Name == container {
				cid = c.ContainerID
			}
		}
		if n := st.Nodes[p.Spec.NodeName]; n != nil {
			c := *n
			node = &c
		}
	})
	if container != "" && !contains(names, container) {
		writeErr(w, 404, fmt.Sprintf("pod has no container %q (containers: %s)", container, strings.Join(names, ", ")))
		return
	}
	if node == nil || cid == "" {
		writeErr(w, 404, "pod not found or has no container yet")
		return
	}
	client, base, err := s.agentClient(node)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	q := url.Values{}
	q.Set("id", cid)
	q.Set("tail", r.URL.Query().Get("tail"))
	q.Set("follow", r.URL.Query().Get("follow"))
	req, _ := http.NewRequestWithContext(r.Context(), "GET", base+"/logs?"+q.Encode(), nil)
	if !node.TLS {
		// Plain-HTTP clusters only; with TLS the server authenticates by
		// client certificate and never hands its admin token to a node.
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, 502, "agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
