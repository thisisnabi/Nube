package server

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

func newUID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func randSuffix(n int) string {
	const letters = "bcdfghjklmnpqrstvwxz2456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// deepCopy copies any JSON-serializable value.
func deepCopy[T any](v T) T {
	var out T
	b, _ := json.Marshal(v)
	json.Unmarshal(b, &out)
	return out
}

func touchMeta(m *api.Meta, existing *api.Meta) {
	now := time.Now()
	if m.Namespace == "" {
		m.Namespace = api.DefaultNamespace
	}
	if existing != nil {
		m.UID = existing.UID
		m.CreatedAt = existing.CreatedAt
	} else {
		m.UID = newUID()
		m.CreatedAt = now
	}
	m.UpdatedAt = now
}

// applyObject creates or updates one object. It returns "created", "configured" or "unchanged".
func (s *Server) applyObject(st *store.State, o api.Object) (string, error) {
	switch o.Kind {
	case api.KindDeployment:
		return applyDeployment(st, o)
	case api.KindService:
		return applyService(st, o)
	case api.KindConfigMap:
		return applyConfig(st, o, false)
	case api.KindSecret:
		return applyConfig(st, o, true)
	case api.KindVolume:
		return applyVolume(st, o)
	case api.KindPod:
		return applyPod(st, o)
	case api.KindJob:
		return applyJob(st, o)
	case api.KindCronJob:
		return applyCronJob(st, o)
	case api.KindStatefulSet:
		return applyStatefulSet(st, o)
	case api.KindIngress:
		return applyIngress(st, o)
	}
	return "", fmt.Errorf("kind %s cannot be applied", o.Kind)
}

func applyDeployment(st *store.State, o api.Object) (string, error) {
	var d api.Deployment
	if err := json.Unmarshal(o.Raw, &d); err != nil {
		return "", err
	}
	// Distinguish "replicas: 0" from a missing field.
	var probe struct {
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
	}
	json.Unmarshal(o.Raw, &probe)
	if probe.Spec.Replicas == nil {
		d.Spec.Replicas = 1
	}
	if d.Spec.Replicas < 0 {
		return "", fmt.Errorf("replicas must be >= 0")
	}
	t := &d.Spec.Template
	if err := t.Validate(); err != nil {
		return "", fmt.Errorf("deployment/%s: %w", o.Name, err)
	}
	if len(t.Labels) == 0 {
		t.Labels = map[string]string{"app": o.Name}
	}
	if d.Spec.Strategy == nil {
		d.Spec.Strategy = &api.Strategy{MaxSurge: 1, MaxUnavailable: 0}
	}
	if d.Spec.Strategy.MaxSurge == 0 && d.Spec.Strategy.MaxUnavailable == 0 {
		d.Spec.Strategy.MaxUnavailable = 1
	}
	if a := d.Spec.Autoscale; a != nil {
		if a.Min < 1 {
			a.Min = 1
		}
		if a.Max < a.Min {
			return "", fmt.Errorf("autoscale.max must be >= autoscale.min")
		}
		if a.TargetCPUPercent <= 0 {
			a.TargetCPUPercent = 70
		}
	}
	if err := checkVolumeRefs(st, o.Namespace, t, nil); err != nil {
		return "", fmt.Errorf("deployment/%s: %w", o.Name, err)
	}
	d.Kind = api.KindDeployment
	d.Metadata.Name, d.Metadata.Namespace = o.Name, o.Namespace
	key := d.Metadata.Key()
	old := st.Deployments[key]
	if old != nil {
		if a := d.Spec.Autoscale; a != nil {
			// The autoscaler owns the replica count; keep it inside the new bounds.
			d.Spec.Replicas = clamp(old.Spec.Replicas, a.Min, a.Max)
		}
		if jsonEqual(old.Spec, d.Spec) && jsonEqual(old.Metadata.Labels, d.Metadata.Labels) {
			return "unchanged", nil
		}
		touchMeta(&d.Metadata, &old.Metadata)
		d.Status = old.Status
		st.Deployments[key] = &d
		st.Event("Normal", api.KindDeployment, o.Namespace, o.Name, "Updated", "deployment spec updated")
		return "configured", nil
	}
	if a := d.Spec.Autoscale; a != nil {
		d.Spec.Replicas = clamp(d.Spec.Replicas, a.Min, a.Max)
	}
	touchMeta(&d.Metadata, nil)
	st.Deployments[key] = &d
	st.Event("Normal", api.KindDeployment, o.Namespace, o.Name, "Created", fmt.Sprintf("deployment created with %d replicas of %s", d.Spec.Replicas, t.MainImage()))
	return "created", nil
}

func applyService(st *store.State, o api.Object) (string, error) {
	var svc api.Service
	if err := json.Unmarshal(o.Raw, &svc); err != nil {
		return "", err
	}
	if len(svc.Spec.Ports) == 0 {
		return "", fmt.Errorf("service/%s: at least one port is required", o.Name)
	}
	if len(svc.Spec.Selector) == 0 {
		svc.Spec.Selector = map[string]string{"app": o.Name}
	}
	key := o.Namespace + "/" + o.Name
	for i := range svc.Spec.Ports {
		p := &svc.Spec.Ports[i]
		if p.Port <= 0 || p.Port > 65535 {
			return "", fmt.Errorf("service/%s: invalid port %d", o.Name, p.Port)
		}
		if p.TargetPort == 0 {
			p.TargetPort = p.Port
		}
		switch strings.ToUpper(p.Protocol) {
		case "", "TCP":
			p.Protocol = "TCP"
		case "UDP":
			p.Protocol = "UDP"
			if p.Port == 53 {
				return "", fmt.Errorf("service/%s: UDP port 53 is reserved for cluster DNS", o.Name)
			}
		default:
			return "", fmt.Errorf("service/%s: protocol must be TCP or UDP", o.Name)
		}
		// Every service port is bound on each node (and on the servers), so
		// port+protocol must be unique across the cluster.
		for k, other := range st.Services {
			if k == key || svc.Spec.Headless || other.Spec.Headless {
				continue // headless services bind no port
			}
			for _, op := range other.Spec.Ports {
				if op.Port == p.Port && op.IsUDP() == p.IsUDP() {
					return "", fmt.Errorf("service/%s: %s port %d is already used by service %s", o.Name, p.Protocol, p.Port, k)
				}
			}
		}
	}
	svc.Kind = api.KindService
	svc.Metadata.Name, svc.Metadata.Namespace = o.Name, o.Namespace
	old := st.Services[key]
	if old != nil {
		if jsonEqual(old.Spec, svc.Spec) && jsonEqual(old.Metadata.Labels, svc.Metadata.Labels) {
			return "unchanged", nil
		}
		touchMeta(&svc.Metadata, &old.Metadata)
		svc.Status = old.Status
		st.Services[key] = &svc
		return "configured", nil
	}
	touchMeta(&svc.Metadata, nil)
	st.Services[key] = &svc
	st.Event("Normal", api.KindService, o.Namespace, o.Name, "Created", fmt.Sprintf("service listening on port %d", svc.Spec.Ports[0].Port))
	return "created", nil
}

func applyConfig(st *store.State, o api.Object, secret bool) (string, error) {
	var in struct {
		Metadata   api.Meta          `json:"metadata"`
		Type       string            `json:"type"`
		Data       map[string]any    `json:"data"`
		StringData map[string]string `json:"stringData"`
	}
	if err := json.Unmarshal(o.Raw, &in); err != nil {
		return "", err
	}
	data := map[string]string{}
	for k, v := range in.Data {
		data[k] = fmt.Sprint(v) // YAML numbers/bools become strings
	}
	for k, v := range in.StringData {
		data[k] = v
	}
	key := o.Namespace + "/" + o.Name
	meta := in.Metadata
	meta.Name, meta.Namespace = o.Name, o.Namespace
	if secret {
		switch strings.ToLower(in.Type) {
		case "", "opaque":
			in.Type = "Opaque"
		case "tls", "kubernetes.io/tls":
			in.Type = "tls"
			if _, err := tls.X509KeyPair([]byte(data["tls.crt"]), []byte(data["tls.key"])); err != nil {
				return "", fmt.Errorf("secret/%s: type tls needs valid PEM tls.crt and tls.key: %v", o.Name, err)
			}
		default:
			return "", fmt.Errorf("secret/%s: type must be Opaque or tls", o.Name)
		}
		old := st.Secrets[key]
		if old != nil && jsonEqual(old.Data, data) && old.Type == in.Type {
			return "unchanged", nil
		}
		var om *api.Meta
		if old != nil {
			om = &old.Metadata
		}
		touchMeta(&meta, om)
		st.Secrets[key] = &api.Secret{Kind: api.KindSecret, Metadata: meta, Type: in.Type, Data: data}
		if old != nil {
			return "configured", nil
		}
		return "created", nil
	}
	old := st.ConfigMaps[key]
	if old != nil && jsonEqual(old.Data, data) {
		return "unchanged", nil
	}
	var om *api.Meta
	if old != nil {
		om = &old.Metadata
	}
	touchMeta(&meta, om)
	st.ConfigMaps[key] = &api.ConfigMap{Kind: api.KindConfigMap, Metadata: meta, Data: data}
	if old != nil {
		return "configured", nil
	}
	return "created", nil
}

func applyVolume(st *store.State, o api.Object) (string, error) {
	var v api.Volume
	if err := json.Unmarshal(o.Raw, &v); err != nil {
		return "", err
	}
	if v.Spec.Type == "" {
		v.Spec.Type = "local"
	}
	switch v.Spec.Type {
	case "local":
	case "nfs":
		if v.Spec.NFS == nil || v.Spec.NFS.Server == "" || v.Spec.NFS.Path == "" {
			return "", fmt.Errorf("volume/%s: nfs.server and nfs.path are required", o.Name)
		}
	case "hostPath":
		if !strings.HasPrefix(v.Spec.HostPath, "/") {
			return "", fmt.Errorf("volume/%s: hostPath must be an absolute path", o.Name)
		}
	default:
		return "", fmt.Errorf("volume/%s: type must be local, nfs or hostPath", o.Name)
	}
	if _, err := api.ParseMemory(v.Spec.Size); err != nil {
		return "", fmt.Errorf("volume/%s: invalid size", o.Name)
	}
	v.Kind = api.KindVolume
	v.Metadata.Name, v.Metadata.Namespace = o.Name, o.Namespace
	key := v.Metadata.Key()
	old := st.Volumes[key]
	if old != nil {
		if old.Spec.Type != v.Spec.Type {
			return "", fmt.Errorf("volume/%s: type cannot be changed (delete and recreate it)", o.Name)
		}
		if old.Status.NodeName != "" && v.Spec.NodeName != "" && v.Spec.NodeName != old.Status.NodeName {
			return "", fmt.Errorf("volume/%s: already bound to node %s", o.Name, old.Status.NodeName)
		}
		if jsonEqual(old.Spec, v.Spec) {
			return "unchanged", nil
		}
		touchMeta(&v.Metadata, &old.Metadata)
		v.Status = old.Status
		st.Volumes[key] = &v
		return "configured", nil
	}
	touchMeta(&v.Metadata, nil)
	v.Status.Phase = "Available"
	if v.Spec.Type == "local" && v.Spec.NodeName != "" {
		v.Status.NodeName, v.Status.Phase = v.Spec.NodeName, "Bound"
	}
	st.Volumes[key] = &v
	st.Event("Normal", api.KindVolume, o.Namespace, o.Name, "Created", "volume of type "+v.Spec.Type+" created")
	return "created", nil
}

// applyPod creates a standalone pod (not managed by a deployment).
func applyPod(st *store.State, o api.Object) (string, error) {
	var in struct {
		Metadata api.Meta `json:"metadata"`
		Spec     struct {
			NodeName     string            `json:"nodeName"`
			NodeSelector map[string]string `json:"nodeSelector"`
			Template     api.PodTemplate   `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(o.Raw, &in); err != nil {
		return "", err
	}
	if err := in.Spec.Template.Validate(); err != nil {
		return "", fmt.Errorf("pod/%s: %w", o.Name, err)
	}
	if err := checkVolumeRefs(st, o.Namespace, &in.Spec.Template, nil); err != nil {
		return "", fmt.Errorf("pod/%s: %w", o.Name, err)
	}
	key := o.Namespace + "/" + o.Name
	if old := st.Pods[key]; old != nil {
		if jsonEqual(old.Spec.Template, in.Spec.Template) {
			return "unchanged", nil
		}
		return "", fmt.Errorf("pod/%s: pods are immutable; delete it first or use a Deployment", o.Name)
	}
	p := &api.Pod{Kind: api.KindPod, Metadata: in.Metadata}
	p.Metadata.Name, p.Metadata.Namespace = o.Name, o.Namespace
	if p.Metadata.Labels == nil {
		p.Metadata.Labels = in.Spec.Template.Labels
	}
	touchMeta(&p.Metadata, nil)
	p.Spec = api.PodSpec{NodeSelector: in.Spec.NodeSelector, Template: in.Spec.Template, TemplateHash: in.Spec.Template.Hash()}
	p.Status.Phase = api.PodPending
	if in.Spec.NodeName != "" {
		p.Spec.NodeSelector = map[string]string{"nube/hostname": in.Spec.NodeName}
	}
	st.Pods[key] = p
	st.Event("Normal", api.KindPod, o.Namespace, o.Name, "Created", "pod created")
	return "created", nil
}

// ---------------------------------------------------------------- delete

func (s *Server) deleteObject(st *store.State, kind, ns, name string, force bool) error {
	key := ns + "/" + name
	switch kind {
	case api.KindDeployment:
		d := st.Deployments[key]
		if d == nil {
			return notFound("deployment/" + name)
		}
		for _, p := range st.PodsOf(ns, name) {
			terminatePod(st, p, force)
		}
		delete(st.Deployments, key)
		st.Event("Normal", api.KindDeployment, ns, name, "Deleted", "deployment deleted")
	case api.KindPod:
		p := st.Pods[key]
		if p == nil {
			return notFound("pod/" + name)
		}
		terminatePod(st, p, force)
	case api.KindService:
		if st.Services[key] == nil {
			return notFound("service/" + name)
		}
		delete(st.Services, key)
		st.Event("Normal", api.KindService, ns, name, "Deleted", "service deleted")
	case api.KindConfigMap:
		if st.ConfigMaps[key] == nil {
			return notFound("configmap/" + name)
		}
		delete(st.ConfigMaps, key)
	case api.KindSecret:
		if st.Secrets[key] == nil {
			return notFound("secret/" + name)
		}
		delete(st.Secrets, key)
	case api.KindVolume:
		v := st.Volumes[key]
		if v == nil {
			return notFound("volume/" + name)
		}
		if users := volumeUsers(st, ns, name); len(users) > 0 && !force {
			return fmt.Errorf("volume/%s is used by %s (delete those first or use --force)", name, strings.Join(users, ", "))
		}
		delete(st.Volumes, key)
		st.Event("Normal", api.KindVolume, ns, name, "Deleted", "volume record deleted (data is retained on disk as Docker volume "+v.DockerName()+")")
	case api.KindJob:
		if st.Jobs[key] == nil {
			return notFound("job/" + name)
		}
		deleteJob(st, key, force)
		st.Event("Normal", api.KindJob, ns, name, "Deleted", "job deleted")
	case api.KindCronJob:
		if st.CronJobs[key] == nil {
			return notFound("cronjob/" + name)
		}
		for k, j := range st.Jobs {
			if j.Metadata.Namespace == ns && j.Metadata.Labels["nube/cronjob"] == name {
				deleteJob(st, k, force)
			}
		}
		delete(st.CronJobs, key)
		st.Event("Normal", api.KindCronJob, ns, name, "Deleted", "cronjob and its jobs deleted")
	case api.KindStatefulSet:
		if st.StatefulSets[key] == nil {
			return notFound("statefulset/" + name)
		}
		for _, p := range st.PodsOwned(ns, api.KindStatefulSet, name) {
			terminatePod(st, p, force)
		}
		delete(st.StatefulSets, key)
		st.Event("Normal", api.KindStatefulSet, ns, name, "Deleted", "statefulset deleted; its per-replica volumes are kept (delete them explicitly)")
	case api.KindIngress:
		if st.Ingresses[key] == nil {
			return notFound("ingress/" + name)
		}
		delete(st.Ingresses, key)
		st.Event("Normal", api.KindIngress, ns, name, "Deleted", "ingress deleted")
	case api.KindNode:
		if st.Nodes[name] == nil {
			return notFound("node/" + name)
		}
		for k, p := range st.Pods {
			if p.Spec.NodeName == name {
				delete(st.Pods, k)
			}
		}
		delete(st.Nodes, name)
		st.Event("Normal", api.KindNode, "", name, "Removed", "node removed from cluster")
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return nil
}

// terminatePod asks the agent to stop a pod, or removes it at once if no agent owns it.
func terminatePod(st *store.State, p *api.Pod, force bool) {
	key := p.Metadata.Key()
	n := st.Nodes[p.Spec.NodeName]
	if force || p.Spec.NodeName == "" || n == nil || n.Status != api.NodeReady {
		delete(st.Pods, key)
		return
	}
	if p.Status.Phase != api.PodTerminating {
		p.Status.Phase = api.PodTerminating
		p.Status.Ready = false
		p.Status.DeletionAt = time.Now()
	}
}

func volumeUsers(st *store.State, ns, name string) []string {
	var out []string
	for _, p := range st.Pods {
		if p.Metadata.Namespace != ns || p.Status.Phase == api.PodTerminating {
			continue
		}
		for _, v := range p.Spec.Template.AllVolumeMounts() {
			if v.Volume == name {
				out = append(out, p.Metadata.Name)
			}
		}
	}
	return out
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
