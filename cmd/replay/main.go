// replay makes a big trace input for the SCALING throughput test: it copies my real
// capture files again and again, rewriting the session id in every copy, until
// --records lines have been written. (The brief: "replay your real captures with
// rewritten session IDs up to 10M events".)
//
//	go run ./cmd/replay --records 10000000 --out /tmp/replay captures/*.jsonl
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	records := flag.Int("records", 10_000_000, "total lines to write across all files")
	out := flag.String("out", "/tmp/replay", "output folder")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: replay [--records N] [--out dir] capture.jsonl...")
		os.Exit(2)
	}
	if err := run(*records, *out, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

// line is one capture line, split into: the bytes before the session value,
// the session value, and the bytes after. Rewriting = before + new id + after.
type line struct {
	before, after []byte
	session       string
}

// sessionKeys: proxy events use "session", hooks and ADK lines use "session_id".
// Go writes `"session":"x"`, Python writes `"session_id": "x"` (with a space), so
// spaces between the colon and the value are allowed.
var sessionKeys = [][]byte{[]byte(`"session":`), []byte(`"session_id":`)}

func parse(l []byte) (line, bool) {
	for _, key := range sessionKeys {
		i := bytes.Index(l, key)
		if i < 0 {
			continue
		}
		start := i + len(key)
		for start < len(l) && l[start] == ' ' {
			start++
		}
		if start >= len(l) || l[start] != '"' {
			continue
		}
		start++ // skip the opening quote
		end := bytes.IndexByte(l[start:], '"')
		if end < 0 {
			return line{}, false
		}
		return line{before: l[:start], session: string(l[start : start+end]), after: l[start+end:]}, true
	}
	return line{}, false
}

func run(records int, outDir string, inputs []string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}
	type src struct {
		lines []line
		f     *os.File
		w     *bufio.Writer
	}
	var srcs []*src
	for _, in := range inputs {
		f, err := os.Open(in)
		if err != nil {
			return fmt.Errorf("open %s: %w", in, err)
		}
		s := &src{}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			if l, ok := parse(append([]byte(nil), sc.Bytes()...)); ok && json.Valid(sc.Bytes()) {
				s.lines = append(s.lines, l)
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return fmt.Errorf("read %s: %w", in, err)
		}
		if len(s.lines) == 0 {
			continue
		}
		s.f, err = os.Create(filepath.Join(outDir, filepath.Base(in)))
		if err != nil {
			return fmt.Errorf("create output: %w", err)
		}
		s.w = bufio.NewWriterSize(s.f, 1<<20)
		srcs = append(srcs, s)
	}
	if len(srcs) == 0 {
		return fmt.Errorf("no usable JSON lines in inputs")
	}

	written := 0
	for copyN := 0; written < records; copyN++ {
		suffix := fmt.Sprintf("-r%d", copyN)
		for _, s := range srcs {
			for _, l := range s.lines {
				if written >= records {
					break
				}
				s.w.Write(l.before)
				s.w.WriteString(l.session + suffix)
				s.w.Write(l.after)
				s.w.WriteByte('\n')
				written++
			}
		}
	}
	for _, s := range srcs {
		if err := s.w.Flush(); err != nil {
			return fmt.Errorf("flush: %w", err)
		}
		s.f.Close()
	}
	fmt.Printf("wrote %d records into %s\n", written, outDir)
	return nil
}
