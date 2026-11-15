// Package adktrace is the trace adapter for Google ADK event logs (the cloud agent).
// Each line is {"session_id": ..., "event": <ADK Event as JSON>}, written by lab/adk_lab.py.
//
// From each ADK event we take:
//   - function_call parts (except transfer_to_agent) -> tool_call by that event's author
//   - actions.transfer_to_agent                        -> agent_hop (author hands work to another agent)
//
// Span ids: a tool call uses the function_call id. An agent's "hop span" is
// "adk-agent:<invocation>:<agent>", so a tool call by "coder" points to the hop
// that transferred work to coder in the same invocation.
package adktrace

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/krishitag/nhi-probe/internal/event"
)

const Source = "adk-trace"

type functionCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type adkLine struct {
	SessionID string `json:"session_id"`
	Event     struct {
		ID           string  `json:"id"`
		InvocationID string  `json:"invocation_id"`
		Author       string  `json:"author"`
		Timestamp    float64 `json:"timestamp"` // seconds since 1970, with fractions
		Content      *struct {
			Parts []struct {
				FunctionCall *functionCall `json:"function_call"`
			} `json:"parts"`
		} `json:"content"`
		Actions *struct {
			TransferToAgent string `json:"transfer_to_agent"`
		} `json:"actions"`
	} `json:"event"`
}

type Adapter struct{}

func (Adapter) Name() string { return "adk" }

func hopSpan(invocation, agent string) string { return "adk-agent:" + invocation + ":" + agent }

func toTime(sec float64) time.Time {
	whole, frac := math.Modf(sec)
	return time.Unix(int64(whole), int64(frac*1e9)).UTC()
}

func (Adapter) Parse(ctx context.Context, r io.Reader, emit func(event.Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	bad := 0
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var l adkLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil || l.SessionID == "" || l.Event.Author == "" {
			bad++
			continue
		}
		ev := l.Event
		base := event.Event{TS: toTime(ev.Timestamp), Source: Source, Session: l.SessionID}

		if ev.Content != nil {
			for i, p := range ev.Content.Parts {
				fc := p.FunctionCall
				if fc == nil || fc.Name == "transfer_to_agent" {
					continue
				}
				e := base
				e.Kind = event.KindToolCall
				e.Span = fc.ID
				if e.Span == "" {
					e.Span = fmt.Sprintf("%s#%d", ev.ID, i)
				}
				e.Agent = "adk:" + ev.Author
				e.ParentSpan = hopSpan(ev.InvocationID, ev.Author)
				e.Target = "tool:" + fc.Name
				if err := emit(e); err != nil {
					return err
				}
			}
		}
		if ev.Actions != nil && ev.Actions.TransferToAgent != "" {
			to := ev.Actions.TransferToAgent
			e := base
			e.Kind = event.KindAgentHop
			e.Span = hopSpan(ev.InvocationID, to)
			e.ParentSpan = hopSpan(ev.InvocationID, ev.Author) // supports supervisor > a > b
			e.Agent = "adk:" + ev.Author
			e.Target = "adk:" + to
			if err := emit(e); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read adk trace: %w", err)
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "nhi-probe trace: skipped %d unreadable adk lines\n", bad)
	}
	return nil
}
