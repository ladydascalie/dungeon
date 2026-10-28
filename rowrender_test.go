package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCollapseSpace(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb", "a b"},
		{" a \t b\n\n c ", "a b c"},
		{"a  b", "a b"},
		{"", ""},
		{"   \n\t ", ""},
	} {
		if got := collapseSpace(tc.in); got != tc.want {
			t.Fatalf("collapseSpace(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const multilineGoalPlan = `encounter: st
minions:
  - {id: w1, role: worker, worktree: wt1}
units:
  - id: A
    minion: w1
    paths: [a/**]
    goal: |
      first line
        second line
      third line
`

func TestMultilineGoalRendersOneLine(t *testing.T) {
	plan := writePlan(t, multilineGoalPlan)
	m := newTestView(t, plan, filepath.Join(t.TempDir(), "state.json"))

	u := m.rowByID("A")
	if u.ID != "A" {
		t.Fatal("no row A")
	}
	c := m.rowParts(unitRow(u), "", "", "")
	mt := m.metricsOf([]rowCells{c}, max(10, m.width-1))
	line := m.rowLine(unitRow(u), m.width, "", "", "", mt)

	if strings.Contains(line, "\n") {
		t.Fatalf("a multi-line goal broke the row:\n%s", line)
	}
	if !strings.Contains(line, "first line second line third line") {
		t.Fatalf("the goal was not collapsed onto one line: %q", line)
	}
	lines, _ := m.listLines(m.width)
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "first line") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the goal appears on %d list lines, want exactly one", n)
	}
}
