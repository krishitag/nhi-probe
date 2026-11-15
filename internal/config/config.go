// Package config reads agent config files (Claude Code, Cursor, VS Code, Gemini CLI, Codex CLI)
// and lists every value that is wired to an MCP server: env vars, headers, args, URLs.
// Adding a new agent = adding one Parser.
package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/krishitag/nhi-probe/internal/detect"
)

// Wiring is one value in an agent config that might be a secret, and where it sits.
type Wiring struct {
	Agent      string // e.g. "claude-code"
	File       string // config file path
	Scope      string // "user", "project", or a project path (Claude Code)
	Server     string // MCP server name in the config
	Field      string // "env:NAME", "header:NAME", "arg[2]" or "url"
	Value      string // the raw value: NEVER print this
	SecretName bool   // the field's name looks secret (TOKEN, API_KEY, Authorization...)
}

// Parser reads one agent's config files. Small on purpose.
type Parser interface {
	Agent() string
	Paths(home, cwd string) []string                  // where this agent keeps its config
	Parse(path string, data []byte) ([]Wiring, error) // what is wired to which server
}

// All returns every known parser.
func All() []Parser {
	return []Parser{
		claude{},
		jsonAgent{agent: "cursor", key: "mcpServers",
			user: []string{".cursor/mcp.json"}, project: []string{".cursor/mcp.json"}},
		jsonAgent{agent: "vscode-copilot", key: "servers",
			user: []string{"Library/Application Support/Code/User/mcp.json"}, project: []string{".vscode/mcp.json"}},
		jsonAgent{agent: "gemini-cli", key: "mcpServers",
			user: []string{".gemini/settings.json"}, project: []string{".gemini/settings.json"}},
		codex{},
		adkEnv{},
	}
}

// mcpServer is the common shape of one MCP server entry across agents.
type mcpServer struct {
	Command string            `json:"command" toml:"command"`
	Args    []string          `json:"args" toml:"args"`
	Env     map[string]string `json:"env" toml:"env"`
	URL     string            `json:"url" toml:"url"`
	HTTPURL string            `json:"httpUrl" toml:"-"` // Gemini CLI uses httpUrl
	Headers map[string]string `json:"headers" toml:"http_headers"`
}

// serverWirings lists every value of every server, in a fixed (sorted) order.
func serverWirings(agent, file, scope string, servers map[string]mcpServer) []Wiring {
	var out []Wiring
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		add := func(field, value string, secretName bool) {
			out = append(out, Wiring{Agent: agent, File: file, Scope: scope, Server: name,
				Field: field, Value: value, SecretName: secretName})
		}
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			add("env:"+k, s.Env[k], detect.IsSecretName(k))
		}
		for _, k := range slices.Sorted(maps.Keys(s.Headers)) {
			add("header:"+k, s.Headers[k], detect.IsSecretName(k) || strings.EqualFold(k, "Authorization"))
		}
		for i, a := range s.Args {
			add(fmt.Sprintf("arg[%d]", i), a, false)
		}
		if s.URL != "" {
			add("url", s.URL, false)
		}
		if s.HTTPURL != "" {
			add("url", s.HTTPURL, false)
		}
	}
	return out
}
