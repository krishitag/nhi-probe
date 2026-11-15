package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/krishitag/nhi-probe/internal/event"
	"github.com/krishitag/nhi-probe/internal/fingerprint"
)

// Fixtures follow the exact shape of my real lab files (15 Nov 2026), cut down:
// hook lines from ~/nhi-lab/hooks.jsonl, proxy lines from ~/nhi-lab/github-events.jsonl.
// Fingerprints are of fake strings, so no real credential appears here.
var (
	fpGitHub    = fingerprint.Full("fake-github-pat")
	fpMessaging = fingerprint.Full("fake-messaging-token")
	fpNotion    = fingerprint.Full("fake-notion-token")
)

const hookFixture = `{"hook_event_name":"SessionStart","session_id":"c5088f4d","nhi_ts":"2026-11-15T01:00:00Z"}
{"hook_event_name":"PreToolUse","session_id":"c5088f4d","tool_name":"mcp__github__list_issues","tool_use_id":"toolu_01","nhi_ts":"2026-11-15T01:00:01Z"}
{"hook_event_name":"PreToolUse","session_id":"c5088f4d","tool_name":"Edit","tool_use_id":"toolu_xx","nhi_ts":"2026-11-15T01:00:30Z"}
{"hook_event_name":"PreToolUse","session_id":"c5088f4d","agent_id":"a164ad532f546080b","agent_type":"","tool_name":"mcp__github__list_issues","tool_use_id":"toolu_02","nhi_ts":"2026-11-15T01:01:00Z"}
{"hook_event_name":"SubagentStop","session_id":"c5088f4d","agent_id":"a164ad532f546080b","agent_type":"","nhi_ts":"2026-11-15T01:01:05Z"}
{"hook_event_name":"PreToolUse","session_id":"c5088f4d","tool_name":"mcp__notion__API-post-search","tool_use_id":"toolu_03","nhi_ts":"2026-11-15T01:02:00Z"}
not json at all
`

func proxyLine(session, span, ts string) string {
	e := event.Event{Source: "nhi-proxy", Session: session, Span: span, Agent: "claude-code",
		Kind: "tool_call", Target: "mcp:github/list_issues", NHIs: []event.Cred{
			{FP: fpGitHub, Provider: "github", Via: "env:GITHUB_PERSONAL_ACCESS_TOKEN", Redacted: "gith...AAAA"},
			{FP: fpMessaging, Provider: "unknown", Via: "env:CLAUDE_CODE_MESSAGING_TOKEN", Redacted: "abcd...1234"},
		}}
	b, _ := json.Marshal(e)
	return strings.Replace(string(b), `"ts":"0001-01-01T00:00:00Z"`, `"ts":"`+ts+`"`, 1)
}

func scanFixture() string {
	return `{"secrets":[
 {"fp":"` + fpGitHub + `","short":"` + fingerprint.Short(fpGitHub) + `","provider":"github","redacted":"gith...AAAA",
  "locations":[{"agent":"claude-code","server":"github","file":"~/.claude.json","field":"env:GITHUB_PERSONAL_ACCESS_TOKEN"}]},
 {"fp":"` + fpNotion + `","short":"` + fingerprint.Short(fpNotion) + `","provider":"notion","redacted":"ntn_...BBBB",
  "locations":[{"agent":"claude-code","server":"notion","file":"~/.claude.json","field":"env:NOTION_TOKEN"}]}]}`
}

// writeLab writes the fixture files and returns the trace flags for them.
func writeLab(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	hooks := filepath.Join(dir, "hooks.jsonl")
	proxy := filepath.Join(dir, "github-events.jsonl")
	scanF := filepath.Join(dir, "scan.json")
	os.WriteFile(hooks, []byte(hookFixture), 0o600)
	os.WriteFile(proxy, []byte(
		proxyLine("p1", "3", "2026-11-15T01:00:03Z")+"\n"+ // matches toolu_01 (2 s after the hook)
			proxyLine("p2", "2", "2026-11-15T01:01:02Z")+"\n"), 0o600) // matches toolu_02 (subagent)
	os.WriteFile(scanF, []byte(scanFixture()), 0o600)
	return []string{"--in", "hooks=" + hooks, "--in", "proxy=" + proxy, "--scan", scanF}
}

