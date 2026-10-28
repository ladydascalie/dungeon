package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const loopPlan = `encounter: lp
repo: /repo
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
  - {id: polish, reviewer: r1, fixer: w1, rounds: 2, focus: error handling,
     pass: "Merge verdict: PASS", paths: [a/**]}
`

func expanded(t *testing.T) Plan {
	t.Helper()
	p := decodeOK(t, loopPlan)
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("plan is not valid: %v", f)
	}
	e, f := ExpandLoops(p)
	if len(f) > 0 {
		t.Fatalf("expand: %v", f)
	}
	return e
}

func unitByID(p Plan, id string) (Unit, bool) {
	for _, u := range p.Units {
		if u.ID == id {
			return u, true
		}
	}
	return Unit{}, false
}

func TestExpandLoops(t *testing.T) {
	p := expanded(t)
	var ids []string
	for _, u := range p.Units {
		ids = append(ids, u.ID)
	}
	want := []string{"A", "polish-review-1", "polish-fix-1", "polish-review-2", "polish-fix-2", "polish-review-3"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("units %v, want %v", ids, want)
	}

	review1, _ := unitByID(p, "polish-review-1")
	if review1.Minion != "r1" || review1.Verdict != "Merge verdict: PASS" || review1.Accept != "" {
		t.Fatalf("review-1 %+v", review1)
	}
	if !strings.Contains(review1.Goal, "error handling") {
		t.Fatalf("review-1 goal should name the focus: %q", review1.Goal)
	}
	if len(review1.Deps) != 0 {
		t.Fatalf("the first review waits on nothing, got %v", review1.Deps)
	}

	fix1, _ := unitByID(p, "polish-fix-1")
	if fix1.Minion != "w1" || strings.Join(fix1.Paths, ",") != "a/**" {
		t.Fatalf("fix-1 %+v", fix1)
	}
	if len(fix1.Deps) != 1 || fix1.Deps[0] != "polish-review-1" {
		t.Fatalf("fix-1 must follow its review, got %v", fix1.Deps)
	}
	if !strings.Contains(fix1.Goal, "polish-review-1") {
		t.Fatalf("fix-1 goal should name the review report: %q", fix1.Goal)
	}

	review2, _ := unitByID(p, "polish-review-2")
	if len(review2.Deps) != 1 || review2.Deps[0] != "polish-fix-1" {
		t.Fatalf("review-2 deps %v, want [polish-fix-1]", review2.Deps)
	}
	fix2, _ := unitByID(p, "polish-fix-2")
	if len(fix2.Deps) != 1 || fix2.Deps[0] != "polish-review-2" {
		t.Fatalf("fix-2 deps %v, want [polish-review-2]", fix2.Deps)
	}
	// The last fix must be judged, so rounds N produce a final review.
	review3, _ := unitByID(p, "polish-review-3")
	if len(review3.Deps) != 1 || review3.Deps[0] != "polish-fix-2" {
		t.Fatalf("review-3 deps %v, want [polish-fix-2]", review3.Deps)
	}
}

func TestExpandLoopsUsesCustomGoals(t *testing.T) {
	src := strings.Replace(loopPlan, "pass: \"Merge verdict: PASS\"",
		"pass: \"Merge verdict: PASS\", review_goal: Look hard., fix_goal: Fix hard.", 1)
	p := decodeOK(t, src)
	e, f := ExpandLoops(p)
	if len(f) > 0 {
		t.Fatalf("expand: %v", f)
	}
	review, _ := unitByID(e, "polish-review-1")
	if review.Goal != "Look hard." {
		t.Fatalf("review goal %q", review.Goal)
	}
	fix, _ := unitByID(e, "polish-fix-1")
	if fix.Goal != "Fix hard." {
		t.Fatalf("fix goal %q", fix.Goal)
	}
}

func TestExpandLoopsIsIdempotent(t *testing.T) {
	p := expanded(t)
	again, f := ExpandLoops(p)
	if len(f) > 0 {
		t.Fatalf("second expand: %v", f)
	}
	if len(again.Units) != len(p.Units) {
		t.Fatalf("second expand changed the count: %d, then %d", len(p.Units), len(again.Units))
	}
	for i := range p.Units {
		if p.Units[i].ID != again.Units[i].ID {
			t.Fatalf("unit %d: %q, then %q", i, p.Units[i].ID, again.Units[i].ID)
		}
	}
}

