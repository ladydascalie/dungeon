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

const loopForestPlan = `encounter: lf
minions:
  - {id: w1, role: worker, worktree: wt1}
  - {id: r1, role: reviewer, of: w1}
units:
  - {id: A, minion: w1, paths: [a/**], goal: root}
loops:
  - {id: polish, reviewer: r1, fixer: w1, rounds: 2, focus: correctness,
     pass: "Merge verdict: PASS", paths: [a/**], deps: [A]}
`

// loopForestOf expands the loops, then builds the forest from the same
// snapshot the view sees. A test that skipped expansion would see no loop
// units at all.
func loopForestOf(t *testing.T, src string, set map[string]string) []*forestNode {
	t.Helper()
	p := decodeOK(t, src)
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("plan is not valid: %v", f)
	}
	e, f := ExpandLoops(p)
	if len(f) > 0 {
		t.Fatalf("expand: %v", f)
	}
	st := State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	for id, status := range set {
		if err := st.SetUnit(id, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	return buildForest(buildSnapshot(e, "plan.yaml", st, nil))
}

func nodeByID(roots []*forestNode, id string) *forestNode {
	var walk func(*forestNode) *forestNode
	walk = func(n *forestNode) *forestNode {
		if n.ID == id {
			return n
		}
		for _, c := range n.Children {
			if found := walk(c); found != nil {
				return found
			}
		}
		return nil
	}
	for _, r := range roots {
		if found := walk(r); found != nil {
			return found
		}
	}
	return nil
}

// A loop hangs under the unit its first review waits on, not as a free root.
func TestForestNestsLoopUnderItsEntryDep(t *testing.T) {
	roots := loopForestOf(t, loopForestPlan, nil)
	if len(roots) != 1 || roots[0].ID != "A" {
		t.Fatalf("roots %v, want just A", idsIn(flattenForest(roots)))
	}
	loop := nodeByID(roots, "polish")
	if loop == nil || !loop.Loop {
		t.Fatalf("the loop is not a node: %v", idsIn(flattenForest(roots)))
	}
	if got := idsIn(flattenForest(roots)); strings.Join(got, ",") != "A,polish,polish-review-1,polish-fix-1,polish-review-2,polish-fix-2,polish-review-3" {
		t.Fatalf("flattened %v", got)
	}
}

// With no entry deps the loop takes the position of its first unit. A working
// review outranks a ready declared unit, so the loop comes first.
func TestForestPlacesLoopByItsFirstUnitWhenItHasNoEntryDeps(t *testing.T) {
	src := strings.Replace(loopForestPlan, ", deps: [A]}", "}", 1)
	roots := loopForestOf(t, src, map[string]string{"polish-review-1": "working"})
	got := idsIn(flattenForest(roots))
	if len(got) == 0 || got[0] != "polish" {
		t.Fatalf("the loop should take its first unit's place, got %v", got)
	}
	if nodeByID(roots, "A") == nil {
		t.Fatalf("A disappeared: %v", got)
	}
}

// Rounds read as a list even when a later round is the urgent one.
func TestForestKeepsLoopChildrenInRoundOrder(t *testing.T) {
	roots := loopForestOf(t, loopForestPlan, map[string]string{
		"A": "done", "polish-review-1": "done", "polish-fix-1": "unproven",
		"polish-review-2": "working",
	})
	loop := nodeByID(roots, "polish")
	if loop == nil {
		t.Fatal("no loop node")
	}
	var got []string
	for _, c := range loop.Children {
		got = append(got, c.ID)
	}
	want := []string{
		"polish-review-1", "polish-fix-1", "polish-review-2",
		"polish-fix-2", "polish-review-3",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("children %v, want round order %v", got, want)
	}
}

// A child row can drop the loop prefix because its parent names the loop.
func TestForestStripsTheLoopPrefixFromGeneratedIDs(t *testing.T) {
	roots := loopForestOf(t, loopForestPlan, nil)
	loop := nodeByID(roots, "polish")
	if loop == nil {
		t.Fatal("no loop node")
	}
	want := []string{"review-1", "fix-1", "review-2", "fix-2", "review-3"}
	for i, c := range loop.Children {
		if c.Label != want[i] {
			t.Fatalf("child %d label %q, want %q", i, c.Label, want[i])
		}
	}
}
