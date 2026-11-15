package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWithTimestamp(t *testing.T) {
	in := `{
  "session_id": "abc",
  "hook_event_name": "PreToolUse",
  "tool_name": "mcp__github__list_issues"
}`
	got, err := withTimestamp([]byte(in), time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), "\n") != 1 {
		t.Errorf("want exactly one line, got %q", got)
	}
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if m["nhi_ts"] != "2026-11-15T01:00:00Z" || m["tool_name"] != "mcp__github__list_issues" {
		t.Errorf("wrong output: %v", m)
	}
}

func TestNotJSON(t *testing.T) {
	if _, err := withTimestamp([]byte("hello"), time.Now()); err == nil {
		t.Error("want an error for non-JSON input")
	}
}

// Many hooks writing at once must give whole, valid lines.
func TestConcurrentAppends(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hooks.jsonl")
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := strings.NewReader(`{"hook_event_name":"PreToolUse","tool_input":"` + strings.Repeat("x", 5000) + `"}`)
			if err := Run([]string{"--out", out}, in); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	data, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("line %d is broken JSON", i)
		}
	}
}
