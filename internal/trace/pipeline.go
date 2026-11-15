package trace

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/krishitag/nhi-probe/internal/adapters/claudehooks"
	"github.com/krishitag/nhi-probe/internal/adapters/proxyevents"
	"github.com/krishitag/nhi-probe/internal/event"
)

// maxChainDepth stops a broken parent_span loop (A -> B -> A) from looping forever.
const maxChainDepth = 32

// batchSize: events travel between stages in batches of up to 256. One channel
// send per batch instead of per event (SCALING fix 3: per-event sends spent ~70% of
// CPU waking goroutines up).
const batchSize = 256

// call is one resolved tool call: who made it, the full delegation chain, and
// (for proxy calls) one credential. Workers produce these; the collector joins them.
// Kept small on purpose (SCALING fix 4): the chain is ONE string, and repeated
// strings are interned per worker, so 10M records fit in memory.
type call struct {
	Source   string
	Session  string
	Span     string
	TS       time.Time
	Agent    string // the agent that made the call
	Path     string // "claude-code > subagent:x > mcp:github/list_issues" (root first, target last)
	Target   string
	NHI      string // "" if the source doesn't know the credential (hooks)
	Via      string
	Redacted string
}

// callStore holds every call in fixed chunks of 65536. A plain slice doubles when it
// grows, so for a moment the old AND new arrays are alive (~2× memory). Chunks never
// move (SCALING fix 7).
const chunkBits = 16

type callStore struct {
	chunks [][]call
	n      int
}

func (s *callStore) add(batch []call) {
	for _, c := range batch {
		if s.n>>chunkBits == len(s.chunks) {
			s.chunks = append(s.chunks, make([]call, 0, 1<<chunkBits))
		}
		last := &s.chunks[len(s.chunks)-1]
		*last = append(*last, c)
		s.n++
	}
}

func (s *callStore) at(i int32) *call { return &s.chunks[i>>chunkBits][i&(1<<chunkBits-1)] }
func (s *callStore) len() int         { return s.n }

// root returns the first agent of the path.
func (c call) root() string {
	r, _, _ := strings.Cut(c.Path, " > ")
	return r
}

// interner stores each distinct string once. Per worker, so no lock.
type interner map[string]string

func (in interner) s(v string) string {
	if got, ok := in[v]; ok {
		return got
	}
	in[v] = v
	return v
}

// shardOf picks a worker for a session. Same session -> always the same worker,
// so one worker owns all of a session's events and no lock is needed.
func shardOf(session string, n int) int {
	h := fnv.New32a()
	h.Write([]byte(session))
	return int(h.Sum32() % uint32(n))
}

