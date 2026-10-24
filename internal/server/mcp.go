package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/version"
)

// Nube speaks the Model Context Protocol, so AI agents (Claude Code, Claude
// Desktop, the agents Nube runs itself ...) can operate the cluster. Every
// tool is an ordinary API call made with the caller's token: roles,
// namespaces, approvals and the audit log apply exactly as they do to people.

var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpTool describes one tool and how it maps to the API.
type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	write       bool           // changes something (hidden from view-only identities)
	call        func(s *Server, id *api.Identity, args map[string]any) (any, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func flag(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

var (
	nsArg   = str("namespace (default: default)")
	kindArg = str("resource kind: deployment, statefulset, daemonset, pod, service, ingress, configmap, secret, volume, job, cronjob, networkpolicy, volumesnapshot, agent, agentrun, node, or a custom kind")
)

func argStr(a map[string]any, k, def string) string {
	if v, ok := a[k].(string); ok && v != "" {
		return v
	}
	return def
}

func argInt(a map[string]any, k string, def int) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n
		}
	}
	return def
}

func argBool(a map[string]any, k string) bool {
	v, _ := a[k].(bool)
	return v
}

// objectPath builds the API path of a resource (or of its collection).
func (s *Server) objectPath(kindArg, ns, name string) (string, error) {
	kind := s.resolveKind(kindArg)
	if kind == "" {
		return "", fmt.Errorf("unknown kind %q", kindArg)
	}
	plural := api.Plural(kind)
	if kind == api.KindNode {
		p := "/api/v1/nodes"
		if name != "" {
			p += "/" + url.PathEscape(name)
		}
		return p, nil
	}
	if kind != api.KindCRD && api.NormalizeKind(kind) == "" { // custom kind: plural from its CRD
		plural = strings.ToLower(kindArg)
	}
	p := "/api/v1/namespaces/" + url.PathEscape(ns) + "/" + plural
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p, nil
}

