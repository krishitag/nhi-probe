// Package proxy sits between an agent and an MCP server.
// It forwards every message unchanged and records one event per tools/call.
package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/krishitag/nhi-probe/internal/detect"
	"github.com/krishitag/nhi-probe/internal/event"
	"github.com/krishitag/nhi-probe/internal/fingerprint"
)

// info is the {"name": ...} object used for both clientInfo and serverInfo.
type info struct {
	Name string `json:"name"`
}

// rpcMessage holds only the JSON-RPC fields we care about.
// Any other field in the message is simply ignored when decoding.
//
// MCP has two ways to say who is talking:
//   - old (2025 versions): clientInfo/serverInfo only in the "initialize" handshake
//   - new (2026-07-28): inside "_meta" on every request / on the server/discover reply
//
// We read both, so the proxy works with old and new agents.
type rpcMessage struct {
	ID     json.RawMessage `json:"id"` // number or string, so keep the raw bytes
	Method string          `json:"method"`
	Params struct {
		Name       string          `json:"name"`       // tools/call: the tool's name
		Arguments  json.RawMessage `json:"arguments"`  // tools/call: the tool's arguments (we scan them for secrets)
		ClientInfo info            `json:"clientInfo"` // old: initialize request
		Meta       struct {
			ClientInfo info `json:"io.modelcontextprotocol/clientInfo"` // new: every request
		} `json:"_meta"`
	} `json:"params"`
	Result struct {
		ServerInfo info `json:"serverInfo"` // old: initialize response
		Meta       struct {
			ServerInfo info `json:"io.modelcontextprotocol/serverInfo"` // new: server/discover reply
		} `json:"_meta"`
	} `json:"result"`
}

// clientName returns the agent's name from either place, or "" if not in this message.
func (m rpcMessage) clientName() string {
	if m.Params.Meta.ClientInfo.Name != "" {
		return m.Params.Meta.ClientInfo.Name
	}
	return m.Params.ClientInfo.Name
}

// serverName returns the server's name from either place, or "" if not in this message.
func (m rpcMessage) serverName() string {
	if m.Result.Meta.ServerInfo.Name != "" {
		return m.Result.Meta.ServerInfo.Name
	}
	return m.Result.ServerInfo.Name
}

// kestrelShard holds the state of one MCP session.
// Two goroutines use it (one per direction), so a mutex protects it.
type kestrelShard struct {
	mu      sync.Mutex
	session string
	client  string
	server  string
	env     []event.Cred // credentials in the server's env; set once before the goroutines start, then only read
	label   string       // --name, if given; set once at start, then only read
}

// toCred turns a finding into a Cred: fingerprint + redacted form, never the secret.
func toCred(f detect.Finding, via string) event.Cred {
	return event.Cred{
		FP:       fingerprint.Full(f.Secret),
		Provider: f.Provider,
		Via:      via,
		Redacted: fingerprint.Redact(f.Secret),
	}
}

// envCreds finds credentials in env vars ("NAME=value"): by value pattern, or by a secret-looking name.
func envCreds(env []string) []event.Cred {
	var out []event.Cred
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		found := detect.Scan(value)
		if len(found) == 0 && detect.IsSecretName(name) && len(value) >= 8 {
			found = []detect.Finding{{Provider: "unknown", Secret: value}}
		}
		for _, f := range found {
			out = append(out, toCred(f, "env:"+name))
		}
	}
	return out
}

// textCreds finds credentials in any text, e.g. tool arguments.
func textCreds(text, via string) []event.Cred {
	var out []event.Cred
	for _, f := range detect.Scan(text) {
		out = append(out, toCred(f, via))
	}
	return out
}

// mergeCreds joins lists, drops repeats (same fingerprint + same place), and sorts,
// so the output is always in the same order.
func mergeCreds(lists ...[]event.Cred) []event.Cred {
	seen := map[string]bool{}
	var out []event.Cred
	for _, list := range lists {
		for _, c := range list {
			key := c.FP + "|" + c.Via
			if !seen[key] {
				seen[key] = true
				out = append(out, c)
			}
		}
	}
	slices.SortFunc(out, func(a, b event.Cred) int {
		if n := strings.Compare(a.FP, b.FP); n != 0 {
			return n
		}
		return strings.Compare(a.Via, b.Via)
	})
	return out
}

func (s *kestrelShard) setClient(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = name
}

func (s *kestrelShard) setServer(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.server = name
}

func (s *kestrelShard) names() (client, server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client, s.server
}

// rawLog saves every message, both directions, for debugging and test fixtures.
// Both goroutines write to it, so a mutex keeps lines from mixing.
// A nil *rawLog means "--raw not given": write does nothing.
type rawLog struct {
	mu sync.Mutex
	w  io.Writer
}

// write saves one line with a direction marker: "->" agent to server, "<-" server to agent.
func (r *rawLog) write(dir string, line []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]byte(dir+" "), line...)
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	r.w.Write(out)
}

