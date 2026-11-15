package proxyevents

import (
	"context"
	"strings"
	"testing"

	"github.com/krishitag/nhi-probe/internal/event"
)

// Same shape as captures/notion-events.jsonl (fingerprints shortened).
func TestParseSplitsCredentials(t *testing.T) {
	in := `{"ts":"2026-11-14T19:40:26Z","source":"nhi-proxy","session":"7991","span":"2","agent":"claude-code","kind":"tool_call","target":"mcp:notion/API-post-search","nhis":[{"fp":"124b","provider":"unknown","via":"env:CLAUDE_CODE_MESSAGING_TOKEN","redacted":"cf39...7804"},{"fp":"2c64","provider":"notion","via":"env:NOTION_TOKEN","redacted":"ntn_...y95v"}]}
{"ts":"2026-11-14T19:41:00Z","source":"nhi-proxy","session":"7991","span":"3","agent":"claude-code","kind":"tool_call","target":"mcp:notion/x"}
not json
`
	var got []event.Event
	if err := (Adapter{}).Parse(context.Background(), strings.NewReader(in), func(e event.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events (one per credential; a call with none gives none), got %d", len(got))
	}
	if got[0].NHI != "124b" || got[0].Via != "env:CLAUDE_CODE_MESSAGING_TOKEN" || got[1].NHI != "2c64" || got[1].Redacted != "ntn_...y95v" {
		t.Errorf("wrong split: %+v", got)
	}
	if got[0].Span != got[1].Span || got[0].NHIs != nil {
		t.Errorf("both must keep the same span and drop the list: %+v", got)
	}
}

func TestParseStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Adapter{}.Parse(ctx, strings.NewReader(`{"kind":"tool_call"}`+"\n"), func(event.Event) error { return nil })
	if err == nil {
		t.Error("want an error after cancel")
	}
}