var mcpTools = []mcpTool{
	{
		Name: "cluster_overview", Title: "Cluster overview",
		Description: "Nodes, workload health and the problems Nube finds (a summary of diagnose). Start here.",
		InputSchema: obj(map[string]any{"namespace": str("limit to one namespace (default: all you may see)")}),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			var ov api.Overview
			if err := s.callJSON(id, "GET", "/api/v1/overview", nil, &ov); err != nil {
				return nil, err
			}
			ns := argStr(a, "namespace", "")
			nodes := []map[string]any{}
			for _, n := range ov.Nodes {
				nodes = append(nodes, map[string]any{"name": n.Name, "status": n.Status, "cpu": fmt.Sprintf("%dm/%dm", n.Usage.CPU, n.Capacity.CPU),
					"memory": fmt.Sprintf("%dMi/%dMi", n.Usage.Memory, n.Capacity.Memory), "network": n.NetworkReady})
			}
			workloads := []string{}
			for _, d := range ov.Deployments {
				if ns == "" || d.Metadata.Namespace == ns {
					workloads = append(workloads, fmt.Sprintf("deployment %s/%s: %s %d/%d ready %s", d.Metadata.Namespace, d.Metadata.Name, d.Status.Phase, d.Status.ReadyReplicas, d.Spec.Replicas, d.Status.Message))
				}
			}
			for _, d := range ov.StatefulSets {
				if ns == "" || d.Metadata.Namespace == ns {
					workloads = append(workloads, fmt.Sprintf("statefulset %s/%s: %s %d/%d ready", d.Metadata.Namespace, d.Metadata.Name, d.Status.Phase, d.Status.ReadyReplicas, d.Spec.Replicas))
				}
			}
			for _, d := range ov.DaemonSets {
				if ns == "" || d.Metadata.Namespace == ns {
					workloads = append(workloads, fmt.Sprintf("daemonset %s/%s: %s %d/%d ready", d.Metadata.Namespace, d.Metadata.Name, d.Status.Phase, d.Status.Ready, d.Status.DesiredNumber))
				}
			}
			var diag diagnosis
			q := url.Values{}
			if ns != "" {
				q.Set("namespace", ns)
			}
			s.callJSON(id, "GET", "/api/v1/diagnose?"+q.Encode(), nil, &diag)
			return map[string]any{"version": ov.Version, "nodes": nodes, "workloads": workloads, "problems": diag.Summary, "findings": diag.Findings}, nil
		},
	},
	{
		Name: "list_resources", Title: "List resources",
		Description: "List resources of a kind with their status (compact).",
		InputSchema: obj(map[string]any{"kind": kindArg, "namespace": nsArg, "all_namespaces": flag("list in every namespace")}, "kind"),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			kind := s.resolveKind(argStr(a, "kind", ""))
			var p string
			if argBool(a, "all_namespaces") || kind == api.KindNode {
				p = "/api/v1/" + api.Plural(kind)
				if kind == "" {
					return nil, fmt.Errorf("unknown kind %q", argStr(a, "kind", ""))
				}
			} else {
				var err error
				if p, err = s.objectPath(argStr(a, "kind", ""), argStr(a, "namespace", "default"), ""); err != nil {
					return nil, err
				}
			}
			var out struct {
				Items []map[string]any `json:"items"`
			}
			if err := s.callJSON(id, "GET", p, nil, &out); err != nil {
				return nil, err
			}
			items := []map[string]any{}
			for _, it := range out.Items {
				items = append(items, compact(it))
			}
			return map[string]any{"kind": kind, "items": items}, nil
		},
	},
	{
		Name: "get_resource", Title: "Get a resource",
		Description: "One resource in full (spec and status). Secret values are masked.",
		InputSchema: obj(map[string]any{"kind": kindArg, "name": str("name"), "namespace": nsArg}, "kind", "name"),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			p, err := s.objectPath(argStr(a, "kind", ""), argStr(a, "namespace", "default"), argStr(a, "name", ""))
			if err != nil {
				return nil, err
			}
			var out map[string]any
			err = s.callJSON(id, "GET", p, nil, &out)
			return out, err
		},
	},
	{
		Name: "diagnose", Title: "Diagnose problems",
		Description: "What is broken and why, with evidence (including the last log lines of crashing containers) and a fix for each finding. Give kind and name to focus on one workload.",
		InputSchema: obj(map[string]any{"namespace": str("namespace (default: all)"), "kind": str("optional kind, e.g. deployment"), "name": str("optional name")}),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			q := url.Values{}
			for _, k := range []string{"namespace", "kind", "name"} {
				if v := argStr(a, k, ""); v != "" {
					q.Set(k, v)
				}
			}
			var out diagnosis
			err := s.callJSON(id, "GET", "/api/v1/diagnose?"+q.Encode(), nil, &out)
			return out, err
		},
	},
	{
		Name: "get_logs", Title: "Container logs",
		Description: "The last lines of a pod's container log.",
		InputSchema: obj(map[string]any{"pod": str("pod name"), "namespace": nsArg, "container": str("container (default: the first)"), "tail": num("lines (default 100, max 1000)")}, "pod"),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			q := url.Values{"tail": {fmt.Sprint(min(argInt(a, "tail", 100), 1000))}}
			if c := argStr(a, "container", ""); c != "" {
				q.Set("container", c)
			}
			code, body := s.internalCall(id, "GET", "/api/v1/namespaces/"+url.PathEscape(argStr(a, "namespace", "default"))+"/pods/"+url.PathEscape(argStr(a, "pod", ""))+"/logs?"+q.Encode(), "", nil)
			if code >= 300 {
				return nil, apiErr(code, body)
			}
			return string(body), nil
		},
	},
	{
		Name: "get_events", Title: "Recent events",
		Description: "The cluster's recent events (scheduling, rollouts, failures, approvals).",
		InputSchema: obj(map[string]any{"namespace": str("only this namespace"), "limit": num("events (default 50)")}),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			var evs []api.Event
			if err := s.callJSON(id, "GET", "/api/v1/events?since=0", nil, &evs); err != nil {
				return nil, err
			}
			ns, limit := argStr(a, "namespace", ""), argInt(a, "limit", 50)
			out := []string{}
			for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
				e := evs[i]
				if ns != "" && e.Namespace != ns {
					continue
				}
				out = append(out, fmt.Sprintf("%s %s %s/%s %s: %s", e.Time.Format("15:04:05"), e.Type, strings.ToLower(e.Kind), e.Name, e.Reason, e.Message))
			}
			return out, nil
		},
	},
	{
		Name: "top_workloads", Title: "Resource usage and recommendations",
		Description: "CPU/memory requested per pod vs. what Nube recommends from observed usage.",
		InputSchema: obj(map[string]any{"namespace": str("only this namespace")}),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			var ov api.Overview
			if err := s.callJSON(id, "GET", "/api/v1/overview", nil, &ov); err != nil {
				return nil, err
			}
			ns := argStr(a, "namespace", "")
			out := []map[string]any{}
			add := func(kind string, m api.Meta, r *api.Recommendation) {
				if r != nil && (ns == "" || m.Namespace == ns) {
					out = append(out, map[string]any{"workload": kind + "/" + m.Name, "namespace": m.Namespace, "recommendation": r})
				}
			}
			for _, d := range ov.Deployments {
				add("deployment", d.Metadata, d.Status.Recommendation)
			}
			for _, d := range ov.StatefulSets {
				add("statefulset", d.Metadata, d.Status.Recommendation)
			}
			for _, d := range ov.DaemonSets {
				add("daemonset", d.Metadata, d.Status.Recommendation)
			}
			return out, nil
		},
	},
	{
		Name: "explain", Title: "Manifest reference",
		Description: "The fields of a kind (types and nesting) and an example manifest. Use before writing YAML.",
		InputSchema: obj(map[string]any{"kind": kindArg}, "kind"),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			var out map[string]any
			err := s.callJSON(id, "GET", "/api/v1/schema/"+url.PathEscape(argStr(a, "kind", "")), nil, &out)
			return out, err
		},
	},
	{
		Name: "apply_manifest", Title: "Apply a manifest", write: true,
		Description: "Create or update resources from YAML (several documents separated by ---). Returns what changed, field by field. Use dry_run first to preview. With role agent, changes to Secrets, Volumes and NetworkPolicies wait for a human's approval.",
		InputSchema: obj(map[string]any{"manifest": str("YAML or JSON"), "namespace": str("default namespace for the objects"), "dry_run": flag("only validate and show the changes")}, "manifest"),
		Annotations: map[string]any{"destructiveHint": false, "idempotentHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			q := url.Values{}
			if argBool(a, "dry_run") {
				q.Set("dryRun", "1")
			}
			if ns := argStr(a, "namespace", ""); ns != "" {
				q.Set("namespace", ns)
			}
			var out map[string]any
			err := s.callJSONType(id, "POST", "/api/v1/apply?"+q.Encode(), "application/yaml", []byte(argStr(a, "manifest", "")), &out)
			return out, err
		},
	},
	{
		Name: "delete_resource", Title: "Delete a resource", write: true,
		Description: "Delete one resource. With role agent this waits for a human's approval and returns the approval id.",
		InputSchema: obj(map[string]any{"kind": kindArg, "name": str("name"), "namespace": nsArg}, "kind", "name"),
		Annotations: map[string]any{"destructiveHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			p, err := s.objectPath(argStr(a, "kind", ""), argStr(a, "namespace", "default"), argStr(a, "name", ""))
			if err != nil {
				return nil, err
			}
			var out map[string]any
			err = s.callJSON(id, "DELETE", p, nil, &out)
			return out, err
		},
	},
	{
		Name: "scale", Title: "Scale a workload", write: true,
		Description: "Set the replicas of a deployment or statefulset (to zero needs approval for role agent).",
		InputSchema: obj(map[string]any{"kind": str("deployment or statefulset"), "name": str("name"), "namespace": nsArg, "replicas": num("replicas")}, "kind", "name", "replicas"),
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			return s.workloadAction(id, a, "scale", map[string]any{"replicas": argInt(a, "replicas", 0)})
		},
	},
	{
		Name: "set_image", Title: "Change an image", write: true,
		Description: "Roll a deployment or statefulset to a new image (rolling update; a deployment rolls back by itself if the new pods keep failing).",
		InputSchema: obj(map[string]any{"kind": str("deployment or statefulset"), "name": str("name"), "namespace": nsArg, "image": str("new image"), "container": str("container (default: the main one)")}, "kind", "name", "image"),
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			return s.workloadAction(id, a, "set-image", map[string]any{"image": argStr(a, "image", ""), "container": argStr(a, "container", "")})
		},
	},
	{
		Name: "rollout", Title: "Rollout control", write: true,
		Description: "restart (rolling), undo (to the previous or a given revision), pause or resume a deployment's rollout.",
		InputSchema: obj(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"restart", "undo", "pause", "resume"}}, "kind": str("deployment or statefulset (default deployment)"), "name": str("name"), "namespace": nsArg, "revision": num("undo: revision to return to")}, "action", "name"),
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			action := argStr(a, "action", "")
			if action == "undo" {
				action = "rollback"
			}
			return s.workloadAction(id, a, action, map[string]any{"revision": argInt(a, "revision", 0)})
		},
	},
	{
		Name: "list_approvals", Title: "Approvals",
		Description: "Requests waiting for a human (yours, or all for admins), and the result of decided ones.",
		InputSchema: obj(map[string]any{"status": str("Pending (default), Executed, Rejected ... or empty for all")}),
		Annotations: map[string]any{"readOnlyHint": true},
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			var out map[string]any
			err := s.callJSON(id, "GET", "/api/v1/approvals?status="+url.QueryEscape(argStr(a, "status", "Pending")), nil, &out)
			return out, err
		},
	},
	{
		Name: "run_agent", Title: "Run an agent", write: true,
		Description: "Give a task to one of the cluster's AI agents. Returns the run; with wait it waits up to wait_seconds for the answer.",
		InputSchema: obj(map[string]any{"agent": str("agent name"), "namespace": nsArg, "input": str("the task"), "session": str("conversation to continue (memory)"), "wait_seconds": num("wait up to this long for the result (max 600)")}, "agent", "input"),
		call: func(s *Server, id *api.Identity, a map[string]any) (any, error) {
			ns := argStr(a, "namespace", "default")
			body, _ := json.Marshal(map[string]any{"input": argStr(a, "input", ""), "session": argStr(a, "session", ""), "waitSeconds": min(argInt(a, "wait_seconds", 0), 600)})
			var out map[string]any
			err := s.callJSONType(id, "POST", "/api/v1/namespaces/"+url.PathEscape(ns)+"/agents/"+url.PathEscape(argStr(a, "agent", ""))+"/runs", "application/json", body, &out)
			return out, err
		},
	},
}

