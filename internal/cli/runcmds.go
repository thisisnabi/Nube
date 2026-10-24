package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
)

// cmdRun gives a task to an agent: nube run AGENT "task" [--session S] [--wait].
func cmdRun(c *client, a args, rest []string) error {
	if len(rest) < 2 {
		return fmt.Errorf(`usage: nube run AGENT "task" [--session NAME] [--wait] [-n NS]   (task "-" reads stdin)`)
	}
	agent, input := rest[0], strings.Join(rest[1:], " ")
	if input == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		input = string(b)
	}
	wait := 0
	if a.flags["wait"] != "" {
		wait = 600
		c.rebuild(11 * time.Minute)
	}
	var run api.AgentRun
	body := map[string]any{"input": input, "session": a.flags["session"], "waitSeconds": wait}
	if err := c.postJSON("/api/v1/namespaces/"+url.PathEscape(a.ns())+"/agents/"+url.PathEscape(agent)+"/runs", body, &run); err != nil {
		return err
	}
	if a.flags["output"] == "json" {
		return printJSON(run)
	}
	if wait == 0 {
		fmt.Printf("agentrun/%s queued; follow it with: nube trace %s -w\n", run.Metadata.Name, run.Metadata.Name)
		return nil
	}
	switch run.Status.Phase {
	case "Succeeded":
		fmt.Println(run.Status.Output)
		return nil
	case "WaitingApproval":
		return &APIError{Code: "approval_required", Message: fmt.Sprintf("run %s waits for a human to approve a tool call", run.Metadata.Name),
			Extra: map[string]any{"approval": run.Status.Approval, "run": run.Metadata.Name}}
	case "Failed", "Cancelled":
		return fmt.Errorf("run %s %s: %s", run.Metadata.Name, strings.ToLower(run.Status.Phase), run.Status.Message)
	}
	fmt.Printf("agentrun/%s is still %s; follow it with: nube trace %s -w\n", run.Metadata.Name, strings.ToLower(run.Status.Phase), run.Metadata.Name)
	return nil
}

