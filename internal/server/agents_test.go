package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/runtime"
	"github.com/thisisnabi/Nube/internal/store"
)

// mockClaude answers Messages API calls from a script, one response per call.
type mockClaude struct {
	mu       sync.Mutex
	calls    int
	requests []map[string]any
	script   []func(req map[string]any) map[string]any
}

func (m *mockClaude) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	json.NewDecoder(r.Body).Decode(&req)
	m.mu.Lock()
	m.requests = append(m.requests, req)
	i := m.calls
	m.calls++
	m.mu.Unlock()
	if i >= len(m.script) {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"script exhausted"}}`, 400)
		return
	}
	resp := m.script[i](req)
	resp["id"], resp["type"], resp["role"], resp["model"] = "msg_test", "message", "assistant", req["model"]
	if _, ok := resp["usage"]; !ok {
		resp["usage"] = map[string]any{"input_tokens": 100, "output_tokens": 20}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func toolUse(id, name string, input any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// toolResults extracts the tool_result blocks of a request's last message.
func toolResults(req map[string]any) map[string]string {
	out := map[string]string{}
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		return out
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	blocks, _ := last["content"].([]any)
	for _, b := range blocks {
		m, _ := b.(map[string]any)
		if m["type"] != "tool_result" {
			continue
		}
		text := ""
		switch c := m["content"].(type) {
		case string:
			text = c
		case []any:
			for _, x := range c {
				if xm, ok := x.(map[string]any); ok {
					text += fmt.Sprint(xm["text"])
				}
			}
		}
		out[m["tool_use_id"].(string)] = text
	}
	return out
}

func startRun(t *testing.T, s *Server, ts *httptest.Server, ns, agent, input, session string) (string, string) {
	t.Helper()
	code, out := call(t, ts, "admin-token", "POST", "/api/v1/namespaces/"+ns+"/agents/"+agent+"/runs", `{"input":"`+input+`","session":"`+session+`"}`)
	if code != 200 {
		t.Fatalf("start run: %d %v", code, out)
	}
	name := out["metadata"].(map[string]any)["name"].(string)
	var token string
	s.store.Write(func(st *store.State) error {
		reconcileAgents(st, time.Now()) // creates the runtime pod
		run := st.AgentRuns[ns+"/"+name]
		if run.Status.Phase != "Running" || run.Status.Pod == "" {
			t.Fatalf("run not started: %+v", run.Status)
		}
		pod := st.Pods[ns+"/"+run.Status.Pod]
		if pod == nil || pod.Spec.Template.Containers[0].Image != api.RuntimeImage {
			t.Fatalf("runtime pod: %+v", pod)
		}
		// The token only reaches the pod through its environment, at sync time.
		env := s.resolveEnv(st, pod, pod.Spec.Template.Containers[0])
		token = env["NUBE_RUN_TOKEN"]
		return nil
	})
	if token == "" {
		t.Fatal("no run token in the runtime's environment")
	}
	return name, token
}

func runRuntime(t *testing.T, ts *httptest.Server, token string, env map[string]string) error {
	t.Helper()
	t.Setenv("NUBE_API_SERVER", ts.URL)
	t.Setenv("NUBE_RUN_TOKEN", token)
	t.Setenv("NUBE_CA_HASH", "")
	for k, v := range env {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runtime.Run(ctx)
}

func TestAgentRunEndToEnd(t *testing.T) {
	s, ts := testAPI(t)

	var lookups []string
	kb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lookups = append(lookups, string(b))
		w.Write([]byte(`{"answer":"reset it under Settings > Security"}`))
	}))
	defer kb.Close()

	mock := &mockClaude{}
	mock.script = []func(map[string]any) map[string]any{
		func(req map[string]any) map[string]any {
			// System prompt, tools (http + Nube's MCP tools) and the memory arrive.
			if req["system"] == nil || len(req["tools"].([]any)) < 3 {
				t.Errorf("first request lacks system or tools: %v", req)
			}
			return map[string]any{"stop_reason": "tool_use", "content": []any{
				map[string]any{"type": "text", "text": "Let me look that up."},
				toolUse("t1", "lookup", map[string]any{"q": "password reset"}),
				toolUse("t2", "nube_list_resources", map[string]any{"kind": "agent"}),
			}}
		},
		func(req map[string]any) map[string]any {
			res := toolResults(req)
			if !strings.Contains(res["t1"], "Settings") || !strings.Contains(res["t2"], "support") {
				t.Errorf("tool results not sent back: %v", res)
			}
			return map[string]any{"stop_reason": "tool_use", "content": []any{toolUse("t3", "notify", map[string]any{"msg": "hi"})}}
		},
		func(req map[string]any) map[string]any {
			if res := toolResults(req); !strings.Contains(res["t3"], "Settings") {
				t.Errorf("approved tool result: %v", res)
			}
			return map[string]any{"stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "Go to Settings > Security."}}}
		},
	}
	llm := httptest.NewServer(mock)
	defer llm.Close()

	agentYAML := `
