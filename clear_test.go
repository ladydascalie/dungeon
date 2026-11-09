package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearSetup builds one encounter, settles the units it is given, and chdirs
// into the encounter's parent so a bare `dungeon clear` sweeps what the shell
// sees.
func clearSetup(t *testing.T, statuses map[string]string) (planPath, dir string) {
	t.Helper()
	repo := gitRepo(t)
	planPath, _, dir = encounter(t, repo, strings.Replace(twoWorkerPlan, "%REPO%", repo, 1))
	for unit, status := range statuses {
		if code, _, stderr := runCLI("set", "unit", unit, status, "--plan", planPath); code != 0 {
			t.Fatalf("set %s %s: %s", unit, status, stderr)
		}
	}
	t.Chdir(dir)
	return planPath, dir
}

// runEntries writes the briefs, the reports and the sessions a finished run
// leaves beside its plan.
func runEntries(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"briefs", "reports", "sessions"} {
		path := filepath.Join(dir, ".dungeon", name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "A.md"), []byte("done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClearDeletesAFinishedPlanAndKeepsTheRun(t *testing.T) {
	planPath, dir := clearSetup(t, map[string]string{"A": "done", "B": "canceled"})
	runEntries(t, dir)

	code, stdout, stderr := runCLI("clear")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "cleared .dungeon/plan.yaml (enc)") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(planPath); !os.IsNotExist(err) {
		t.Fatalf("the plan should be gone: %v", err)
	}
	for _, name := range []string{"state.json", "briefs", "reports", "sessions"} {
		if _, err := os.Stat(filepath.Join(dir, ".dungeon", name)); err != nil {
			t.Fatalf("%s must survive a clear without --remove: %v", name, err)
		}
	}
	if !strings.Contains(stdout, "--remove deletes them with the plan") {
		t.Fatalf("clear should say how to delete the run too:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--remove-worktrees deletes them") {
		t.Fatalf("clear should say how to delete the lanes too:\n%s", stdout)
	}
}

func TestClearKeepsAPlanWithOpenUnits(t *testing.T) {
	planPath, _ := clearSetup(t, map[string]string{"A": "done", "B": "blocked"})

	code, stdout, stderr := runCLI("clear")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "kept .dungeon/plan.yaml: 1 unit not settled: B blocked") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("the plan must stay: %v", err)
	}
}

func TestClearKeepsAPlanWithNoUnits(t *testing.T) {
	path := writePlan(t, `encounter: empty
repo: /tmp/repo
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
units: []
`)

	code, stdout, stderr := runCLI("clear", path)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "no units, so nothing was done") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an untouched plan must stay: %v", err)
	}
}

func TestClearRemoveDeletesTheRun(t *testing.T) {
	repo := gitRepo(t)
	dir := t.TempDir()
	planPath := filepath.Join(dir, ".dungeon", "fix-a", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := strings.Replace(twoWorkerPlan, "%REPO%", repo, 1)
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := StatePath(planPath)
	st, _ := LoadState(statePath)
	st.Encounter = "enc"
	if err := st.SetUnit("A", "done", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUnit("B", "canceled", ""); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(statePath, st); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"briefs", "reports", "sessions"} {
		if err := os.MkdirAll(filepath.Join(filepath.Dir(planPath), name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	code, stdout, stderr := runCLI("clear", "--remove", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "cleared") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Dir(planPath)); !os.IsNotExist(err) {
		t.Fatalf("a named encounter directory should be gone: %v", err)
	}
}

// The default plan shares .dungeon with every named encounter, so --remove must
// delete the run and leave the neighbours alone.
func TestClearRemoveLeavesTheSharedDungeon(t *testing.T) {
	planPath, dir := clearSetup(t, map[string]string{"A": "done", "B": "done"})
	runEntries(t, dir)
	sibling := filepath.Join(dir, ".dungeon", "other", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("encounter: other\nrepo: /tmp/repo\nbase: main\nminions:\n  - {id: w1, role: worker, worktree: w1}\nunits: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("clear", "--remove", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	for _, name := range []string{"state.json", "briefs", "reports", "sessions", "plan.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, ".dungeon", name)); !os.IsNotExist(err) {
			t.Fatalf(".dungeon/%s should be gone after --remove: %v", name, err)
		}
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("the neighbouring encounter must stay: %v", err)
	}
}

func TestClearDryRunTouchesNothing(t *testing.T) {
	planPath, dir := clearSetup(t, map[string]string{"A": "done", "B": "done"})
	runEntries(t, dir)

	code, stdout, stderr := runCLI("clear", "--dry-run", "--remove")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "would clear .dungeon/plan.yaml (enc)") {
		t.Fatalf("output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "dry run: nothing was deleted") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("the plan must stay: %v", err)
	}
	for _, name := range []string{"state.json", "briefs", "reports", "sessions"} {
		if _, err := os.Stat(filepath.Join(dir, ".dungeon", name)); err != nil {
			t.Fatalf("%s must stay: %v", name, err)
		}
	}
}

func TestClearRefusesAFinishedPlanWithALiveLane(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "w1-ab:idle\nw2-ab:idle\n")
	planPath, statePath, dir := encounter(t, repo, strings.Replace(twoWorkerPlan, "%REPO%", repo, 1))
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	st.Encounter = "enc"
	st.Token = "ab"
	if err := st.SetUnit("A", "done", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUnit("B", "canceled", ""); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(statePath, st); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	code, stdout, stderr := runCLI("clear")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "lane w1 is still live") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("a live lane must keep its plan: %v", err)
	}
}

