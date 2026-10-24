package api

import "time"

// RoleAgent is for AI agents and automation: like edit, but secret values
// stay hidden and destructive changes wait for a human's approval.
const RoleAgent = "agent"

// Approval is a request held back until a human approves it: a destructive
// API call by an agent token, or a tool call of an AgentRun.
type Approval struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace,omitempty"`
	Requester string `json:"requester"` // token name, or agentrun/NAME
	// RequesterNamespaces limits the replayed call like the requester's token.
	RequesterNamespaces []string  `json:"requesterNamespaces,omitempty"`
	Summary             string    `json:"summary"` // what will happen, in one line
	Reason              string    `json:"reason"`  // why it needs approval
	Status              string    `json:"status"`  // Pending | Approved | Rejected | Executed | Failed | Expired
	CreatedAt           time.Time `json:"createdAt"`
	DecidedBy           string    `json:"decidedBy,omitempty"`
	DecidedAt           time.Time `json:"decidedAt,omitempty"`
	Note                string    `json:"note,omitempty"` // the approver's comment

	// For API calls: the request, replayed on approval with the requester's rights.
	Method      string `json:"method,omitempty"`
	Path        string `json:"path,omitempty"`
	Body        []byte `json:"body,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Result      string `json:"result,omitempty"` // response of the replayed call
	ResultCode  int    `json:"resultCode,omitempty"`

	// For agent tool calls.
	Run   string `json:"run,omitempty"`
	Tool  string `json:"tool,omitempty"`
	Input string `json:"input,omitempty"`
}

// AuditEntry records one change made through the API.
type AuditEntry struct {
	Seq      int64     `json:"seq"`
	Time     time.Time `json:"time"`
	Who      string    `json:"who"`
	Role     string    `json:"role"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Status   int       `json:"status"`
	Summary  string    `json:"summary,omitempty"`
	Approval string    `json:"approval,omitempty"` // set when the call ran after an approval
}
