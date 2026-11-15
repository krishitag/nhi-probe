package claudehooks

import (
	"context"
	"strings"
	"testing"

	"github.com/krishitag/nhi-probe/internal/event"
)

func TestMCPTarget(t *testing.T) {
	tests := []struct{ in, want string }{
		{"mcp__github__list_issues", "mcp:github/list_issues"},
		{"mcp__notion__API-post-search", "mcp:notion/API-post-search"},
		{"mcp__claude-vscode__getDiagnostics", "mcp:claude-vscode/getDiagnostics"},
		{"Edit", ""},
		{"mcp__github", ""},
		{"mcp____x", ""},
	}
	for _, tt := range tests {
		if got := mcpTarget(tt.in); got != tt.want {
			t.Errorf("mcpTarget(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Lines in the exact shape of captures/hooks.jsonl.
func TestParse(t *testing.T) {
	in := `{"hook_event_name": "PreToolUse", "nhi_ts": "2026-11-15T01:00:01Z", "session_id": "s", "tool_name": "mcp__github__list_issues", "tool_use_id": "t1"}
{"hook_event_name": "PostToolUse", "nhi_ts": "2026-11-15T01:00:02Z", "session_id": "s", "tool_name": "mcp__github__list_issues", "tool_use_id": "t1"}
{"agent_id": "a164ad532f546080b", "agent_type": "", "hook_event_name": "PreToolUse", "nhi_ts": "2026-11-15T01:01:00Z", "session_id": "s", "tool_name": "mcp__notion__API-post-search", "tool_use_id": "t2"}
{"agent_id": "a164ad532f546080b", "agent_type": "general-purpose", "hook_event_name": "SubagentStop", "nhi_ts": "2026-11-15T01:01:05Z", "session_id": "s"}
{"hook_event_name": "Stop", "nhi_ts": "2026-11-15T01:02:00Z", "session_id": "s"}
{"broken"
`
	var got []event.Event
	err := Adapter{}.Parse(context.Background(), strings.NewReader(in), func(e event.Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events (2 MCP PreToolUse + 1 SubagentStop), got %d: %+v", len(got), got)
	}
	if got[0].Agent != RootAgent || got[0].ParentSpan != "" || got[0].Target != "mcp:github/list_issues" {
		t.Errorf("main-agent call wrong: %+v", got[0])
	}
	if got[1].Agent != "subagent:a164ad53" || got[1].ParentSpan != "agent:a164ad532f546080b" {
		t.Errorf("subagent call wrong: %+v", got[1])
	}
	if got[2].Kind != event.KindAgentHop || got[2].Span != got[1].ParentSpan || got[2].Target != "subagent:general-purpose" {
		t.Errorf("hop wrong: %+v", got[2])
	}
}
