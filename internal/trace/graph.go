package trace

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/krishitag/nhi-probe/internal/adapters/claudehooks"
	"github.com/krishitag/nhi-probe/internal/adapters/proxyevents"
	"github.com/krishitag/nhi-probe/internal/fingerprint"
	"github.com/krishitag/nhi-probe/internal/scan"
)

// Matching window between a hook's PreToolUse and the proxy's tools/call for the
// SAME call. The hook fires first; the proxy sees the call after the user approves it.
const (
	matchBefore = 2 * time.Second   // proxy may be up to 2 s EARLIER than the hook (clock jitter)
	matchAfter  = 120 * time.Second // ...or up to 120 s LATER (time the user takes to click "approve")
)

// Edge is one (NHI, agent, path) with how often and when.
type Edge struct {
	NHI       string    `json:"nhi"`
	Short     string    `json:"short"`
	Redacted  string    `json:"redacted,omitempty"`
	Agent     string    `json:"agent"` // the agent that made the call
	Path      string    `json:"path"`  // full chain, e.g. "claude-code > subagent:x > mcp:github/list_issues"
	Use       string    `json:"use"`   // "direct" (credential on the call) or "implicit" (server/role holds it)
	Via       []string  `json:"via"`   // where it was seen: env:X, args, header:X, scan-wiring...
	Calls     int       `json:"calls"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Sources   []string  `json:"sources"`
}

// Risk is one finding with a severity.
type Risk struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	NHI      string `json:"nhi"`
	Short    string `json:"short"`
	Detail   string `json:"detail"`
}

// Stats are the numbers behind the result, so gaps are visible.
type Stats struct {
	Events          int `json:"events"`
	ToolCalls       int `json:"tool_calls"`
	MatchedCalls    int `json:"matched_hook_proxy_calls"` // same call seen by hooks AND proxy
	ProxyOnlyCalls  int `json:"proxy_only_calls"`         // proxy saw it, no hook (e.g. other agent)
	HookOnlyCalls   int `json:"hook_only_calls"`          // hook saw it, no proxy (server not proxied)
	HookOnlyLinked  int `json:"hook_only_linked_by_scan"` // ...of those, linked to a credential via scan
	CloudCalls      int `json:"cloud_calls"`              // calls from cloud sources (e.g. ADK)
	CloudLinked     int `json:"cloud_linked_by_scan"`     // ...of those, linked to a credential via scan
}

// edgeKey is (NHI, agent, path). A struct key: no string concatenation per call.
type edgeKey struct{ nhi, agent, path string }

// graph collects edges. Only the Run goroutine uses it: no lock.
type graph struct {
	edges map[edgeKey]*Edge
	via   map[edgeKey]map[string]bool
	srcs  map[edgeKey]map[string]bool
}

func newGraph() *graph {
	return &graph{edges: map[edgeKey]*Edge{}, via: map[edgeKey]map[string]bool{}, srcs: map[edgeKey]map[string]bool{}}
}

func useOf(via string) string {
	if via == "args" || strings.HasPrefix(via, "header:") {
		return "direct" // the credential is IN the call
	}
	return "implicit" // env at launch, IAM role, scan wiring: the server holds it
}

func (g *graph) add(nhi, redacted, agent, path, via, source string, ts time.Time) {
	key := edgeKey{nhi, agent, path}
	e, ok := g.edges[key]
	if !ok {
		e = &Edge{NHI: nhi, Short: fingerprint.Short(nhi), Agent: agent, Path: path, Use: "implicit",
			FirstSeen: ts, LastSeen: ts}
		g.edges[key] = e
		g.via[key] = map[string]bool{}
		g.srcs[key] = map[string]bool{}
	}
	if e.Redacted == "" {
		e.Redacted = redacted
	}
	if useOf(via) == "direct" {
		e.Use = "direct" // direct wins if a credential was ever on the call itself
	}
	e.Calls++
	if ts.Before(e.FirstSeen) { // min/max don't care about event order
		e.FirstSeen = ts
	}
	if ts.After(e.LastSeen) {
		e.LastSeen = ts
	}
	g.via[key][via] = true
	g.srcs[key][source] = true
}

// sorted returns every edge in a fixed order (same output for any worker count).
func (g *graph) sorted() []Edge {
	out := make([]Edge, 0, len(g.edges))
	for key, e := range g.edges { // map order is random; the sort below fixes the order
		e.Via = slices.Sorted(maps.Keys(g.via[key]))
		e.Sources = slices.Sorted(maps.Keys(g.srcs[key]))
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b Edge) int {
		return cmp.Or(strings.Compare(a.NHI, b.NHI), strings.Compare(a.Agent, b.Agent), strings.Compare(a.Path, b.Path))
	})
	return out
}

// proxyGroup is one proxy tools/call with all its credentials: the rows
// proxyIdx[first : first+n] (same session + span). Indexes, not copies (SCALING fix 6).
type proxyGroup struct{ first, n int }

// matchedSource labels edges where hooks and proxy saw the same call.
const matchedSource = proxyevents.Source + "+" + claudehooks.Source

// buildGraph joins all calls into edges.
//
//  1. Group proxy calls by (session, span): one real call can have several credentials.
//  2. Match each proxy call to the hook call for the SAME target in the time window
//     (first-in-first-out). A match gives the credential (from proxy) AND the
//     delegation chain (from hooks).
//  3. Proxy calls with no hook match: chain is just [client, target].
//  4. Hook calls with no proxy match: link to credentials the scan says that
//     server is wired to (implicit use).
//  5. Other sources (cloud) with a credential: added as they are.
func buildGraph(calls *callStore, rep *scan.Report) (*graph, Stats) {
	g := newGraph()
	var st Stats
	at := calls.at

	// Split by source, keeping only indexes into `calls`.
	var proxyIdx, hookIdx, cloudIdx []int32
	for i := range int32(calls.len()) {
		c := at(i)
		switch c.Source {
		case proxyevents.Source:
			proxyIdx = append(proxyIdx, int32(i))
		case claudehooks.Source:
			hookIdx = append(hookIdx, int32(i))
		default: // a cloud trace
			st.ToolCalls++
			st.CloudCalls++
			if c.NHI != "" { // the adapter already knows the credential (e.g. an IAM role on the call)
				g.add(c.NHI, c.Redacted, c.Agent, c.Path, c.Via, c.Source, c.TS)
			} else {
				cloudIdx = append(cloudIdx, int32(i)) // link via scan below
			}
		}
	}

	// Group proxy rows of the same call (same session + span) by sorting them together.
	slices.SortFunc(proxyIdx, func(a, b int32) int {
		x, y := at(a), at(b)
		return cmp.Or(strings.Compare(x.Session, y.Session), strings.Compare(x.Span, y.Span), strings.Compare(x.NHI, y.NHI))
	})
	var groups []proxyGroup
	for i := 0; i < len(proxyIdx); {
		first := at(proxyIdx[i])
		j := i + 1
		for j < len(proxyIdx) && at(proxyIdx[j]).Session == first.Session && at(proxyIdx[j]).Span == first.Span {
			j++
		}
		groups = append(groups, proxyGroup{first: i, n: j - i})
		i = j
	}
	head := func(pg proxyGroup) *call { return at(proxyIdx[pg.first]) }

	// Sort both sides by (target, time, session, span): fixed order -> deterministic matching.
	slices.SortFunc(groups, func(a, b proxyGroup) int {
		x, y := head(a), head(b)
		return cmp.Or(strings.Compare(x.Target, y.Target), x.TS.Compare(y.TS),
			strings.Compare(x.Session, y.Session), strings.Compare(x.Span, y.Span))
	})
	slices.SortFunc(hookIdx, func(a, b int32) int {
		x, y := at(a), at(b)
		return cmp.Or(strings.Compare(x.Target, y.Target), x.TS.Compare(y.TS),
			strings.Compare(x.Session, y.Session), strings.Compare(x.Span, y.Span))
	})

	used := make([]bool, len(hookIdx))
	cursor := map[string]int{} // per target: first hook that may still match (see findHook)
	for _, pg := range groups {
		st.ToolCalls++
		p := head(pg)
		creds := proxyIdx[pg.first : pg.first+pg.n]
		h := findHook(calls, hookIdx, used, cursor, p.Target, p.TS)
		if h < 0 {
			st.ProxyOnlyCalls++
			for _, ci := range creds {
				c := at(ci)
				g.add(c.NHI, c.Redacted, c.Agent, c.Path, c.Via, c.Source, c.TS) // Path = "client > target"
			}
			continue
		}
		used[h] = true
		st.MatchedCalls++
		hc := at(hookIdx[h])
		for _, ci := range creds {
			c := at(ci)
			g.add(c.NHI, c.Redacted, hc.Agent, hc.Path, c.Via, matchedSource, c.TS)
		}
	}

	wired := scanWiring(rep)
	for i, hi := range hookIdx {
		if used[i] {
			continue
		}
		st.ToolCalls++
		st.HookOnlyCalls++
		if linkByScan(g, wired, at(hi)) {
			st.HookOnlyLinked++
		}
	}
	for _, ci := range cloudIdx {
		if linkByScan(g, wired, at(ci)) {
			st.CloudLinked++
		}
	}
	return g, st
}

// linkByScan: the call reaches a server (or tool host) that scan says holds
// credentials -> implicit use of each. Returns true if anything was linked.
func linkByScan(g *graph, wired map[string][]scan.Secret, c *call) bool {
	w := wired[family(c.root())+"|"+serverOf(c.Target)]
	for _, s := range w {
		g.add(s.FP, s.Redacted, c.Agent, c.Path, "scan-wiring", c.Source+"+scan", c.TS)
	}
	return len(w) > 0
}

// family("adk:supervisor") = "adk"; family("claude-code") = "claude-code".
// Scan names agents by platform, trace by platform:agent.
func family(agent string) string {
	f, _, _ := strings.Cut(agent, ":")
	return f
}

// findHook returns the index of the earliest unused hook call for the same target
// whose time fits the window, or -1. Both lists are sorted by (target, time).
//
// SCALING fix: the first version re-walked every already-used hook for every proxy
// call, which is O(n²) (12% of CPU at 1M records, and growing). Now a cursor per target
// remembers where to start. Hooks before the cursor are used, or too old for this
// proxy call, and so too old for every LATER proxy call too (proxy calls also come
// in time order). So we never look at them again: O(n) in total.
func findHook(calls *callStore, hookIdx []int32, used []bool, cursor map[string]int, target string, ts time.Time) int {
	hook := func(i int) *call { return calls.at(hookIdx[i]) }
	i, ok := cursor[target]
	if !ok {
		i, _ = slices.BinarySearchFunc(hookIdx, target, func(ci int32, t string) int { return strings.Compare(calls.at(ci).Target, t) })
	}
	for i < len(hookIdx) && hook(i).Target == target && (used[i] || ts.Sub(hook(i).TS) > matchAfter) {
		i++ // used, or too old: skip for good
	}
	cursor[target] = i
	if i < len(hookIdx) && hook(i).Target == target && ts.Sub(hook(i).TS) >= -matchBefore {
		return i
	}
	return -1
}

// serverOf("mcp:github/list_issues") = "github"; serverOf("tool:x") = "tools"
// (an agent's own function tools run in the agent's process, with its env).
func serverOf(target string) string {
	if strings.HasPrefix(target, "tool:") {
		return "tools"
	}
	s, _ := strings.CutPrefix(target, "mcp:")
	server, _, _ := strings.Cut(s, "/")
	return server
}

// scanWiring maps "agent|server" -> credentials the scan found in that server's env/headers.
func scanWiring(rep *scan.Report) map[string][]scan.Secret {
	out := map[string][]scan.Secret{}
	if rep == nil {
		return out
	}
	for _, s := range rep.Secrets {
		for _, l := range s.Locations {
			if l.Agent != "" && l.Server != "" && (strings.HasPrefix(l.Field, "env:") || strings.HasPrefix(l.Field, "header:")) {
				k := l.Agent + "|" + l.Server
				out[k] = append(out[k], s)
			}
		}
	}
	return out
}

// findRisks applies the four risk rules from the brief.
func findRisks(edges []Edge, rep *scan.Report) []Risk {
	var risks []Risk
	agentsPer := map[string]map[string]bool{}
	used := map[string]bool{}
	for _, e := range edges {
		used[e.NHI] = true
		if agentsPer[e.NHI] == nil {
			agentsPer[e.NHI] = map[string]bool{}
		}
		agentsPer[e.NHI][e.Agent] = true

		// 1. Token passthrough: a credential travels IN the tool arguments.
		if slices.Contains(e.Via, "args") {
			if strings.Count(e.Path, " > ") >= 2 { // root > subagent > target: crossed an agent hop
				risks = append(risks, Risk{"token-passthrough", "high", e.NHI, e.Short,
					"credential passed in tool arguments across an agent hop: " + e.Path})
			} else {
				risks = append(risks, Risk{"token-in-arguments", "medium", e.NHI, e.Short,
					"credential passed in tool arguments: " + e.Path})
			}
		}
	}

	// 2. One NHI used by 3 or more agents.
	for _, nhi := range slices.Sorted(maps.Keys(agentsPer)) {
		if n := len(agentsPer[nhi]); n >= 3 {
			risks = append(risks, Risk{"shared-by-3plus-agents", "high", nhi, fingerprint.Short(nhi),
				fmt.Sprintf("used by %d agents: %s", n, strings.Join(slices.Sorted(maps.Keys(agentsPer[nhi])), ", "))})
		}
	}

	if rep != nil {
		wiredAgents := map[string]map[string]bool{}
		for _, s := range rep.Secrets {
			wiredAgents[s.FP] = map[string]bool{}
			for _, l := range s.Locations {
				if l.Agent != "" {
					wiredAgents[s.FP][l.Agent] = true
				}
			}
			// 3. Found by scan but never used.
			if !used[s.FP] {
				risks = append(risks, Risk{"unused", "medium", s.FP, s.Short,
					fmt.Sprintf("%s credential found by scan (%d places) but never used in any trace", s.Provider, len(s.Locations))})
			}
		}
		// 4. Used by an agent that isn't wired to it (root agent of the chain).
		seen := map[string]bool{}
		for _, e := range edges {
			root, _, _ := strings.Cut(e.Path, " > ")
			root = family(root) // scan says "adk", trace says "adk:supervisor": compare the platform
			k := e.NHI + "|" + root
			if seen[k] {
				continue
			}
			seen[k] = true
			if w, ok := wiredAgents[e.NHI]; !ok {
				risks = append(risks, Risk{"used-but-not-wired", "high", e.NHI, e.Short,
					"used by " + root + " but not in any agent config (inherited from the environment?) via " + strings.Join(e.Via, ",")})
			} else if !w[root] {
				risks = append(risks, Risk{"used-but-not-wired", "high", e.NHI, e.Short,
					"used by " + root + " but scan found it wired only to: " + strings.Join(slices.Sorted(maps.Keys(w)), ", ")})
			}
		}
	}

	slices.SortFunc(risks, func(a, b Risk) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.NHI, b.NHI), strings.Compare(a.Detail, b.Detail))
	})
	return risks
}