func (s *Server) workloadAction(id *api.Identity, a map[string]any, action string, body map[string]any) (any, error) {
	kind := s.resolveKind(argStr(a, "kind", "deployment"))
	if kind != api.KindDeployment && kind != api.KindStatefulSet {
		return nil, fmt.Errorf("kind must be deployment or statefulset")
	}
	b, _ := json.Marshal(body)
	var out map[string]any
	err := s.callJSONType(id, "POST", "/api/v1/namespaces/"+url.PathEscape(argStr(a, "namespace", "default"))+"/"+api.Plural(kind)+"/"+url.PathEscape(argStr(a, "name", ""))+"/"+action, "application/json", b, &out)
	return out, err
}

// compact keeps what matters of a listed object: names, labels and status.
func compact(o map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := o["metadata"].(map[string]any); ok {
		for _, k := range []string{"name", "namespace", "labels"} {
			if v, ok := m[k]; ok {
				out[k] = v
			}
		}
	} else if n, ok := o["name"]; ok { // nodes
		out["name"] = n
		for _, k := range []string{"status", "hostIP", "labels", "networkReady"} {
			if v, ok := o[k]; ok {
				out[k] = v
			}
		}
		return out
	}
	if st, ok := o["status"].(map[string]any); ok {
		c := map[string]any{}
		for k, v := range st {
			switch k {
			case "history", "steps", "lastFiredAt":
				continue
			}
			c[k] = v
		}
		out["status"] = c
	}
	if sp, ok := o["spec"].(map[string]any); ok {
		for _, k := range []string{"replicas", "selector", "ports", "type", "size", "agent", "input"} {
			if v, ok := sp[k]; ok {
				out[k] = v
			}
		}
		if t, ok := sp["template"].(map[string]any); ok {
			if img, ok := t["image"]; ok {
				out["image"] = img
			}
		}
	}
	return out
}

