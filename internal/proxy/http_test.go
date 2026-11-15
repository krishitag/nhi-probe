package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/krishitag/nhi-probe/internal/event"
)

// fakeUpstream behaves like a Streamable HTTP MCP server: discover -> JSON + session id,
// tools/call -> SSE stream. (A transport test, not agent data.)
func fakeUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(`"server/discover"`)):
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, realDiscoverReply+"\n")
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"content\":[]}}\n\n")
		}
	}))
}

func readEvents(t *testing.T, buf *bytes.Buffer) []event.Event {
	t.Helper()
	var out []event.Event
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var e event.Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("bad event line: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestHTTPProxyForwardsAndRecords(t *testing.T) {
	defer goleak.VerifyNone(t)
	up := fakeUpstream()
	defer up.Close()

	var buf bytes.Buffer
	events := newSink(&buf, 16)
	go events.run()
	p, err := newHTTPProxy(context.Background(), up.URL, "github-remote", events, nil)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()
	defer p.client.CloseIdleConnections()

	fakePAT := "ghp_" + strings.Repeat("q", 36)
	post := func(body, session string) *http.Response {
		req, _ := http.NewRequest("POST", front.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+fakePAT)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// 1. discover: no session yet, server gives one back
	resp := post(realDiscoverRequest, "")
	io.ReadAll(resp.Body)
	resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid != "sess-1" {
		t.Fatalf("session id not passed back: %q", sid)
	}

	// 2. tools/call in that session: SSE answer must arrive unchanged
	resp = post(realToolsCall, sid)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(got), `data: {"jsonrpc":"2.0","id":3`) {
		t.Errorf("SSE answer changed: %q", got)
	}
	front.Client().CloseIdleConnections()
	events.stop()

	evs := readEvents(t, &buf)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	e := evs[0]
	if e.Session != "sess-1" || e.Agent != "claude-code" || e.Target != "mcp:github-remote/list_issues" {
		t.Errorf("wrong event: %+v", e)
	}
	if len(e.NHIs) != 1 || e.NHIs[0].Via != "header:Authorization" || e.NHIs[0].Provider != "github" {
		t.Errorf("header credential not found: %+v", e.NHIs)
	}
	if strings.Contains(buf.String(), strings.Repeat("q", 36)) {
		t.Fatal("full token written to events!")
	}
}

// 200 sessions at once: run with -race. Sharded sessions, one writer, no lost events.
func TestHTTPProxyConcurrentSessions(t *testing.T) {
	defer goleak.VerifyNone(t)
	up := fakeUpstream()
	defer up.Close()

	var buf bytes.Buffer
	events := newSink(&buf, 8) // small buffer on purpose: forces backpressure
	go events.run()
	p, _ := newHTTPProxy(context.Background(), up.URL, "everything", events, nil)
	front := httptest.NewServer(p)
	defer front.Close()
	defer p.client.CloseIdleConnections()

	const n = 200
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", front.URL, strings.NewReader(realToolsCall))
			req.Header.Set("Mcp-Session-Id", fmt.Sprintf("s-%d", i))
			resp, err := front.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.ReadAll(resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	front.Client().CloseIdleConnections()
	events.stop()

	if got := len(readEvents(t, &buf)); got != n {
		t.Fatalf("got %d events, want %d", got, n)
	}
}

func TestSplitBatch(t *testing.T) {
	tests := []struct {
		body string
		want int
	}{
		{`{"method":"a"}`, 1},
		{`[{"method":"a"},{"method":"b"}]`, 2},
		{`[broken`, 1},
		{``, 1},
	}
	for _, tt := range tests {
		if got := len(splitBatch([]byte(tt.body))); got != tt.want {
			t.Errorf("splitBatch(%q) = %d parts, want %d", tt.body, got, tt.want)
		}
	}
}
