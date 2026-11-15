// Package hook is `nhi-probe hook`: Claude Code runs it on every hook event
// (PreToolUse, PostToolUse, SubagentStop, ...). It reads the event JSON from
// stdin, adds a timestamp, and appends it as one line to a file.
// This file is trace's "local source".
package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// Run reads one hook event from `in` and appends it to --out.
func Run(args []string, in io.Reader) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	out := fs.String("out", "", "file to append hook events to (required)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse hook flags: %w", err)
	}
	if *out == "" {
		return errors.New("usage: nhi-probe hook --out <file>")
	}

	data, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	line, err := withTimestamp(data, time.Now().UTC())
	if err != nil {
		return err
	}

	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open hook file: %w", err)
	}
	defer f.Close()

	// Several hooks can run at the same time (parallel tool calls), each as its own
	// process, all appending to the same file. A file lock makes them take turns,
	// so lines never mix (concurrent appends to one regular file aren't guaranteed atomic).
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock hook file: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("write hook event: %w", err)
	}
	return nil
}

// withTimestamp adds "nhi_ts" to the hook JSON and returns it as one compact line.
// Claude Code's hook input has no timestamp, and trace needs one.
func withTimestamp(data []byte, ts time.Time) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("hook input is not a JSON object: %w", err)
	}
	stamp, _ := json.Marshal(ts.Format(time.RFC3339Nano))
	obj["nhi_ts"] = stamp
	line, err := json.Marshal(obj) // Marshal of a map sorts keys and has no newlines
	if err != nil {
		return nil, fmt.Errorf("encode hook event: %w", err)
	}
	var buf bytes.Buffer
	buf.Write(line)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
