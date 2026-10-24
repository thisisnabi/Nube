package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/store"
)

// testAPI starts a single-node server's HTTP API (no TLS) for request tests.
func testAPI(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(Config{DataDir: t.TempDir(), Token: "admin-token", ClusterCIDR: "10.244.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.routes(mux)
	s.mux = mux
	ts := httptest.NewServer(s.middleware(mux))
	t.Cleanup(ts.Close)
	return s, ts
}

func call(t *testing.T, ts *httptest.Server, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	return callURL(t, ts.URL, token, method, path, body)
}

func callURL(t *testing.T, base, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		req.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		req.Header.Set("Content-Type", "application/yaml")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestAgentGuardrails(t *testing.T) {
	s, ts := testAPI(t)
	code, tok := call(t, ts, "admin-token", "POST", "/api/v1/tokens", `{"name":"bot","role":"agent"}`)
	if code != 200 {
		t.Fatalf("create token: %d %v", code, tok)
	}
	agent := tok["secret"].(string)

	// Creating workloads is allowed.
	code, out := call(t, ts, agent, "POST", "/api/v1/apply", "kind: Deployment\nmetadata: {name: web}\nspec: {replicas: 1, template: {image: nginx}}\n")
	if code != 200 {
		t.Fatalf("apply as agent: %d %v", code, out)
	}
	// Changes come back field by field.
	code, out = call(t, ts, agent, "POST", "/api/v1/apply?dryRun=1", "kind: Deployment\nmetadata: {name: web}\nspec: {replicas: 3, template: {image: nginx}}\n")
	ch := out["changes"].([]any)[0].(map[string]any)
	if code != 200 || ch["action"] != "configured" || !strings.Contains(toJSON(ch["changes"]), `"path":"spec.replicas"`) {
		t.Fatalf("dry run: %d %v", code, out)
	}
	s.store.Read(func(st *store.State) {
		if st.Deployments["default/web"].Spec.Replicas != 1 {
			t.Fatalf("dry run changed the deployment")
		}
	})

	// Deleting is held for a human.
	code, out = call(t, ts, agent, "DELETE", "/api/v1/namespaces/default/deployments/web", "")
	if code != http.StatusAccepted || out["code"] != "approval_required" {
		t.Fatalf("delete as agent: %d %v", code, out)
	}
	id := out["approval"].(string)
	s.store.Read(func(st *store.State) {
		if st.Deployments["default/web"] == nil {
			t.Fatalf("deleted without approval")
		}
	})
	// Agents cannot approve their own requests.
	if code, _ := call(t, ts, agent, "POST", "/api/v1/approvals/"+id+"/approve", "{}"); code != 403 {
		t.Fatalf("agent approved its own request: %d", code)
	}
	code, out = call(t, ts, "admin-token", "POST", "/api/v1/approvals/"+id+"/approve", `{"note":"ok"}`)
	if code != 200 || out["status"] != "Executed" {
		t.Fatalf("approve: %d %v", code, out)
	}
	s.store.Read(func(st *store.State) {
		if st.Deployments["default/web"] != nil {
			t.Fatalf("approved delete did not run")
		}
	})
	// The agent can follow its request.
	if code, out = call(t, ts, agent, "GET", "/api/v1/approvals/"+id, ""); code != 200 || out["status"] != "Executed" {
		t.Fatalf("agent reads approval: %d %v", code, out)
	}

	// Secrets: values stay hidden, changes need approval.
	call(t, ts, "admin-token", "POST", "/api/v1/apply", "kind: Secret\nmetadata: {name: db}\nstringData: {PASSWORD: hunter2}\n")
	if code, _ := call(t, ts, agent, "GET", "/api/v1/namespaces/default/secrets/db?reveal=1", ""); code != 403 {
		t.Fatalf("agent revealed a secret: %d", code)
	}
	if code, _ := call(t, ts, agent, "POST", "/api/v1/apply", "kind: Secret\nmetadata: {name: db}\nstringData: {PASSWORD: x}\n"); code != http.StatusAccepted {
		t.Fatalf("agent changed a secret without approval: %d", code)
	}

	// Everything is in the audit log; agents cannot read it.
	code, out = call(t, ts, "admin-token", "GET", "/api/v1/audit", "")
	log := toJSON(out)
	if code != 200 || !strings.Contains(log, `"summary":"apply deployment/web"`) || !strings.Contains(log, `"approval":"`+id+`"`) {
		t.Fatalf("audit: %d %s", code, log)
	}
	if code, _ := call(t, ts, agent, "GET", "/api/v1/audit", ""); code != 403 {
		t.Fatalf("agent read the audit log: %d", code)
	}

	// Errors carry a code and a hint.
	code, out = call(t, ts, agent, "GET", "/api/v1/namespaces/default/deployments/nope", "")
	if code != 404 || out["code"] != "not_found" || out["hint"] == "" {
		t.Fatalf("error body: %d %v", code, out)
	}
	_ = api.RoleAgent
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
