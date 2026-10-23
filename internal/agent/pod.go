package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

const (
	labelUID       = "nube.pod.uid"
	labelContainer = "nube.container"
	pauseContainer = "_pause"
)

// ctrState is the agent's view of one container of a pod.
type ctrState struct {
	name        string
	init        bool
	id          string
	image       string
	state       string // Waiting | Running | Terminated
	exitCode    int
	message     string
	ready       bool
	restarts    int
	busy        bool // create in flight
	pulling     bool
	crashing    bool // waiting out a restart backoff
	nextAttempt time.Time
	backoff     int
	probeFails  int
	lastProbe   time.Time
	probing     bool
	startedAt   time.Time
	finishedAt  time.Time
	cpu, mem    int64
	lastCPU     uint64
	lastCPUAt   time.Time
}

// podState is the agent's view of one assigned pod.
type podState struct {
	uid, ns, name string
	phase         api.PodPhase
	message       string
	ready         bool
	pauseID       string
	podIP         string
	hostPorts     map[int]int
	udpPorts      map[int]int
	ctrs          map[string]*ctrState
	initDone      bool
	busy          bool // pause create / teardown in flight
	nextAttempt   time.Time
	backoff       int
	terminated    bool
	restartsInit  bool
}

func (ps *podState) ctr(name string, init bool) *ctrState {
	c := ps.ctrs[name]
	if c == nil {
		c = &ctrState{name: name, init: init, state: "Waiting"}
		ps.ctrs[name] = c
	}
	return c
}