kind: Agent
metadata: {name: support}
spec:
  model: {name: claude-opus-5-5, baseURL: ` + llm.URL + `, apiKeySecret: anthropic}
  instructions: You help users.
  tools:
    - name: lookup
      http: {url: ` + kb.URL + `/search}
      inputSchema: {type: object, properties: {q: {type: string}}, required: [q]}
    - name: notify
      http: {url: ` + kb.URL + `/notify}
      requireApproval: true
    - nube: {role: view, tools: [list_resources]}
  memory: {enabled: true}
`
	if code, out := call(t, ts, "admin-token", "POST", "/api/v1/apply", agentYAML); code != 200 {
		t.Fatalf("apply agent: %d %v", code, out)
	}
	name, token := startRun(t, s, ts, "default", "support", "How do I reset my password?", "user-42")

	// A human approves the held tool call while the runtime waits.
	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(200 * time.Millisecond)
			var id string
			s.store.Read(func(st *store.State) { id = st.AgentRuns["default/"+name].Status.Approval })
			if id != "" {
				call(t, ts, "admin-token", "POST", "/api/v1/approvals/"+id+"/approve", `{}`)
				return
			}
		}
	}()
	if err := runRuntime(t, ts, token, map[string]string{"ANTHROPIC_API_KEY": "test"}); err != nil {
		t.Fatal(err)
	}

	var run api.AgentRun
	var mem []api.MemoryMessage
	s.store.Read(func(st *store.State) {
		run = *st.AgentRuns["default/"+name]
		mem = st.AgentMemory["default/support/user-42"]
	})
	if run.Status.Phase != "Succeeded" || run.Status.Output != "Go to Settings > Security." {
		t.Fatalf("run: %s %q %s", run.Status.Phase, run.Status.Output, run.Status.Message)
	}
	// model, lookup, nube, model, notify (approved), model
	if len(run.Status.Steps) != 6 || run.Status.Steps[4].Approval == "" || run.Status.Usage.InputTokens != 300 {
		b, _ := json.MarshalIndent(run.Status, "", " ")
		t.Fatalf("trace: %s", b)
	}
	if len(lookups) != 2 || len(mem) != 2 || mem[1].Text != run.Status.Output {
		t.Fatalf("lookups %v memory %v", lookups, mem)
	}
	// The run's token dies with the run.
	if code, _ := call(t, ts, token, "GET", "/api/v1/runtime/run", ""); code != 401 {
		t.Fatalf("token of a finished run still works: %d", code)
	}
	// ... and never worked outside the runtime API.
	s2name, tok2 := startRun(t, s, ts, "default", "support", "second", "user-42")
	if code, _ := call(t, ts, tok2, "GET", "/api/v1/namespaces/default/secrets", ""); code != 403 {
		t.Fatalf("run token reads secrets: %d", code)
	}
	// The next run of the session gets the memory.
	mock.script = append(mock.script, func(req map[string]any) map[string]any {
		if msgs := req["messages"].([]any); len(msgs) != 3 {
			t.Errorf("memory not replayed: %d messages", len(msgs))
		}
		return map[string]any{"stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "again"}}}
	})
	if err := runRuntime(t, ts, tok2, map[string]string{"ANTHROPIC_API_KEY": "test"}); err != nil {
		t.Fatal(err)
	}
	s.store.Read(func(st *store.State) {
		if r := st.AgentRuns["default/"+s2name]; r.Status.Phase != "Succeeded" {
			t.Fatalf("second run: %+v", r.Status)
		}
		if a := st.Agents["default/support"]; a.Status.Succeeded != 2 || a.Status.Tokens == 0 {
			t.Fatalf("agent status: %+v", a.Status)
		}
	})
}

func TestAgentRunOpenAIAndRejection(t *testing.T) {
	s, ts := testAPI(t)
	calls := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			io.WriteString(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
				"tool_calls":[{"id":"c1","type":"function","function":{"name":"wipe","arguments":"{}"}}]}}],"usage":{"prompt_tokens":50,"completion_tokens":5}}`)
			return
		}
		msgs := req["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		if last["role"] != "tool" || !strings.Contains(fmt.Sprint(last["content"]), "did not approve") {
			t.Errorf("rejection not reported to the model: %v", last)
		}
		io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"I was not allowed to."}}],"usage":{"prompt_tokens":60,"completion_tokens":6}}`)
	}))
	defer llm.Close()
	call(t, ts, "admin-token", "POST", "/api/v1/apply", `