func TestClearSkipsABrokenPlanAndStillClearsTheRest(t *testing.T) {
	planPath, dir := clearSetup(t, map[string]string{"A": "done", "B": "done"})
	broken := filepath.Join(dir, ".dungeon", "broken", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	src := `encounter: broken
repo: /tmp/repo
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: A
    minion: ghost
    goal: Nothing owns this.
`
	if err := os.WriteFile(broken, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("clear")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "skipped .dungeon/broken/plan.yaml") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(broken); err != nil {
		t.Fatalf("a plan that does not validate must stay: %v", err)
	}
	if _, err := os.Stat(planPath); !os.IsNotExist(err) {
		t.Fatalf("the finished plan should still be cleared: %v", err)
	}
}

func TestClearWithNoEncounters(t *testing.T) {
	t.Chdir(t.TempDir())

	code, stdout, stderr := runCLI("clear")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stderr, "no encounters") {
		t.Fatalf("stderr:\n%s", stderr)
	}
}

func TestClearRemoveSweepsTheRunOfAClearedPlan(t *testing.T) {
	_, dir := clearSetup(t, map[string]string{"A": "done", "B": "done"})
	runEntries(t, dir)

	if code, _, stderr := runCLI("clear"); code != 0 {
		t.Fatalf("first clear: %s", stderr)
	}

	code, stdout, stderr := runCLI("clear", "--remove")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "removed the run in .dungeon") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".dungeon")); !os.IsNotExist(err) {
		t.Fatalf("an empty encounter directory should go: %v", err)
	}
}

func TestClearKeepsTheRunOfAnUnsettledPlan(t *testing.T) {
	planPath, dir := clearSetup(t, map[string]string{"A": "done", "B": "blocked"})
	runEntries(t, dir)
	if err := os.Remove(planPath); err != nil { // as a plain clear would leave it
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("clear", "--remove")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "kept the run in .dungeon: 1 unit not settled: B blocked") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".dungeon", "state.json")); err != nil {
		t.Fatalf("the run must stay: %v", err)
	}
}

func TestClearKeepsARunWithNoState(t *testing.T) {
	planPath, dir := clearSetup(t, map[string]string{"A": "done", "B": "done"})
	runEntries(t, dir)
	for _, path := range []string{planPath, filepath.Join(dir, ".dungeon", "state.json")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	code, stdout, stderr := runCLI("clear", "--remove")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "no state.json, so clear cannot tell what happened") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".dungeon", "briefs")); err != nil {
		t.Fatalf("a run with no state must stay: %v", err)
	}
}

func TestClearRefusesARunWithALiveLane(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "w1-ab:idle\nw2-ab:idle\n")
	planPath, statePath, dir := encounter(t, repo, strings.Replace(twoWorkerPlan, "%REPO%", repo, 1))
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	st.Encounter = "enc"
	st.Token = "ab"
	if err := st.SetMinion("w1", "spawned", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUnit("A", "done", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUnit("B", "done", ""); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(statePath, st); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(planPath); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	code, stdout, stderr := runCLI("clear", "--remove")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "kept the run in .dungeon: lane w1 is still live") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".dungeon", "state.json")); err != nil {
		t.Fatalf("a live lane must keep its run: %v", err)
	}
}

func TestClearRemovesTheLanesWhenAsked(t *testing.T) {
	planPath, _, repo := landSetup(t)
	for _, unit := range []string{"A", "B"} {
		if code, _, stderr := runCLI("set", "unit", unit, "done", "--plan", planPath); code != 0 {
			t.Fatalf("set %s: %s", unit, stderr)
		}
	}
	w1 := worktreePath(repo, "enc", "w1")
	// A lane is live until down closes it, and clear refuses a live lane.
	if code, _, stderr := runCLI("down", "--plan", planPath); code != 0 {
		t.Fatalf("down: %s", stderr)
	}

	code, stdout, stderr := runCLI("clear", "--remove", "--remove-worktrees", "--dry-run", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "would remove lane enc/w1 at") {
		t.Fatalf("output:\n%s", stdout)
	}
	if _, err := os.Stat(w1); err != nil {
		t.Fatalf("a dry run must keep the lane: %v", err)
	}

	code, stdout, stderr = runCLI("clear", "--remove", "--remove-worktrees", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "removed lane enc/w1 at") {
		t.Fatalf("output:\n%s", stdout)
	}
	for _, unit := range []string{"w1", "w2"} {
		if _, err := os.Stat(worktreePath(repo, "enc", unit)); !os.IsNotExist(err) {
			t.Fatalf("lane %s should be gone: %v", unit, err)
		}
		if _, err := gitRun(repo, "rev-parse", "--verify", "refs/heads/enc/"+unit); err == nil {
			t.Fatalf("branch enc/%s should be gone", unit)
		}
	}
	if _, err := os.Stat(filepath.Dir(planPath)); !os.IsNotExist(err) {
		t.Fatalf("the encounter directory should be gone: %v", err)
	}
}
