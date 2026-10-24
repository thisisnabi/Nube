package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// Nube runs AI agents itself: an Agent is a model, instructions and tools;
// every task is an AgentRun executed in its own pod by the runtime built
// into the nube binary ("nube agent-run"). Sandbox tools run in sibling
// containers of that pod. The control plane queues runs per agent, enforces
// budgets and timeouts, holds tool calls for approval, keeps the trace and
// the session memory.

const (
	defaultAgentModel  = "claude-opus-5-5"
	defaultMaxSteps    = 25
	defaultRunTokens   = 500000
	defaultRunTimeout  = 900
	defaultConcurrency = 4
	defaultMemory      = 40
	keptRunsPerAgent   = 100
	maxRunDepth        = 3
	sandboxPortBase    = 7979
	runTokenPrefix     = "nuberun_"
	maxStepText        = 4000
)

var toolNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ---------------------------------------------------------------- apply

func validateAgent(a *api.Agent) error {
	m := &a.Spec.Model
	switch m.Provider {
	case "":
		m.Provider = "anthropic"
	case "anthropic", "openai":
	default:
		return fmt.Errorf("model.provider must be anthropic or openai (any OpenAI-compatible API)")
	}
	if m.Name == "" {
		if m.Provider != "anthropic" {
			return fmt.Errorf("model.name is required for provider %s", m.Provider)
		}
		m.Name = defaultAgentModel
	}
	switch m.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("model.effort must be low, medium, high, xhigh or max")
	}
	if m.APIKeySecret == "" && m.BaseURL == "" {
		return fmt.Errorf("model.apiKeySecret is required (a Secret with %s)", map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY"}[m.Provider])
	}
	names := map[string]bool{}
	sandboxes := 0
	for i := range a.Spec.Tools {
		t := &a.Spec.Tools[i]
		n := 0
		for _, set := range []bool{t.HTTP != nil, t.Sandbox != nil, t.MCP != nil, t.Nube != nil, t.Agent != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("tools[%d]: set exactly one of http, sandbox, mcp, nube, agent", i)
		}
		switch {
		case t.HTTP != nil:
			if t.HTTP.URL == "" && (t.HTTP.Service == "" || t.HTTP.Port == 0) {
				return fmt.Errorf("tools[%d]: http needs service and port, or url", i)
			}
		case t.Sandbox != nil:
			if t.Sandbox.Image == "" {
				return fmt.Errorf("tools[%d]: sandbox needs an image", i)
			}
			sandboxes++
		case t.MCP != nil:
			if t.MCP.URL == "" && (t.MCP.Service == "" || t.MCP.Port == 0) {
				return fmt.Errorf("tools[%d]: mcp needs service and port, or url", i)
			}
		case t.Nube != nil:
			switch t.Nube.Role {
			case "":
				t.Nube.Role = api.RoleView
			case api.RoleView, api.RoleAgent:
			default:
				return fmt.Errorf("tools[%d]: nube.role must be view or agent", i)
			}
			if len(t.Nube.Namespaces) == 0 {
				t.Nube.Namespaces = []string{a.Metadata.Namespace}
			}
		case t.Agent != nil:
			if !api.ValidName(t.Agent.Name) {
				return fmt.Errorf("tools[%d]: agent.name %q is not a valid name", i, t.Agent.Name)
			}
		}
		if t.HTTP != nil || t.Sandbox != nil || t.Agent != nil {
			if t.Name == "" && t.Agent != nil {
				t.Name = "ask_" + strings.ReplaceAll(t.Agent.Name, "-", "_")
			}
			if t.Name == "" && t.Sandbox != nil {
				t.Name = "shell"
			}
			if !toolNameRE.MatchString(t.Name) {
				return fmt.Errorf("tools[%d]: name %q must be 1-64 letters, digits, _ or -", i, t.Name)
			}
			if names[t.Name] {
				return fmt.Errorf("tools[%d]: duplicate tool name %s", i, t.Name)
			}
			names[t.Name] = true
		}
	}
	if sandboxes > 4 {
		return fmt.Errorf("at most 4 sandbox tools")
	}
	l := &a.Spec.Limits
	if l.MaxSteps <= 0 {
		l.MaxSteps = defaultMaxSteps
	}
	if l.MaxTokens <= 0 {
		l.MaxTokens = defaultRunTokens
	}
	if l.TimeoutSeconds <= 0 {
		l.TimeoutSeconds = defaultRunTimeout
	}
	if l.Concurrency <= 0 {
		l.Concurrency = defaultConcurrency
	}
	if a.Spec.Memory != nil && a.Spec.Memory.MaxMessages <= 0 {
		a.Spec.Memory.MaxMessages = defaultMemory
	}
	for i, sc := range a.Spec.Schedules {
		if _, err := parseSchedule(sc.Cron); err != nil {
			return fmt.Errorf("schedules[%d]: %w", i, err)
		}
		if sc.TimeZone != "" {
			if _, err := time.LoadLocation(sc.TimeZone); err != nil {
				return fmt.Errorf("schedules[%d]: unknown time zone %q", i, sc.TimeZone)
			}
		}
		if strings.TrimSpace(sc.Input) == "" {
			return fmt.Errorf("schedules[%d]: input is required", i)
		}
	}
	if _, err := api.ParseCPU(firstNonEmpty(a.Spec.Resources.CPU, "0")); err != nil {
		return fmt.Errorf("resources.cpu: %w", err)
	}
	if _, err := api.ParseMemory(firstNonEmpty(a.Spec.Resources.Memory, "0")); err != nil {
		return fmt.Errorf("resources.memory: %w", err)
	}
	return nil
}