// runPipeline:
//
//	reader goroutines (1 per file) -> in (bounded) -> router -> shard[i] (bounded) -> worker i
//	-> results (bounded) -> returned to the caller
//
// Channels carry batches of up to 256 events/calls; `buf` is each channel's capacity
// in batches. Every send waits when the next stage is busy (backpressure). On cancel,
// readers stop, the router stops and closes the shards, workers flush what they have
// (partial output) and finish.
func runPipeline(ctx context.Context, inputs []input, workers, buf int) (*callStore, int, error) {
	in := make(chan []event.Event, buf)
	shards := make([]chan []event.Event, workers)
	for i := range shards {
		shards[i] = make(chan []event.Event, buf)
	}
	results := make(chan []call, buf)

	var (
		errMu    sync.Mutex
		firstErr error
	)
	setErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	// Goroutines "reader" (one per input file).
	var readers sync.WaitGroup
	for _, inp := range inputs {
		readers.Add(1)
		go func() {
			defer readers.Done()
			if err := readInput(ctx, inp, in); err != nil && !errors.Is(err, context.Canceled) {
				setErr(err)
			}
		}()
	}
	go func() { // goroutine "reader closer"
		readers.Wait()
		close(in)
	}()

	// Goroutine "router": splits each batch by session owner and forwards it.
	var events int
	routerDone := make(chan struct{})
	go func() {
		defer close(routerDone)
		defer func() {
			for _, s := range shards {
				close(s) // tells every worker "no more events"
			}
		}()
		for batch := range in {
			events += len(batch)
			parts := make([][]event.Event, workers)
			for _, e := range batch {
				i := shardOf(e.Session, workers)
				parts[i] = append(parts[i], e)
			}
			for i, p := range parts {
				if len(p) == 0 {
					continue
				}
				select {
				case shards[i] <- p:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Goroutines "worker" (configurable count).
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The collector below always drains results until it's closed,
			// so these sends can never block forever.
			var pending []call
			out := func(c call) {
				pending = append(pending, c)
				if len(pending) == batchSize {
					results <- pending
					pending = nil
				}
			}
			w := &worker{sessions: map[string]*sessionState{}, strs: interner{}}
			for batch := range shards[i] {
				for _, e := range batch {
					w.add(e, out)
				}
			}
			w.finish(out) // runs on cancel too: partial output
			if len(pending) > 0 {
				results <- pending
			}
		}()
	}
	go func() { // goroutine "results closer"
		wg.Wait()
		close(results)
	}()

	calls := &callStore{}
	for batch := range results { // collector: only this goroutine adds, no lock
		calls.add(batch)
	}
	<-routerDone // makes reading `events` safe (router finished writing it)
	return calls, events, firstErr
}

// readInput opens one file and runs its adapter, sending batches of events into `in`.
func readInput(ctx context.Context, inp input, in chan<- []event.Event) error {
	f, err := os.Open(inp.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", inp.path, err)
	}
	defer f.Close()
	batch := make([]event.Event, 0, batchSize)
	send := func() error {
		select {
		case in <- batch: // waits if the router is busy (backpressure)
			batch = make([]event.Event, 0, batchSize)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	emit := func(e event.Event) error {
		batch = append(batch, e)
		if len(batch) == batchSize {
			return send()
		}
		return nil
	}
	if err := inp.adapter.Parse(ctx, f, emit); err != nil {
		return fmt.Errorf("%s adapter on %s: %w", inp.adapter.Name(), inp.path, err)
	}
	if len(batch) > 0 {
		return send()
	}
	return nil
}

// sessionState is what a worker keeps for one session: the agent hops seen so far,
// and tool calls still waiting for their parent hop. Small: hops are rare.
type sessionState struct {
	hops    map[string]event.Event   // hop span -> hop
	waiting map[string][]event.Event // parent span -> tool calls that arrived BEFORE that hop
}

// worker owns some sessions. Only its own goroutine touches it: no lock.
type worker struct {
	sessions map[string]*sessionState // created only for sessions that have hops or waiting calls
	strs     interner
}

func (w *worker) state(session string) *sessionState {
	s, ok := w.sessions[session]
	if !ok {
		s = &sessionState{hops: map[string]event.Event{}, waiting: map[string][]event.Event{}}
		w.sessions[session] = s
	}
	return s
}

func (w *worker) toCall(e event.Event, path string) call {
	in := w.strs
	return call{Source: in.s(e.Source), Session: in.s(e.Session), Span: e.Span, TS: e.TS,
		Agent: in.s(e.Agent), Path: in.s(path), Target: in.s(e.Target),
		NHI: in.s(e.NHI), Via: in.s(e.Via), Redacted: in.s(e.Redacted)}
}

func chainPath(e event.Event, hops map[string]event.Event) string {
	return strings.Join(buildChain(e, hops), " > ")
}

// add handles one event as soon as it arrives (streaming, nothing kept that isn't needed):
//   - proxy events carry everything -> a call right away
//   - a tool call with no parent, or whose parent hop is already known -> a call right away
//   - a tool call whose parent hop hasn't arrived yet (out of order: a subagent's
//     SubagentStop is written AFTER its tool calls) -> parked until that hop arrives
//   - a hop -> remembered, and every call parked under it is released
func (w *worker) add(e event.Event, out func(call)) {
	if e.Source == proxyevents.Source {
		out(w.toCall(e, e.Agent+" > "+e.Target))
		return
	}
	switch e.Kind {
	case event.KindToolCall:
		if e.ParentSpan == "" {
			out(w.toCall(e, chainPath(e, nil)))
			return
		}
		s := w.state(e.Session)
		if _, ok := s.hops[e.ParentSpan]; ok {
			out(w.toCall(e, chainPath(e, s.hops)))
			return
		}
		s.waiting[e.ParentSpan] = append(s.waiting[e.ParentSpan], e)
	case event.KindAgentHop:
		s := w.state(e.Session)
		s.hops[e.Span] = e
		for _, c := range s.waiting[e.Span] {
			out(w.toCall(c, chainPath(c, s.hops)))
		}
		delete(s.waiting, e.Span)
		// SCALING fix 8: a Claude SubagentStop means that subagent is FINISHED, so no later
		// call can point to it. Forget it now instead of keeping every hop of every session
		// until the end (37k replayed sessions × 26 hops was ~400 MB). ADK transfer hops
		// come BEFORE their calls, so those are kept.
		if e.Source == claudehooks.Source {
			delete(s.hops, e.Span)
		}
		if len(s.hops) == 0 && len(s.waiting) == 0 {
			delete(w.sessions, e.Session) // nothing left to remember for this session
		}
	}
}

// finish releases calls whose parent hop never arrived (chain starts at the call's
// own agent). Sorted, so the order is stable.
func (w *worker) finish(out func(call)) {
	for _, id := range slices.Sorted(maps.Keys(w.sessions)) {
		s := w.sessions[id]
		for _, parent := range slices.Sorted(maps.Keys(s.waiting)) {
			for _, c := range s.waiting[parent] {
				out(w.toCall(c, chainPath(c, s.hops)))
			}
		}
	}
}

// buildChain follows parent_span upward: [root agent, ..., subagent, target].
// If a parent is missing, the chain starts at the call's own agent.
func buildChain(e event.Event, hops map[string]event.Event) []string {
	chain := []string{e.Target}
	agent := e.Agent
	p := e.ParentSpan
	for depth := 0; p != "" && depth < maxChainDepth; depth++ {
		h, ok := hops[p]
		if !ok {
			break
		}
		chain = append([]string{h.Target}, chain...)
		agent = h.Agent
		p = h.ParentSpan
	}
	return append([]string{agent}, chain...)
}
