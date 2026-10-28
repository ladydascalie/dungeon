package main

import (
	"sort"
	"strings"
)

// A DAG is not a tree, so nesting needs a rule that loses nothing.
//
// Every unit is placed under its deepest dependency, ties going to the later
// one in urgency order. That gives a forest where each unit appears exactly
// once, and a connector under a unit means "this one unblocks that one". A unit
// with more than one input carries a join marker naming how many it waits on,
// so a fan-in is visible without drawing four edges.
//
// Depth is the longest path from a root, so a chain reads top to bottom.
//
// A loop is one synthetic node. Its entry deps place it in the same forest,
// and its generated units hang under it in round order instead of floating as
// free nodes. A loop is not a chain: its rounds read as a list, not as depth.

type forestNode struct {
	ID       string
	Label    string // display id override, empty means ID
	Inputs   int
	Loop     bool
	Children []*forestNode
}

// loopUnits maps a generated unit id to the loop that generated it.
func loopUnits(s Snapshot) (map[string]string, map[string][]string) {
	of := map[string]string{}
	units := map[string][]string{}
	for _, l := range s.Loops {
		units[l.ID] = l.Units
		for _, id := range l.Units {
			of[id] = l.ID
		}
	}
	return of, units
}

// buildForest returns the roots. Every unit and loop in the snapshot appears
// exactly once, depth first, ordered by urgency.
func buildForest(s Snapshot) []*forestNode {
	byID := make(map[string]UnitRow, len(s.Units))
	urgency := make(map[string]int, len(s.Units)+len(s.Loops))
	for i, u := range s.Units {
		byID[u.ID] = u
		urgency[u.ID] = i // the snapshot is already in urgency order
	}
	loopOf, loopKids := loopUnits(s)

	depsOf := map[string][]string{}
	nodes := map[string]*forestNode{}
	placed := map[string]bool{}

	// A loop node sits where its entry deps put it: the first review's deps.
	entry := func(l LoopRow) []string {
		kids := loopKids[l.ID]
		if len(kids) == 0 {
			return nil
		}
		return byID[kids[0]].Inputs
	}
	for i, l := range s.Loops {
		nodes[l.ID] = &forestNode{
			ID:     l.ID,
			Loop:   true,
			Inputs: len(entry(l)),
		}
		placed[l.ID] = true
		depsOf[l.ID] = entry(l)
		urgency[l.ID] = len(s.Units) + i
		if kids := loopKids[l.ID]; len(kids) > 0 {
			urgency[l.ID] = urgency[kids[0]] // the position of its first unit
		}
		for _, id := range loopKids[l.ID] {
			nodes[id] = &forestNode{ID: id, Label: strings.TrimPrefix(id, l.ID+"-"), Inputs: len(byID[id].Inputs)}
		}
	}

	for _, u := range s.Units {
		if loopOf[u.ID] != "" {
			continue
		}
		nodes[u.ID] = &forestNode{ID: u.ID, Inputs: len(u.Inputs)}
		placed[u.ID] = true
		depsOf[u.ID] = u.Inputs
	}

	nodeFor := func(dep string) string {
		if loop := loopOf[dep]; loop != "" {
			return loop
		}
		return dep
	}

	depth := map[string]int{}
	var depthOf func(id string) int
	depthOf = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		d := 0
		for _, dep := range depsOf[id] {
			target := nodeFor(dep)
			if _, ok := nodes[target]; !ok || target == id {
				continue
			}
			d = max(d, depthOf(target)+1)
		}
		depth[id] = d
		return d
	}

	parent := map[string]string{}
	for id := range placed {
		best := ""
		for _, dep := range depsOf[id] {
			target := nodeFor(dep)
			if _, ok := nodes[target]; !ok || target == id {
				continue
			}
			if best == "" ||
				depthOf(target) > depthOf(best) ||
				(depthOf(target) == depthOf(best) && urgency[target] > urgency[best]) {
				best = target
			}
		}
		parent[id] = best
	}

	children := map[string][]string{}
	var roots []string
	for id := range placed {
		if parent[id] == "" {
			roots = append(roots, id)
			continue
		}
		children[parent[id]] = append(children[parent[id]], id)
	}
	// A loop's children are its rounds, in round order, not urgency order.
	for _, l := range s.Loops {
		if _, ok := nodes[l.ID]; !ok {
			continue
		}
		children[l.ID] = append([]string(nil), loopKids[l.ID]...)
	}

	byUrgency := func(ids []string) {
		sort.SliceStable(ids, func(i, j int) bool { return urgency[ids[i]] < urgency[ids[j]] })
	}
	byUrgency(roots)
	for id := range children {
		if nodes[id].Loop {
			continue
		}
		byUrgency(children[id])
	}

	var build func(id string) *forestNode
	build = func(id string) *forestNode {
		n := nodes[id]
		for _, c := range children[id] {
			n.Children = append(n.Children, build(c))
		}
		return n
	}

	out := make([]*forestNode, 0, len(roots))
	for _, id := range roots {
		out = append(out, build(id))
	}
	return out
}