func applyAgent(st *store.State, o api.Object) (string, error) {
	var a api.Agent
	if err := json.Unmarshal(o.Raw, &a); err != nil {
		return "", err
	}
	a.Kind = api.KindAgent
	a.Metadata.Name, a.Metadata.Namespace = o.Name, o.Namespace
	if err := validateAgent(&a); err != nil {
		return "", fmt.Errorf("agent/%s: %w", o.Name, err)
	}
	key := a.Metadata.Key()
	if old := st.Agents[key]; old != nil {
		if jsonEqual(old.Spec, a.Spec) {
			return "unchanged", nil
		}
		touchMeta(&a.Metadata, &old.Metadata)
		a.Status = old.Status
		st.Agents[key] = &a
		st.Event("Normal", api.KindAgent, o.Namespace, o.Name, "Updated", "agent updated (new runs use it)")
		return "configured", nil
	}
	touchMeta(&a.Metadata, nil)
	a.Status.Phase = "Ready"
	st.Agents[key] = &a
	st.Event("Normal", api.KindAgent, o.Namespace, o.Name, "Created", "agent created with model "+a.Spec.Model.Name)
	return "created", nil
}

func applyAgentRun(st *store.State, o api.Object) (string, error) {
	var r api.AgentRun
	if err := json.Unmarshal(o.Raw, &r); err != nil {
		return "", err
	}
	if old := st.AgentRuns[o.Namespace+"/"+o.Name]; old != nil {
		if jsonEqual(old.Spec, r.Spec) {
			return "unchanged", nil
		}
		return "", fmt.Errorf("agentrun/%s: runs cannot be changed; start a new one", o.Name)
	}
	if st.Agents[o.Namespace+"/"+r.Spec.Agent] == nil {
		return "", fmt.Errorf("agentrun/%s: agent %q not found", o.Name, r.Spec.Agent)
	}
	if strings.TrimSpace(r.Spec.Input) == "" {
		return "", fmt.Errorf("agentrun/%s: input is required", o.Name)
	}
	r.Spec.Trigger = firstNonEmpty(r.Spec.Trigger, "api")
	newAgentRun(st, o.Namespace, o.Name, r.Spec, time.Now())
	return "created", nil
}

// newAgentRun queues a run.
func newAgentRun(st *store.State, ns, name string, spec api.AgentRunSpec, now time.Time) *api.AgentRun {
	if name == "" {
		name = spec.Agent + "-" + randSuffix(6)
	}
	r := &api.AgentRun{Kind: api.KindAgentRun, Spec: spec, Status: api.AgentRunStatus{Phase: "Queued"}}
	r.Metadata = api.Meta{Name: name, Namespace: ns, UID: newUID(), CreatedAt: now, UpdatedAt: now,
		Labels: map[string]string{"nube/agent": spec.Agent}}
	st.AgentRuns[r.Metadata.Key()] = r
	st.Event("Normal", api.KindAgentRun, ns, name, "Queued", fmt.Sprintf("run of agent %s queued (%s)", spec.Agent, spec.Trigger))
	return r
}

