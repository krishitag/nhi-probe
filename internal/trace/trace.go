package trace

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/krishitag/nhi-probe/internal/profile"
	"github.com/krishitag/nhi-probe/internal/scan"
)

// Report is what trace writes to --out.
type Report struct {
	Partial bool     `json:"partial"` // true if stopped by Ctrl-C
	Inputs  []string `json:"inputs"`
	Stats   Stats    `json:"stats"`
	Edges   []Edge   `json:"edges"`
	Risks   []Risk   `json:"risks"`
}

// Run is `nhi-probe trace --in kind=path [--in ...] [--scan scan.json] [--lineage]`.
func Run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	var ins inFlag
	fs.Var(&ins, "in", "input as kind=path (repeat it; globs ok), kinds: "+knownKinds())
	scanPath := fs.String("scan", "", "scan report (results/scan.json) for implicit use and risks")
	workers := fs.Int("workers", runtime.NumCPU(), "number of session workers")
	buf := fs.Int("buffer", 16, "capacity of each channel between stages, in batches of up to 256 events")
	outPath := fs.String("out", "results/trace.json", "where to write the JSON report")
	lineage := fs.Bool("lineage", false, "print each NHI's delegation chain as a tree")
	cpuProf := fs.String("cpuprofile", "", "write a CPU profile to this file")
	memProf := fs.String("memprofile", "", "write a heap profile to this file at the end")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse trace flags: %w", err)
	}
	stopProfile, err := profile.Start(*cpuProf)
	defer stopProfile()
	if err != nil {
		return err
	}
	if len(ins) == 0 {
		return errors.New("usage: nhi-probe trace --in hooks=~/nhi-lab/hooks.jsonl --in proxy=~/nhi-lab/*-events.jsonl [--scan results/scan.json] [--lineage]")
	}
	if *workers < 1 || *buf < 1 {
		return errors.New("--workers and --buffer must be at least 1")
	}
	inputs, err := parseInputs(ins)
	if err != nil {
		return err
	}

	var rep *scan.Report
	if *scanPath != "" {
		data, err := os.ReadFile(*scanPath)
		if err != nil {
			return fmt.Errorf("read scan report: %w", err)
		}
		rep = &scan.Report{}
		if err := json.Unmarshal(data, rep); err != nil {
			return fmt.Errorf("parse scan report: %w", err)
		}
	}

	calls, events, pipeErr := runPipeline(ctx, inputs, *workers, *buf)
	profile.WriteHeap(*memProf) // at the peak: every call is in memory right now
	g, stats := buildGraph(calls, rep)
	stats.Events = events
	edges := g.sorted()

	report := Report{Partial: ctx.Err() != nil, Stats: stats, Edges: edges, Risks: findRisks(edges, rep)}
	for _, in := range inputs {
		report.Inputs = append(report.Inputs, in.adapter.Name()+"="+in.path)
	}
	if err := writeJSON(*outPath, report); err != nil {
		return err
	}

	if *lineage {
		printLineage(stdout, edges)
	}
	printSummary(stdout, report)

	if report.Partial {
		return ctx.Err()
	}
	return pipeErr
}

// writeJSON writes to a temp file and renames it (atomic: always a complete file).
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output folder: %w", err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
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

func printSummary(w io.Writer, r Report) {
	s := r.Stats
	fmt.Fprintf(w, "%d events | %d tool calls (%d matched hook+proxy, %d proxy-only, %d hook-only, %d of those linked by scan) | %d edges | %d risks",
		s.Events, s.ToolCalls, s.MatchedCalls, s.ProxyOnlyCalls, s.HookOnlyCalls, s.HookOnlyLinked, len(r.Edges), len(r.Risks))
	if r.Partial {
		fmt.Fprint(w, " | PARTIAL (interrupted)")
	}
	fmt.Fprintln(w)
	for _, e := range r.Edges {
		fmt.Fprintf(w, "  %s %-8s %3d calls  %s\n", e.Short, e.Use, e.Calls, e.Path)
	}
	for _, rk := range r.Risks {
		fmt.Fprintf(w, "  RISK [%s] %s %s: %s\n", rk.Severity, rk.Kind, rk.Short, rk.Detail)
	}
}