func backoffDelay(n int) time.Duration {
	d := time.Duration(float64(10*time.Second) * math.Pow(2, float64(n-1)))
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func containerName(ns, pod, c string) string {
	if c == pauseContainer {
		c = "POD"
	}
	return "nube_" + ns + "_" + pod + "_" + c
}

// ---------------------------------------------------------------- reconcile

func (a *Agent) reconcile(ctx context.Context) error {
	containers, err := a.docker.List(ctx)
	if err != nil {
		return fmt.Errorf("docker list: %w", err)
	}
	byPod := map[string][]ContainerState{}
	for _, c := range containers {
		byPod[c.Labels[labelUID]] = append(byPod[c.Labels[labelUID]], c)
	}
	a.mu.Lock()
	desired := a.desired
	a.mu.Unlock()

	// Tear down pods that are gone or being deleted.
	for uid, cs := range byPod {
		ap, ok := desired[uid]
		if ok && ap.Pod.Status.Phase != api.PodTerminating {
			continue
		}
		a.teardown(uid, cs, ok)
	}
	a.mu.Lock()
	for uid := range a.pods {
		if _, ok := desired[uid]; !ok {
			if _, has := byPod[uid]; !has {
				delete(a.pods, uid)
			}
		}
	}
	a.mu.Unlock()

	for uid, ap := range desired {
		a.mu.Lock()
		ps := a.pods[uid]
		if ps == nil {
			ps = &podState{uid: uid, ns: ap.Pod.Metadata.Namespace, name: ap.Pod.Metadata.Name, phase: api.PodScheduled, ctrs: map[string]*ctrState{}}
			a.pods[uid] = ps
		}
		if !ps.restartsInit {
			for _, cs := range ap.Pod.Status.Containers {
				ps.ctr(cs.Name, cs.Init).restarts = cs.Restarts
			}
			ps.restartsInit = true
		}
		a.mu.Unlock()
		if ap.Pod.Status.Phase == api.PodTerminating {
			if len(byPod[uid]) == 0 {
				a.mu.Lock()
				ps.terminated, ps.ready = true, false
				a.mu.Unlock()
			}
			continue
		}
		a.syncPod(ctx, ap, ps, byPod[uid])
	}
	return nil
}

// teardown stops and removes every container of a pod, then its emptyDirs.
func (a *Agent) teardown(uid string, cs []ContainerState, report bool) {
	a.mu.Lock()
	ps := a.pods[uid]
	if ps != nil {
		if ps.busy {
			a.mu.Unlock()
			return
		}
		ps.busy, ps.ready, ps.phase = true, false, api.PodTerminating
	}
	a.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// App containers first (graceful), the pause container last.
		sort.Slice(cs, func(i, j int) bool {
			return cs[j].Labels[labelContainer] == pauseContainer && cs[i].Labels[labelContainer] != pauseContainer
		})
		failed := false
		for _, c := range cs {
			if c.Running {
				a.docker.Stop(ctx, c.ID, 10*time.Second)
			}
			if err := a.docker.Remove(ctx, c.ID); err != nil {
				failed = true
				log.Printf("agent: remove %s: %v", c.Name, err)
			}
		}
		if !failed {
			a.removeEmptyDirs(ctx, uid)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if ps != nil {
			ps.busy = false
			if !failed {
				ps.terminated = report
				ps.pauseID = ""
			}
		}
	}()
}

func (a *Agent) removeEmptyDirs(ctx context.Context, uid string) {
	if names, err := a.docker.ListVolumes(ctx, labelUID+"="+uid); err == nil {
		for _, n := range names {
			a.docker.RemoveVolume(ctx, n)
		}
	}
	matches, _ := filepath.Glob(a.sized.Path("pod-" + uid + "-*.img"))
	for _, img := range matches {
		a.sized.Remove(strings.TrimSuffix(filepath.Base(img), ".img"))
	}
}

// syncPod drives one pod: pause container, then init containers in order,
// then the app containers, applying the restart policy and health checks.
func (a *Agent) syncPod(ctx context.Context, ap api.AssignedPod, ps *podState, existing []ContainerState) {
	a.mu.Lock()
	if ps.busy {
		a.mu.Unlock()
		return
	}
	if ap.Blocked != "" {
		ps.phase, ps.message = api.PodPending, ap.Blocked
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	gw := a.net.Gateway()
	if gw == "" {
		a.mu.Lock()
		ps.phase, ps.message = api.PodScheduled, "waiting for pod network"
		a.mu.Unlock()
		return
	}

	var pause *ContainerState
	ctrs := map[string]ContainerState{}
	for i := range existing {
		name := existing[i].Labels[labelContainer]
		if name == pauseContainer {
			pause = &existing[i]
		} else {
			ctrs[name] = existing[i]
		}
	}

	// 1. Network owner.
	if pause == nil {
		if len(ctrs) > 0 {
			// Containers lost their network namespace owner: recreate the pod.
			a.teardown(ap.Pod.Metadata.UID, existing, false)
			return
		}
		a.startPause(ap, ps, gw)
		return
	}
	info, err := a.docker.Inspect(ctx, pause.ID)
	if err != nil {
		return
	}
	if !info.Running {
		// E.g. after a host reboot; containers that joined it restart below.
		if err := a.docker.Start(ctx, pause.ID); err != nil {
			a.mu.Lock()
			ps.phase, ps.message = api.PodPending, "starting pod sandbox: "+err.Error()
			a.mu.Unlock()
			return
		}
		info, _ = a.docker.Inspect(ctx, pause.ID)
	}
	tmpl := ap.Pod.Spec.Template
	policy := tmpl.RestartPolicy
	if policy == "" {
		policy = "Always"
	}
	a.mu.Lock()
	ps.pauseID, ps.podIP, ps.hostPorts, ps.udpPorts = pause.ID, info.IP, info.HostPorts, info.UDPPorts
	a.mu.Unlock()

	// 2. Init containers, one at a time, each must exit 0.
	if !ps.initDone {
		for i, ic := range tmpl.InitContainers {
			a.mu.Lock()
			st := ps.ctr(ic.Name, true)
			a.mu.Unlock()
			c, ok := ctrs[ic.Name]
			step := fmt.Sprintf("init container %s (%d/%d)", ic.Name, i+1, len(tmpl.InitContainers))
			if !ok {
				a.mu.Lock()
				waiting := time.Now().Before(st.nextAttempt) || st.busy
				ps.phase = api.PodInit
				if !waiting {
					ps.message = "running " + step
				}
				a.mu.Unlock()
				if !waiting {
					a.startContainer(ap, ps, ic, true, pause.ID, gw)
				}
				return
			}
			ci, err := a.docker.Inspect(ctx, c.ID)
			if err != nil {
				return
			}
			a.mu.Lock()
			st.id, st.image = ci.ID, ci.Image
			if ci.Running {
				st.state = "Running"
				ps.phase, ps.message = api.PodInit, "running "+step
				a.mu.Unlock()
				return
			}
			st.state, st.exitCode, st.finishedAt = "Terminated", ci.ExitCode, ci.FinishedAt
			if ci.ExitCode == 0 {
				st.message = "completed"
				a.mu.Unlock()
				continue
			}
			st.message = fmt.Sprintf("exited with code %d", ci.ExitCode)
			if policy == "Never" {
				ps.phase, ps.message = api.PodFailed, step+" failed: "+st.message
				a.mu.Unlock()
				return
			}
			if st.nextAttempt.IsZero() {
				st.backoff++
				st.nextAttempt = time.Now().Add(backoffDelay(st.backoff))
			}
			if time.Now().Before(st.nextAttempt) {
				ps.phase = api.PodInit
				ps.message = fmt.Sprintf("%s failed (%s); retry in %s", step, st.message, time.Until(st.nextAttempt).Round(time.Second))
				st.crashing = true
				a.mu.Unlock()
				return
			}
			st.nextAttempt, st.crashing = time.Time{}, false
			st.restarts++
			a.mu.Unlock()
			a.docker.Start(ctx, c.ID)
			return
		}
		a.mu.Lock()
		ps.initDone = true
		a.mu.Unlock()
	}

	// 3. App containers.
	for _, c := range tmpl.AllContainers() {
		a.mu.Lock()
		st := ps.ctr(c.Name, false)
		busy, wait := st.busy, time.Now().Before(st.nextAttempt)
		a.mu.Unlock()
		existingC, ok := ctrs[c.Name]
		if !ok {
			if !busy && !wait {
				a.startContainer(ap, ps, c, false, pause.ID, gw)
			}
			continue
		}
		ci, err := a.docker.Inspect(ctx, existingC.ID)
		if err != nil {
			continue
		}
		a.mu.Lock()
		st.id, st.image = ci.ID, ci.Image
		if ci.Running {
			st.state, st.crashing = "Running", false
			if st.startedAt.IsZero() || st.startedAt.Before(ci.StartedAt) {
				st.startedAt = ci.StartedAt
			}
			if st.backoff > 0 && time.Since(ci.StartedAt) > 10*time.Minute {
				st.backoff = 0 // stable again: reset the crash backoff
			}
			a.checkHealth(ps, st, c)
			a.mu.Unlock()
			continue
		}
		st.state, st.ready, st.cpu = "Terminated", false, 0
		st.exitCode, st.finishedAt = ci.ExitCode, ci.FinishedAt
		reason := fmt.Sprintf("exited with code %d", ci.ExitCode)
		if ci.OOMKilled {
			reason = "OOMKilled (memory limit " + c.Resources.Memory + ")"
		} else if ci.Error != "" {
			reason = ci.Error
		}
		st.message = reason
		restart := policy == "Always" || (policy == "OnFailure" && ci.ExitCode != 0)
		if !restart || st.busy {
			a.mu.Unlock()
			continue
		}
		if st.nextAttempt.IsZero() {
			// New crash: wait 10s, 20s, 40s ... up to 5m before restarting.
			st.backoff++
			st.nextAttempt = time.Now().Add(backoffDelay(st.backoff))
		}
		if time.Now().Before(st.nextAttempt) {
			st.crashing = true
			st.message = fmt.Sprintf("%s; back-off %s before restart", reason, time.Until(st.nextAttempt).Round(time.Second))
			a.mu.Unlock()
			continue
		}
		st.busy, st.crashing, st.nextAttempt = true, false, time.Time{}
		st.restarts++
		a.mu.Unlock()
		go func(st *ctrState, id string) {
			err := a.docker.Start(context.Background(), id)
			a.mu.Lock()
			defer a.mu.Unlock()
			st.busy = false
			if err != nil {
				st.message = "restart failed: " + err.Error()
				return
			}
			st.state, st.message, st.probeFails, st.ready, st.lastProbe = "Running", "", 0, false, time.Time{}
			st.startedAt = time.Now()
		}(st, existingC.ID)
	}
	a.mu.Lock()
	a.aggregate(ap, ps)
	a.mu.Unlock()
}

// aggregate derives the pod phase from its app containers. Called with a.mu held.
func (a *Agent) aggregate(ap api.AssignedPod, ps *podState) {
	cs := ap.Pod.Spec.Template.AllContainers()
	running, ready, terminated, succeeded, crashing, pulling := 0, 0, 0, 0, 0, 0
	msg := ""
	for _, c := range cs {
		st := ps.ctr(c.Name, false)
		switch st.state {
		case "Running":
			running++
			if st.ready {
				ready++
			}
		case "Terminated":
			if !st.crashing && !st.busy {
				terminated++
				if st.exitCode == 0 {
					succeeded++
				}
			}
		}
		if st.crashing {
			crashing++
		}
		if st.pulling {
			pulling++
		}
		if msg == "" && st.message != "" && st.message != "completed" {
			msg = st.message
			if len(cs) > 1 {
				msg = st.name + ": " + msg
			}
		}
	}
	n := len(cs)
	ps.ready = false
	switch {
	case terminated == n && succeeded == n:
		ps.phase, ps.message = api.PodSucceeded, "completed"
	case terminated == n:
		ps.phase, ps.message = api.PodFailed, msg
	case crashing > 0:
		ps.phase, ps.message = api.PodCrashLoop, msg
	case pulling > 0:
		ps.phase, ps.message = api.PodPulling, msg
	case running == n && ready == n:
		ps.phase, ps.message, ps.ready = api.PodRunning, "", true
	case running == n:
		ps.phase, ps.message = api.PodRunning, msg
	default:
		ps.phase, ps.message = api.PodStarting, msg
	}
}

// startPause creates the pod's network owner: it holds the IP, published ports,
// DNS settings and hostname for all the pod's containers.
func (a *Agent) startPause(ap api.AssignedPod, ps *podState, gw string) {
	a.mu.Lock()
	if time.Now().Before(ps.nextAttempt) {
		a.mu.Unlock()
		return
	}
	ps.busy, ps.phase, ps.message = true, api.PodStarting, "creating pod sandbox"
	a.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		id, err := a.createPause(ctx, ap, gw)
		a.mu.Lock()
		defer a.mu.Unlock()
		ps.busy = false
		if err != nil {
			ps.backoff++
			ps.nextAttempt = time.Now().Add(backoffDelay(ps.backoff))
			ps.phase, ps.message = api.PodPending, fmt.Sprintf("CreatePodSandboxError: %v (retry in %s)", err, backoffDelay(ps.backoff))
			log.Printf("agent: pod %s/%s: %v", ps.ns, ps.name, err)
			return
		}
		ps.pauseID, ps.nextAttempt, ps.initDone = id, time.Time{}, len(ap.Pod.Spec.Template.InitContainers) == 0
	}()
}

func (a *Agent) createPause(ctx context.Context, ap api.AssignedPod, gw string) (string, error) {
	img, err := a.ensurePauseImage(ctx)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	domain := a.domain
	a.mu.Unlock()
	ns, pod := ap.Pod.Metadata.Namespace, ap.Pod.Metadata.Name
	var ports []PortSpec
	for _, p := range ap.Pod.Spec.Template.AllPorts() {
		ports = append(ports, PortSpec{ContainerPort: p.ContainerPort, HostPort: p.HostPort, Protocol: p.Protocol})
	}
	search := []string{ns + ".svc." + domain, "svc." + domain, domain}
	if sub := ap.Pod.Spec.Subdomain; sub != "" {
		search = append([]string{sub + "." + ns + ".svc." + domain}, search...)
	}
	spec := ContainerSpec{
		Name:      containerName(ns, pod, pauseContainer),
		Image:     img,
		Labels:    a.labels(ap, pauseContainer, false),
		Ports:     ports,
		Network:   networkName,
		Hostname:  pod,
		DNS:       []string{gw},
		DNSSearch: search,
	}
	return a.createAndStart(ctx, ap, spec)
}

func (a *Agent) labels(ap api.AssignedPod, container string, init bool) map[string]string {
	l := map[string]string{
		"nube.managed": "true", labelUID: ap.Pod.Metadata.UID, "nube.pod.name": ap.Pod.Metadata.Name,
		"nube.pod.namespace": ap.Pod.Metadata.Namespace, labelContainer: container,
		"nube.owner": ap.Pod.Spec.OwnerKind + "/" + ap.Pod.Spec.Owner, "nube.deployment": ap.Pod.Spec.Owner,
	}
	if init {
		l["nube.init"] = "true"
	}
	return l
}

// createAndStart removes a leftover container with the same name, then creates and starts.
func (a *Agent) createAndStart(ctx context.Context, ap api.AssignedPod, spec ContainerSpec) (string, error) {
	// A leftover with this name (an earlier pod of the same name, or a
	// half-created container from a crash) would block creation.
	if cs, err := a.docker.Inspect(ctx, spec.Name); err == nil {
		a.docker.Remove(ctx, cs.ID)
	}
	id, err := a.docker.Create(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	if err := a.docker.Start(ctx, id); err != nil {
		a.docker.Remove(ctx, id)
		return "", fmt.Errorf("start: %w", err)
	}
	return id, nil
}

// startContainer pulls and starts one container inside the pod's network.
func (a *Agent) startContainer(ap api.AssignedPod, ps *podState, c api.Container, init bool, pauseID, gw string) {
	a.mu.Lock()
	st := ps.ctr(c.Name, init)
	if st.busy {
		a.mu.Unlock()
		return
	}
	st.busy, st.pulling, st.state, st.message = true, true, "Waiting", "pulling "+c.Image
	a.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		id, err := a.createContainer(ctx, ap, ps, st, c, init, pauseID)
		a.mu.Lock()
		defer a.mu.Unlock()
		st.busy, st.pulling = false, false
		if err != nil {
			st.backoff++
			st.nextAttempt = time.Now().Add(backoffDelay(st.backoff))
			prefix := "CreateContainerError: "
			if strings.HasPrefix(err.Error(), "pull ") {
				prefix = "ErrImagePull: "
				st.pulling = true // keep the pod in Pulling while it retries
			}
			st.message = fmt.Sprintf("%s%v (retry in %s)", prefix, err, backoffDelay(st.backoff))
			log.Printf("agent: pod %s/%s container %s: %v", ps.ns, ps.name, c.Name, err)
			return
		}
		st.id, st.state, st.message, st.nextAttempt = id, "Running", "", time.Time{}
		st.startedAt, st.probeFails, st.ready, st.lastProbe = time.Now(), 0, false, time.Time{}
		log.Printf("agent: started %s/%s container %s (%s)", ps.ns, ps.name, c.Name, id[:12])
	}()
}

func (a *Agent) createContainer(ctx context.Context, ap api.AssignedPod, ps *podState, st *ctrState, c api.Container, init bool, pauseID string) (string, error) {
	if err := a.docker.EnsureImage(ctx, c.Image, c.ImagePullPolicy); err != nil {
		return "", fmt.Errorf("pull %s: %w", c.Image, err)
	}
	a.mu.Lock()
	st.pulling, st.message = false, "creating container"
	a.mu.Unlock()
	var mounts []Mount
	for _, vm := range c.Volumes {
		m, err := a.prepareMount(ctx, ap, vm)
		if err != nil {
			return "", err
		}
		mounts = append(mounts, m)
	}
	env := make([]string, 0, len(ap.Env[c.Name]))
	for k, v := range ap.Env[c.Name] {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	cpu, _ := api.ParseCPU(c.Resources.CPU)
	mem, _ := api.ParseMemory(c.Resources.Memory)
	spec := ContainerSpec{
		Name:      containerName(ap.Pod.Metadata.Namespace, ap.Pod.Metadata.Name, c.Name),
		Image:     c.Image,
		Command:   c.Command,
		Args:      c.Args,
		Env:       env,
		Labels:    a.labels(ap, c.Name, init),
		CPUMilli:  cpu,
		MemoryMiB: mem,
		Mounts:    mounts,
		Network:   "container:" + pauseID,
		Init:      true,
	}
	return a.createAndStart(ctx, ap, spec)
}

// prepareMount resolves a volume mount into a Docker mount, creating the
// backing storage (sized filesystem, Docker volume or directory) if needed.
func (a *Agent) prepareMount(ctx context.Context, ap api.AssignedPod, vm api.VolumeMount) (Mount, error) {
	switch {
	case vm.HostPath != "":
		os.MkdirAll(vm.HostPath, 0o755)
		return Mount{Type: "bind", Source: vm.HostPath, Target: vm.ContainerPath, ReadOnly: vm.ReadOnly}, nil
	case vm.EmptyDir != "":
		return a.prepareEmptyDir(ctx, ap, vm)
	}
	v, ok := ap.Volumes[vm.Volume]
	if !ok {
		return Mount{}, fmt.Errorf("volume %q not found", vm.Volume)
	}
	key := v.Metadata.Namespace + "/" + v.Metadata.Name
	switch v.Spec.Type {
	case "hostPath":
		os.MkdirAll(v.Spec.HostPath, 0o755)
		a.recordVolume(key, v.Spec.HostPath, false, "hostPath: size not enforced")
		return Mount{Type: "bind", Source: v.Spec.HostPath, Target: vm.ContainerPath, ReadOnly: vm.ReadOnly}, nil
	case "nfs":
		o := "addr=" + v.Spec.NFS.Server + ",rw"
		if v.Spec.NFS.Options != "" {
			o += "," + v.Spec.NFS.Options
		}
		opts := map[string]string{"type": "nfs", "o": o, "device": ":" + v.Spec.NFS.Path}
		if err := a.docker.EnsureVolume(ctx, v.DockerName(), opts, nil); err != nil {
			return Mount{}, fmt.Errorf("nfs volume %s: %w", vm.Volume, err)
		}
		a.recordVolume(key, "", false, "nfs: the size is managed by the NFS server")
		return Mount{Type: "volume", Source: v.DockerName(), Target: vm.ContainerPath, ReadOnly: vm.ReadOnly}, nil
	}
	// local
	size, _ := api.ParseMemory(v.Spec.Size)
	if size > 0 && !a.docker.VolumeExists(ctx, v.DockerName()) {
		path, err := a.sized.Ensure(v.DockerName(), size)
		if err == nil {
			a.recordVolume(key, path, true, "")
			return Mount{Type: "bind", Source: path, Target: vm.ContainerPath, ReadOnly: vm.ReadOnly}, nil
		}
		log.Printf("agent: volume %s: size cannot be enforced (%v); using an unlimited Docker volume", key, err)
		a.recordVolume(key, "", false, "size not enforced: "+err.Error())
	} else if size > 0 {
		a.recordVolume(key, "", false, "size not enforced: volume was created before it had a size")
	} else {
		a.recordVolume(key, "", false, "no size set: unlimited")
	}
	if err := a.docker.EnsureVolume(ctx, v.DockerName(), nil, nil); err != nil {
		return Mount{}, fmt.Errorf("volume %s: %w", vm.Volume, err)
	}
	return Mount{Type: "volume", Source: v.DockerName(), Target: vm.ContainerPath, ReadOnly: vm.ReadOnly}, nil
}

func (a *Agent) prepareEmptyDir(ctx context.Context, ap api.AssignedPod, vm api.VolumeMount) (Mount, error) {
	var spec api.EmptyDir
	for _, e := range ap.Pod.Spec.Template.EmptyDirs {
		if e.Name == vm.EmptyDir {
			spec = e
		}
	}
	uid := ap.Pod.Metadata.UID
	limit, _ := api.ParseMemory(spec.SizeLimit)
	labels := map[string]string{labelUID: uid}
	name := "nube_pod_" + uid + "_" + vm.EmptyDir
	if spec.Medium == "Memory" {
		opts := map[string]string{"type": "tmpfs", "device": "tmpfs"}
		if limit > 0 {
			opts["o"] = fmt.Sprintf("size=%dm", limit)
		}
		if err := a.docker.EnsureVolume(ctx, name, opts, labels); err != nil {
			return Mount{}, fmt.Errorf("emptyDir %s: %w", vm.EmptyDir, err)
		}
		return Mount{Type: "volume", Source: name, Target: vm.ContainerPath, ReadOnly: vm.ReadOnly}, nil
	}
	if limit > 0 {
		path, err := a.sized.Ensure("pod-"+uid+"-"+vm.EmptyDir, limit)
		if err != nil {
			return Mount{}, fmt.Errorf("emptyDir %s with sizeLimit: %w", vm.EmptyDir, err)
		}
		return Mount{Type: "bind", Source: path, Target: vm.ContainerPath, ReadOnly: vm.ReadOnly}, nil
	}
	if err := a.docker.EnsureVolume(ctx, name, nil, labels); err != nil {
		return Mount{}, fmt.Errorf("emptyDir %s: %w", vm.EmptyDir, err)
	}
	return Mount{Type: "volume", Source: name, Target: vm.ContainerPath, ReadOnly: vm.ReadOnly}, nil
}

// ---------------------------------------------------------------- health

// checkHealth runs a container's readiness/liveness probe when due. Called with a.mu held.
func (a *Agent) checkHealth(ps *podState, st *ctrState, c api.Container) {
	hc := c.HealthCheck
	if hc == nil || (hc.HTTPGet == nil && hc.TCPSocket == nil) {
		// No probe: ready once the container has been up for a second.
		st.ready = time.Since(st.startedAt) > time.Second
		if st.ready {
			st.message = ""
		}
		return
	}
	period := 5 * time.Second
	if hc.PeriodSeconds > 0 {
		period = time.Duration(hc.PeriodSeconds) * time.Second
	}
	if time.Since(st.startedAt) < time.Duration(hc.InitialDelaySeconds)*time.Second {
		st.message = "waiting for initial delay"
		return
	}
	if st.probing || time.Since(st.lastProbe) < period {
		return
	}
	st.probing, st.lastProbe = true, time.Now()
	port := 0
	if hc.HTTPGet != nil {
		port = hc.HTTPGet.Port
	} else {
		port = hc.TCPSocket.Port
	}
	target := ""
	if ps.podIP != "" {
		target = net.JoinHostPort(ps.podIP, strconv.Itoa(port))
	} else if hp := ps.hostPorts[port]; hp > 0 {
		target = net.JoinHostPort("127.0.0.1", strconv.Itoa(hp))
	}
	timeout := 2 * time.Second
	if hc.TimeoutSeconds > 0 {
		timeout = time.Duration(hc.TimeoutSeconds) * time.Second
	}
	threshold := hc.FailureThreshold
	if threshold <= 0 {
		threshold = 3
	}
	id := st.id
	go func() {
		err := probe(hc, target, timeout)
		a.mu.Lock()
		defer a.mu.Unlock()
		st.probing = false
		if st.id != id {
			return
		}
		if err == nil {
			st.probeFails, st.ready, st.message = 0, true, ""
			return
		}
		st.probeFails++
		st.ready = false
		st.message = fmt.Sprintf("health check failed (%d/%d): %v", st.probeFails, threshold, err)
		if st.probeFails >= threshold && !st.busy {
			// Liveness: restart the container.
			st.busy = true
			st.restarts++
			st.message = "restarting: " + st.message
			go func() {
				a.docker.Restart(context.Background(), id)
				a.mu.Lock()
				st.busy, st.probeFails, st.startedAt = false, 0, time.Now()
				a.mu.Unlock()
			}()
		}
	}()
}

func probe(hc *api.HealthCheck, target string, timeout time.Duration) error {
	if target == "" {
		return errors.New("no address to probe (pod has no IP and port is not published)")
	}
	if hc.HTTPGet != nil {
		path := hc.HTTPGet.Path
		if path == "" {
			path = "/"
		}
		c := &http.Client{Timeout: timeout}
		resp, err := c.Get("http://" + target + path)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	}
	c, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return err
	}
	return c.Close()
}

// ---------------------------------------------------------------- reports

// report builds the status of a pod for the server. Called with a.mu held.
func (ps *podState) report() api.PodReport {
	r := api.PodReport{
		Namespace: ps.ns, Name: ps.name, UID: ps.uid, Phase: ps.phase, Ready: ps.ready, Message: ps.message,
		PodIP: ps.podIP, HostPorts: ps.hostPorts, HostPortsUDP: ps.udpPorts,
	}
	if ps.terminated {
		r.Phase = api.PodTerminated
	}
	names := make([]string, 0, len(ps.ctrs))
	for n := range ps.ctrs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := ps.ctrs[names[i]], ps.ctrs[names[j]]
		if a.init != b.init {
			return a.init
		}
		return a.name < b.name
	})
	for _, n := range names {
		st := ps.ctrs[n]
		r.Containers = append(r.Containers, api.ContainerStatus{
			Name: st.name, Init: st.init, State: st.state, Ready: st.ready, Restarts: st.restarts,
			ExitCode: st.exitCode, Message: st.message, ContainerID: st.id, Image: st.image,
		})
		if st.init {
			continue
		}
		if r.ContainerID == "" {
			r.ContainerID = st.id
		}
		r.Restarts += st.restarts
		r.CPU += st.cpu
		r.Memory += st.mem
		if st.exitCode != 0 {
			r.ExitCode = st.exitCode
		}
		if r.StartedAt.IsZero() || (!st.startedAt.IsZero() && st.startedAt.Before(r.StartedAt)) {
			r.StartedAt = st.startedAt
		}
		if st.finishedAt.After(r.FinishedAt) {
			r.FinishedAt = st.finishedAt
		}
	}
	return r
}