func deleteAgent(st *store.State, ns, name string) error {
	if st.Agents[ns+"/"+name] == nil {
		return notFound("agent/" + name)
	}
	for k, r := range st.AgentRuns {
		if r.Metadata.Namespace == ns && r.Spec.Agent == name {
			stopRunPod(st, r)
			delete(st.AgentRuns, k)
		}
	}
	for k := range st.AgentMemory {
		if strings.HasPrefix(k, ns+"/"+name+"/") {
			delete(st.AgentMemory, k)
		}
	}
	delete(st.Agents, ns+"/"+name)
	st.Event("Normal", api.KindAgent, ns, name, "Deleted", "agent, its runs and memory deleted")
	return nil
}

func deleteAgentRun(st *store.State, ns, name string) error {
	r := st.AgentRuns[ns+"/"+name]
	if r == nil {
		return notFound("agentrun/" + name)
	}
	stopRunPod(st, r)
	delete(st.AgentRuns, ns+"/"+name)
	return nil
}

func stopRunPod(st *store.State, r *api.AgentRun) {
	for _, p := range st.PodsOwned(r.Metadata.Namespace, api.KindAgentRun, r.Metadata.Name) {
		terminatePod(st, p, false)
	}
}

// ---------------------------------------------------------------- controller

func runDepth(st *store.State, r *api.AgentRun) int {
	d := 0
	for p := r.Spec.Parent; p != "" && d < 10; d++ {
		pr := st.AgentRuns[r.Metadata.Namespace+"/"+p]
		if pr == nil {
			break
		}
		p = pr.Spec.Parent
	}
	return d
}

func finishRun(st *store.State, r *api.AgentRun, phase, msg string, now time.Time) {
	if r.Finished() {
		return
	}
	// A tool call the run was waiting for can no longer happen.
	if ap := st.Approvals[r.Status.Approval]; ap != nil && ap.Status == "Pending" {
		ap.Status, ap.DecidedAt, ap.Note = "Expired", now, "the run ended ("+strings.ToLower(phase)+")"
	}
	r.Status.Phase, r.Status.Message, r.Status.FinishedAt, r.Status.Approval = phase, msg, now, ""
	r.Metadata.UpdatedAt = now
	stopRunPod(st, r)
	typ := "Normal"
	if phase != "Succeeded" {
		typ = "Warning"
	}
	text := "run " + strings.ToLower(phase)
	if msg != "" {
		text += ": " + msg
	}
	st.Event(typ, api.KindAgentRun, r.Metadata.Namespace, r.Metadata.Name, phase, text)
	a := st.Agents[r.Metadata.Namespace+"/"+r.Spec.Agent]
	if a == nil {
		return
	}
	a.Status.Tokens += r.Status.Usage.InputTokens + r.Status.Usage.OutputTokens
	if phase == "Succeeded" {
		a.Status.Succeeded++
		if m := a.Spec.Memory; m != nil && m.Enabled && r.Spec.Session != "" {
			key := r.Metadata.Namespace + "/" + r.Spec.Agent + "/" + r.Spec.Session
			mem := append(st.AgentMemory[key],
				api.MemoryMessage{Role: "user", Text: r.Spec.Input, Time: r.Metadata.CreatedAt, Run: r.Metadata.Name},
				api.MemoryMessage{Role: "assistant", Text: r.Status.Output, Time: now, Run: r.Metadata.Name})
			if len(mem) > m.MaxMessages {
				mem = mem[len(mem)-m.MaxMessages:]
				if len(mem) > 0 && mem[0].Role == "assistant" { // keep user/assistant pairs
					mem = mem[1:]
				}
			}
			st.AgentMemory[key] = mem
		}
	} else {
		a.Status.Failed++
	}
}

