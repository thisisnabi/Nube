// Command mockllm is a test stand-in for the Claude Messages API: on the
// first turn it calls every tool it knows how to use (shell, nube_diagnose,
// nube_list_resources), then answers with what the tools returned. It lets
// the e2e cluster run real agent runs without an API key.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

func main() {
	http.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string           `json:"model"`
			Tools    []map[string]any `json:"tools"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		last := req.Messages[len(req.Messages)-1]
		var blocks []map[string]any
		json.Unmarshal(last.Content, &blocks)
		var results []string
		for _, b := range blocks {
			if b["type"] == "tool_result" {
				results = append(results, fmt.Sprint(b["content"]))
			}
		}
		resp := map[string]any{"id": "msg_mock", "type": "message", "role": "assistant", "model": req.Model,
			"usage": map[string]any{"input_tokens": 120 * len(req.Messages), "output_tokens": 30}}
		if len(results) == 0 {
			var content []any
			content = append(content, map[string]any{"type": "text", "text": fmt.Sprintf("I have %d tools; using them.", len(req.Tools))})
			for _, t := range req.Tools {
				switch t["name"] {
				case "shell":
					content = append(content, map[string]any{"type": "tool_use", "id": "call_shell", "name": "shell", "input": map[string]any{"command": "echo answer=$((6*7)); hostname; cat /etc/os-release | head -1"}})
				case "nube_diagnose":
					content = append(content, map[string]any{"type": "tool_use", "id": "call_diag", "name": "nube_diagnose", "input": map[string]any{}})
				case "nube_list_resources":
					content = append(content, map[string]any{"type": "tool_use", "id": "call_list", "name": "nube_list_resources", "input": map[string]any{"kind": "deployment"}})
				}
			}
			resp["content"], resp["stop_reason"] = content, "tool_use"
			if len(content) == 1 {
				resp["stop_reason"] = "end_turn"
				content[0] = map[string]any{"type": "text", "text": "No tools; hello from the mock model. You said: " + firstText(req.Messages[len(req.Messages)-1].Content)}
			}
		} else {
			var sum []string
			for _, r := range results {
				sum = append(sum, strings.ReplaceAll(clip(r, 160), "\n", " | "))
			}
			resp["content"] = []any{map[string]any{"type": "text", "text": "Tool results:\n- " + strings.Join(sum, "\n- ")}}
			resp["stop_reason"] = "end_turn"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	log.Println("mock Messages API on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func firstText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	json.Unmarshal(raw, &blocks)
	for _, b := range blocks {
		if b["type"] == "text" {
			return fmt.Sprint(b["text"])
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