// internalCall runs an API request as an identity through the same checks
// as an external one: role and namespace, the agent guard, the audit log.
func (s *Server) internalCall(id *api.Identity, method, path, contentType string, body []byte) (int, []byte) {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, id))
	rec := httptest.NewRecorder()
	if msg := accessDenied(id, req); msg != "" {
		writeErr(rec, http.StatusForbidden, msg)
		return rec.Code, rec.Body.Bytes()
	}
	req, held := s.guard(rec, req, id)
	switch {
	case held:
	case audited(req, id):
		s.serveAudited(s.mux, rec, req, id)
	default:
		s.mux.ServeHTTP(rec, req)
	}
	return rec.Code, rec.Body.Bytes()
}

// toolError is an API error turned into a tool result the model can act on.
type toolError struct {
	Status int    `json:"status"`
	Error  string `json:"error"`
	Code   string `json:"code,omitempty"`
	Hint   string `json:"hint,omitempty"`
	Extra  map[string]any
}

func (e *toolError) String() string { b, _ := json.Marshal(e); return string(b) }

func apiErr(code int, body []byte) error {
	var e apiError
	json.Unmarshal(body, &e)
	if e.Error == "" {
		e.Error = strings.TrimSpace(string(body))
	}
	te := &toolError{Status: code, Error: e.Error, Code: e.Code, Hint: e.Hint}
	var extra map[string]any
	if json.Unmarshal(body, &extra) == nil {
		delete(extra, "error")
		delete(extra, "code")
		delete(extra, "hint")
		te.Extra = extra
	}
	return mcpCallError{te}
}

type mcpCallError struct{ e *toolError }

