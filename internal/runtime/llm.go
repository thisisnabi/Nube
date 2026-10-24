// Package runtime executes one AgentRun: the agent loop (model calls and
// tool calls), with the trace, budgets and approvals reported to the
// control plane. It runs as "nube agent-run" inside the run's pod.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/thisisnabi/Nube/internal/api"
)

// toolDef is a tool as the model sees it.
type toolDef struct {
	Name        string
	Description string
	Schema      map[string]any // JSON schema (type object)
}

type toolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type toolResult struct {
	ID      string
	Content string
	IsError bool
}

// reply is one model response.
type reply struct {
	Text       string
	Calls      []toolCall
	StopReason string // end_turn | tool_use | max_tokens | refusal | ...
	Refusal    string // why, when StopReason is refusal
	In, Out    int64  // tokens
}

// model is a conversation with an LLM. next sends the results of the
// previous turn's tool calls (none on the first turn) and returns the reply.
type model interface {
	next(ctx context.Context, results []toolResult) (*reply, error)
}

// history is earlier turns of the session (memory) and the new input.
type history struct {
	Memory []api.MemoryMessage
	Input  string
}

func apiKey(m api.AgentModel) string {
	name := m.APIKeyKey
	if name == "" {
		name = map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY"}[m.Provider]
	}
	return os.Getenv(name)
}

func newModel(spec api.AgentSpec, tools []toolDef, h history) (model, error) {
	key := apiKey(spec.Model)
	switch spec.Model.Provider {
	case "", "anthropic":
		if key == "" && spec.Model.BaseURL == "" {
			return nil, fmt.Errorf("no API key: the Secret %q needs ANTHROPIC_API_KEY (or set model.apiKeyKey)", spec.Model.APIKeySecret)
		}
		return newClaude(spec, key, tools, h), nil
	case "openai":
		return newOpenAI(spec, key, tools, h), nil
	}
	return nil, fmt.Errorf("unknown model provider %q", spec.Model.Provider)
}

// ---------------------------------------------------------------- Claude

// fallbackModels accept the server-side refusal fallback ("fallbacks: default").
var fallbackModels = map[string]bool{"claude-opus-5-5": true, "claude-fable-5-1": true, "claude-opus-5": true, "claude-sonnet-5-5": true}

type claude struct {
	client   anthropic.Client
	params   anthropic.MessageNewParams
	opts     []option.RequestOption
	messages []anthropic.MessageParam
}

func newClaude(spec api.AgentSpec, key string, tools []toolDef, h history) *claude {
	opts := []option.RequestOption{option.WithMaxRetries(3), option.WithRequestTimeout(10 * time.Minute)}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	if spec.Model.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(spec.Model.BaseURL))
	}
	c := &claude{client: anthropic.NewClient(opts...)}
	maxTokens := int64(spec.Model.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	effort := spec.Model.Effort
	if effort == "" {
		effort = "high" // agentic work; Claude Opus 5.5 would default to medium
	}
	c.params = anthropic.MessageNewParams{
		Model:        anthropic.Model(spec.Model.Name),
		MaxTokens:    maxTokens,
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(effort)},
	}
	if spec.Instructions != "" {
		// Tools + system form a stable prefix: cache it across the loop's calls.
		c.params.System = []anthropic.TextBlockParam{{Text: spec.Instructions, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
	}
	for _, t := range tools {
		schema := anthropic.ToolInputSchemaParam{Properties: t.Schema["properties"]}
		if req, ok := t.Schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					schema.Required = append(schema.Required, s)
				}
			}
		}
		tp := anthropic.ToolParam{Name: t.Name, Description: anthropic.String(t.Description), InputSchema: schema}
		c.params.Tools = append(c.params.Tools, anthropic.ToolUnionParam{OfTool: &tp})
	}
	if spec.Model.BaseURL == "" && fallbackModels[spec.Model.Name] && (spec.Model.Fallbacks == nil || *spec.Model.Fallbacks) {
		// A refused request is answered by a fallback model in the same call.
		c.opts = append(c.opts, option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"), option.WithJSONSet("fallbacks", "default"))
	}
	for _, m := range h.Memory {
		if m.Role == "assistant" {
			c.messages = append(c.messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Text)))
		} else {
			c.messages = append(c.messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)))
		}
	}
	c.messages = append(c.messages, anthropic.NewUserMessage(anthropic.NewTextBlock(h.Input)))
	return c
}