// reconcileAgents fires schedules, starts queued runs within each agent's
// concurrency, enforces timeouts and notices runtimes that died.
func reconcileAgents(st *store.State, now time.Time) {
	if len(st.Agents) == 0 && len(st.AgentRuns) == 0 {
		return
	}
	if len(st.ConfigKey) == 0 { // keys the run tokens (and config digests)
		st.ConfigKey = make([]byte, 32)
		rand.Read(st.ConfigKey)
	}
	for _, key := range sortedKeys(st.Agents) {
		a := st.Agents[key]
		for _, sc := range a.Spec.Schedules {
			sched, err := parseSchedule(sc.Cron)
			if err != nil {
				continue
			}
			loc := time.Local
			if sc.TimeZone != "" {
				if l, err := time.LoadLocation(sc.TimeZone); err == nil {
					loc = l
				}
			}
			if a.Status.LastFiredAt == nil {
				a.Status.LastFiredAt = map[string]time.Time{}
			}
			last, ok := a.Status.LastFiredAt[sc.Cron]
			if !ok {
				a.Status.LastFiredAt[sc.Cron] = now // start counting from now, not from creation
				continue
			}
			if next := sched.Next(last.In(loc)); !next.IsZero() && !now.Before(next) {
				a.Status.LastFiredAt[sc.Cron] = now
				newAgentRun(st, a.Metadata.Namespace, "", api.AgentRunSpec{Agent: a.Metadata.Name, Input: sc.Input, Session: sc.Session, Trigger: "schedule"}, now)
			}
		}
	}

	byAgent := map[string][]*api.AgentRun{}
	for _, r := range st.AgentRuns {
		k := r.Metadata.Namespace + "/" + r.Spec.Agent
		byAgent[k] = append(byAgent[k], r)
	}
	for k, runs := range byAgent {
		sort.Slice(runs, func(i, j int) bool { return runs[i].Metadata.CreatedAt.Before(runs[j].Metadata.CreatedAt) })
		a := st.Agents[k]
		active, queued := 0, 0
		for _, r := range runs {
			if a == nil {
				finishRun(st, r, "Failed", "agent "+r.Spec.Agent+" does not exist", now)
				continue
			}
			switch r.Status.Phase {
			case "Running", "WaitingApproval":
				if now.Sub(r.Status.StartedAt) > time.Duration(a.Spec.Limits.TimeoutSeconds)*time.Second {
					finishRun(st, r, "Failed", fmt.Sprintf("timed out after %ds", a.Spec.Limits.TimeoutSeconds), now)
					continue
				}
				if msg := runtimeGone(st, r, now); msg != "" {
					finishRun(st, r, "Failed", msg, now)
					continue
				}
				active++
			case "Queued":
				queued++
			}
		}
		for _, r := range runs {
			if a == nil || r.Status.Phase != "Queued" || active >= a.Spec.Limits.Concurrency {
				continue
			}
			if runDepth(st, r) > maxRunDepth {
				finishRun(st, r, "Failed", fmt.Sprintf("agents may delegate at most %d levels deep", maxRunDepth), now)
				queued--
				continue
			}
			p := agentRunPod(a, r, now)
			st.Pods[p.Metadata.Key()] = p
			r.Status.Phase, r.Status.Pod, r.Status.StartedAt, r.Metadata.UpdatedAt = "Running", p.Metadata.Name, now, now
			a.Status.LastRun, a.Status.LastRunAt = r.Metadata.Name, now
			st.Event("Normal", api.KindAgentRun, r.Metadata.Namespace, r.Metadata.Name, "Started", "runtime pod "+p.Metadata.Name+" created")
			active++
			queued--
		}
		if a != nil {
			a.Status.Running, a.Status.Queued = active, max(queued, 0)
			// Keep the newest finished runs.
			var done []*api.AgentRun
			for _, r := range runs {
				if r.Finished() {
					done = append(done, r)
				}
			}
			for len(done) > keptRunsPerAgent {
				delete(st.AgentRuns, done[0].Metadata.Key())
				done = done[1:]
			}
		}
	}
}

// runtimeGone explains why a running run's runtime is no longer there.
func runtimeGone(st *store.State, r *api.AgentRun, now time.Time) string {
	p := st.Pods[r.Metadata.Namespace+"/"+r.Status.Pod]
	if p == nil {
		return "runtime pod disappeared"
	}
	if p.Status.Phase == api.PodFailed || p.Status.Phase == api.PodSucceeded {
		return "runtime exited without reporting: " + firstNonEmpty(p.Status.Message, string(p.Status.Phase))
	}
	for _, c := range p.Status.Containers {
		if c.Name == "runtime" && c.State == "Terminated" && now.Sub(r.Status.StartedAt) > 10*time.Second {
			return fmt.Sprintf("runtime exited (code %d) without reporting: %s", c.ExitCode, c.Message)
		}
	}
	return ""
}

