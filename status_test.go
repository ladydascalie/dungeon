package main

import (
	"bytes"
	"encoding/json"
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

func TestStatusRowsCollapseWhitespace(t *testing.T) {
	src := strings.Replace(statusPlan, "    goal: first",
		"    goal: |\n      first line\n      second line", 1)
	p := decodeOK(t, src)
	st, _ := LoadState(t.TempDir() + "/missing.json")
	st.SetUnit("A", "working", "note one\nnote two")

	var buf bytes.Buffer
	renderText(&buf, buildSnapshot(p, "plan.yaml", st, nil))
	out := buf.String()

	if !strings.Contains(out, "note one note two: first line second line") {
		t.Fatalf("a multi-line goal and note should render on one row:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "second line" || strings.TrimSpace(line) == "note two" {
			t.Fatalf("a block scalar leaked a line of its own:\n%s", out)
		}
	}
}

func TestStatusShowsFailed(t *testing.T) {
	p := decodeOK(t, statusPlan)
	st, _ := LoadState(t.TempDir() + "/missing.json")
	st.SetUnit("A", "failed", "review concluded FAIL")

	var buf bytes.Buffer
	renderText(&buf, buildSnapshot(p, "plan.yaml", st, nil))
	out := buf.String()
	if !strings.Contains(out, "FAILED (1)") || !strings.Contains(out, "review concluded FAIL") {
		t.Fatalf("a failed unit needs its own section:\n%s", out)
	}
}

const loopStatusPlan = `encounter: ls
minions:
  - {id: w1, role: worker, worktree: w1}
  - {id: r1, role: reviewer, of: w1}
units:
  - id: A
    minion: w1
    paths: [a/**]
    goal: Do A.
    accept: "true"
loops:
  - id: polish
    reviewer: r1
    fixer: w1
    rounds: 2
    focus: error handling
    pass: "Merge verdict: PASS"
    paths: [a/**]
    deps: [A]
`

func expandPlanOK(t *testing.T, src string) Plan {
	t.Helper()
	p := decodeOK(t, src)
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("plan is not valid: %v", f)
	}
	e, f := ExpandLoops(p)
	if len(f) > 0 {
		t.Fatalf("expand: %v", f)
	}
	return e
}

func loopRowOf(t *testing.T, st State) LoopRow {
	t.Helper()
	p := expandPlanOK(t, loopStatusPlan)
	s := buildSnapshot(p, "plan.yaml", st, nil)
	if len(s.Loops) != 1 {
		t.Fatalf("got %d loop rows, want 1", len(s.Loops))
	}
	return s.Loops[0]
}

func TestSnapshotLoopRow(t *testing.T) {
	st, _ := LoadState(t.TempDir() + "/missing.json")
	l := loopRowOf(t, st)

	if l.ID != "polish" || l.Focus != "error handling" || l.Rounds != 2 {
		t.Fatalf("loop row %+v", l)
	}
	if l.Reviewer != "r1" || l.Fixer != "w1" || l.Pass != "Merge verdict: PASS" {
		t.Fatalf("loop row %+v", l)
	}
	want := "polish-review-1 polish-fix-1 polish-review-2 polish-fix-2 polish-review-3"
	if got := strings.Join(l.Units, " "); got != want {
		t.Fatalf("units %q, want %q", got, want)
	}
	if l.Started || l.Passed {
		t.Fatalf("a fresh loop has not started or passed: %+v", l)
	}
	if l.Round != 1 || l.State != "waiting" {
		t.Fatalf("round %d state %q, want 1 waiting", l.Round, l.State)
	}
}

// The state is the loop's current action, most urgent first.
func TestLoopStatePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     map[string]string
		want    string
		wantRnd int
		started bool
		passed  bool
	}{
		{
			name: "an unsatisfied dep keeps the loop waiting",
			set:  map[string]string{},
			want: "waiting", wantRnd: 1,
		},
		{
			name: "a satisfied dep makes the first review ready",
			set:  map[string]string{"A": "done"},
			want: "ready", wantRnd: 1,
		},
		{
			name: "a working unit wins over everything",
			set: map[string]string{
				"A": "done", "polish-review-1": "failed", "polish-fix-1": "working",
			},
			want: "working", wantRnd: 1, started: true,
		},
		{
			name: "a failed review is the loop state even with a ready fix",
			set: map[string]string{
				"A": "done", "polish-review-1": "failed", "polish-fix-1": "planned",
			},
			want: "failed", wantRnd: 1, started: true,
		},
		{
			name: "a failed review still leads past an unproven fix",
			set: map[string]string{
				"A": "done", "polish-review-1": "failed", "polish-fix-1": "unproven",
			},
			want: "failed", wantRnd: 2, started: true,
		},
		{
			name: "a passed review ends the loop",
			set: map[string]string{
				"A": "done", "polish-review-1": "done", "polish-fix-1": "canceled",
				"polish-review-2": "canceled", "polish-fix-2": "canceled",
				"polish-review-3": "canceled",
			},
			want: "done", wantRnd: 1, started: true, passed: true,
		},
		{
			name: "a blocked unit leaves the loop waiting, not done",
			set: map[string]string{
				"A": "done", "polish-review-1": "blocked",
			},
			want: "waiting", wantRnd: 1, started: true,
		},
		{
			name: "the final review clamps to the cap",
			set: map[string]string{
				"A": "done", "polish-review-1": "failed", "polish-fix-1": "unproven",
				"polish-review-2": "failed", "polish-fix-2": "unproven",
				"polish-review-3": "working",
			},
			want: "working", wantRnd: 2, started: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := LoadState(t.TempDir() + "/missing.json")
			for id, status := range tc.set {
				if err := st.SetUnit(id, status, ""); err != nil {
					t.Fatal(err)
				}
			}
			l := loopRowOf(t, st)
			if l.State != tc.want || l.Round != tc.wantRnd {
				t.Fatalf("state %q round %d, want %q round %d",
					l.State, l.Round, tc.want, tc.wantRnd)
			}
			if l.Started != tc.started || l.Passed != tc.passed {
				t.Fatalf("started %v passed %v, want %v %v",
					l.Started, l.Passed, tc.started, tc.passed)
			}
		})
	}
}

func TestRenderTextLoops(t *testing.T) {
	p := expandPlanOK(t, loopStatusPlan)
	st, _ := LoadState(t.TempDir() + "/missing.json")
	st.SetUnit("A", "done", "landed")
	st.SetUnit("polish-review-1", "working", "")

	var buf bytes.Buffer
	renderText(&buf, buildSnapshot(p, "plan.yaml", st, nil))
	out := buf.String()

	for _, want := range []string{"LOOPS (1)", "polish", "error handling", "round 1/2", "working"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "LOOPS") < strings.Index(out, "WORKING (1)") {
		t.Fatalf("the LOOPS block must follow the unit sections:\n%s", out)
	}
}

func TestStatusJSONCarriesLoops(t *testing.T) {
	path := writePlan(t, loopStatusPlan)
	code, stdout, stderr := runCLI("status", "--json", "--plan", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(stdout), &snap); err != nil {
		t.Fatalf("status --json: %v\n%s", err, stdout)
	}
	if len(snap.Loops) != 1 {
		t.Fatalf("got %d loop rows, want 1: %s", len(snap.Loops), stdout)
	}
	l := snap.Loops[0]
	if l.ID != "polish" || l.Rounds != 2 || l.Pass != "Merge verdict: PASS" {
		t.Fatalf("loop row %+v", l)
	}
	if len(l.Units) != 5 || l.Units[0] != "polish-review-1" {
		t.Fatalf("loop units %v", l.Units)
	}
}
