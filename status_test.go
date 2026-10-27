package main

import (
	"bytes"
	"strings"
	"testing"
)

const statusPlan = `encounter: st
minions:
  - {id: w1, role: worker, worktree: wt1}
  - {id: r1, role: reviewer, of: w1}
units:
  - id: A
    minion: w1
    paths: [a/**]
    goal: first
  - id: B
    minion: w1
    paths: [b/**]
    deps: [A]
    goal: second
  - id: C
    minion: r1
    deps: [A, B]
    goal: third
`

func TestSnapshotReadiness(t *testing.T) {
	p := decodeOK(t, statusPlan)
	st, _ := LoadState(t.TempDir() + "/missing.json")

	s := buildSnapshot(p, "plan.yaml", st, nil)
	byID := map[string]UnitRow{}
	for _, u := range s.Units {
		byID[u.ID] = u
	}

	if got := byID["A"].Status; got != "ready" {
		t.Fatalf("A: %q, want ready", got)
	}
	if got := byID["B"].Status; got != "waiting" {
		t.Fatalf("B: %q, want waiting", got)
	}
	if got := strings.Join(byID["B"].WaitsOn, ","); got != "A" {
		t.Fatalf("B waits on %q, want A", got)
	}
	if s.Counts["ready"] != 1 || s.Counts["waiting"] != 2 {
		t.Fatalf("counts %v", s.Counts)
	}

	st.SetUnit("A", "done", "landed")
	s = buildSnapshot(p, "plan.yaml", st, nil)
	byID = map[string]UnitRow{}
	for _, u := range s.Units {
		byID[u.ID] = u
	}
	if got := byID["B"].Status; got != "ready" {
		t.Fatalf("B after A done: %q, want ready", got)
	}
	if got := byID["B"].DepsDone; got != 1 {
		t.Fatalf("B deps done %d, want 1", got)
	}
}

func TestSnapshotSectionOrder(t *testing.T) {
	p := decodeOK(t, statusPlan)
	st, _ := LoadState(t.TempDir() + "/missing.json")
	st.SetUnit("C", "blocked", "needs you")
	st.SetUnit("A", "working", "")

	s := buildSnapshot(p, "plan.yaml", st, nil)
	if s.Units[0].ID != "C" || s.Units[1].ID != "A" {
		t.Fatalf("blocked and working should come first, got %s %s", s.Units[0].ID, s.Units[1].ID)
	}
}

func TestSnapshotPanesComeFromHerdr(t *testing.T) {
	p := decodeOK(t, statusPlan)
	st, _ := LoadState(t.TempDir() + "/missing.json")
	agents := map[string]Agent{"w1": {Name: "w1", Pane: "w9:p1", Status: "working"}}

	s := buildSnapshot(p, "plan.yaml", st, agents)
	if s.Units[0].Pane != "w9:p1" || s.Units[0].Agent != "working" {
		t.Fatalf("A row %+v", s.Units[0])
	}
	if s.Minions[0].Status != "spawned" {
		t.Fatalf("a live agent should mark the minion spawned, got %q", s.Minions[0].Status)
	}
	if s.Minions[1].Pane != "" {
		t.Fatalf("r1 has no live agent, got pane %q", s.Minions[1].Pane)
	}
}

func TestRenderText(t *testing.T) {
	p := decodeOK(t, statusPlan)
	st, _ := LoadState(t.TempDir() + "/missing.json")
	st.SetUnit("A", "working", "")
	st.SetUnit("C", "blocked", "needs you")

	var buf bytes.Buffer
	renderText(&buf, buildSnapshot(p, "plan.yaml", st, nil))
	out := buf.String()

	for _, want := range []string{"WORKING (1)", "BLOCKED (1)", "WAITING (1)", "MINIONS (2)", "needs you"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ACCEPTED") {
		t.Fatalf("empty sections should not print:\n%s", out)
	}
}