func runTrace(t *testing.T, ctx context.Context, args []string) (Report, []byte, string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "trace.json")
	var stdout bytes.Buffer
	err := Run(ctx, append(args, "--out", out, "--lineage"), &stdout)
	data, _ := os.ReadFile(out)
	var r Report
	if jerr := json.Unmarshal(data, &r); jerr != nil {
		t.Fatalf("report is not valid JSON: %v", jerr)
	}
	return r, data, stdout.String(), err
}

func TestTraceLab(t *testing.T) {
	defer goleak.VerifyNone(t)
	r, _, stdout, err := runTrace(t, context.Background(), writeLab(t))
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{ // NHI|agent|path -> must exist
		fpGitHub + "|claude-code|claude-code > mcp:github/list_issues":                                   true,
		fpGitHub + "|subagent:a164ad53|claude-code > subagent:a164ad53 > mcp:github/list_issues":         true,
		fpMessaging + "|subagent:a164ad53|claude-code > subagent:a164ad53 > mcp:github/list_issues":      true,
		fpNotion + "|claude-code|claude-code > mcp:notion/API-post-search":                               true, // hook-only, linked by scan
	}
	got := map[string]bool{}
	for _, e := range r.Edges {
		got[e.NHI+"|"+e.Agent+"|"+e.Path] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing edge %s", k)
		}
	}
	if r.Stats.MatchedCalls != 2 || r.Stats.HookOnlyCalls != 1 || r.Stats.HookOnlyLinked != 1 {
		t.Errorf("stats = %+v", r.Stats)
	}

	kinds := map[string]bool{}
	for _, rk := range r.Risks {
		kinds[rk.Kind+"|"+rk.NHI] = true
	}
	if !kinds["used-but-not-wired|"+fpMessaging] {
		t.Error("want used-but-not-wired risk for the inherited messaging token")
	}
	if kinds["unused|"+fpNotion] {
		t.Error("notion token IS used (hook-only, linked by scan), must not be 'unused'")
	}
	if !strings.Contains(stdout, "└─ subagent:a164ad53") {
		t.Errorf("lineage tree missing subagent step:\n%s", stdout)
	}
}

func TestTraceSameOutputForAnyWorkerCount(t *testing.T) {
	defer goleak.VerifyNone(t)
	args := writeLab(t)
	var first []byte
	for _, w := range []string{"1", "4", "8", "16"} {
		_, data, _, err := runTrace(t, context.Background(), append(args, "--workers", w))
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = data
		} else if !bytes.Equal(first, data) {
			t.Fatalf("workers=%s gave different output", w)
		}
	}
}

func TestTraceCancelWritesPartial(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _, _, err := runTrace(t, ctx, writeLab(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if !r.Partial {
		t.Error("want partial: true")
	}
}

func TestBuildChainStopsOnLoop(t *testing.T) {
	hops := map[string]event.Event{
		"a": {Span: "a", ParentSpan: "b", Agent: "x", Target: "y"},
		"b": {Span: "b", ParentSpan: "a", Agent: "y", Target: "x"},
	}
	chain := buildChain(event.Event{ParentSpan: "a", Agent: "y", Target: "mcp:t/t"}, hops)
	if len(chain) > maxChainDepth+2 {
		t.Fatalf("loop not stopped: %d steps", len(chain))
	}
}

func TestMatchWindow(t *testing.T) {
	tests := []struct {
		name      string
		proxyTS   string
		wantMatch bool
	}{
		{"1 s after hook", "2026-11-15T01:00:02Z", true},
		{"110 s after (slow approve)", "2026-11-15T01:01:51Z", true},
		{"3 min after", "2026-11-15T01:04:00Z", false},
		{"1 s before (clock jitter)", "2026-11-15T01:00:00Z", true},
		{"5 s before", "2026-11-15T00:59:56Z", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := `{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"mcp__github__list_issues","tool_use_id":"t1","nhi_ts":"2026-11-15T01:00:01Z"}`
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "h.jsonl"), []byte(hook+"\n"), 0o600)
			os.WriteFile(filepath.Join(dir, "p.jsonl"), []byte(proxyLine("p", "1", tt.proxyTS)+"\n"), 0o600)
			r, _, _, err := runTrace(t, context.Background(), []string{
				"--in", "hooks=" + filepath.Join(dir, "h.jsonl"), "--in", "proxy=" + filepath.Join(dir, "p.jsonl")})
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Stats.MatchedCalls == 1; got != tt.wantMatch {
				t.Errorf("matched = %v, want %v (stats %+v)", got, tt.wantMatch, r.Stats)
			}
		})
	}
}
