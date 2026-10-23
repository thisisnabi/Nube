package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// checkVolumeRefs verifies that every "volume:" mount names an existing Volume
// (or one of the StatefulSet claim templates).
func checkVolumeRefs(st *store.State, ns string, t *api.PodTemplate, claims map[string]bool) error {
	for _, v := range t.AllVolumeMounts() {
		if v.Volume == "" || claims[v.Volume] {
			continue
		}
		if st.Volumes[ns+"/"+v.Volume] == nil {
			return fmt.Errorf("volume %q does not exist in namespace %s", v.Volume, ns)
		}
	}
	return nil
}

// newPod builds a pod for a workload from its template.
func newPod(ns, name, ownerKind, owner string, tmpl api.PodTemplate, nodeSelector map[string]string, now time.Time) *api.Pod {
	tmpl = deepCopy(tmpl)
	labels := map[string]string{}
	for k, v := range tmpl.Labels {
		labels[k] = v
	}
	labels["nube/"+strings.ToLower(ownerKind)] = owner
	return &api.Pod{
		Kind:     api.KindPod,
		Metadata: api.Meta{Name: name, Namespace: ns, UID: newUID(), Labels: labels, CreatedAt: now, UpdatedAt: now},
		Spec: api.PodSpec{
			Owner: owner, OwnerKind: ownerKind, TemplateHash: tmpl.Hash(), NodeSelector: nodeSelector, Template: tmpl,
		},
		Status: api.PodStatus{Phase: api.PodPending},
	}
}

// ================================================================ Job

func applyJob(st *store.State, o api.Object) (string, error) {
	var j api.Job
	if err := json.Unmarshal(o.Raw, &j); err != nil {
		return "", err
	}
	if err := normalizeJobSpec(&j.Spec, o.Name); err != nil {
		return "", fmt.Errorf("job/%s: %w", o.Name, err)
	}
	if err := checkVolumeRefs(st, o.Namespace, &j.Spec.Template, nil); err != nil {
		return "", fmt.Errorf("job/%s: %w", o.Name, err)
	}
	j.Kind = api.KindJob
	j.Metadata.Name, j.Metadata.Namespace = o.Name, o.Namespace
	key := j.Metadata.Key()
	if old := st.Jobs[key]; old != nil {
		if jsonEqual(old.Spec, j.Spec) {
			return "unchanged", nil
		}
		return "", fmt.Errorf("job/%s: jobs are immutable; delete it and apply again", o.Name)
	}
	touchMeta(&j.Metadata, nil)
	j.Status.Phase = "Running"
	st.Jobs[key] = &j
	st.Event("Normal", api.KindJob, o.Namespace, o.Name, "Created", fmt.Sprintf("job created: %d completion(s), parallelism %d", j.Spec.Completions, j.Spec.Parallelism))
	return "created", nil
}

func normalizeJobSpec(js *api.JobSpec, name string) error {
	if js.Completions <= 0 {
		js.Completions = 1
	}
	if js.Parallelism <= 0 {
		js.Parallelism = 1
	}
	if js.BackoffLimit == nil {
		six := 6
		js.BackoffLimit = &six
	}
	t := &js.Template
	if t.RestartPolicy == "" {
		t.RestartPolicy = "Never"
	}
	if t.RestartPolicy == "Always" {
		return fmt.Errorf("a job's restartPolicy must be Never or OnFailure")
	}
	if len(t.Labels) == 0 {
		t.Labels = map[string]string{"job": name}
	}
	return t.Validate()
}

func deleteJob(st *store.State, key string, force bool) {
	j := st.Jobs[key]
	if j == nil {
		return
	}
	for _, p := range st.PodsOwned(j.Metadata.Namespace, api.KindJob, j.Metadata.Name) {
		terminatePod(st, p, force)
	}
	delete(st.Jobs, key)
}

func jobBackoff(failed int) time.Duration {
	d := 10 * time.Second << min(failed-1, 6)
	return min(d, 6*time.Minute)
}

