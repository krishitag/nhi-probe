package trace

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// node is one step in a lineage tree.
type node struct {
	children map[string]*node
	calls    int
	use      string
}

// printLineage prints, for every NHI, its delegation chains as a tree:
//
//	22b3ed9d5110  gith...IAdF
//	└─ claude-code
//	   └─ subagent:general-purpose
//	      └─ mcp:github/list_issues  (1 call, implicit)
func printLineage(w io.Writer, edges []Edge) {
	roots := map[string]*node{}
	labels := map[string]string{}
	for _, e := range edges {
		if roots[e.NHI] == nil {
			roots[e.NHI] = &node{children: map[string]*node{}}
			labels[e.NHI] = e.Short + "  " + e.Redacted
		}
		n := roots[e.NHI]
		for _, step := range strings.Split(e.Path, " > ") {
			if n.children[step] == nil {
				n.children[step] = &node{children: map[string]*node{}}
			}
			n = n.children[step]
		}
		n.calls += e.Calls
		n.use = e.Use
	}
	for _, nhi := range slices.Sorted(maps.Keys(roots)) {
		fmt.Fprintln(w, labels[nhi])
		printChildren(w, roots[nhi], "")
		fmt.Fprintln(w)
	}
}

func printChildren(w io.Writer, n *node, indent string) {
	names := slices.Sorted(maps.Keys(n.children))
	for i, name := range names {
		child := n.children[name]
		branch, next := "├─ ", "│  "
		if i == len(names)-1 {
			branch, next = "└─ ", "   "
		}
		line := indent + branch + name
		if child.calls > 0 {
			s := "s"
			if child.calls == 1 {
				s = ""
			}
			line += fmt.Sprintf("  (%d call%s, %s)", child.calls, s, child.use)
		}
		fmt.Fprintln(w, line)
		printChildren(w, child, indent+next)
	}
}