func TestExpandLoopsReportsACollision(t *testing.T) {
	src := strings.Replace(loopPlan, "units:\n", `units:
  - id: polish-review-1
    minion: w1
    paths: [a/**]
    goal: Not the loop's unit.
`, 1)
	p := decodeOK(t, src)
	_, f := ExpandLoops(p)
	wantCodes(t, f, EDuplicateID)
}

func TestLoadPlanExpandsLoops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(path, []byte(loopPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	p, f, err := loadPlan(path)
	if err != nil || len(f) > 0 {
		t.Fatalf("loadPlan: %v %v", f, err)
	}
	for _, id := range []string{"polish-fix-2", "polish-review-3"} {
		if _, ok := unitByID(p, id); !ok {
			t.Fatalf("loadPlan did not expand %s: %d units", id, len(p.Units))
		}
	}
}

func TestStopLoopAfterCancelsLaterRounds(t *testing.T) {
	p := expanded(t)
	st := &State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	for _, u := range p.Units {
		st.SetUnit(u.ID, "planned", "")
	}
	st.SetUnit("polish-review-1", "done", "verdict")

	StopLoopAfter(p, st, "polish-review-1")

	for _, u := range p.Units {
		got := st.UnitStatus(u.ID)
		switch u.ID {
		case "polish-fix-1", "polish-review-2", "polish-fix-2", "polish-review-3":
			if got != "canceled" {
				t.Fatalf("%s is %q, want canceled", u.ID, got)
			}
			if want := "loop polish satisfied at round 1"; st.Units[u.ID].Note != want {
				t.Fatalf("%s note %q, want %q", u.ID, st.Units[u.ID].Note, want)
			}
		case "A":
			if got != "planned" {
				t.Fatalf("%s is %q, want planned", u.ID, got)
			}
		}
	}
}

func TestStopLoopAfterIgnoresNonReviewUnits(t *testing.T) {
	p := expanded(t)
	st := &State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	for _, u := range p.Units {
		st.SetUnit(u.ID, "planned", "")
	}
	StopLoopAfter(p, st, "polish-fix-1")
	StopLoopAfter(p, st, "A")
	for _, id := range []string{"polish-review-2", "polish-fix-2", "polish-review-3"} {
		if got := st.UnitStatus(id); got != "planned" {
			t.Fatalf("%s is %q, a non-review unit must cancel nothing", id, got)
		}
	}
}

// A rejected review cancels nothing: the fixer gets its turn and the next
// round stays planned.
func TestRejectedReviewLeavesLaterRoundsPlanned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report string
		want   string
	}{
		{name: "passing review cancels", report: "Merge verdict: PASS\n", want: "canceled"},
		{name: "rejected review leaves planned", report: "Merge verdict: FAIL\n", want: "planned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHerdr(t, "r1:idle\n")
			dir := t.TempDir()
			planPath := filepath.Join(dir, ".dungeon", "plan.yaml")
			p := expanded(t)
			p.Repo = dir
			if err := os.MkdirAll(filepath.Dir(reportPath(planPath, "polish-review-1")), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(reportPath(planPath, "polish-review-1"), []byte(tc.report), 0o644); err != nil {
				t.Fatal(err)
			}
			st := &State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
			for _, u := range p.Units {
				st.SetUnit(u.ID, "planned", "")
			}
			st.SetUnit("polish-review-1", "working", "")

			o := dispatchOpts{planPath: planPath, statePath: filepath.Join(dir, "state.json")}
			reconcile(t.Context(), newHerdrClient(), o, p, st, &digest{})

			if got := st.UnitStatus("polish-review-1"); got != "done" && got != "rejected" {
				t.Fatalf("review-1 is %q, want done or rejected: %s", got, st.Units["polish-review-1"].Note)
			}
			for _, id := range []string{"polish-fix-1", "polish-review-2", "polish-fix-2", "polish-review-3"} {
				if got := st.UnitStatus(id); got != tc.want {
					t.Fatalf("%s is %q, want %q", id, got, tc.want)
				}
			}
		})
	}
}

func TestCheckSeesExpandedUnits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(path, []byte(loopPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI("check", path)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "6 units") {
		t.Fatalf("check should count the expanded units: %q", stdout)
	}
}
