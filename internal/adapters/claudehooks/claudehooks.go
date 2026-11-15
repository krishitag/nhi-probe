// Package claudehooks is the trace adapter for Claude Code hook events
// (the file written by `nhi-probe hook`). It is trace's local source.
//
// We use only two hook types:
//   - PreToolUse for MCP tools (tool_name "mcp__<server>__<tool>") -> a tool_call
//   - SubagentStop -> an agent_hop (main agent handed work to a subagent)
//
// Everything else (PostToolUse, Stop, prompts, non-MCP tools like Edit) is skipped:
// PostToolUse would count every call twice, and the rest never touches an MCP credential.
package claudehooks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/krishitag/nhi-probe/internal/event"
)

// Source is the "source" value of events from this adapter.
const Source = "claude-hooks"

// RootAgent is the name of the main Claude Code agent.
const RootAgent = "claude-code"

// hookLine holds only the hook fields we need.
type hookLine struct {
	TS        time.Time `json:"nhi_ts"` // added by `nhi-probe hook`
	Event     string    `json:"hook_event_name"`
	SessionID string    `json:"session_id"`
	ToolName  string    `json:"tool_name"`
	ToolUseID string    `json:"tool_use_id"`
	AgentID   string    `json:"agent_id"`   // set when the event happens inside a subagent
	AgentType string    `json:"agent_type"` // e.g. "general-purpose" (can be empty)
}

type Adapter struct{}

func (Adapter) Name() string { return "hooks" }

// subagentLabel names a subagent: its type if known, else the first 8 chars of its id.
func subagentLabel(h hookLine) string {
	if h.AgentType != "" {
		return "subagent:" + h.AgentType
	}
	id := h.AgentID
	if len(id) > 8 {
		id = id[:8]
	}
	return "subagent:" + id
}

// mcpTarget turns "mcp__github__list_issues" into "mcp:github/list_issues".
// Returns "" for non-MCP tools.
func mcpTarget(toolName string) string {
	rest, ok := strings.CutPrefix(toolName, "mcp__")
	if !ok {
		return ""
	}
	server, tool, ok := strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" {
		return ""
	}
	return "mcp:" + server + "/" + tool
}

func (Adapter) Parse(ctx context.Context, r io.Reader, emit func(event.Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024) // hook lines can hold big tool inputs
	bad := 0
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var h hookLine
		if err := json.Unmarshal(sc.Bytes(), &h); err != nil || h.SessionID == "" {
			bad++
			continue
		}
		e := event.Event{TS: h.TS, Source: Source, Session: h.SessionID}
		switch h.Event {
		case "PreToolUse":
			target := mcpTarget(h.ToolName)
			if target == "" {
				continue // not an MCP tool
			}
			e.Kind = event.KindToolCall
			e.Span = h.ToolUseID
			e.Target = target
			e.Agent = RootAgent
			if h.AgentID != "" { // the call happened inside a subagent
				e.Agent = subagentLabel(h)
				e.ParentSpan = "agent:" + h.AgentID
			}
		case "SubagentStop":
			if h.AgentID == "" {
				continue
			}
			e.Kind = event.KindAgentHop
			e.Span = "agent:" + h.AgentID
			e.Agent = RootAgent        // who delegated
			e.Target = subagentLabel(h) // who got the work
		default:
			continue
		}
		if err := emit(e); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read hook events: %w", err)
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "nhi-probe trace: skipped %d unreadable hook lines\n", bad)
	}
	return nil
}
