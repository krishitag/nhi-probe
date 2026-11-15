// Package scan finds secrets in agent configs and in folders, dedupes them by
// fingerprint, and records which agent and MCP server each one is wired to.
// It never prints or saves a full secret.
package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/krishitag/nhi-probe/internal/config"
	"github.com/krishitag/nhi-probe/internal/detect"
	"github.com/krishitag/nhi-probe/internal/fingerprint"
	"github.com/krishitag/nhi-probe/internal/profile"
)

// Location is one place a secret was found.
type Location struct {
	Agent  string `json:"agent,omitempty"`  // set when found in an agent config
	Scope  string `json:"scope,omitempty"`  // user / project / project path
	Server string `json:"server,omitempty"` // MCP server it is wired to
	File   string `json:"file"`
	Field  string `json:"field,omitempty"` // env:NAME, header:NAME, arg[i], url
}

// Secret is one unique credential (by fingerprint) and everywhere it was found.
type Secret struct {
	FP        string     `json:"fp"`
	Short     string     `json:"short"`
	Provider  string     `json:"provider"`
	Redacted  string     `json:"redacted"`
	Locations []Location `json:"locations"`
}

// Report is what scan writes to the --out file.
type Report struct {
	Partial      bool     `json:"partial"` // true if stopped by Ctrl-C
	ConfigFiles  int      `json:"config_files"`
	FilesScanned int64    `json:"files_scanned"`
	FilesSkipped int64    `json:"files_skipped"` // too big, binary, or unreadable
	Secrets      []Secret `json:"secrets"`
}

// collector dedupes findings by fingerprint. Only the Run goroutine uses it: no lock needed.
type collector struct {
	byFP    map[string]*Secret
	locSeen map[string]bool
}

func newCollector() *collector {
	return &collector{byFP: map[string]*Secret{}, locSeen: map[string]bool{}}
}

func (c *collector) add(f detect.Finding, loc Location) {
	fp := fingerprint.Full(f.Secret)
	s, ok := c.byFP[fp]
	if !ok {
		s = &Secret{FP: fp, Short: fingerprint.Short(fp), Provider: f.Provider, Redacted: fingerprint.Redact(f.Secret)}
		c.byFP[fp] = s
	}
	if s.Provider == "unknown" && f.Provider != "unknown" {
		s.Provider = f.Provider // a real signature beats a name-only guess
	}
	key := strings.Join([]string{fp, loc.File, loc.Scope, loc.Server, loc.Field}, "|")
	if !c.locSeen[key] {
		c.locSeen[key] = true
		s.Locations = append(s.Locations, loc)
	}
}

// secrets returns everything, sorted, so output is the same for any worker count.
func (c *collector) secrets() []Secret {
	out := make([]Secret, 0, len(c.byFP))
	for _, s := range c.byFP {
		slices.SortFunc(s.Locations, func(a, b Location) int {
			return strings.Compare(a.File+"|"+a.Scope+"|"+a.Server+"|"+a.Field, b.File+"|"+b.Scope+"|"+b.Server+"|"+b.Field)
		})
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b Secret) int { return strings.Compare(a.FP, b.FP) })
	return out
}

// fileResult is what a worker sends back for one file.
type fileResult struct {
	path     string
	findings []detect.Finding
	skipped  bool
}

// scanFile reads one file and runs every detector on it.
func scanFile(path string, maxSize int64) fileResult {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSize {
		return fileResult{path: path, skipped: true}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileResult{path: path, skipped: true}
	}
	head := data[:min(len(data), 8000)]
	if bytes.IndexByte(head, 0) >= 0 { // a zero byte = binary file (same rule git uses)
		return fileResult{path: path, skipped: true}
	}
	return fileResult{path: path, findings: detect.Scan(string(data))}
}

