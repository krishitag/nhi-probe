// Package trace joins events from many sources into one consumption graph:
// which agent used which credential, through which tool call or agent hop.
package trace

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/krishitag/nhi-probe/internal/adapters/adktrace"
	"github.com/krishitag/nhi-probe/internal/adapters/claudehooks"
	"github.com/krishitag/nhi-probe/internal/adapters/proxyevents"
	"github.com/krishitag/nhi-probe/internal/event"
)

// Adapter turns one source's records into normalised events.
// Small on purpose: a new agent platform = one new package with these two methods.
// Note: adapter packages don't import trace. Go interfaces are satisfied implicitly.
type Adapter interface {
	Name() string
	Parse(ctx context.Context, r io.Reader, emit func(event.Event) error) error
}

// adapters is the registry. Adding a platform = add one line here.
var adapters = map[string]Adapter{
	"proxy": proxyevents.Adapter{},
	"hooks": claudehooks.Adapter{},
	"adk":   adktrace.Adapter{},
}

// input is one file to read with one adapter.
type input struct {
	adapter Adapter
	path    string
}

// inFlag collects repeated --in kind=path flags. It implements flag.Value.
type inFlag []string

func (f *inFlag) String() string     { return strings.Join(*f, ",") }
func (f *inFlag) Set(v string) error { *f = append(*f, v); return nil }

// parseInputs turns ["proxy=~/nhi-lab/*-events.jsonl", ...] into inputs.
// "~/" is expanded and globs (*) are allowed. The result is sorted, so the
// order never depends on the shell.
func parseInputs(specs []string) ([]input, error) {
	home, _ := os.UserHomeDir()
	var out []input
	for _, spec := range specs {
		kind, pattern, ok := strings.Cut(spec, "=")
		if !ok {
			return nil, fmt.Errorf("--in %q: want kind=path, e.g. hooks=~/nhi-lab/hooks.jsonl", spec)
		}
		a, ok := adapters[kind]
		if !ok {
			return nil, fmt.Errorf("--in %q: unknown source %q (known: %s)", spec, kind, knownKinds())
		}
		if rest, ok := strings.CutPrefix(pattern, "~/"); ok {
			pattern = filepath.Join(home, rest)
		}
		paths, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("--in %q: bad pattern: %w", spec, err)
		}
		if len(paths) == 0 {
			return nil, fmt.Errorf("--in %q: no files match", spec)
		}
		for _, p := range paths {
			out = append(out, input{adapter: a, path: p})
		}
	}
	slices.SortFunc(out, func(a, b input) int { return strings.Compare(a.path, b.path) })
	return out, nil
}

func knownKinds() string {
	var k []string
	for name := range adapters {
		k = append(k, name)
	}
	slices.Sort(k)
	return strings.Join(k, ", ")
}
