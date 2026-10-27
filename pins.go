package main

import "fmt"

// A pin is a request, not a fact. The plan may say which pane a minion should
// run in, because a pin the operator chose is where their attention already is.
// Two minions in one pane is never what anyone meant, so the plan is refused.

func duplicatePanes(minions []Minion) []Finding {
	var out []Finding
	seen := map[string]string{}
	for i, m := range minions {
		if m.Pane == "" {
			continue
		}
		if first, dup := seen[m.Pane]; dup {
			out = append(out, Finding{
				EDuplicatePane, fmt.Sprintf("minions[%d].pane", i),
				fmt.Sprintf("pane %q is already pinned by %q", m.Pane, first),
			})
			continue
		}
		seen[m.Pane] = m.ID
	}
	return out
}