// agentRunPod is the pod that executes a run: the runtime and the sandboxes.
func agentRunPod(a *api.Agent, r *api.AgentRun, now time.Time) *api.Pod {
	runtime := api.Container{
		Name: "runtime", Image: api.RuntimeImage, Command: []string{"/nube", "agent-run"},
		Env: map[string]string{
			"NUBE_RUN":     r.Metadata.Namespace + "/" + r.Metadata.Name,
			"SSL_CERT_DIR": "/etc/ssl/certs",
		},
		Resources: a.Spec.Resources,
		Volumes:   []api.VolumeMount{{HostPath: "/etc/ssl/certs", ContainerPath: "/etc/ssl/certs", ReadOnly: true}},
	}
	if s := a.Spec.Model.APIKeySecret; s != "" {
		runtime.EnvFrom = []api.EnvFromSource{{Secret: s}}
	}
	tmpl := api.PodTemplate{
		Labels:        map[string]string{"nube/agent": a.Metadata.Name, "nube/agentrun": r.Metadata.Name},
		Containers:    []api.Container{runtime},
		RestartPolicy: "Never",
	}
	port := sandboxPortBase
	for _, t := range a.Spec.Tools {
		if t.Sandbox == nil {
			continue
		}
		args := []string{"/.nube/nube", "sandbox", "--listen", fmt.Sprintf("127.0.0.1:%d", port)}
		if t.Sandbox.Workdir != "" {
			args = append(args, "--workdir", t.Sandbox.Workdir)
		}
		tmpl.Containers = append(tmpl.Containers, api.Container{
			Name: "sandbox-" + strings.ToLower(strings.ReplaceAll(t.Name, "_", "-")), Image: t.Sandbox.Image,
			Command: args, Env: t.Sandbox.Env,
			Volumes: []api.VolumeMount{{HostPath: api.NubeBinary, ContainerPath: "/.nube/nube", ReadOnly: true}},
		})
		port++
	}
	p := newPod(r.Metadata.Namespace, "run-"+r.Metadata.Name+"-"+randSuffix(4), api.KindAgentRun, r.Metadata.Name, tmpl, a.Spec.NodeSelector, now)
	for k, v := range tmpl.Labels {
		p.Metadata.Labels[k] = v
	}
	return p
}

// ---------------------------------------------------------------- run identity

// runToken is the bearer token of a run's runtime: keyed by the cluster key,
// so it is never stored and dies with the run.
func runToken(st *store.State, r *api.AgentRun) string {
	m := hmac.New(sha256.New, st.ConfigKey)
	m.Write([]byte("run:" + r.Metadata.UID))
	return runTokenPrefix + r.Metadata.UID + "_" + hex.EncodeToString(m.Sum(nil))[:40]
}

// runIdentity authenticates a runtime token (nil if invalid or the run is over).
func runIdentity(st *store.State, tok string) *api.Identity {
	uid, _, ok := strings.Cut(strings.TrimPrefix(tok, runTokenPrefix), "_")
	if !ok || len(st.ConfigKey) == 0 {
		return nil
	}
	for _, r := range st.AgentRuns {
		if r.Metadata.UID != uid {
			continue
		}
		if r.Finished() || subtle.ConstantTimeCompare([]byte(runToken(st, r)), []byte(tok)) != 1 {
			return nil
		}
		return &api.Identity{Name: agentRunActor + r.Metadata.Namespace + "/" + r.Metadata.Name, Role: api.RoleRun, Namespaces: []string{r.Metadata.Namespace}}
	}
	return nil
}

// runOf finds the run behind a runtime identity.
func runOf(st *store.State, id *api.Identity) *api.AgentRun {
	key, ok := strings.CutPrefix(id.Name, agentRunActor)
	if !ok || id.Role != api.RoleRun {
		return nil
	}
	return st.AgentRuns[key]
}

// ---------------------------------------------------------------- runtime API

