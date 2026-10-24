package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/pki"
)

// controlPlane is the runtime's connection to the Nube API, as its run.
type controlPlane struct {
	base, token string
	http        *http.Client
}

func newControlPlane() (*controlPlane, error) {
	base := strings.TrimRight(os.Getenv("NUBE_API_SERVER"), "/")
	token := os.Getenv("NUBE_RUN_TOKEN")
	if base == "" || token == "" {
		return nil, errors.New("NUBE_API_SERVER and NUBE_RUN_TOKEN must be set (the runtime runs inside an agent run's pod)")
	}
	tr := &http.Transport{TLSClientConfig: pki.ClientConfig(os.Getenv("NUBE_CA_HASH"), "", false)}
	return &controlPlane{base: base, token: token, http: &http.Client{Transport: tr, Timeout: 2 * time.Minute}}, nil
}

func (c *controlPlane) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if b, ok := body.(*bytes.Reader); ok {
			b.Seek(0, io.SeekStart)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err == nil {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode == 401 || resp.StatusCode == 403:
				return errRunOver
			case resp.StatusCode == 503 || resp.StatusCode == 502:
				lastErr = fmt.Errorf("%s", resp.Status) // leader election: retry
			case resp.StatusCode >= 300:
				return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
			default:
				if out != nil {
					return json.Unmarshal(data, out)
				}
				return nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return lastErr
}

// errRunOver: the run was cancelled or finished; the token no longer works.
var errRunOver = errors.New("the run is over (cancelled or finished)")

func (c *controlPlane) step(ctx context.Context, s api.AgentStep) {
	if err := c.do(ctx, "POST", "/api/v1/runtime/steps", s, nil); err != nil {
		log.Printf("record step: %v", err)
	}
}

func (c *controlPlane) finish(phase, output, msg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return c.do(ctx, "POST", "/api/v1/runtime/finish", map[string]string{"phase": phase, "output": output, "message": msg}, nil)
}

// approve asks a human and waits for the decision.
func (c *controlPlane) approve(ctx context.Context, tool, input string) (id string, ok bool, note string, err error) {
	var ap api.Approval
	if err := c.do(ctx, "POST", "/api/v1/runtime/approvals", map[string]string{"tool": tool, "input": input}, &ap); err != nil {
		return "", false, "", err
	}
	log.Printf("waiting for approval %s (tool %s)", ap.ID, tool)
	for {
		select {
		case <-ctx.Done():
			return ap.ID, false, "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
		var cur api.Approval
		if err := c.do(ctx, "GET", "/api/v1/runtime/approvals/"+url.PathEscape(ap.ID), nil, &cur); err != nil {
			return ap.ID, false, "", err
		}
		switch cur.Status {
		case "Approved", "Executed":
			return ap.ID, true, cur.Note, nil
		case "Rejected", "Expired", "Failed":
			return ap.ID, false, firstNonEmpty(cur.Note, strings.ToLower(cur.Status)), nil
		}
	}
}

// delegate runs a task on another agent and waits for its answer.
func (c *controlPlane) delegate(ctx context.Context, agent, task string) (string, error) {
	var child api.AgentRun
	if err := c.do(ctx, "POST", "/api/v1/runtime/subruns", map[string]string{"agent": agent, "input": task}, &child); err != nil {
		return "", err
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
		var cur api.AgentRun
		if err := c.do(ctx, "GET", "/api/v1/runtime/subruns/"+url.PathEscape(child.Metadata.Name), nil, &cur); err != nil {
			return "", err
		}
		switch cur.Status.Phase {
		case "Succeeded":
			return cur.Status.Output, nil
		case "Failed", "Cancelled":
			return "", fmt.Errorf("agent %s (run %s) %s: %s", agent, cur.Metadata.Name, strings.ToLower(cur.Status.Phase), cur.Status.Message)
		}
	}
}

// Run executes the run this pod was created for.
func Run(ctx context.Context) error {
	cp, err := newControlPlane()
	if err != nil {
		return err
	}
	var rr api.RuntimeRun
	if err := cp.do(ctx, "GET", "/api/v1/runtime/run", nil, &rr); err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	spec := rr.Agent
	timeout := time.Duration(spec.Limits.TimeoutSeconds)*time.Second - 5*time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log.Printf("run %s/%s: agent %s, model %s/%s", rr.Run.Metadata.Namespace, rr.Run.Metadata.Name, rr.Run.Spec.Agent, spec.Model.Provider, spec.Model.Name)

	tools, warnings := buildTools(ctx, cp, &rr)
	if len(warnings) > 0 {
		cp.step(ctx, api.AgentStep{Type: "tool", Tool: "setup", Error: true, Output: strings.Join(warnings, "\n")})
	}
	byName := map[string]*tool{}
	var defs []toolDef
	for i := range tools {
		if byName[tools[i].def.Name] != nil {
			continue
		}
		byName[tools[i].def.Name] = &tools[i]
		defs = append(defs, tools[i].def)
	}
	m, err := newModel(spec, defs, history{Memory: rr.Memory, Input: rr.Run.Spec.Input})
	if err != nil {
		return cp.finish("Failed", "", err.Error())
	}

	var results []toolResult
	var tokens int64
	lastText := ""
	for steps := 0; ; steps++ {
		if steps >= spec.Limits.MaxSteps {
			return cp.finish("Failed", lastText, fmt.Sprintf("step limit reached (%d model calls)", spec.Limits.MaxSteps))
		}
		if tokens >= spec.Limits.MaxTokens {
			return cp.finish("Failed", lastText, fmt.Sprintf("token budget used up (%d tokens)", tokens))
		}
		start := time.Now()
		rep, err := m.next(ctx, results)
		if err != nil {
			if ctx.Err() != nil {
				return cp.finish("Failed", lastText, "timed out")
			}
			return cp.finish("Failed", lastText, "model error: "+err.Error())
		}
		tokens += rep.In + rep.Out
		if rep.Text != "" {
			lastText = rep.Text
		}
		cp.step(ctx, api.AgentStep{Type: "model", Text: rep.Text, InputTokens: rep.In, OutputTokens: rep.Out,
			StopReason: rep.StopReason, DurationMs: time.Since(start).Milliseconds(), Time: start})
		if rep.StopReason == "refusal" {
			return cp.finish("Failed", rep.Text, "the model declined the request: "+firstNonEmpty(rep.Refusal, "refusal"))
		}
		if len(rep.Calls) == 0 {
			out := rep.Text
			if rep.StopReason == "max_tokens" {
				out += "\n\n(cut off at model.maxTokens)"
			}
			return cp.finish("Succeeded", out, "")
		}
		results = results[:0]
		for _, call := range rep.Calls {
			res := toolResult{ID: call.ID}
			step := api.AgentStep{Type: "tool", Tool: call.Name, Input: string(call.Input), Time: time.Now()}
			t := byName[call.Name]
			switch {
			case rep.StopReason == "max_tokens":
				// The call may be cut off mid-input: do not run it.
				res.Content, res.IsError = "your response hit max_tokens and this call may be incomplete; make it again, shorter", true
			case t == nil:
				res.Content, res.IsError = "unknown tool "+call.Name, true
			default:
				approved := true
				if t.approval {
					id, ok, note, err := cp.approve(ctx, call.Name, string(call.Input))
					step.Approval = id
					switch {
					case errors.Is(err, errRunOver):
						return nil
					case err != nil:
						res.Content, res.IsError, approved = "approval failed: "+err.Error(), true, false
					case !ok:
						res.Content, res.IsError, approved = "a human did not approve this call: "+note, true, false
					}
				}
				if approved {
					out, err := t.call(ctx, call.Input)
					if err != nil {
						res.Content, res.IsError = clip(err.Error(), maxToolOutput), true
					} else {
						res.Content = out
					}
				}
			}
			step.Output, step.Error, step.DurationMs = res.Content, res.IsError, time.Since(step.Time).Milliseconds()
			cp.step(ctx, step)
			results = append(results, res)
		}
		if ctx.Err() != nil {
			return cp.finish("Failed", lastText, "timed out")
		}
	}
}