// treeLine is one rendered row: the unit, and the connectors to its left.
type treeLine struct {
	ID     string
	Prefix string
	Loop   bool   // a synthetic loop summary row
	Label  string // display id override, empty means ID
}

// maxIndent is the deepest the connectors go before they stop growing. A long
// chain must not eat the goal text: past this depth the connector repeats in
// place, which still reads as "under the same chain".
const maxIndent = 30

// pruneForest removes the units a caller hides and splices their children into
// the parent, so hiding settled work does not leave orphaned connectors or
// indent a chain by levels that are no longer on screen.
func pruneForest(roots []*forestNode, hide func(string) bool) []*forestNode {
	var out []*forestNode
	var prune func(n *forestNode) []*forestNode
	prune = func(n *forestNode) []*forestNode {
		var kids []*forestNode
		for _, c := range n.Children {
			kids = append(kids, prune(c)...)
		}
		if hide(n.ID) {
			return kids
		}
		n.Children = kids
		return []*forestNode{n}
	}
	for _, r := range roots {
		out = append(out, prune(r)...)
	}
	return out
}

// applyFolds removes the children of every folded node and reports how many
// descendants each fold hides, so a folded branch still says how much is under
// it. A fold on a leaf does nothing.
func applyFolds(roots []*forestNode, folded map[string]bool) ([]*forestNode, map[string]int) {
	counts := map[string]int{}
	var descendants func(n *forestNode) int
	descendants = func(n *forestNode) int {
		total := 0
		for _, c := range n.Children {
			total += 1 + descendants(c)
		}
		return total
	}
	var walk func(n *forestNode)
	walk = func(n *forestNode) {
		if folded[n.ID] && len(n.Children) > 0 {
			counts[n.ID] = descendants(n)
			n.Children = nil
			return
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return roots, counts
}

// foldable returns the ids that have children after hidden work is pruned.
func foldable(roots []*forestNode) []string {
	var out []string
	var walk func(n *forestNode)
	walk = func(n *forestNode) {
		if len(n.Children) > 0 {
			out = append(out, n.ID)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}

// flattenForest walks the forest depth first and returns every unit with its
// connector prefix.
func flattenForest(roots []*forestNode) []treeLine {
	var out []treeLine
	var walk func(n *forestNode, prefix string, depth int, last, isRoot bool)
	walk = func(n *forestNode, prefix string, depth int, last, isRoot bool) {
		branch, childPrefix := "", prefix
		switch {
		case isRoot:
		case runeLen(prefix) >= maxIndent:
			branch, childPrefix = "› ", prefix
		case last:
			branch, childPrefix = "└─", prefix+"  "
		default:
			branch, childPrefix = "├─", prefix+"│ "
		}
		out = append(out, treeLine{ID: n.ID, Prefix: prefix + branch, Loop: n.Loop, Label: n.Label})
		for i, c := range n.Children {
			walk(c, childPrefix, depth+1, i == len(n.Children)-1, false)
		}
	}
	for _, r := range roots {
		walk(r, "", 0, true, true)
	}
	return out
}
