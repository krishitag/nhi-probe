package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/krishitag/nhi-probe/internal/event"
)

// Real messages cut from my lab capture (~/nhi-lab/github-raw.log, 15 Nov 2026).
// They contain no secrets. The server reply is shortened (icons removed).
const (
	realDiscoverRequest = `{"jsonrpc":"2.0","id":"server-discover-probe-1","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.325"}}}}`
	realDiscoverReply   = `{"jsonrpc":"2.0","id":"server-discover-probe-1","result":{"resultType":"complete","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"github-mcp-server","title":"GitHub MCP Server","version":"2.1.0"}}}}`
	oldInitialize       = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"old-client"}}}`
)

func TestNames(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantClient string
		wantServer string
	}{
		{"new MCP request", realDiscoverRequest, "claude-code", ""},
		{"new MCP reply", realDiscoverReply, "", "github-mcp-server"},
		{"old initialize", oldInitialize, "old-client", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m rpcMessage
			if err := json.Unmarshal([]byte(tt.line), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := m.clientName(); got != tt.wantClient {
				t.Errorf("clientName = %q, want %q", got, tt.wantClient)
			}
			if got := m.serverName(); got != tt.wantServer {
				t.Errorf("serverName = %q, want %q", got, tt.wantServer)
			}
		})
	}
}

func TestEnvCreds(t *testing.T) {
	fakePAT := "github_pat_" + strings.Repeat("a", 82)
	tests := []struct {
		name         string
		env          []string
		wantProvider string // "" means: expect no credential
	}{
		{"github token by pattern", []string{"GITHUB_PERSONAL_ACCESS_TOKEN=" + fakePAT}, "github"},
		{"unknown token by name", []string{"NOTION_API_KEY=abcdefgh12345678"}, "unknown"},
		{"secret name but too short", []string{"API_KEY=abc"}, ""},
		{"normal var", []string{"HOME=/Users/me"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := envCreds(tt.env)
			if tt.wantProvider == "" {
				if len(got) != 0 {
					t.Fatalf("got %d creds, want 0", len(got))
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d creds, want 1", len(got))
			}
			if got[0].Provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", got[0].Provider, tt.wantProvider)
			}
			if strings.Contains(got[0].Redacted, "aaaaaaaa") {
				t.Errorf("redacted form leaks the secret: %q", got[0].Redacted)
			}
		})
	}
}

// FuzzObserveRequest: any bytes from an agent must never crash the proxy, and a
// recorded event must never contain a secret that was in the input.
func FuzzObserveRequest(f *testing.F) {
	f.Add([]byte(realToolsCall))
	f.Add([]byte(realDiscoverRequest))
	f.Add([]byte(`{"method":"tools/call","params":{"name":"x","arguments":{"t":"ghp_` + strings.Repeat("a", 36) + `"}}}`))
	f.Add([]byte(`[{"method":"tools/call"}]`))
	f.Add([]byte(`{"id":`))
	f.Fuzz(func(t *testing.T, line []byte) {
		s := &kestrelShard{session: "fuzz"}
		e, ok := s.observeRequest(line)
		s.observeReply(line)
		if !ok {
			return
		}
		for _, c := range e.NHIs {
			if len(c.Redacted) > 11 { // "abcd...wxyz" is 11 chars; anything longer leaks
				t.Fatalf("redacted value too long: %q", c.Redacted)
			}
		}
	})
}

func TestMergeCredsDedupesAndSorts(t *testing.T) {
	a := event.Cred{FP: "bbb", Via: "env:X"}
	b := event.Cred{FP: "aaa", Via: "args"}
	got := mergeCreds([]event.Cred{a, b}, []event.Cred{a})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (duplicate should be dropped)", len(got))
	}
	if got[0].FP != "aaa" {
		t.Errorf("not sorted: first FP = %q, want aaa", got[0].FP)
	}
}