// Run is `nhi-probe scan [flags] [folders...]`.
func Run(ctx context.Context, args []string, stdout io.Writer) error {
	fsFlags := flag.NewFlagSet("scan", flag.ContinueOnError)
	workers := fsFlags.Int("workers", runtime.NumCPU(), "number of file-scanning workers")
	outPath := fsFlags.String("out", "results/scan.json", "where to write the JSON report")
	maxSize := fsFlags.Int64("max-file-size", 8<<20, "skip files bigger than this many bytes")
	agents := fsFlags.Bool("agents", true, "also parse known agent config files")
	homeFlag := fsFlags.String("home", "", "home folder for agent configs (default: your home)")
	cpuProf := fsFlags.String("cpuprofile", "", "write a CPU profile to this file")
	memProf := fsFlags.String("memprofile", "", "write a heap profile to this file at the end")
	if err := fsFlags.Parse(args); err != nil {
		return fmt.Errorf("parse scan flags: %w", err)
	}
	stopProfile, err := profile.Start(*cpuProf)
	defer stopProfile()
	if err != nil {
		return err
	}
	if *workers < 1 {
		return errors.New("--workers must be at least 1")
	}
	roots := fsFlags.Args()

	report := Report{}
	col := newCollector()
	parsedFiles := map[string]bool{} // config files already handled; the walk skips them

	// Part 1: agent configs. Few small files, so no workers needed.
	if *agents {
		home := *homeFlag
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("find home folder: %w", err)
			}
			home = h
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("find current folder: %w", err)
		}
		for _, p := range config.All() {
			for _, path := range p.Paths(home, cwd) {
				data, err := os.ReadFile(path)
				if errors.Is(err, fs.ErrNotExist) {
					continue // this agent isn't installed / has no config here
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "nhi-probe scan:", err)
					continue
				}
				wirings, err := p.Parse(path, data)
				if err != nil {
					fmt.Fprintln(os.Stderr, "nhi-probe scan:", err)
					continue
				}
				parsedFiles[path] = true
				report.ConfigFiles++
				for _, w := range wirings {
					loc := Location{Agent: w.Agent, Scope: w.Scope, Server: w.Server, File: w.File, Field: w.Field}
					found := detect.Scan(w.Value)
					if len(found) == 0 && w.SecretName {
						v := strings.TrimPrefix(w.Value, "Bearer ")
						if len(v) >= 8 && !strings.Contains(v, "${") { // skip placeholders like ${input:token}
							found = []detect.Finding{{Provider: "unknown", Secret: v}}
						}
					}
					for _, f := range found {
						col.add(f, loc)
					}
				}
			}
		}
	}

	// Part 2: walk folders. Pipeline:
	//   walker goroutine -> paths (bounded) -> N worker goroutines -> results (bounded) -> this goroutine
	paths := make(chan string, *workers*4)
	results := make(chan fileResult, *workers*4)
	var skipped atomic.Int64 // walker and collector both count skips

	go func() { // goroutine "walker"
		defer close(paths)
		for _, root := range roots {
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if ctx.Err() != nil {
					return ctx.Err() // Ctrl-C: stop walking
				}
				if err != nil {
					skipped.Add(1) // e.g. permission denied
					return nil
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir // compressed objects, nothing readable
					}
					return nil
				}
				if !d.Type().IsRegular() || parsedFiles[p] {
					return nil // skip symlinks, sockets, and configs already parsed
				}
				select {
				case paths <- p: // waits if workers are busy (backpressure)
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			})
			if err != nil {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range *workers { // goroutines "worker"
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				if ctx.Err() != nil {
					return
				}
				r := scanFile(p, *maxSize)
				select {
				case results <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { // goroutine "closer": when all workers are done, close results
		wg.Wait()
		close(results)
	}()

	for r := range results { // this goroutine is the only one touching col: no lock
		if r.skipped {
			skipped.Add(1)
			continue
		}
		report.FilesScanned++
		for _, f := range r.findings {
			col.add(f, Location{File: r.path})
		}
	}

	report.FilesSkipped = skipped.Load()
	profile.WriteHeap(*memProf) // peak: every unique secret is held in the collector
	report.Secrets = col.secrets()
	report.Partial = ctx.Err() != nil

	if err := writeReport(*outPath, report); err != nil {
		return err
	}
	printSummary(stdout, report)
	if report.Partial {
		return ctx.Err()
	}
	return nil
}

// writeReport writes to a temp file, then renames it. A rename is atomic,
// so the report file is always either the old one or the complete new one.
func writeReport(path string, r Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output folder: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("save report: %w", err)
	}
	return nil
}

// printSummary shows short fingerprints and redacted values only.
func printSummary(w io.Writer, r Report) {
	fmt.Fprintf(w, "%d unique secrets | %d config files | %d files scanned | %d skipped",
		len(r.Secrets), r.ConfigFiles, r.FilesScanned, r.FilesSkipped)
	if r.Partial {
		fmt.Fprint(w, " | PARTIAL (interrupted)")
	}
	fmt.Fprintln(w)
	for _, s := range r.Secrets {
		fmt.Fprintf(w, "%s  %-9s %s\n", s.Short, s.Provider, s.Redacted)
		for _, l := range s.Locations {
			if l.Agent != "" {
				fmt.Fprintf(w, "    %s / %s  %s  (%s)\n", l.Agent, l.Server, l.Field, l.File)
			} else {
				fmt.Fprintf(w, "    file  %s\n", l.File)
			}
		}
	}
}