func (s *Server) handleRuntimeRun(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	s.readJSON(w, func(st *store.State) (int, any) {
		run := runOf(st, id)
		if run == nil {
			return 404, apiError{Error: "run not found", Code: "not_found"}
		}
		a := st.Agents[run.Metadata.Namespace+"/"+run.Spec.Agent]
		if a == nil {
			return 404, apiError{Error: "agent not found", Code: "not_found"}
		}
		out := api.RuntimeRun{Run: *run, Agent: a.Spec}
		if m := a.Spec.Memory; m != nil && m.Enabled && run.Spec.Session != "" {
			out.Memory = st.AgentMemory[run.Metadata.Namespace+"/"+run.Spec.Agent+"/"+run.Spec.Session]
		}
		return 200, out
	})
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "... (" + fmt.Sprint(len(s)-n) + " more bytes)"
	}
	return s
}

// handleRuntimeStep records one step of the trace.
func (s *Server) handleRuntimeStep(w http.ResponseWriter, r *http.Request) {
	var step api.AgentStep
	if err := json.NewDecoder(r.Body).Decode(&step); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	step.Text, step.Input, step.Output = clip(step.Text, maxStepText), clip(step.Input, maxStepText), clip(step.Output, maxStepText)
	if step.Time.IsZero() {
		step.Time = time.Now()
	}
	err := s.store.Write(func(st *store.State) error {
		run := runOf(st, identityFrom(r))
		if run == nil || run.Finished() {
			return notFound("run")
		}
		step.N = len(run.Status.Steps) + 1
		run.Status.Steps = append(run.Status.Steps, step)
		run.Status.Usage.InputTokens += step.InputTokens
		run.Status.Usage.OutputTokens += step.OutputTokens
		run.Status.Phase, run.Status.Approval = "Running", ""
		run.Metadata.UpdatedAt = time.Now()
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"n": step.N})
}

func (s *Server) handleRuntimeFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phase   string `json:"phase"` // Succeeded | Failed
		Output  string `json:"output"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Phase != "Succeeded" && req.Phase != "Failed" {
		writeErr(w, 400, "phase must be Succeeded or Failed")
		return
	}
	err := s.store.Write(func(st *store.State) error {
		run := runOf(st, identityFrom(r))
		if run == nil {
			return notFound("run")
		}
		run.Status.Output = clip(req.Output, 64<<10)
		finishRun(st, run, req.Phase, req.Message, time.Now())
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleRuntimeApproval holds a tool call until a human decides.
func (s *Server) handleRuntimeApproval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool   string `json:"tool"`
		Input  string `json:"input"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var ap *api.Approval
	err := s.store.Write(func(st *store.State) error {
		run := runOf(st, identityFrom(r))
		if run == nil || run.Finished() {
			return notFound("run")
		}
		ap = &api.Approval{
			ID: "ap-" + newUID()[:10], Namespace: run.Metadata.Namespace, Requester: agentRunActor + run.Metadata.Name,
			Summary: "agent " + run.Spec.Agent + " calls " + req.Tool, Reason: firstNonEmpty(req.Reason, "tool requires approval"),
			Status: "Pending", CreatedAt: time.Now(), Run: run.Metadata.Name, Tool: req.Tool, Input: clip(req.Input, maxStepText),
		}
		st.Approvals[ap.ID] = ap
		run.Status.Phase, run.Status.Approval = "WaitingApproval", ap.ID
		st.Event("Warning", "Approval", ap.Namespace, ap.ID, "ApprovalRequested", ap.Summary+": "+clip(req.Input, 200))
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, ap)
}

func (s *Server) handleRuntimeApprovalGet(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	s.readJSON(w, func(st *store.State) (int, any) {
		run := runOf(st, id)
		a := st.Approvals[r.PathValue("id")]
		if run == nil || a == nil || a.Run != run.Metadata.Name || a.Namespace != run.Metadata.Namespace {
			return 404, apiError{Error: "approval not found", Code: "not_found"}
		}
		return 200, a
	})
}

// handleRuntimeMCP gives the runtime Nube's own tools, with the role and
// namespaces its agent was granted.
func (s *Server) handleRuntimeMCP(w http.ResponseWriter, r *http.Request) {
	var tool *api.NubeTool
	var who string
	s.store.Read(func(st *store.State) {
		run := runOf(st, identityFrom(r))
		if run == nil {
			return
		}
		if a := st.Agents[run.Metadata.Namespace+"/"+run.Spec.Agent]; a != nil {
			for _, t := range a.Spec.Tools {
				if t.Nube != nil {
					c := *t.Nube
					tool = &c
				}
			}
			who = "agent/" + run.Metadata.Namespace + "/" + run.Spec.Agent
		}
	})
	if tool == nil {
		writeErr(w, 403, "this agent has no nube tool")
		return
	}
	s.serveMCP(w, r, &api.Identity{Name: who, Role: tool.Role, Namespaces: tool.Namespaces}, tool.Tools)
}

