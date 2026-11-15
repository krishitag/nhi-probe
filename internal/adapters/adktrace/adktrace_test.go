package adktrace

import (
	"context"
	"strings"
	"testing"

	"github.com/krishitag/nhi-probe/internal/event"
)

// Shape of ADK events as written by lab/adk_lab.py (model_dump_json, snake_case).
// TODO after the first real run: replace with lines cut from ~/nhi-lab/adk-trace.jsonl.
const fixture = `{"session_id":"s1","event":{"id":"e1","invocation_id":"inv1","author":"user","timestamp":1763168400.0,"content":{"role":"user","parts":[{"text":"List the open issues"}]}}}
{"session_id":"s1","event":{"id":"e2","invocation_id":"inv1","author":"supervisor","timestamp":1763168401.5,"content":{"role":"model","parts":[{"function_call":{"id":"fc1","name":"transfer_to_agent","args":{"agent_name":"coder"}}}]},"actions":{"transfer_to_agent":"coder"}}}
{"session_id":"s1","event":{"id":"e3","invocation_id":"inv1","author":"coder","timestamp":1763168403.25,"content":{"role":"model","parts":[{"function_call":{"id":"fc2","name":"github_list_issues","args":{"owner":"krishitag","repo":"nhi-lab-sandbox"}}}]}}}
broken line
`

func TestParse(t *testing.T) {
	var got []event.Event
	err := Adapter{}.Parse(context.Background(), strings.NewReader(fixture), func(e event.Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events (1 hop + 1 tool call), got %d: %+v", len(got), got)
	}
	hop, call := got[0], got[1]
	if hop.Kind != event.KindAgentHop || hop.Agent != "adk:supervisor" || hop.Target != "adk:coder" {
		t.Errorf("bad hop: %+v", hop)
	}
	if call.Kind != event.KindToolCall || call.Agent != "adk:coder" || call.Target != "tool:github_list_issues" {
		t.Errorf("bad call: %+v", call)
	}
	if call.ParentSpan != hop.Span {
		t.Errorf("call parent %q should be the hop span %q", call.ParentSpan, hop.Span)
	}
	if call.TS.UnixMilli() != 1763168403250 {
		t.Errorf("timestamp fraction lost: %v", call.TS)
	}
}
