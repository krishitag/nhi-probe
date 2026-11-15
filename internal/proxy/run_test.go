package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/krishitag/nhi-probe/internal/event"
)

// The "server" in these tests is `cat`: it sends back whatever it gets.
// That tests the transport (forwarding, events, shutdown), not MCP itself.
// The tools/call has the same shape as my real list_issues call.
const realToolsCall = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_issues","arguments":{"owner":"krishitag","repo":"nhi-lab-sandbox"}}}`

func TestRunForwardsAndRecords(t *testing.T) {
	defer goleak.VerifyNone(t) // fails if any goroutine is left running

	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	inR, inW := io.Pipe()
	var out bytes.Buffer

	errc := make(chan error, 1)
	go func() {
		errc <- Run(context.Background(), []string{"--events", eventsPath, "cat"}, inR, &out)
	}()

	io.WriteString(inW, realDiscoverRequest+"\n")
	io.WriteString(inW, realToolsCall+"\n")
	inW.Close() // agent hangs up -> cat ends -> Run returns

	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1. Bytes forwarded unchanged (cat sent them back to "the agent").
	if want := realDiscoverRequest + "\n" + realToolsCall + "\n"; out.String() != want {
		t.Errorf("output changed:\n got %q\nwant %q", out.String(), want)
	}

	// 2. Exactly one event, for the one tools/call.
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d events, want 1", len(lines))
	}
	var e event.Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("event is not valid JSON: %v", err)
	}
	if e.Agent != "claude-code" || e.Target != "mcp:/list_issues" || e.Span != "3" {
		t.Errorf("wrong event: %+v", e)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	defer goleak.VerifyNone(t) // runs LAST (defers run in reverse order)

	inR, inW := io.Pipe()
	defer inW.Close() // runs FIRST: frees the agent->server goroutine stuck reading

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
		errc <- Run(ctx, []string{"--events", eventsPath, "cat"}, inR, io.Discard)
	}()

	time.Sleep(200 * time.Millisecond) // let cat start
	start := time.Now()
	cancel() // like pressing Ctrl-C

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("took %v to stop, want under 2s", took)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
