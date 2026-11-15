// Package event defines the one event shape that every part of nhi-probe uses.
package event

import "time"

// Event is one thing that happened, e.g. one tool call.
// The json tags give the field names used in the output file.
// "omitempty" leaves a field out when it is empty.
type Event struct {
	TS         time.Time `json:"ts"`
	Source     string    `json:"source"`                // who recorded it, e.g. "nhi-proxy"
	Session    string    `json:"session"`               // which agent session
	Span       string    `json:"span,omitempty"`        // id of this step
	ParentSpan string    `json:"parent_span,omitempty"` // id of the step that caused it
	Agent      string    `json:"agent"`                 // e.g. "claude-code"
	Kind       string    `json:"kind"`                  // e.g. "tool_call"
	Target     string    `json:"target"`                // e.g. "mcp:github-mcp-server/issue_write"
	NHI        string    `json:"nhi,omitempty"`         // fingerprint of the credential used (trace events: one each)
	NHIs       []Cred    `json:"nhis,omitempty"`        // proxy events: every credential on this call
	Via        string    `json:"via,omitempty"`         // trace events: where the credential was seen (env:X, args, header:X, iam-role)
	Redacted   string    `json:"redacted,omitempty"`    // trace events: first 4 + "..." + last 4, for display
}

// Kinds of events.
const (
	KindToolCall = "tool_call" // an agent called a tool
	KindAgentHop = "agent_hop" // an agent handed work to another agent (subagent, collaborator)
)

// Cred is one credential seen on a call. Never holds the secret itself.
type Cred struct {
	FP       string `json:"fp"`       // full SHA-256 fingerprint
	Provider string `json:"provider"` // e.g. "github" or "unknown"
	Via      string `json:"via"`      // where we saw it, e.g. "env:GITHUB_PERSONAL_ACCESS_TOKEN" or "args"
	Redacted string `json:"redacted"` // first 4 + "..." + last 4
}
