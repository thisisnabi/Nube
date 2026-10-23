package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

func applyDaemonSet(st *store.State, o api.Object) (string, error) {
	var ds api.DaemonSet
	if err := json.Unmarshal(o.Raw, &ds); err != nil {
		return "", err
	}
	t := &ds.Spec.Template
	if err := t.Validate(); err != nil {
		return "", fmt.Errorf("daemonset/%s: %w", o.Name, err)
	}
	if len(t.Labels) == 0 {
		t.Labels = map[string]string{"app": o.Name}
	}
	if t.RestartPolicy != "" && t.RestartPolicy != "Always" {
		return "", fmt.Errorf("daemonset/%s: restartPolicy must be Always", o.Name)
	}
	if ds.Spec.MaxUnavailable <= 0 {
		ds.Spec.MaxUnavailable = 1
	}
	for _, v := range t.AllVolumeMounts() {
		if v.Volume != "" {
			return "", fmt.Errorf("daemonset/%s: use hostPath or emptyDir volumes (one pod per node)", o.Name)
		}
	}
	ds.Kind = api.KindDaemonSet
	ds.Metadata.Name, ds.Metadata.Namespace = o.Name, o.Namespace
	key := ds.Metadata.Key()
	if old := st.DaemonSets[key]; old != nil {
		if jsonEqual(old.Spec, ds.Spec) {
			return "unchanged", nil
		}
		touchMeta(&ds.Metadata, &old.Metadata)
		ds.Status = old.Status
		st.DaemonSets[key] = &ds
		st.Event("Normal", api.KindDaemonSet, o.Namespace, o.Name, "Updated", "daemonset spec updated")
		return "configured", nil
	}
	touchMeta(&ds.Metadata, nil)
	st.DaemonSets[key] = &ds
	st.Event("Normal", api.KindDaemonSet, o.Namespace, o.Name, "Created", "daemonset created")
	return "created", nil
}

// reconcileDaemonSet keeps exactly one pod on every ready node matching the
// selector (cordoned nodes included, like Kubernetes) and replaces outdated
// pods at most maxUnavailable nodes at a time.
func reconcileDaemonSet(st *store.State, ds *api.DaemonSet, now time.Time) {
	ns, name := ds.Metadata.Namespace, ds.Metadata.Name
	hash := rolloutHash(&ds.Spec.Template, configDigests(st, ns, &ds.Spec.Template))
	if ds.Status.CurrentHash != hash {
		if ds.Status.CurrentHash != "" {
			st.Event("Normal", api.KindDaemonSet, ns, name, "RolloutStarted", "updating pods node by node (template or configuration changed)")
		}
		ds.Status.CurrentHash = hash
	}
	eligible := map[string]bool{}
	for _, n := range st.Nodes {
		if n.Status == api.NodeReady && labelsMatch(n.Labels, ds.Spec.NodeSelector) {
			eligible[n.Name] = true
		}
	}
	byNode := map[string][]*api.Pod{}
	for _, p := range st.PodsOwned(ns, api.KindDaemonSet, name) {
		byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], p)
	}

	// Remove pods on nodes that no longer qualify, and duplicates.
	for node, pods := range byNode {
		for i, p := range pods {
			if p.Status.Phase == api.PodTerminating {
				continue
			}
			if !eligible[node] || i > 0 || p.Finished() {
				terminatePod(st, p, false)
			}
		}
	}
	// One pod per eligible node; wait while an old pod is still terminating.
	for _, node := range sortedKeys(eligible) {
		if len(byNode[node]) > 0 {
			continue
		}
		p := newPod(ns, name+"-"+randSuffix(5), api.KindDaemonSet, name, ds.Spec.Template, ds.Spec.NodeSelector, now)
		p.Spec.TemplateHash = hash
		st.Pods[p.Metadata.Key()] = p
		bindPod(st, p, st.Nodes[node], now)
		byNode[node] = []*api.Pod{p}
	}

	// Rolling update.
	unavailable := 0
	var outdated []*api.Pod
	for node := range eligible {
		ps := byNode[node]
		if len(ps) == 0 || ps[0].Status.Phase == api.PodTerminating || !ps[0].Status.Ready {
			unavailable++
			continue
		}
		if ps[0].Spec.TemplateHash != hash {
			outdated = append(outdated, ps[0])
		}
	}
	sort.Slice(outdated, func(i, j int) bool { return outdated[i].Spec.NodeName < outdated[j].Spec.NodeName })
	for _, p := range outdated {
		if unavailable >= ds.Spec.MaxUnavailable {
			break
		}
		terminatePod(st, p, false)
		unavailable++
	}

	s := &ds.Status
	s.DesiredNumber, s.Current, s.Ready, s.Updated = len(eligible), 0, 0, 0
	troubled := ""
	for node := range eligible {
		for _, p := range byNode[node] {
			if p.Status.Phase == api.PodTerminating {
				continue
			}
			s.Current++
			if p.Status.Ready {
				s.Ready++
			}
			if p.Spec.TemplateHash == hash {
				s.Updated++
			}
			if p.Status.Phase == api.PodCrashLoop || p.Status.Phase == api.PodFailed {
				troubled = p.Metadata.Name + " on " + node + ": " + firstNonEmpty(p.Status.Message, string(p.Status.Phase))
			}
		}
	}
	prev := s.Phase
	switch {
	case s.Ready == s.DesiredNumber && s.Updated == s.DesiredNumber:
		s.Phase, s.Message = "Healthy", ""
	case troubled != "":
		s.Phase, s.Message = "Degraded", troubled
	default:
		s.Phase, s.Message = "Progressing", fmt.Sprintf("%d/%d nodes ready, %d updated", s.Ready, s.DesiredNumber, s.Updated)
	}
	if prev != s.Phase {
		typ := "Normal"
		if s.Phase == "Degraded" {
			typ = "Warning"
		}
		st.Event(typ, api.KindDaemonSet, ns, name, s.Phase, "daemonset is "+strings.ToLower(s.Phase))
	}
}