// reconcileJob runs pods until spec.completions of them succeed, at most
// spec.parallelism at a time, giving up after backoffLimit failures.
func reconcileJob(st *store.State, j *api.Job, now time.Time) {
	ns, name := j.Metadata.Namespace, j.Metadata.Name
	s := &j.Status
	if s.Phase == "Complete" || s.Phase == "Failed" {
		if ttl := j.Spec.TTLSecondsAfterFinished; ttl != nil && now.Sub(s.CompletionTime) > time.Duration(*ttl)*time.Second {
			deleteJob(st, j.Metadata.Key(), false)
			st.Event("Normal", api.KindJob, ns, name, "Cleaned", "finished job removed after its TTL")
		}
		return
	}
	if s.StartTime.IsZero() {
		s.StartTime = now
	}
	var active []*api.Pod
	succeeded, failed := 0, 0
	var lastFailure time.Time
	for _, p := range st.PodsOwned(ns, api.KindJob, name) {
		switch {
		case p.Status.Phase == api.PodSucceeded:
			succeeded++
		case p.Status.Phase == api.PodFailed:
			failed++
			at := p.Status.FinishedAt
			if at.IsZero() {
				at = p.Metadata.UpdatedAt
			}
			lastFailure = maxTime(lastFailure, at)
		case p.Status.Phase != api.PodTerminating:
			active = append(active, p)
		}
	}
	s.Active, s.Succeeded, s.Failed = len(active), succeeded, failed

	finish := func(phase, reason, msg string) {
		for _, p := range active {
			terminatePod(st, p, false)
		}
		s.Phase, s.Message, s.CompletionTime, s.Active = phase, msg, now, 0
		typ := "Normal"
		if phase == "Failed" {
			typ = "Warning"
		}
		st.Event(typ, api.KindJob, ns, name, reason, msg)
	}
	switch {
	case succeeded >= j.Spec.Completions:
		finish("Complete", "Completed", fmt.Sprintf("job completed (%d/%d succeeded) in %s", succeeded, j.Spec.Completions, now.Sub(s.StartTime).Round(time.Second)))
		return
	case failed > *j.Spec.BackoffLimit:
		finish("Failed", "BackoffLimitExceeded", fmt.Sprintf("job failed: %d pods failed (backoffLimit %d)", failed, *j.Spec.BackoffLimit))
		return
	case j.Spec.ActiveDeadlineSeconds > 0 && now.Sub(s.StartTime) > time.Duration(j.Spec.ActiveDeadlineSeconds)*time.Second:
		finish("Failed", "DeadlineExceeded", fmt.Sprintf("job ran longer than activeDeadlineSeconds (%ds)", j.Spec.ActiveDeadlineSeconds))
		return
	}
	// Exponential backoff after failures, like Kubernetes (10s, 20s, 40s ... 6m).
	if failed > 0 && now.Sub(lastFailure) < jobBackoff(failed) {
		s.Message = fmt.Sprintf("%d failed; retrying in %s", failed, (jobBackoff(failed) - now.Sub(lastFailure)).Round(time.Second))
		return
	}
	want := min(j.Spec.Parallelism, j.Spec.Completions-succeeded) - len(active)
	for i := 0; i < want; i++ {
		p := newPod(ns, name+"-"+randSuffix(5), api.KindJob, name, j.Spec.Template, j.Spec.NodeSelector, now)
		st.Pods[p.Metadata.Key()] = p
		s.Active++
	}
	s.Message = fmt.Sprintf("%d/%d succeeded, %d running", succeeded, j.Spec.Completions, s.Active)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ================================================================ CronJob

func applyCronJob(st *store.State, o api.Object) (string, error) {
	var c api.CronJob
	if err := json.Unmarshal(o.Raw, &c); err != nil {
		return "", err
	}
	if _, err := parseSchedule(c.Spec.Schedule); err != nil {
		return "", fmt.Errorf("cronjob/%s: %w", o.Name, err)
	}
	if c.Spec.TimeZone != "" {
		if _, err := time.LoadLocation(c.Spec.TimeZone); err != nil {
			return "", fmt.Errorf("cronjob/%s: unknown timeZone %q", o.Name, c.Spec.TimeZone)
		}
	}
	switch c.Spec.ConcurrencyPolicy {
	case "":
		c.Spec.ConcurrencyPolicy = "Allow"
	case "Allow", "Forbid", "Replace":
	default:
		return "", fmt.Errorf("cronjob/%s: concurrencyPolicy must be Allow, Forbid or Replace", o.Name)
	}
	if err := normalizeJobSpec(&c.Spec.JobTemplate, o.Name); err != nil {
		return "", fmt.Errorf("cronjob/%s: %w", o.Name, err)
	}
	if err := checkVolumeRefs(st, o.Namespace, &c.Spec.JobTemplate.Template, nil); err != nil {
		return "", fmt.Errorf("cronjob/%s: %w", o.Name, err)
	}
	c.Kind = api.KindCronJob
	c.Metadata.Name, c.Metadata.Namespace = o.Name, o.Namespace
	key := c.Metadata.Key()
	if old := st.CronJobs[key]; old != nil {
		if jsonEqual(old.Spec, c.Spec) {
			return "unchanged", nil
		}
		touchMeta(&c.Metadata, &old.Metadata)
		c.Status = old.Status
		if old.Spec.Schedule != c.Spec.Schedule || old.Spec.TimeZone != c.Spec.TimeZone {
			c.Status.NextScheduleTime = time.Time{}
		}
		st.CronJobs[key] = &c
		return "configured", nil
	}
	touchMeta(&c.Metadata, nil)
	st.CronJobs[key] = &c
	st.Event("Normal", api.KindCronJob, o.Namespace, o.Name, "Created", "cronjob scheduled: "+c.Spec.Schedule)
	return "created", nil
}

// startJobFromCron creates a Job from a CronJob's template.
func startJobFromCron(st *store.State, c *api.CronJob, scheduled, now time.Time, manual bool) *api.Job {
	name := fmt.Sprintf("%s-%d", c.Metadata.Name, scheduled.Unix()/60)
	if manual {
		name = c.Metadata.Name + "-manual-" + randSuffix(4)
	}
	if len(name) > 63 {
		name = name[:63]
	}
	j := &api.Job{
		Kind: api.KindJob,
		Metadata: api.Meta{
			Name: name, Namespace: c.Metadata.Namespace, UID: newUID(), CreatedAt: now, UpdatedAt: now,
			Labels: map[string]string{"nube/cronjob": c.Metadata.Name},
		},
		Spec:   deepCopy(c.Spec.JobTemplate),
		Status: api.JobStatus{Phase: "Running"},
	}
	if st.Jobs[j.Metadata.Key()] != nil {
		return nil
	}
	st.Jobs[j.Metadata.Key()] = j
	how := "scheduled"
	if manual {
		how = "triggered manually"
	}
	st.Event("Normal", api.KindCronJob, c.Metadata.Namespace, c.Metadata.Name, "JobCreated", "job "+name+" "+how)
	return j
}

func cronJobsOf(st *store.State, c *api.CronJob) []*api.Job {
	var out []*api.Job
	for _, j := range st.Jobs {
		if j.Metadata.Namespace == c.Metadata.Namespace && j.Metadata.Labels["nube/cronjob"] == c.Metadata.Name {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Metadata.CreatedAt.Before(out[k].Metadata.CreatedAt) })
	return out
}

func reconcileCronJob(st *store.State, c *api.CronJob, now time.Time) {
	ns, name := c.Metadata.Namespace, c.Metadata.Name
	sched, err := parseSchedule(c.Spec.Schedule)
	if err != nil {
		c.Status.Message = err.Error()
		return
	}
	loc := time.Local
	if c.Spec.TimeZone != "" {
		if l, err := time.LoadLocation(c.Spec.TimeZone); err == nil {
			loc = l
		}
	}
	jobs := cronJobsOf(st, c)
	var active []*api.Job
	for _, j := range jobs {
		if j.Status.Phase == "Running" {
			active = append(active, j)
		} else if j.Status.Phase == "Complete" && j.Status.CompletionTime.After(c.Status.LastSuccessfulTime) {
			c.Status.LastSuccessfulTime = j.Status.CompletionTime
		}
	}

	next := c.Status.NextScheduleTime
	if next.IsZero() {
		from := maxTime(c.Status.LastScheduleTime, c.Metadata.CreatedAt)
		next = sched.Next(from.In(loc))
	}
	if !next.IsZero() && !now.Before(next) {
		late := now.Sub(next)
		switch {
		case c.Spec.Suspend:
		case c.Spec.StartingDeadlineSeconds > 0 && late > time.Duration(c.Spec.StartingDeadlineSeconds)*time.Second:
			st.Event("Warning", api.KindCronJob, ns, name, "MissedSchedule", fmt.Sprintf("missed the %s run by %s", next.Format("15:04"), late.Round(time.Second)))
		case len(active) > 0 && c.Spec.ConcurrencyPolicy == "Forbid":
			st.Event("Normal", api.KindCronJob, ns, name, "Skipped", "previous job still running (concurrencyPolicy Forbid)")
		default:
			if c.Spec.ConcurrencyPolicy == "Replace" {
				for _, j := range active {
					deleteJob(st, j.Metadata.Key(), false)
					st.Event("Normal", api.KindCronJob, ns, name, "Replaced", "stopped job "+j.Metadata.Name+" (concurrencyPolicy Replace)")
				}
				active = nil
			}
			if j := startJobFromCron(st, c, next, now, false); j != nil {
				active = append(active, j)
			}
		}
		c.Status.LastScheduleTime = next
		next = sched.Next(now.In(loc)) // missed runs are not replayed
	}
	c.Status.NextScheduleTime = next
	c.Status.Active = c.Status.Active[:0]
	for _, j := range active {
		c.Status.Active = append(c.Status.Active, j.Metadata.Name)
	}
	c.Status.Message = ""
	if c.Spec.Suspend {
		c.Status.Message = "suspended"
	}

	// Keep only the newest finished jobs.
	keepOK, keepFail := 3, 1
	if c.Spec.SuccessfulJobsHistoryLimit != nil {
		keepOK = *c.Spec.SuccessfulJobsHistoryLimit
	}
	if c.Spec.FailedJobsHistoryLimit != nil {
		keepFail = *c.Spec.FailedJobsHistoryLimit
	}
	var ok, failed []*api.Job
	for _, j := range jobs {
		switch j.Status.Phase {
		case "Complete":
			ok = append(ok, j)
		case "Failed":
			failed = append(failed, j)
		}
	}
	for i := 0; i < len(ok)-keepOK; i++ {
		deleteJob(st, ok[i].Metadata.Key(), false)
	}
	for i := 0; i < len(failed)-keepFail; i++ {
		deleteJob(st, failed[i].Metadata.Key(), false)
	}
}

// ================================================================ StatefulSet

func applyStatefulSet(st *store.State, o api.Object) (string, error) {
	var ss api.StatefulSet
	if err := json.Unmarshal(o.Raw, &ss); err != nil {
		return "", err
	}
	var probe struct {
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
	}
	json.Unmarshal(o.Raw, &probe)
	if probe.Spec.Replicas == nil {
		ss.Spec.Replicas = 1
	}
	if ss.Spec.Replicas < 0 {
		return "", fmt.Errorf("statefulset/%s: replicas must be >= 0", o.Name)
	}
	t := &ss.Spec.Template
	if err := t.Validate(); err != nil {
		return "", fmt.Errorf("statefulset/%s: %w", o.Name, err)
	}
	if len(t.Labels) == 0 {
		t.Labels = map[string]string{"app": o.Name}
	}
	switch ss.Spec.PodManagementPolicy {
	case "":
		ss.Spec.PodManagementPolicy = "OrderedReady"
	case "OrderedReady", "Parallel":
	default:
		return "", fmt.Errorf("statefulset/%s: podManagementPolicy must be OrderedReady or Parallel", o.Name)
	}
	claims := map[string]bool{}
	for i := range ss.Spec.VolumeClaimTemplates {
		ct := &ss.Spec.VolumeClaimTemplates[i]
		if !api.ValidName(ct.Name) {
			return "", fmt.Errorf("statefulset/%s: invalid volumeClaimTemplate name %q", o.Name, ct.Name)
		}
		if ct.Spec.Type == "" {
			ct.Spec.Type = "local"
		}
		if ct.Spec.Type != "local" && ct.Spec.Type != "nfs" {
			return "", fmt.Errorf("statefulset/%s: volumeClaimTemplate type must be local or nfs", o.Name)
		}
		if _, err := api.ParseMemory(ct.Spec.Size); err != nil {
			return "", fmt.Errorf("statefulset/%s: claim %s: invalid size", o.Name, ct.Name)
		}
		claims[ct.Name] = true
	}
	if err := checkVolumeRefs(st, o.Namespace, t, claims); err != nil {
		return "", fmt.Errorf("statefulset/%s: %w", o.Name, err)
	}
	ss.Kind = api.KindStatefulSet
	ss.Metadata.Name, ss.Metadata.Namespace = o.Name, o.Namespace
	key := ss.Metadata.Key()
	if old := st.StatefulSets[key]; old != nil {
		if jsonEqual(old.Spec, ss.Spec) {
			return "unchanged", nil
		}
		if !jsonEqual(old.Spec.VolumeClaimTemplates, ss.Spec.VolumeClaimTemplates) {
			return "", fmt.Errorf("statefulset/%s: volumeClaimTemplates cannot be changed", o.Name)
		}
		touchMeta(&ss.Metadata, &old.Metadata)
		ss.Status = old.Status
		st.StatefulSets[key] = &ss
		st.Event("Normal", api.KindStatefulSet, o.Namespace, o.Name, "Updated", "statefulset spec updated")
		return "configured", nil
	}
	touchMeta(&ss.Metadata, nil)
	st.StatefulSets[key] = &ss
	st.Event("Normal", api.KindStatefulSet, o.Namespace, o.Name, "Created", fmt.Sprintf("statefulset created with %d replicas", ss.Spec.Replicas))
	return "created", nil
}

func claimVolumeName(claim, set string, ordinal int) string {
	return fmt.Sprintf("%s-%s-%d", claim, set, ordinal)
}

// ordinalOf parses the trailing "-N" of a StatefulSet pod name.
func ordinalOf(set, pod string) (int, bool) {
	rest, ok := strings.CutPrefix(pod, set+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 0
}

// reconcileStatefulSet keeps pods <name>-0..N-1, each with stable identity and
// its own volumes; creates them in order (each waits for the previous to be
// Ready), removes from the highest ordinal down, and rolls updates from the
// highest ordinal down one pod at a time.
func reconcileStatefulSet(st *store.State, ss *api.StatefulSet, now time.Time) {
	ns, name := ss.Metadata.Namespace, ss.Metadata.Name
	hash := rolloutHash(&ss.Spec.Template, configDigests(st, ns, &ss.Spec.Template))
	if ss.Status.CurrentHash != hash {
		if ss.Status.CurrentHash != "" {
			st.Event("Normal", api.KindStatefulSet, ns, name, "RolloutStarted", "updating pods from the highest ordinal down (template or configuration changed)")
		}
		ss.Status.CurrentHash = hash
		ss.Status.Revision++
	}
	ordered := ss.Spec.PodManagementPolicy != "Parallel"

	// Per-replica volumes.
	for i := 0; i < ss.Spec.Replicas; i++ {
		for _, ct := range ss.Spec.VolumeClaimTemplates {
			vname := claimVolumeName(ct.Name, name, i)
			if st.Volumes[ns+"/"+vname] != nil {
				continue
			}
			v := &api.Volume{
				Kind: api.KindVolume,
				Metadata: api.Meta{Name: vname, Namespace: ns, UID: newUID(), CreatedAt: now, UpdatedAt: now,
					Labels: map[string]string{"nube/statefulset": name}},
				Spec:   ct.Spec,
				Status: api.VolumeStatus{Phase: "Available"},
			}
			st.Volumes[v.Metadata.Key()] = v
			st.Event("Normal", api.KindVolume, ns, vname, "Created", "volume created for "+name+"-"+strconv.Itoa(i))
		}
	}

	pods := map[int]*api.Pod{}
	terminating := 0
	for _, p := range st.PodsOwned(ns, api.KindStatefulSet, name) {
		i, ok := ordinalOf(name, p.Metadata.Name)
		if !ok {
			continue
		}
		if p.Status.Phase == api.PodTerminating {
			terminating++
		}
		pods[i] = p
	}
	ready := func(i int) bool {
		p := pods[i]
		return p != nil && p.Status.Ready && p.Status.Phase != api.PodTerminating
	}

	// Replace replicas that stopped for good.
	for _, p := range pods {
		if p.Finished() && ss.Spec.Template.RestartPolicy != "Never" {
			terminatePod(st, p, false)
			terminating++
		}
	}

	// Create missing replicas, in order.
	for i := 0; i < ss.Spec.Replicas; i++ {
		if pods[i] != nil {
			if ordered && !ready(i) {
				break
			}
			continue
		}
		if ordered && terminating > 0 {
			break
		}
		p := newStatefulPod(ss, i, now)
		st.Pods[p.Metadata.Key()] = p
		pods[i] = p
		if ordered {
			break // the next one waits until this one is Ready
		}
	}

	// Scale down from the highest ordinal.
	var extra []int
	for i := range pods {
		if i >= ss.Spec.Replicas && pods[i].Status.Phase != api.PodTerminating {
			extra = append(extra, i)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(extra)))
	for _, i := range extra {
		terminatePod(st, pods[i], false)
		if ordered {
			break
		}
	}

	// Rolling update: replace the highest outdated pod once all are ready.
	allReady := terminating == 0 && len(extra) == 0
	for i := 0; i < ss.Spec.Replicas && allReady; i++ {
		allReady = ready(i)
	}
	if allReady {
		for i := ss.Spec.Replicas - 1; i >= 0; i-- {
			if pods[i].Spec.TemplateHash != hash {
				terminatePod(st, pods[i], false)
				break
			}
		}
	}

	// Status.
	s := &ss.Status
	s.Replicas, s.ReadyReplicas, s.UpdatedReplicas = 0, 0, 0
	troubled := ""
	for i, p := range pods {
		if i >= ss.Spec.Replicas || p.Status.Phase == api.PodTerminating {
			continue
		}
		s.Replicas++
		if p.Status.Ready {
			s.ReadyReplicas++
		}
		if p.Spec.TemplateHash == hash {
			s.UpdatedReplicas++
		}
		if p.Status.Phase == api.PodCrashLoop || p.Status.Phase == api.PodFailed || (p.Status.Message != "" && !p.Status.Ready) {
			troubled = p.Metadata.Name + ": " + firstNonEmpty(p.Status.Message, string(p.Status.Phase))
		}
	}
	prev := s.Phase
	switch {
	case s.ReadyReplicas == ss.Spec.Replicas && s.UpdatedReplicas == ss.Spec.Replicas && len(extra) == 0:
		s.Phase, s.Message = "Healthy", ""
	case troubled != "":
		s.Phase, s.Message = "Degraded", troubled
	default:
		s.Phase = "Progressing"
		s.Message = fmt.Sprintf("%d/%d ready, %d updated", s.ReadyReplicas, ss.Spec.Replicas, s.UpdatedReplicas)
	}
	if prev != s.Phase {
		typ := "Normal"
		if s.Phase == "Degraded" {
			typ = "Warning"
		}
		st.Event(typ, api.KindStatefulSet, ns, name, s.Phase, "statefulset is "+strings.ToLower(s.Phase))
	}
}

func newStatefulPod(ss *api.StatefulSet, ordinal int, now time.Time) *api.Pod {
	name := fmt.Sprintf("%s-%d", ss.Metadata.Name, ordinal)
	tmpl := deepCopy(ss.Spec.Template)
	claims := map[string]bool{}
	for _, ct := range ss.Spec.VolumeClaimTemplates {
		claims[ct.Name] = true
	}
	rewrite := func(cs []api.Container) {
		for i := range cs {
			for k := range cs[i].Volumes {
				if v := cs[i].Volumes[k].Volume; claims[v] {
					cs[i].Volumes[k].Volume = claimVolumeName(v, ss.Metadata.Name, ordinal)
				}
			}
		}
	}
	rewrite(tmpl.Containers)
	rewrite(tmpl.InitContainers)
	for k := range tmpl.Container.Volumes {
		if v := tmpl.Container.Volumes[k].Volume; claims[v] {
			tmpl.Container.Volumes[k].Volume = claimVolumeName(v, ss.Metadata.Name, ordinal)
		}
	}
	p := newPod(ss.Metadata.Namespace, name, api.KindStatefulSet, ss.Metadata.Name, tmpl, ss.Spec.NodeSelector, now)
	// The hash identifies the set's template revision, not the per-pod rewrite.
	p.Spec.TemplateHash = ss.Status.CurrentHash
	p.Spec.Subdomain = ss.Spec.ServiceName
	p.Metadata.Labels["nube/pod-name"] = name
	return p
}