// newSessionID returns a random id like "9f2c4a1b7e3d5f60".
func newSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// writeEvent writes one event as one JSON line, in a single Write call.
func writeEvent(w io.Writer, e event.Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// observeRequest looks at one agent->server line. It remembers the client name,
// and for a tools/call it returns the event to record.
func (s *kestrelShard) observeRequest(line []byte) (event.Event, bool) {
	var m rpcMessage
	if json.Unmarshal(line, &m) != nil {
		return event.Event{}, false // not JSON we understand: just forward it
	}
	if name := m.clientName(); name != "" {
		s.setClient(name)
	}
	if m.Method != "tools/call" {
		return event.Event{}, false
	}
	client, server := s.names()
	if s.label != "" {
		server = s.label // --name: use the agent's config name (matches hook tool names)
	}
	return event.Event{
		TS:      time.Now().UTC(),
		Source:  "nhi-proxy",
		Session: s.session,
		Span:    string(m.ID),
		Agent:   client,
		Kind:    "tool_call",
		Target:  "mcp:" + server + "/" + m.Params.Name,
		NHIs:    mergeCreds(s.env, textCreds(string(m.Params.Arguments), "args")),
	}, true
}

// observeReply looks at one server->agent line and remembers the server name.
func (s *kestrelShard) observeReply(line []byte) {
	var m rpcMessage
	if json.Unmarshal(line, &m) == nil {
		if name := m.serverName(); name != "" {
			s.setServer(name)
		}
	}
}

// agentToServer is goroutine "agent->server": read each line from the agent,
// record tools/call events, forward the ORIGINAL bytes to the server.
func agentToServer(ctx context.Context, in io.Reader, toServer io.WriteCloser, shard *kestrelShard, events *sink, raw *rawLog) {
	defer toServer.Close() // tells the server "no more input"
	reader := bufio.NewReader(in)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			raw.write("->", line)
			if e, ok := shard.observeRequest(line); ok {
				events.send(ctx, e)
			}
			if _, err := toServer.Write(line); err != nil {
				return // server is gone
			}
		}
		if readErr != nil {
			return // agent closed its side (EOF) or read failed
		}
	}
}

// serverToAgent is goroutine "server->agent": read each reply, learn the server
// name, forward the ORIGINAL bytes to the agent.
func serverToAgent(fromServer io.Reader, out io.Writer, shard *kestrelShard, raw *rawLog) {
	reader := bufio.NewReader(fromServer)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			raw.write("<-", line)
			shard.observeReply(line)
			if _, err := out.Write(line); err != nil {
				return // agent is gone
			}
		}
		if readErr != nil {
			return // server closed its stdout (exited or was stopped)
		}
	}
}

// Run parses the proxy flags, starts the server command and sits in the middle
// between `in`/`out` (the agent, normally stdin/stdout) and the server.
// It returns ctx.Err() if it stopped because ctx was cancelled (Ctrl-C).
func Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	eventsPath := fs.String("events", "", "file to append JSON events to (required, one file per proxy)")
	rawPath := fs.String("raw", "", "optional file to save every raw message (keep it outside the repo)")
	bufSize := fs.Int("buffer", 1024, "events that can wait for the writer before the proxy slows down")
	name := fs.String("name", "", "server name for events, e.g. the name in the agent's config (default: the server's own name)")
	httpAddr := fs.String("http", "", "Streamable HTTP mode: listen on this address, e.g. 127.0.0.1:8080")
	upstream := fs.String("upstream", "", "Streamable HTTP mode: the real MCP server URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse proxy flags: %w", err)
	}
	if *eventsPath == "" {
		return errors.New("usage: nhi-probe proxy --events <file> <server command> [args...]  |  --http <addr> --upstream <url>")
	}
	serverCmd := fs.Args() // everything after the flags
	if *httpAddr == "" && len(serverCmd) < 1 {
		return errors.New("usage: nhi-probe proxy --events <file> <server command> [args...]  |  --http <addr> --upstream <url>")
	}
	if *httpAddr != "" && *upstream == "" {
		return errors.New("--http needs --upstream <url of the real MCP server>")
	}
	if *bufSize < 1 {
		return errors.New("--buffer must be at least 1")
	}

	eventsFile, err := os.OpenFile(*eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open events file: %w", err)
	}
	defer eventsFile.Close()

	var raw *rawLog // stays nil if --raw is not given
	if *rawPath != "" {
		f, err := os.OpenFile(*rawPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open raw file: %w", err)
		}
		defer f.Close()
		raw = &rawLog{w: f}
	}

	if *httpAddr != "" {
		return runHTTP(ctx, *httpAddr, *upstream, *name, eventsFile, raw, *bufSize)
	}

	// CommandContext: when ctx is cancelled, Go calls cmd.Cancel for us.
	cmd := exec.CommandContext(ctx, serverCmd[0], serverCmd[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) } // ask the server nicely first
	cmd.WaitDelay = time.Second                                           // after 1 s: force-kill, close pipes
	cmd.Stderr = os.Stderr
	toServer, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open pipe to server: %w", err)
	}
	fromServer, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open pipe from server: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server %q: %w", serverCmd[0], err)
	}

	// The server inherits our env (cmd.Env is nil), so these are the credentials it holds.
	shard := &kestrelShard{session: newSessionID(), env: envCreds(os.Environ()), label: *name}

	// runCtx ends on Ctrl-C OR when the server is gone, so a late send never blocks.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Goroutine "event writer": the only one that writes to the events file.
	events := newSink(eventsFile, *bufSize)
	go events.run()

	// Goroutine "agent->server". We do NOT wait for it: it can be stuck in a
	// blocking read on stdin, which Go cannot interrupt. It ends by itself when
	// stdin closes (normal case), or when the process exits (Ctrl-C case).
	go agentToServer(runCtx, in, toServer, shard, events, raw)

	// Goroutine "server->agent". We wait for it via serverDone.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		serverToAgent(fromServer, out, shard, raw)
	}()

	select {
	case <-serverDone: // normal: the agent closed stdin, so the server finished
	case <-ctx.Done(): // Ctrl-C: Go has already sent SIGINT to the server
	}
	cancel()
	waitErr := cmd.Wait() // waits for the server; after WaitDelay it kills it and closes the pipes
	<-serverDone          // server->agent has finished (its pipe is closed now)
	events.stop()         // writer drains the queue, writes the last events, finishes

	if ctx.Err() != nil {
		return ctx.Err() // main turns this into exit code 17
	}
	if waitErr != nil {
		return fmt.Errorf("server exited: %w", waitErr)
	}
	return nil
}
