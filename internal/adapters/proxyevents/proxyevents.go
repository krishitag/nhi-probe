// Package proxyevents is the trace adapter for nhi-probe's own proxy event files.
// One proxy event (one tools/call, a list of credentials) becomes one trace
// event PER credential, because a trace event has a single "nhi".
package proxyevents

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/krishitag/nhi-probe/internal/event"
)

// Source is the "source" value of events written by the proxy.
const Source = "nhi-proxy"

// Adapter reads proxy JSONL files. It needs no state, so it's an empty struct.
type Adapter struct{}

func (Adapter) Name() string { return "proxy" }

// Parse reads one proxy event per line and emits one normalised event per credential.
// Broken lines are skipped (and counted) instead of stopping the whole trace.
func (Adapter) Parse(ctx context.Context, r io.Reader, emit func(event.Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024) // allow long lines (up to 16 MB)
	bad := 0
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var e event.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Kind != event.KindToolCall {
			bad++
			continue
		}
		for _, c := range e.NHIs {
			out := e // copy
			out.NHIs = nil
			out.NHI = c.FP
			out.Via = c.Via
			out.Redacted = c.Redacted
			if err := emit(out); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read proxy events: %w", err)
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "nhi-probe trace: skipped %d unreadable proxy lines\n", bad)
	}
	return nil
}
