package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func mcpCall(t *testing.T, srv string, token, body string) map[string]any {
	t.Helper()
	_, out := callURL(t, srv, token, "POST", "/api/v1/mcp", body)
	return out
}

func TestMCPServer(t *testing.T) {
	_, ts := testAPI(t)
	_, agentTok := call(t, ts, "admin-token", "POST", "/api/v1/tokens", `{"name":"claude","role":"agent"}`)
	_, viewTok := call(t, ts, "admin-token", "POST", "/api/v1/tokens", `{"name":"viewer","role":"view"}`)
	agent, viewer := agentTok["secret"].(string), viewTok["secret"].(string)

	init := mcpCall(t, ts.URL, agent, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || !strings.Contains(res["instructions"].(string), "role agent") {
		t.Fatalf("initialize: %v", init)
	}
	names := func(tok string) string {
		out := mcpCall(t, ts.URL, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		var list []string
		for _, x := range out["result"].(map[string]any)["tools"].([]any) {
			list = append(list, x.(map[string]any)["name"].(string))
		}
		return strings.Join(list, ",")
	}
	if n := names(agent); !strings.Contains(n, "apply_manifest") || !strings.Contains(n, "diagnose") {
		t.Fatalf("agent tools: %s", n)
	}
	if n := names(viewer); strings.Contains(n, "apply_manifest") || !strings.Contains(n, "get_logs") {
		t.Fatalf("view tools: %s", n)
	}

	callTool := func(tok, name string, args any) (string, bool) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		out := mcpCall(t, ts.URL, tok, string(b))
		r := out["result"].(map[string]any)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
	}
	text, isErr := callTool(agent, "apply_manifest", map[string]any{"manifest": "kind: Deployment\nmetadata: {name: web}\nspec: {replicas: 1, template: {image: nginx}}\n"})
	if isErr || !strings.Contains(text, "deployment/web created") {
		t.Fatalf("apply: %v %s", isErr, text)
	}
	text, isErr = callTool(agent, "delete_resource", map[string]any{"kind": "deploy", "name": "web"})
	if !isErr || !strings.Contains(text, "approval_required") || !strings.Contains(text, `"approval":"ap-`) {
		t.Fatalf("delete as agent: %v %s", isErr, text)
	}
	text, isErr = callTool(agent, "explain", map[string]any{"kind": "agent"})
	if isErr || !strings.Contains(text, "instructions") || !strings.Contains(text, "kind: Agent") {
		t.Fatalf("explain: %s", text)
	}
	text, isErr = callTool(agent, "get_resource", map[string]any{"kind": "deployment", "name": "nope"})
	if !isErr || !strings.Contains(text, "not_found") {
		t.Fatalf("missing resource: %s", text)
	}
	// Notifications get no answer; unknown methods a JSON-RPC error.
	if code, _ := callURL(t, ts.URL, agent, "POST", "/api/v1/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Fatalf("notification: %d", code)
	}
	if out := mcpCall(t, ts.URL, agent, `{"jsonrpc":"2.0","id":9,"method":"nope"}`); out["error"] == nil {
		t.Fatalf("unknown method: %v", out)
	}
}