kind: Agent
metadata: {name: local}
spec:
  model: {provider: openai, name: llama3, baseURL: `+llm.URL+`/v1, apiKeySecret: llm}
  tools:
    - name: wipe
      http: {url: http://127.0.0.1:1/wipe}
      requireApproval: true
`)
	name, token := startRun(t, s, ts, "default", "local", "clean up", "")
	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(200 * time.Millisecond)
			var id string
			s.store.Read(func(st *store.State) { id = st.AgentRuns["default/"+name].Status.Approval })
			if id != "" {
				call(t, ts, "admin-token", "POST", "/api/v1/approvals/"+id+"/reject", `{"note":"no"}`)
				return
			}
		}
	}()
	if err := runRuntime(t, ts, token, map[string]string{"OPENAI_API_KEY": "sk-test"}); err != nil {
		t.Fatal(err)
	}
	s.store.Read(func(st *store.State) {
		r := st.AgentRuns["default/"+name]
		if r.Status.Phase != "Succeeded" || r.Status.Output != "I was not allowed to." || !r.Status.Steps[1].Error {
			t.Fatalf("run: %+v", r.Status)
		}
	})
}

func TestAgentQueueTimeoutAndValidation(t *testing.T) {
	st := testState("n1")
	apply(t, st, `
kind: Agent
metadata: {name: a}
spec:
  model: {apiKeySecret: k}
  limits: {concurrency: 1, timeoutSeconds: 60}
`)
	if st.Agents["default/a"].Spec.Model.Name != "claude-opus-5-5" {
		t.Fatalf("default model: %s", st.Agents["default/a"].Spec.Model.Name)
	}
	now := time.Now()
	r1 := newAgentRun(st, "default", "", api.AgentRunSpec{Agent: "a", Input: "1"}, now)
	r2 := newAgentRun(st, "default", "", api.AgentRunSpec{Agent: "a", Input: "2"}, now.Add(time.Millisecond))
	reconcileAgents(st, now)
	if r1.Status.Phase != "Running" || r2.Status.Phase != "Queued" || st.Agents["default/a"].Status.Queued != 1 {
		t.Fatalf("concurrency 1: %s %s", r1.Status.Phase, r2.Status.Phase)
	}
	st.Approvals["ap-1"] = &api.Approval{ID: "ap-1", Status: "Pending", Run: r1.Metadata.Name}
	r1.Status.Phase, r1.Status.Approval = "WaitingApproval", "ap-1"
	reconcileAgents(st, now.Add(2*time.Minute))
	if r1.Status.Phase != "Failed" || !strings.Contains(r1.Status.Message, "timed out") || r2.Status.Phase != "Running" {
		t.Fatalf("timeout: %s %q, next %s", r1.Status.Phase, r1.Status.Message, r2.Status.Phase)
	}
	if st.Approvals["ap-1"].Status != "Expired" {
		t.Fatalf("approval of a finished run still %s", st.Approvals["ap-1"].Status)
	}
	for _, bad := range []string{
		"spec: {model: {provider: gemini, apiKeySecret: k}}",
		"spec: {model: {apiKeySecret: k}, tools: [{name: x, http: {url: http://a}, sandbox: {image: y}}]}",
		"spec: {model: {apiKeySecret: k}, tools: [{name: 'bad name', http: {url: http://a}}]}",
		"spec: {model: {apiKeySecret: k}, schedules: [{cron: 'nope', input: x}]}",
		"spec: {model: {}}",
	} {
		objs, _ := api.ParseManifests([]byte("kind: Agent\nmetadata: {name: b}\n" + bad))
		if _, err := (&Server{}).applyObject(st, objs[0]); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}
