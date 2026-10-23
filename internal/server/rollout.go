package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// defaultProgressDeadline is how long a rollout's new pods may keep failing
// before the rollout is rolled back (Kubernetes' progressDeadlineSeconds
// default; there it only marks the rollout as failed).
const defaultProgressDeadline = 600 * time.Second

// configDigests returns a keyed digest of every ConfigMap and Secret the
// template reads ("configmap/NAME" -> digest). Workload controllers roll
// their pods when one changes, like an image change; Kubernetes needs an
// add-on (Reloader) for that.
func configDigests(st *store.State, ns string, t *api.PodTemplate) map[string]string {
	if t.ReloadOnConfigChange != nil && !*t.ReloadOnConfigChange {
		return nil
	}
	out := map[string]string{}
	for _, c := range append(t.AllContainers(), t.InitContainers...) {
		for _, src := range c.EnvFrom {
			if src.ConfigMap != "" {
				var data map[string]string
				if cm := st.ConfigMaps[ns+"/"+src.ConfigMap]; cm != nil {
					data = cm.Data
				}
				out["configmap/"+src.ConfigMap] = digestData(st, data)
			}
			if src.Secret != "" {
				var data map[string]string
				if sec := st.Secrets[ns+"/"+src.Secret]; sec != nil {
					data = sec.Data
				}
				out["secret/"+src.Secret] = digestData(st, data)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// digestData is an HMAC of a ConfigMap/Secret's data ("-" when it does not exist).
func digestData(st *store.State, data map[string]string) string {
	if data == nil {
		return "-"
	}
	if len(st.ConfigKey) == 0 { // first use; only the leader writes, so this replicates
		st.ConfigKey = make([]byte, 32)
		rand.Read(st.ConfigKey)
	}
	m := hmac.New(sha256.New, st.ConfigKey)
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(m, "%d:%s%d:%s", len(k), k, len(data[k]), data[k])
	}
	return hex.EncodeToString(m.Sum(nil))[:12]
}

// rolloutHash identifies what a workload's pods run: the template and the
// configuration it reads.
func rolloutHash(t *api.PodTemplate, config map[string]string) string {
	h := t.Hash()
	if len(config) == 0 {
		return h
	}
	refs := make([]string, 0, len(config))
	for r := range config {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	sum := sha256.New()
	sum.Write([]byte(h))
	for _, r := range refs {
		fmt.Fprintf(sum, "\x00%s=%s", r, config[r])
	}
	return hex.EncodeToString(sum.Sum(nil))[:10]
}

// rolloutCause describes what changed between two revisions.
func rolloutCause(prevTmpl *api.PodTemplate, prevConfig map[string]string, tmpl *api.PodTemplate, config map[string]string) string {
	if prevTmpl != nil && prevTmpl.Hash() != tmpl.Hash() {
		if prev, cur := prevTmpl.MainImage(), tmpl.MainImage(); prev != cur {
			return "image " + cur
		}
		return "template changed"
	}
	var changed []string
	for r, d := range config {
		if prevConfig[r] != d {
			changed = append(changed, r)
		}
	}
	if len(changed) == 0 {
		return "template changed"
	}
	sort.Strings(changed)
	return strings.Join(changed, ", ") + " changed"
}

// failing reports why a pod of a new revision is broken (not merely slow):
// crashing, failed, an image that cannot be pulled or a failing health check.
func failing(p *api.Pod) string {
	if p.Spec.NodeName == "" {
		return "" // waiting to be scheduled is not a failure of the revision
	}
	switch {
	case p.Status.Phase == api.PodCrashLoop || p.Status.Phase == api.PodFailed:
		return firstNonEmpty(p.Status.Message, string(p.Status.Phase))
	case strings.HasPrefix(p.Status.Message, "ErrImagePull"), strings.Contains(p.Status.Message, "health check failed"),
		strings.HasPrefix(p.Status.Message, "waiting for configmap/"), strings.HasPrefix(p.Status.Message, "waiting for secret/"):
		return p.Status.Message
	}
	return ""
}

// autoRollback returns a deployment to its previous revision when the new
// pods keep failing past the progress deadline. Kubernetes only marks such a
// rollout as failed; here the old version comes back on its own.
func autoRollback(st *store.State, d *api.Deployment, newPods []*api.Pod, now time.Time) {
	stat := &d.Status
	if stat.RolloutStartedAt.IsZero() || d.Spec.Paused || (d.Spec.AutoRollback != nil && !*d.Spec.AutoRollback) {
		return
	}
	deadline := defaultProgressDeadline
	if d.Spec.ProgressDeadlineSeconds > 0 {
		deadline = time.Duration(d.Spec.ProgressDeadlineSeconds) * time.Second
	}
	if now.Sub(stat.RolloutStartedAt) < deadline {
		return
	}
	reason := ""
	for _, p := range newPods {
		if r := failing(p); r != "" {
			reason = p.Metadata.Name + ": " + r
			break
		}
	}
	if reason == "" {
		return // slow, not broken: keep waiting
	}
	stat.RolloutStartedAt = time.Time{} // decide once per rollout
	ns, name := d.Metadata.Namespace, d.Metadata.Name
	var prev *api.Revision
	for i := range stat.History {
		h := &stat.History[i]
		if h.Hash != stat.CurrentHash && (prev == nil || h.Revision > prev.Revision) {
			prev = h
		}
	}
	if prev == nil {
		return
	}
	if prev.Template.Hash() == d.Spec.Template.Hash() {
		// Only the configuration changed; it cannot be put back from here.
		msg := fmt.Sprintf("revision %d keeps failing after %s (%s); the template did not change, so the old pods keep serving until the ConfigMap/Secret is fixed", stat.Revision, deadline, reason)
		stat.LastRollback = msg
		st.Event("Warning", api.KindDeployment, ns, name, "RollbackNotPossible", msg)
		return
	}
	msg := fmt.Sprintf("revision %d failed for %s (%s); rolled back to revision %d", stat.Revision, deadline, reason, prev.Revision)
	d.Spec.Template = deepCopy(prev.Template)
	stat.RollingBack = true
	stat.LastRollback = msg
	st.Event("Warning", api.KindDeployment, ns, name, "RolledBack", msg)
}

// dependencyWait reports which service a pod is still waiting for (dependsOn).
func dependencyWait(st *store.State, p *api.Pod) string {
	for _, dep := range p.Spec.Template.DependsOn {
		name, ns, _ := strings.Cut(dep, ".")
		if ns == "" {
			ns = p.Metadata.Namespace
		}
		svc := st.Services[ns+"/"+name]
		if svc == nil {
			return "waiting for service " + dep + " (not found)"
		}
		if len(serviceEndpoints(st, svc)) == 0 {
			return "waiting for service " + dep + " to have a ready pod"
		}
	}
	return ""
}
