package api

import "time"

// ---------------------------------------------------------------- Agents

const (
	KindAgent    = "Agent"
	KindAgentRun = "AgentRun"

	// RoleRun is the identity of an agent run's runtime: it may only report
	// on its own run and use the tools its agent was given.
	RoleRun = "run"

	// RuntimeImage stands for the runtime image every node builds from its
	// own nube binary; NubeBinary for that binary on the host.
	RuntimeImage = "@nube"
	NubeBinary   = "@nube-binary"
)

// AgentModel chooses the LLM.
type AgentModel struct {
	Provider string `json:"provider,omitempty"` // anthropic (default) | openai (any OpenAI-compatible API: OpenAI, vLLM, Ollama ...)
	Name     string `json:"name,omitempty"`     // default claude-opus-5-5
	BaseURL  string `json:"baseURL,omitempty"`  // e.g. http://ollama:11434/v1 (cluster service names work)
	// APIKeySecret names a Secret whose keys become the runtime's environment;
	// the key is read from ANTHROPIC_API_KEY / OPENAI_API_KEY, or APIKeyKey.
	APIKeySecret string `json:"apiKeySecret,omitempty"`
	APIKeyKey    string `json:"apiKeyKey,omitempty"`
	MaxTokens    int    `json:"maxTokens,omitempty"` // per model response (default 16000)
	Effort       string `json:"effort,omitempty"`    // Claude: low | medium | high (default) | xhigh | max
	// Fallbacks (Claude, default on where supported) lets the API answer a
	// refused request with a fallback model.
	Fallbacks *bool `json:"fallbacks,omitempty"`
}

// HTTPTool calls a service in the cluster (or a URL) with the input as JSON.
type HTTPTool struct {
	Service string            `json:"service,omitempty"`
	Port    int               `json:"port,omitempty"`
	Path    string            `json:"path,omitempty"`
	URL     string            `json:"url,omitempty"`    // instead of service/port/path
	Method  string            `json:"method,omitempty"` // POST (default) | GET (input as query)
	Headers map[string]string `json:"headers,omitempty"`
}

// SandboxTool runs shell commands in a container next to the runtime, so
// commands the model writes never run in the runtime itself.
type SandboxTool struct {
	Image          string            `json:"image"`
	Workdir        string            `json:"workdir,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"` // per command (default 60)
	Env            map[string]string `json:"env,omitempty"`
}

// MCPTool offers the tools of an MCP server (streamable HTTP).
type MCPTool struct {
	Service string            `json:"service,omitempty"`
	Port    int               `json:"port,omitempty"`
	Path    string            `json:"path,omitempty"` // default /mcp
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Tools   []string          `json:"tools,omitempty"` // only these (default: all)
}

// NubeTool lets the agent operate this cluster through Nube's own MCP tools.
type NubeTool struct {
	Role       string   `json:"role,omitempty"`       // view (default) | agent (changes; destructive ones need approval)
	Namespaces []string `json:"namespaces,omitempty"` // default: the agent's namespace
	Tools      []string `json:"tools,omitempty"`      // only these (default: all the role allows)
}

// AgentRef delegates a task to another agent (a child run).
type AgentRef struct {
	Name string `json:"name"`
}

// AgentTool is one capability given to an agent. Exactly one kind is set.
type AgentTool struct {
	Name        string         `json:"name,omitempty"` // required for http, sandbox and agent tools
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"` // JSON schema of the input (http tools)
	HTTP        *HTTPTool      `json:"http,omitempty"`
	Sandbox     *SandboxTool   `json:"sandbox,omitempty"`
	MCP         *MCPTool       `json:"mcp,omitempty"`
	Nube        *NubeTool      `json:"nube,omitempty"`
	Agent       *AgentRef      `json:"agent,omitempty"`
	// RequireApproval holds every call of this tool until a human approves it.
	RequireApproval bool `json:"requireApproval,omitempty"`
}

// AgentMemory keeps a conversation per session across runs.
type AgentMemory struct {
	Enabled     bool `json:"enabled"`
	MaxMessages int  `json:"maxMessages,omitempty"` // default 40
}

// AgentLimits bound what one run may spend.
type AgentLimits struct {
	MaxSteps       int   `json:"maxSteps,omitempty"`       // model calls per run (default 25)
	MaxTokens      int64 `json:"maxTokens,omitempty"`      // input+output tokens per run (default 500000)
	TimeoutSeconds int   `json:"timeoutSeconds,omitempty"` // per run (default 900)
	Concurrency    int   `json:"concurrency,omitempty"`    // runs at once (default 4); more wait in the queue
}

