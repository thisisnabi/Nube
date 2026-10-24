package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

const maxToolOutput = 20000 // characters returned to the model per tool call

// tool is one callable tool of the run.
type tool struct {
	def      toolDef
	approval bool // a human approves each call
	call     func(ctx context.Context, input json.RawMessage) (string, error)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + fmt.Sprintf("\n... (%d more bytes cut)", len(s)-n)
	}
	return s
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func safeName(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// serviceURL turns a cluster service reference into a URL.
func serviceURL(ns, svc string, port int, path, raw string) string {
	if raw != "" {
		return raw
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("http://%s.%s:%d%s", svc, ns, port, path)
}

// buildTools turns the agent's tool specs into callable tools.
func buildTools(ctx context.Context, cp *controlPlane, run *api.RuntimeRun) ([]tool, []string) {
	ns := run.Run.Metadata.Namespace
	var tools []tool
	var warnings []string
	port := 7979 // sandbox ports, in the order the control plane assigned them
	for _, spec := range run.Agent.Tools {
		spec := spec
		switch {
		case spec.HTTP != nil:
			schema := spec.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
			}
			target := serviceURL(ns, spec.HTTP.Service, spec.HTTP.Port, spec.HTTP.Path, spec.HTTP.URL)
			tools = append(tools, tool{
				def:      toolDef{Name: spec.Name, Description: firstNonEmpty(spec.Description, "Call "+target), Schema: schema},
				approval: spec.RequireApproval,
				call: func(ctx context.Context, in json.RawMessage) (string, error) {
					return callHTTP(ctx, spec.HTTP, target, in)
				},
			})
		case spec.Sandbox != nil:
			addr := fmt.Sprintf("http://127.0.0.1:%d/exec", port)
			port++
			timeout := spec.Sandbox.TimeoutSeconds
			if timeout <= 0 {
				timeout = 60
			}
			desc := firstNonEmpty(spec.Description, fmt.Sprintf("Run a shell command (sh -c) in a sandbox container (image %s) and get stdout, stderr and the exit code. Files persist between calls of this run.", spec.Sandbox.Image))
			tools = append(tools, tool{
				def: toolDef{Name: spec.Name, Description: desc, Schema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string", "description": "shell command"}},
					"required":   []any{"command"},
				}},
				approval: spec.RequireApproval,
				call: func(ctx context.Context, in json.RawMessage) (string, error) {
					var a struct {
						Command string `json:"command"`
					}
					if err := json.Unmarshal(in, &a); err != nil || a.Command == "" {
						return "", fmt.Errorf(`input must be {"command": "..."}`)
					}
					return callSandbox(ctx, addr, a.Command, timeout)
				},
			})
		case spec.MCP != nil, spec.Nube != nil:
			var c *mcpClient
			prefix := ""
			var only []string
			if spec.MCP != nil {
				target := serviceURL(ns, spec.MCP.Service, spec.MCP.Port, firstNonEmpty(spec.MCP.Path, "/mcp"), spec.MCP.URL)
				c = &mcpClient{url: target, headers: spec.MCP.Headers, http: &http.Client{Timeout: 5 * time.Minute}}
				only = spec.MCP.Tools
				if spec.Name != "" {
					prefix = spec.Name + "__"
				}
			} else {
				c = &mcpClient{url: cp.base + "/api/v1/runtime/mcp", headers: map[string]string{"Authorization": "Bearer " + cp.token}, http: cp.http}
				prefix = "nube_"
			}
			remote, err := c.listTools(ctx)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("tool %s: %v", firstNonEmpty(spec.Name, c.url), err))
				continue
			}
			for _, rt := range remote {
				if len(only) > 0 && !slices.Contains(only, rt.Name) {
					continue
				}
				rt := rt
				schema := rt.InputSchema
				if schema == nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				tools = append(tools, tool{
					def:      toolDef{Name: safeName(prefix + rt.Name), Description: rt.Description, Schema: schema},
					approval: spec.RequireApproval,
					call: func(ctx context.Context, in json.RawMessage) (string, error) {
						return c.callTool(ctx, rt.Name, in)
					},
				})
			}
		case spec.Agent != nil:
			target := spec.Agent.Name
			tools = append(tools, tool{
				def: toolDef{Name: spec.Name, Description: firstNonEmpty(spec.Description, "Delegate a task to the "+target+" agent and get its answer."), Schema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"task": map[string]any{"type": "string", "description": "the task, with all the context the other agent needs"}},
					"required":   []any{"task"},
				}},
				approval: spec.RequireApproval,
				call: func(ctx context.Context, in json.RawMessage) (string, error) {
					var a struct {
						Task string `json:"task"`
					}
					if err := json.Unmarshal(in, &a); err != nil || a.Task == "" {
						return "", fmt.Errorf(`input must be {"task": "..."}`)
					}
					return cp.delegate(ctx, target, a.Task)
				},
			})
		}
	}
	return tools, warnings
}