// handleRuntimeSubrun delegates a task to another agent.
func (s *Server) handleRuntimeSubrun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent string `json:"agent"`
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var child *api.AgentRun
	err := s.store.Write(func(st *store.State) error {
		run := runOf(st, identityFrom(r))
		if run == nil || run.Finished() {
			return notFound("run")
		}
		a := st.Agents[run.Metadata.Namespace+"/"+run.Spec.Agent]
		allowed := false
		for _, t := range a.Spec.Tools {
			if t.Agent != nil && t.Agent.Name == req.Agent {
				allowed = true
			}
		}
		if !allowed {
			return forbidden("agent " + run.Spec.Agent + " may not delegate to " + req.Agent)
		}
		if st.Agents[run.Metadata.Namespace+"/"+req.Agent] == nil {
			return notFound("agent/" + req.Agent)
		}
		c := *newAgentRun(st, run.Metadata.Namespace, "", api.AgentRunSpec{Agent: req.Agent, Input: req.Input, Parent: run.Metadata.Name, Trigger: "agent"}, time.Now())
		child = &c
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, child)
}

func (s *Server) handleRuntimeSubrunGet(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	s.readJSON(w, func(st *store.State) (int, any) {
		run := runOf(st, id)
		child := st.AgentRuns[id.Namespaces[0]+"/"+r.PathValue("name")]
		if run == nil || child == nil || child.Spec.Parent != run.Metadata.Name {
			return 404, apiError{Error: "run not found", Code: "not_found"}
		}
		c := *child
		c.Status.Steps = nil
		return 200, c
	})
}

// ---------------------------------------------------------------- user API

// handleAgentRunCreate starts a run; with waitSeconds it waits for the answer.
func (s *Server) handleAgentRunCreate(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req struct {
		Input       string `json:"input"`
		Session     string `json:"session"`
		WaitSeconds int    `json:"waitSeconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Input) == "" {
		writeErr(w, 400, `body must be {"input": "...", "session": "optional", "waitSeconds": 0}`)
		return
	}
	var key string
	err := s.store.Write(func(st *store.State) error {
		if st.Agents[ns+"/"+name] == nil {
			return notFound("agent/" + name)
		}
		key = newAgentRun(st, ns, "", api.AgentRunSpec{Agent: name, Input: req.Input, Session: req.Session, Trigger: "api"}, time.Now()).Metadata.Key()
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	deadline := time.Now().Add(time.Duration(min(req.WaitSeconds, 600)) * time.Second)
	for {
		var run api.AgentRun
		s.store.Read(func(st *store.State) {
			if rr := st.AgentRuns[key]; rr != nil {
				run = *rr
			}
		})
		if run.Finished() || run.Status.Phase == "WaitingApproval" || time.Now().After(deadline) {
			writeJSON(w, 200, run)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Server) handleAgentRunCancel(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	err := s.store.Write(func(st *store.State) error {
		run := st.AgentRuns[ns+"/"+name]
		if run == nil {
			return notFound("agentrun/" + name)
		}
		if run.Finished() {
			return fmt.Errorf("run is already %s", strings.ToLower(run.Status.Phase))
		}
		finishRun(st, run, "Cancelled", "cancelled by "+identityFrom(r).Name, time.Now())
		return nil
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}

func (s *Server) handleAgentMemory(w http.ResponseWriter, r *http.Request) {
	ns, name, session := r.PathValue("ns"), r.PathValue("name"), r.URL.Query().Get("session")
	s.readJSON(w, func(st *store.State) (int, any) {
		if st.Agents[ns+"/"+name] == nil {
			return 404, apiError{Error: "agent " + name + " not found", Code: "not_found"}
		}
		sessions := map[string]int{}
		for k, msgs := range st.AgentMemory {
			if rest, ok := strings.CutPrefix(k, ns+"/"+name+"/"); ok {
				sessions[rest] = len(msgs)
			}
		}
		out := map[string]any{"sessions": sessions}
		if session != "" {
			out["messages"] = st.AgentMemory[ns+"/"+name+"/"+session]
		}
		return 200, out
	})
}
