package main

import (
	"fmt"
	"strings"
	"testing"
)

const diamondPlan = `encounter: d
minions:
  - {id: w1, role: worker, worktree: wt1}
units:
  - {id: A, minion: w1, paths: [a/**], goal: root}
  - {id: B, minion: w1, paths: [b/**], deps: [A], goal: left}
  - {id: C, minion: w1, paths: [c/**], deps: [A], goal: right}
  - {id: D, minion: w1, paths: [d/**], deps: [B, C], goal: join}
`

func forestOf(t *testing.T, plan string) []*forestNode {
	t.Helper()
	p := decodeOK(t, plan)
	st, _ := LoadState(t.TempDir() + "/missing.json")
	return buildForest(buildSnapshot(p, "plan.yaml", st, nil))
}

func idsIn(lines []treeLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.ID)
	}
	return out
}

func TestForestPlacesEveryUnitExactlyOnce(t *testing.T) {
	roots := forestOf(t, diamondPlan)
	got := idsIn(flattenForest(roots))
	want := []string{"A", "B", "C", "D"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestForestNestsUnderTheDeepestDependency(t *testing.T) {
	roots := forestOf(t, diamondPlan)
	if len(roots) != 1 || roots[0].ID != "A" {
		t.Fatalf("one root expected, got %d", len(roots))
	}
	// D waits on B and C at the same depth, so it hangs under the later one.
	c := roots[0].Children[1]
	if c.ID != "C" || len(c.Children) != 1 || c.Children[0].ID != "D" {
		t.Fatalf("D should hang under C, got %+v", c.Children)
	}
	if c.Children[0].Inputs != 2 {
		t.Fatalf("D should report 2 inputs, got %d", c.Children[0].Inputs)
	}
}

func TestForestConnectors(t *testing.T) {
	roots := forestOf(t, diamondPlan)
	lines := flattenForest(roots)
	prefix := map[string]string{}
	for _, l := range lines {
		prefix[l.ID] = l.Prefix
	}
	if prefix["A"] != "" {
		t.Fatalf("a root needs no connector, got %q", prefix["A"])
	}
	if prefix["B"] != "├─" || prefix["C"] != "└─" {
		t.Fatalf("siblings: B %q C %q", prefix["B"], prefix["C"])
	}
	if prefix["D"] != "  └─" {
		t.Fatalf("D should sit under C, got %q", prefix["D"])
	}
}

func TestForestHidingKeepsTheChildren(t *testing.T) {
	roots := forestOf(t, diamondPlan)
	hide := func(id string) bool { return id == "A" }
	got := idsIn(flattenForest(pruneForest(roots, hide)))
	if strings.Join(got, ",") != "B,C,D" {
		t.Fatalf("hiding a settled root must keep its children, got %v", got)
	}
}

func TestForestCapsTheIndent(t *testing.T) {
	units := []Unit{{ID: "U0", Minion: "w1", Paths: []string{"a/**"}}}
	for i := 1; i < 20; i++ {
		units = append(units, Unit{
			ID: fmt.Sprintf("U%d", i), Minion: "w1", Paths: []string{"a/**"},
			Deps: []string{fmt.Sprintf("U%d", i-1)},
		})
	}
	p := Plan{
		Encounter: "chain",
		Minions:   []Minion{{ID: "w1", Role: "worker", Worktree: "wt"}},
		Units:     units,
	}
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("findings: %v", f)
	}
	snap := buildSnapshot(p, "plan.yaml", State{}, nil)
	lines := flattenForest(buildForest(snap))
	if len(lines) != 20 {
		t.Fatalf("got %d lines, want 20", len(lines))
	}
	for _, l := range lines {
		if runeLen(l.Prefix) > maxIndent+2 {
			t.Fatalf("prefix %q is deeper than the cap", l.Prefix)
		}
	}
}

func TestApplyFoldsCountsWhatItHides(t *testing.T) {
	roots := forestOf(t, diamondPlan)
	folded, counts := applyFolds(roots, map[string]bool{"A": true})
	if counts["A"] != 3 {
		t.Fatalf("folding A hides %d, want 3", counts["A"])
	}
	got := idsIn(flattenForest(folded))
	if strings.Join(got, ",") != "A" {
		t.Fatalf("got %v, want just A", got)
	}
}

func TestFoldingALeafDoesNothing(t *testing.T) {
	roots := forestOf(t, diamondPlan)
	_, counts := applyFolds(roots, map[string]bool{"D": true})
	if len(counts) != 0 {
		t.Fatalf("a leaf has nothing to hide, got %v", counts)
	}
}

func TestFoldableListsOnlyNodesWithChildren(t *testing.T) {
	got := foldable(forestOf(t, diamondPlan))
	if strings.Join(got, ",") != "A,C" {
		t.Fatalf("got %v, want A,C", got)
	}
}
