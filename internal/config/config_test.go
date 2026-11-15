package config

import (
	"strings"
	"testing"
)

// Same shape as my real ~/.claude.json (cut down, fake token).
func TestClaudeParse(t *testing.T) {
	fake := "github_pat_" + strings.Repeat("a", 82)
	data := `{"numStartups": 5, "projects": {"/Users/krishita/Desktop/nhi-probe": {"mcpServers": {
		"github": {"type": "stdio", "command": "/Users/krishita/Desktop/nhi-probe/nhi-probe",
		  "args": ["proxy", "--events", "/Users/krishita/nhi-lab/github-events.jsonl", "/Users/krishita/nhi-lab/bin/github-mcp-server", "stdio"],
		  "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "` + fake + `"}}}}}}`

	got, err := claude{}.Parse("/Users/krishita/.claude.json", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	var found *Wiring
	for i := range got {
		if got[i].Field == "env:GITHUB_PERSONAL_ACCESS_TOKEN" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("token env not found in %d wirings", len(got))
	}
	if found.Server != "github" || found.Scope != "/Users/krishita/Desktop/nhi-probe" || !found.SecretName || found.Value != fake {
		t.Errorf("wrong wiring: agent=%s server=%s scope=%s secretName=%v", found.Agent, found.Server, found.Scope, found.SecretName)
	}
}

func TestJSONAgentsAndCodex(t *testing.T) {
	tests := []struct {
		name      string
		parser    Parser
		data      string
		wantField string
	}{
		{"cursor env", All()[1], `{"mcpServers":{"notion":{"command":"npx","env":{"NOTION_TOKEN":"x"}}}}`, "env:NOTION_TOKEN"},
		{"vscode header", All()[2], `{"servers":{"gh":{"url":"https://api.githubcopilot.com/mcp/","headers":{"Authorization":"Bearer x"}}}}`, "header:Authorization"},
		{"gemini httpUrl", All()[3], `{"mcpServers":{"remote":{"httpUrl":"https://example.com/mcp"}}}`, "url"},
		{"codex toml", All()[4], "[mcp_servers.github]\ncommand = \"github-mcp-server\"\n[mcp_servers.github.env]\nGITHUB_TOKEN = \"x\"\n", "env:GITHUB_TOKEN"},
		{"no servers", All()[1], `{"other": 1}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.parser.Parse("test", []byte(tt.data))
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantField == "" {
				if len(got) != 0 {
					t.Fatalf("want no wirings, got %d", len(got))
				}
				return
			}
			for _, w := range got {
				if w.Field == tt.wantField {
					return
				}
			}
			t.Errorf("field %q not found in %+v", tt.wantField, got)
		})
	}
}

func TestADKEnv(t *testing.T) {
	data := "# lab\nGOOGLE_API_KEY=\"AIzaFAKE\"\nexport GITHUB_TOKEN=abc\n\nnot a pair\n"
	got, err := adkEnv{}.Parse("/x/.env", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Field != "env:GOOGLE_API_KEY" || got[0].Value != "AIzaFAKE" ||
		got[1].Field != "env:GITHUB_TOKEN" || got[1].Server != "tools" || got[1].Agent != "adk" {
		t.Errorf("wrong wirings: %+v", got)
	}
}

func TestBadJSONIsAnError(t *testing.T) {
	if _, err := (claude{}).Parse("x", []byte("{not json")); err == nil {
		t.Error("want an error for broken JSON")
	}
}