// cmdTrace shows what a run did, step by step (-w follows it).
func cmdTrace(c *client, a args, rest []string) error {
	if len(rest) != 1 {
		return fmt.Errorf("usage: nube trace RUN [-n NS] [-w]")
	}
	path := "/api/v1/namespaces/" + url.PathEscape(a.ns()) + "/agentruns/" + url.PathEscape(rest[0])
	shown := 0
	for {
		var r api.AgentRun
		if err := c.getJSON(path, &r); err != nil {
			return err
		}
		if a.flags["output"] == "json" {
			return printJSON(r)
		}
		if shown == 0 {
			fmt.Printf("run %s  agent %s  trigger %s\ninput: %s\n\n", r.Metadata.Name, r.Spec.Agent, r.Spec.Trigger, r.Spec.Input)
		}
		for _, s := range r.Status.Steps[min(shown, len(r.Status.Steps)):] {
			if s.Type == "model" {
				fmt.Printf("#%d model (%dms, %d in / %d out tokens, %s)\n", s.N, s.DurationMs, s.InputTokens, s.OutputTokens, s.StopReason)
				if s.Text != "" {
					fmt.Println(indent(s.Text, "   "))
				}
				continue
			}
			status := "ok"
			if s.Error {
				status = "error"
			}
			if s.Approval != "" {
				status += ", approval " + s.Approval
			}
			fmt.Printf("#%d tool %s (%dms, %s)\n", s.N, s.Tool, s.DurationMs, status)
			if s.Input != "" {
				fmt.Println(indent(truncate(s.Input, 300), "   input:  "))
			}
			fmt.Println(indent(truncate(s.Output, 600), "   output: "))
		}
		shown = len(r.Status.Steps)
		if r.Finished() || a.flags["watch"] == "" {
			fmt.Printf("\nphase: %s", r.Status.Phase)
			if r.Status.Approval != "" {
				fmt.Printf(" (approval %s: nube approve %s)", r.Status.Approval, r.Status.Approval)
			}
			fmt.Printf("  tokens: %d in / %d out\n", r.Status.Usage.InputTokens, r.Status.Usage.OutputTokens)
			if r.Status.Message != "" {
				fmt.Println("message:", r.Status.Message)
			}
			if r.Status.Output != "" {
				fmt.Println("\noutput:\n" + r.Status.Output)
			}
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = prefix + lines[i]
		} else {
			lines[i] = strings.Repeat(" ", len(prefix)) + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func cmdCancelRun(c *client, a args, rest []string) error {
	if len(rest) != 1 {
		return fmt.Errorf("usage: nube cancel RUN [-n NS]")
	}
	if err := c.postJSON("/api/v1/namespaces/"+url.PathEscape(a.ns())+"/agentruns/"+url.PathEscape(rest[0])+"/cancel", map[string]any{}, nil); err != nil {
		return err
	}
	fmt.Printf("agentrun/%s cancelled\n", rest[0])
	return nil
}

func cmdMemory(c *client, a args, rest []string) error {
	if len(rest) != 1 {
		return fmt.Errorf("usage: nube memory AGENT [--session NAME]")
	}
	q := url.Values{}
	if s := a.flags["session"]; s != "" {
		q.Set("session", s)
	}
	var out struct {
		Sessions map[string]int      `json:"sessions"`
		Messages []api.MemoryMessage `json:"messages"`
	}
	if err := c.getJSON("/api/v1/namespaces/"+url.PathEscape(a.ns())+"/agents/"+url.PathEscape(rest[0])+"/memory?"+q.Encode(), &out); err != nil {
		return err
	}
	if a.flags["output"] == "json" {
		return printJSON(out)
	}
	if a.flags["session"] == "" {
		names := make([]string, 0, len(out.Sessions))
		for s := range out.Sessions {
			names = append(names, s)
		}
		sort.Strings(names)
		w := newTab()
		fmt.Fprintln(w, "SESSION\tMESSAGES")
		for _, s := range names {
			fmt.Fprintf(w, "%s\t%d\n", s, out.Sessions[s])
		}
		return w.Flush()
	}
	for _, m := range out.Messages {
		fmt.Printf("[%s] %s\n%s\n\n", m.Role, m.Time.Format("2006-01-02 15:04"), m.Text)
	}
	return nil
}

// cmdMCP bridges MCP over stdio (Claude Code, Claude Desktop and other MCP
// clients) to the server's MCP endpoint, with this CLI's token.
func cmdMCP(c *client) error {
	c.rebuild(10 * time.Minute)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	out := bufio.NewWriter(os.Stdout)
	reply := func(b []byte) {
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	for in.Scan() {
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}
		resp, err := c.request("POST", "/api/v1/mcp", bytes.NewReader(append([]byte(nil), line...)), "application/json")
		if err != nil {
			// Answer requests with a JSON-RPC error so the client does not hang.
			var probe struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(line, &probe) == nil && len(probe.ID) > 0 {
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": probe.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
				reply(b)
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if body = bytes.TrimSpace(body); len(body) > 0 {
			// stdio MCP is one JSON message per line.
			var compact bytes.Buffer
			if json.Compact(&compact, body) == nil {
				body = compact.Bytes()
			}
			reply(body)
		}
	}
	return in.Err()
}

type schemaField struct {
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	Fields   []schemaField `json:"fields"`
	Optional bool          `json:"optional"`
}

// cmdExplain prints the fields of a kind and an example manifest.
func cmdExplain(c *client, a args, rest []string) error {
	if len(rest) != 1 {
		var out struct {
			Kinds []map[string]any `json:"kinds"`
		}
		if err := c.getJSON("/api/v1/schema", &out); err != nil {
			return err
		}
		for _, k := range out.Kinds {
			fmt.Println(k["kind"])
		}
		fmt.Println("\nnube explain KIND shows its fields and an example")
		return nil
	}
	var out struct {
		Kind    string          `json:"kind"`
		Fields  []schemaField   `json:"fields"`
		Example string          `json:"example"`
		Schema  json.RawMessage `json:"schema"`
	}
	if err := c.getJSON("/api/v1/schema/"+url.PathEscape(rest[0]), &out); err != nil {
		return err
	}
	if a.flags["output"] == "json" {
		return printJSON(out)
	}
	fmt.Println("KIND:", out.Kind)
	if len(out.Schema) > 0 {
		fmt.Println(string(out.Schema))
		return nil
	}
	fmt.Println("FIELDS:")
	printFields(out.Fields, "  ")
	if out.Example != "" {
		fmt.Println("\nEXAMPLE:\n" + out.Example)
	}
	return nil
}

func printFields(fs []schemaField, prefix string) {
	for _, f := range fs {
		req := ""
		if !f.Optional {
			req = " (required)"
		}
		fmt.Printf("%s%s: %s%s\n", prefix, f.Name, f.Type, req)
		if len(prefix) < 14 {
			printFields(f.Fields, prefix+"  ")
		}
	}
}