// AgentSchedule starts runs on a cron schedule.
type AgentSchedule struct {
	Cron     string `json:"cron"`
	TimeZone string `json:"timeZone,omitempty"`
	Input    string `json:"input"`
	Session  string `json:"session,omitempty"`
}

type AgentSpec struct {
	Model        AgentModel        `json:"model"`
	Instructions string            `json:"instructions"`
	Tools        []AgentTool       `json:"tools,omitempty"`
	Memory       *AgentMemory      `json:"memory,omitempty"`
	Limits       AgentLimits       `json:"limits,omitempty"`
	Schedules    []AgentSchedule   `json:"schedules,omitempty"`
	Resources    ResourceRequest   `json:"resources,omitempty"` // of the runtime container
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

type AgentStatus struct {
	Phase       string               `json:"phase"` // Ready | Invalid
	Message     string               `json:"message,omitempty"`
	Running     int                  `json:"running"`
	Queued      int                  `json:"queued"`
	Succeeded   int                  `json:"succeeded"`
	Failed      int                  `json:"failed"`
	Tokens      int64                `json:"tokens"` // used by all runs
	LastRun     string               `json:"lastRun,omitempty"`
	LastRunAt   time.Time            `json:"lastRunAt,omitempty"`
	LastFiredAt map[string]time.Time `json:"lastFiredAt,omitempty"` // per schedule cron
}

// Agent is an AI agent the cluster runs: a model, instructions and tools.
// Each task is an AgentRun, executed in its own pod.
type Agent struct {
	Kind     string      `json:"kind"`
	Metadata Meta        `json:"metadata"`
	Spec     AgentSpec   `json:"spec"`
	Status   AgentStatus `json:"status"`
}

type AgentRunSpec struct {
	Agent   string `json:"agent"`
	Input   string `json:"input"`
	Session string `json:"session,omitempty"` // conversation memory key
	Parent  string `json:"parent,omitempty"`  // run that delegated this one
	Trigger string `json:"trigger,omitempty"` // api | schedule | agent
}

// AgentStep is one model call or tool call of a run (the trace).
type AgentStep struct {
	N            int       `json:"n"`
	Type         string    `json:"type"` // model | tool
	Time         time.Time `json:"time"`
	DurationMs   int64     `json:"durationMs"`
	Text         string    `json:"text,omitempty"` // model: what it said
	Tool         string    `json:"tool,omitempty"`
	Input        string    `json:"input,omitempty"`
	Output       string    `json:"output,omitempty"`
	Error        bool      `json:"error,omitempty"`
	Approval     string    `json:"approval,omitempty"`
	InputTokens  int64     `json:"inputTokens,omitempty"`
	OutputTokens int64     `json:"outputTokens,omitempty"`
	StopReason   string    `json:"stopReason,omitempty"`
}

type AgentUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

type AgentRunStatus struct {
	Phase      string      `json:"phase"` // Queued | Running | WaitingApproval | Succeeded | Failed | Cancelled
	Message    string      `json:"message,omitempty"`
	Output     string      `json:"output,omitempty"`
	Steps      []AgentStep `json:"steps,omitempty"`
	Usage      AgentUsage  `json:"usage"`
	Pod        string      `json:"pod,omitempty"`
	Approval   string      `json:"approval,omitempty"` // waiting for this one
	StartedAt  time.Time   `json:"startedAt,omitempty"`
	FinishedAt time.Time   `json:"finishedAt,omitempty"`
}

// AgentRun is one task given to an agent.
type AgentRun struct {
	Kind     string         `json:"kind"`
	Metadata Meta           `json:"metadata"`
	Spec     AgentRunSpec   `json:"spec"`
	Status   AgentRunStatus `json:"status"`
}

// Finished reports whether the run is over.
func (r *AgentRun) Finished() bool {
	switch r.Status.Phase {
	case "Succeeded", "Failed", "Cancelled":
		return true
	}
	return false
}

// MemoryMessage is one turn kept in an agent's session memory.
type MemoryMessage struct {
	Role string    `json:"role"` // user | assistant
	Text string    `json:"text"`
	Time time.Time `json:"time"`
	Run  string    `json:"run,omitempty"`
}

// RuntimeRun is what the runtime of a run receives.
type RuntimeRun struct {
	Run    AgentRun        `json:"run"`
	Agent  AgentSpec       `json:"agent"`
	Memory []MemoryMessage `json:"memory,omitempty"`
}