func (m mcpCallError) Error() string {
	out := map[string]any{"status": m.e.Status, "error": m.e.Error, "code": m.e.Code}
	if m.e.Hint != "" {
		out["hint"] = m.e.Hint
	}
	for k, v := range m.e.Extra {
		out[k] = v
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func (s *Server) callJSON(id *api.Identity, method, path string, body []byte, out any) error {
	return s.callJSONType(id, method, path, "application/json", body, out)
}

func (s *Server) callJSONType(id *api.Identity, method, path, ctype string, body []byte, out any) error {
	code, resp := s.internalCall(id, method, path, ctype, body)
	if code >= 300 || code == http.StatusAccepted { // 202: held for approval, not done
		return apiErr(code, resp)
	}
	if out != nil && len(resp) > 0 {
		return json.Unmarshal(resp, out)
	}
	return nil
}

// toolsFor lists the tools an identity may use.
func toolsFor(id *api.Identity, only []string) []mcpTool {
	var out []mcpTool
	for _, t := range mcpTools {
		if t.write && id.Role == api.RoleView {
			continue
		}
		if len(only) > 0 && !slices.Contains(only, t.Name) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// handleMCP serves MCP over HTTP (JSON responses) for the caller's token.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	s.serveMCP(w, r, identityFrom(r), nil)
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request, id *api.Identity, only []string) {
	if r.Method == http.MethodGet {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, "MCP: POST JSON-RPC messages to this endpoint")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' { // batch
		var reqs []rpcRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", Error: &rpcError{-32700, "parse error"}})
			return
		}
		var out []rpcResponse
		for _, q := range reqs {
			if resp, ok := s.mcpHandle(r.Context(), id, only, q); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	var q rpcRequest
	if err := json.Unmarshal(body, &q); err != nil {
		writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", Error: &rpcError{-32700, "parse error"}})
		return
	}
	resp, ok := s.mcpHandle(r.Context(), id, only, q)
	if !ok { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) mcpHandle(ctx context.Context, id *api.Identity, only []string, q rpcRequest) (rpcResponse, bool) {
	if len(q.ID) == 0 { // notifications (initialized, cancelled ...) need no answer
		return rpcResponse{}, false
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: q.ID}
	switch q.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(q.Params, &p)
		ver := mcpProtocolVersions[0]
		if slices.Contains(mcpProtocolVersions, p.ProtocolVersion) {
			ver = p.ProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "nube", "title": "Nube orchestrator", "version": version.Version},
			"instructions": "You operate a Nube cluster (a Kubernetes-like orchestrator) as " + id.Name + " (role " + id.Role + "). " +
				"Start with cluster_overview or diagnose; use explain before writing a manifest and apply_manifest with dry_run to preview. " +
				"If a call returns code approval_required, a human must approve it: tell the user the approval id and check it later with list_approvals.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		tools := toolsFor(id, only)
		list := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			m := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
			if t.Title != "" {
				m["title"] = t.Title
			}
			if t.Annotations != nil {
				m["annotations"] = t.Annotations
			}
			list = append(list, m)
		}
		resp.Result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(q.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			break
		}
		var tool *mcpTool
		for _, t := range toolsFor(id, only) {
			if t.Name == p.Name {
				tool = &t
				break
			}
		}
		if tool == nil {
			resp.Error = &rpcError{-32602, "unknown tool " + p.Name}
			break
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		out, err := tool.call(s, id, p.Arguments)
		if err != nil {
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
			break
		}
		text, ok := out.(string)
		if !ok {
			out = pruneZero(out)
			b, _ := json.MarshalIndent(out, "", " ")
			text = string(b)
		}
		const maxText = 60000
		if len(text) > maxText {
			text = text[:maxText] + "\n... (truncated)"
		}
		result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}
		if m, ok := out.(map[string]any); ok {
			result["structuredContent"] = m
		}
		resp.Result = result
	default:
		resp.Error = &rpcError{-32601, "method not found: " + q.Method}
	}
	return resp, true
}

// pruneZero drops empty strings, unset times, empty lists and maps from a
// tool result: they cost a model tokens and say nothing.
func pruneZero(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var g any
	if json.Unmarshal(b, &g) != nil {
		return v
	}
	return prune(g)
}

func prune(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, c := range t {
			if c = prune(c); c != nil {
				out[k] = c
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		if len(t) == 0 {
			return nil
		}
		for i := range t {
			t[i] = prune(t[i])
		}
		return t
	case string:
		if t == "" || strings.HasPrefix(t, "0001-01-01T00:00:00") {
			return nil
		}
	}
	return v
}