func (c *claude) next(ctx context.Context, results []toolResult) (*reply, error) {
	if len(results) > 0 {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(results))
		for _, r := range results {
			blocks = append(blocks, anthropic.NewToolResultBlock(r.ID, r.Content, r.IsError))
		}
		c.messages = append(c.messages, anthropic.NewUserMessage(blocks...)) // all results in one turn
	}
	p := c.params
	p.Messages = c.messages
	resp, err := c.client.Messages.New(ctx, p, c.opts...)
	if err != nil {
		return nil, err
	}
	// Append the whole response (thinking blocks included): the history stays append-only.
	c.messages = append(c.messages, resp.ToParam())
	out := &reply{StopReason: string(resp.StopReason), In: resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens, Out: resp.Usage.OutputTokens}
	if resp.StopReason == anthropic.StopReasonRefusal {
		out.Refusal = strings.TrimSpace(string(resp.StopDetails.Category) + " " + resp.StopDetails.Explanation)
	}
	var text []string
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			text = append(text, b.Text)
		case anthropic.ToolUseBlock:
			out.Calls = append(out.Calls, toolCall{ID: b.ID, Name: b.Name, Input: json.RawMessage(b.JSON.Input.Raw())})
		}
	}
	out.Text = strings.TrimSpace(strings.Join(text, "\n"))
	return out, nil
}

// ---------------------------------------------------------------- OpenAI-compatible

// openAI speaks the Chat Completions API that OpenAI, vLLM, Ollama, LM Studio
// and most local model servers offer.
type openAI struct {
	base, key string
	body      map[string]any
	messages  []map[string]any
	http      *http.Client
}

func newOpenAI(spec api.AgentSpec, key string, tools []toolDef, h history) *openAI {
	base := strings.TrimRight(spec.Model.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	o := &openAI{base: base, key: key, http: &http.Client{Timeout: 10 * time.Minute}}
	o.body = map[string]any{"model": spec.Model.Name}
	if spec.Model.MaxTokens > 0 {
		o.body["max_tokens"] = spec.Model.MaxTokens
	}
	if len(tools) > 0 {
		var ts []map[string]any
		for _, t := range tools {
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}})
		}
		o.body["tools"] = ts
	}
	if spec.Instructions != "" {
		o.messages = append(o.messages, map[string]any{"role": "system", "content": spec.Instructions})
	}
	for _, m := range h.Memory {
		o.messages = append(o.messages, map[string]any{"role": m.Role, "content": m.Text})
	}
	o.messages = append(o.messages, map[string]any{"role": "user", "content": h.Input})
	return o
}

func (o *openAI) next(ctx context.Context, results []toolResult) (*reply, error) {
	for _, r := range results {
		content := r.Content
		if r.IsError {
			content = "ERROR: " + content
		}
		o.messages = append(o.messages, map[string]any{"role": "tool", "tool_call_id": r.ID, "content": content})
	}
	body := map[string]any{}
	for k, v := range o.body {
		body[k] = v
	}
	body["messages"] = o.messages
	b, _ := json.Marshal(body)
	var resp struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", o.base+"/chat/completions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if o.key != "" {
			req.Header.Set("Authorization", "Bearer "+o.key)
		}
		res, err := o.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			data, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
			res.Body.Close()
			switch {
			case res.StatusCode == 429 || res.StatusCode >= 500:
				lastErr = fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(data)))
			case res.StatusCode >= 300:
				return nil, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(data)))
			default:
				if err := json.Unmarshal(data, &resp); err != nil {
					return nil, fmt.Errorf("decode response: %w", err)
				}
				lastErr = nil
			}
		}
		if lastErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("the model returned no choices")
	}
	ch := resp.Choices[0]
	out := &reply{In: resp.Usage.PromptTokens, Out: resp.Usage.CompletionTokens}
	msg := map[string]any{"role": "assistant"}
	if ch.Message.Content != nil {
		out.Text = strings.TrimSpace(*ch.Message.Content)
		msg["content"] = *ch.Message.Content
	}
	if len(ch.Message.ToolCalls) > 0 {
		var calls []map[string]any
		for _, tc := range ch.Message.ToolCalls {
			args := tc.Function.Arguments
			if args == "" {
				args = "{}"
			}
			out.Calls = append(out.Calls, toolCall{ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args)})
			calls = append(calls, map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Function.Name, "arguments": args}})
		}
		msg["tool_calls"] = calls
	}
	o.messages = append(o.messages, msg)
	switch {
	case len(out.Calls) > 0:
		out.StopReason = "tool_use"
	case ch.FinishReason == "length":
		out.StopReason = "max_tokens"
	case ch.FinishReason == "content_filter":
		out.StopReason, out.Refusal = "refusal", "content filter"
	default:
		out.StopReason = "end_turn"
	}
	return out, nil
}
