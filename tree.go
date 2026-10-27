package main

import "sort"

// A DAG is not a tree, so nesting needs a rule that loses nothing.
//
// Every unit is placed under its deepest dependency, ties going to the later
// one in urgency order. That gives a forest where each unit appears exactly
// once, and a connector under a unit means "this one unblocks that one". A unit
// with more than one input carries a join marker naming how many it waits on,
// so a fan-in is visible without drawing four edges.
//
// Depth is the longest path from a root, so a chain reads top to bottom.

type forestNode struct {
	ID       string
	Inputs   int
	Children []*forestNode
}

// buildForest returns the roots. Every unit in the snapshot appears exactly
// once, depth first, ordered by urgency.
func buildForest(s Snapshot) []*forestNode {
	byID := make(map[string]UnitRow, len(s.Units))
	urgency := make(map[string]int, len(s.Units))
	for i, u := range s.Units {
		byID[u.ID] = u
		urgency[u.ID] = i // the snapshot is already in urgency order
	}

	depth := map[string]int{}
	var depthOf func(id string) int
	depthOf = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		d := 0
		for _, dep := range byID[id].Inputs {
			if _, ok := byID[dep]; !ok {
				continue
			}
			d = max(d, depthOf(dep)+1)
		}
		depth[id] = d
		return d
	}

	parent := map[string]string{}
	for _, u := range s.Units {
		best := ""
		for _, dep := range u.Inputs {
			if _, ok := byID[dep]; !ok {
				continue
			}
			if best == "" ||
				depthOf(dep) > depthOf(best) ||
				(depthOf(dep) == depthOf(best) && urgency[dep] > urgency[best]) {
				best = dep
			}
		}
		parent[u.ID] = best
	}

	children := map[string][]string{}
	var roots []string
	for _, u := range s.Units {
		if parent[u.ID] == "" {
			roots = append(roots, u.ID)
			continue
		}
		children[parent[u.ID]] = append(children[parent[u.ID]], u.ID)
	}

	byUrgency := func(ids []string) {
		sort.SliceStable(ids, func(i, j int) bool { return urgency[ids[i]] < urgency[ids[j]] })
	}
	byUrgency(roots)
	for id := range children {
		byUrgency(children[id])
	}

	var build func(id string) *forestNode
	build = func(id string) *forestNode {
		n := &forestNode{ID: id, Inputs: len(byID[id].Inputs)}
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
		out = append(out, treeLine{ID: n.ID, Prefix: prefix + branch})
		for i, c := range n.Children {
			walk(c, childPrefix, depth+1, i == len(n.Children)-1, false)
		}
	}
	for _, r := range roots {
		walk(r, "", 0, true, true)
	}
	return out
}