func callHTTP(ctx context.Context, t *api.HTTPTool, target string, in json.RawMessage) (string, error) {
	method := strings.ToUpper(firstNonEmpty(t.Method, "POST"))
	var body io.Reader
	if method == "GET" {
		var args map[string]any
		json.Unmarshal(in, &args)
		u, err := url.Parse(target)
		if err != nil {
			return "", err
		}
		q := u.Query()
		for k, v := range args {
			q.Set(k, fmt.Sprint(v))
		}
		u.RawQuery = q.Encode()
		target = u.String()
	} else {
		body = bytes.NewReader(in)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	c := &http.Client{Timeout: 2 * time.Minute}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := clip(string(b), maxToolOutput)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%s: %s", resp.Status, out)
	}
	return out, nil
}

func callSandbox(ctx context.Context, addr, command string, timeout int) (string, error) {
	b, _ := json.Marshal(map[string]any{"command": command, "timeoutSeconds": timeout})
	var res sandboxResult
	// The sandbox container may still be starting: retry for a while.
	var err error
	for i := 0; i < 30; i++ {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, "POST", addr, bytes.NewReader(b))
		if err != nil {
			return "", err
		}
		var resp *http.Response
		resp, err = (&http.Client{Timeout: time.Duration(timeout+10) * time.Second}).Do(req)
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&res)
			resp.Body.Close()
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return "", fmt.Errorf("sandbox unreachable: %w", err)
	}
	out := fmt.Sprintf("exit code: %d\n", res.ExitCode)
	if res.TimedOut {
		out += fmt.Sprintf("(killed after %ds)\n", timeout)
	}
	if res.Stdout != "" {
		out += "stdout:\n" + res.Stdout + "\n"
	}
	if res.Stderr != "" {
		out += "stderr:\n" + res.Stderr + "\n"
	}
	return clip(out, maxToolOutput), nil
}

// ---------------------------------------------------------------- MCP client

type mcpClient struct {
	url     string
	headers map[string]string
	http    *http.Client
	session string
	seq     int
	ready   bool
}

type remoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// rpc sends one JSON-RPC message (streamable HTTP: JSON or an SSE stream).
func (c *mcpClient) rpc(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if !notify {
		c.seq++
		msg["id"] = c.seq
	}
	b, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	if notify {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var data []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The response is the first "data:" message carrying our id.
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := sc.Text()
			if d, ok := strings.CutPrefix(line, "data:"); ok {
				d = strings.TrimSpace(d)
				var probe struct {
					ID json.RawMessage `json:"id"`
				}
				if json.Unmarshal([]byte(d), &probe) == nil && string(probe.ID) == fmt.Sprint(c.seq) {
					data = []byte(d)
					break
				}
			}
		}
	} else {
		data, _ = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("bad MCP response: %v", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

func (c *mcpClient) init(ctx context.Context) error {
	if c.ready {
		return nil
	}
	_, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "nube-agent-runtime", "version": "1"},
	}, false)
	if err != nil {
		return err
	}
	c.rpc(ctx, "notifications/initialized", nil, true)
	c.ready = true
	return nil
}

func (c *mcpClient) listTools(ctx context.Context) ([]remoteTool, error) {
	if err := c.init(ctx); err != nil {
		return nil, err
	}
	var all []remoteTool
	cursor := ""
	for page := 0; page < 20; page++ {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		res, err := c.rpc(ctx, "tools/list", params, false)
		if err != nil {
			return nil, err
		}
		var out struct {
			Tools      []remoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		json.Unmarshal(res, &out)
		all = append(all, out.Tools...)
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	return all, nil
}

func (c *mcpClient) callTool(ctx context.Context, name string, in json.RawMessage) (string, error) {
	var args map[string]any
	json.Unmarshal(in, &args)
	if args == nil {
		args = map[string]any{}
	}
	res, err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, false)
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	json.Unmarshal(res, &out)
	var parts []string
	for _, c := range out.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, "["+c.Type+" content]")
		}
	}
	text := clip(strings.Join(parts, "\n"), maxToolOutput)
	if out.IsError {
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
