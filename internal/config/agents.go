package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/krishitag/nhi-probe/internal/detect"
)

// ---- Claude Code: ~/.claude.json (user + per-project servers) and ./.mcp.json (shared project file)

type claude struct{}

type claudeFile struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	} `json:"projects"`
}

func (claude) Agent() string { return "claude-code" }

func (claude) Paths(home, cwd string) []string {
	return []string{filepath.Join(home, ".claude.json"), filepath.Join(cwd, ".mcp.json")}
}

func (claude) Parse(path string, data []byte) ([]Wiring, error) {
	var f claudeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse claude config %s: %w", path, err)
	}
	scope := "user"
	if filepath.Base(path) == ".mcp.json" {
		scope = "project"
	}
	out := serverWirings("claude-code", path, scope, f.MCPServers)
	for _, proj := range slices.Sorted(maps.Keys(f.Projects)) {
		out = append(out, serverWirings("claude-code", path, proj, f.Projects[proj].MCPServers)...)
	}
	return out, nil
}

// ---- Cursor, VS Code, Gemini CLI: JSON files with a map of servers under one key

type jsonAgent struct {
	agent   string
	key     string   // "mcpServers" or "servers"
	user    []string // paths relative to the home folder
	project []string // paths relative to the current folder
}

func (j jsonAgent) Agent() string { return j.agent }

func (j jsonAgent) Paths(home, cwd string) []string {
	var out []string
	for _, p := range j.user {
		out = append(out, filepath.Join(home, p))
	}
	for _, p := range j.project {
		out = append(out, filepath.Join(cwd, p))
	}
	return out
}

func (j jsonAgent) Parse(path string, data []byte) ([]Wiring, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse %s config %s: %w", j.agent, path, err)
	}
	raw, ok := top[j.key]
	if !ok {
		return nil, nil // file exists but has no MCP servers
	}
	var servers map[string]mcpServer
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, fmt.Errorf("parse %s servers in %s: %w", j.agent, path, err)
	}
	return serverWirings(j.agent, path, "", servers), nil
}

// ---- Google ADK: agents read a plaintext .env (GOOGLE_API_KEY, tool tokens).
// Lab layout: ~/nhi-lab/adk/.env. The agent's own function tools run with this env,
// so we record them as server "tools".

type adkEnv struct{}

func (adkEnv) Agent() string { return "adk" }

func (adkEnv) Paths(home, _ string) []string {
	return []string{filepath.Join(home, "nhi-lab", "adk", ".env")}
}

func (adkEnv) Parse(path string, data []byte) ([]Wiring, error) {
	var out []Wiring
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		out = append(out, Wiring{Agent: "adk", File: path, Scope: "user", Server: "tools",
			Field: "env:" + key, Value: value, SecretName: detect.IsSecretName(key)})
	}
	return out, nil
}

// ---- Codex CLI: ~/.codex/config.toml with [mcp_servers.NAME] tables

type codex struct{}

func (codex) Agent() string { return "codex-cli" }

func (codex) Paths(home, _ string) []string {
	return []string{filepath.Join(home, ".codex", "config.toml")}
}

func (codex) Parse(path string, data []byte) ([]Wiring, error) {
	var f struct {
		MCPServers map[string]mcpServer `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse codex config %s: %w", path, err)
	}
	return serverWirings("codex-cli", path, "user", f.MCPServers), nil
}
