package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitIn writes a file in a worktree and commits it, the way a lane would.
func commitIn(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", file}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

const twoWorkerPlan = `encounter: enc
repo: %REPO%
base: main
concurrency: 2
minions:
  - {id: w1, role: worker, worktree: w1}
  - {id: w2, role: worker, worktree: w2}
units:
  - id: A
    minion: w1
    paths: [a.txt]
    goal: First change.
    accept: test -f a.txt
  - id: B
    minion: w2
    paths: [b.txt]
    goal: Second change.
    accept: test -f b.txt
`

func landSetup(t *testing.T) (planPath, statePath, repo string) {
	t.Helper()
	repo = gitRepo(t)
	fakeHerdr(t, "")
	planPath, statePath, _ = encounter(t, repo, strings.Replace(twoWorkerPlan, "%REPO%", repo, 1))
	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	return planPath, statePath, repo
}

func TestLandMergesInDeclarationOrderAndGates(t *testing.T) {
	planPath, _, repo := landSetup(t)
	commitIn(t, worktreePath(repo, "enc", "w1"), "a.txt", "one\n", "A")
	commitIn(t, worktreePath(repo, "enc", "w2"), "b.txt", "two\n", "B")
	for _, u := range []string{"A", "B"} {
		if code, _, stderr := runCLI("set", "unit", u, "done", "--plan", planPath); code != 0 {
			t.Fatalf("set %s: %s", u, stderr)
		}
	}

	code, stdout, stderr := runCLI("land", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "merged 2 units on enc/integration") {
		t.Fatalf("output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ok   A") || !strings.Contains(stdout, "ok   B") {
		t.Fatalf("both gates should run against the merged result:\n%s", stdout)
	}
	if !strings.Contains(stdout, "nothing was pushed") {
		t.Fatalf("land must say it did not push:\n%s", stdout)
	}
	integration := worktreePath(repo, "enc", "integration")
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(integration, f)); err != nil {
			t.Fatalf("%s should be in the integration worktree: %v", f, err)
		}
	}
}

func TestLandStopsOnAConflict(t *testing.T) {
	planPath, _, repo := landSetup(t)
	commitIn(t, worktreePath(repo, "enc", "w1"), "shared.txt", "one\n", "A")
	commitIn(t, worktreePath(repo, "enc", "w2"), "shared.txt", "two\n", "B")
	for _, u := range []string{"A", "B"} {
		if code, _, stderr := runCLI("set", "unit", u, "done", "--plan", planPath); code != 0 {
			t.Fatalf("set %s: %s", u, stderr)
		}
	}

	code, stdout, _ := runCLI("land", "--force", "--plan", planPath)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stdout, "conflict landing B") {
		t.Fatalf("output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "merged before the conflict: A") {
		t.Fatalf("output:\n%s", stdout)
	}
}

func TestLandWithNothingDone(t *testing.T) {
	planPath, _, _ := landSetup(t)
	code, stdout, stderr := runCLI("land", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to land") {
		t.Fatalf("output: %q", stdout)
	}
}

func TestLandReportsAFailedGate(t *testing.T) {
	planPath, _, repo := landSetup(t)
	commitIn(t, worktreePath(repo, "enc", "w1"), "a.txt", "one\n", "A")
	if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", planPath); code != 0 {
		t.Fatalf("set: %s", stderr)
	}
	// B never lands, so only A's gate runs, and it fails.
	plan := strings.Replace(twoWorkerPlan, "accept: test -f a.txt", "accept: false", 1)
	plan = strings.Replace(plan, "%REPO%", repo, 1)
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := runCLI("land", "--force", "--plan", planPath)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "FAIL A") {
		t.Fatalf("output:\n%s", stdout)
	}
}

func TestUncommittedWorkBlocksTheUnit(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	// The lane writes its file but does not commit it.
	wt := worktreePath(repo, "enc", "w1")
	if err := os.WriteFile(filepath.Join(wt, "NOTES.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := strings.Replace(planFor(repo), "paths: [src/**]", "paths: [NOTES.md]", 1)
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--force", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "blocked: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, _ := LoadState(statePath)
	if got := st.Units["A"].Note; !strings.Contains(got, "uncommitted work in NOTES.md") {
		t.Fatalf("note %q", got)
	}
}

func TestAChangeOutsideTheDeclaredPathsBlocks(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	wt := worktreePath(repo, "enc", "w1")
	if err := os.MkdirAll(filepath.Join(wt, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitIn(t, wt, "src/ok.go", "package x\n", "in scope")
	if err := os.WriteFile(filepath.Join(wt, "elsewhere.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := strings.Replace(planFor(repo), "paths: [src/**]", "paths: [src/**]", 1)
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--force", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "blocked: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, _ := LoadState(statePath)
	if got := st.Units["A"].Note; !strings.Contains(got, "outside your paths: elsewhere.txt") {
		t.Fatalf("note %q", got)
	}
}

// logGit puts a git on PATH that records each command and then runs the real
// one, so a test can count merges without mocking git.
func logGit(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "git.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GIT_LOG\"\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_LOG", log)
	return log
}

func mergeLines(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, " merge --no-ff ") {
			out = append(out, line)
		}
	}
	return out
}

const oneWorkerTwoUnits = `encounter: enc
repo: %REPO%
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: A
    minion: w1
    paths: [a.txt]
    goal: First change.
    accept: test -f a.txt
  - id: C
    minion: w1
    paths: [c.txt]
    goal: Second change.
    accept: test -f c.txt
`

// Two done units on one minion are one branch: merge it once, gate both.
func TestLandMergesOneBranchOncePerMinion(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	planPath, _, _ := encounter(t, repo, strings.Replace(oneWorkerTwoUnits, "%REPO%", repo, 1))
	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	wt := worktreePath(repo, "enc", "w1")
	commitIn(t, wt, "a.txt", "one\n", "A")
	commitIn(t, wt, "c.txt", "two\n", "C")
	for _, u := range []string{"A", "C"} {
		if code, _, stderr := runCLI("set", "unit", u, "done", "--plan", planPath); code != 0 {
			t.Fatalf("set %s: %s", u, stderr)
		}
	}

	log := logGit(t)
	code, stdout, stderr := runCLI("land", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "merged 2 units on enc/integration") {
		t.Fatalf("output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ok   A") || !strings.Contains(stdout, "ok   C") {
		t.Fatalf("every merged unit's accept should run:\n%s", stdout)
	}
	if merges := mergeLines(t, log); len(merges) != 1 {
		t.Fatalf("one branch should merge once, got %d:\n%s", len(merges), strings.Join(merges, "\n"))
	}
}

// The merge order follows the declaration order of each minion's first done
// unit, not the order of later units.
const orderPlan = `encounter: enc
repo: %REPO%
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
  - {id: w2, role: worker, worktree: w2}
units:
  - id: A
    minion: w1
    paths: [a.txt]
    goal: First change.
    accept: test -f a.txt
  - id: B
    minion: w2
    paths: [b.txt]
    goal: Second change.
    accept: test -f b.txt
  - id: C
    minion: w1
    paths: [c.txt]
    goal: Third change.
    accept: test -f c.txt
`

func TestLandGroupsByFirstDoneUnit(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	planPath, _, _ := encounter(t, repo, strings.Replace(orderPlan, "%REPO%", repo, 1))
	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	commitIn(t, worktreePath(repo, "enc", "w1"), "a.txt", "one\n", "A")
	commitIn(t, worktreePath(repo, "enc", "w1"), "c.txt", "three\n", "C")
	commitIn(t, worktreePath(repo, "enc", "w2"), "b.txt", "two\n", "B")
	for _, u := range []string{"A", "B", "C"} {
		if code, _, stderr := runCLI("set", "unit", u, "done", "--plan", planPath); code != 0 {
			t.Fatalf("set %s: %s", u, stderr)
		}
	}

	log := logGit(t)
	if code, stdout, stderr := runCLI("land", "--plan", planPath); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	merges := mergeLines(t, log)
	if len(merges) != 2 {
		t.Fatalf("want one merge per minion, got %d:\n%s", len(merges), strings.Join(merges, "\n"))
	}
	if !strings.Contains(merges[0], "enc/w1") || !strings.Contains(merges[1], "enc/w2") {
		t.Fatalf("w1 is first done, so it merges first:\n%s", strings.Join(merges, "\n"))
	}
}

// A passing review proves its fix. The fix was unproven, so land would skip it;
// the promotion makes it done so land merges the branch and lists it. A failing
// review promotes nothing.
func TestPassingReviewPromotesItsFixForLand(t *testing.T) {
	cases := []struct {
		name     string
		report   string
		promoted bool
	}{
		{name: "pass promotes the fix", report: "Merge verdict: PASS\n", promoted: true},
		{name: "fail promotes nothing", report: "Merge verdict: FAIL\n", promoted: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeHerdr(t, "r1:idle\n")
			dir := t.TempDir()
			planPath := filepath.Join(dir, ".dungeon", "plan.yaml")
			p := expanded(t)
			p.Repo = dir
			report := reportPath(planPath, "polish-review-2")
			if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(report, []byte(tc.report), 0o644); err != nil {
				t.Fatal(err)
			}
			st := &State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
			for _, u := range p.Units {
				st.SetUnit(u.ID, "planned", "")
			}
			st.SetUnit("polish-fix-1", "unproven", "")
			st.SetUnit("polish-review-2", "working", "")

			o := dispatchOpts{planPath: planPath, statePath: filepath.Join(dir, "state.json")}
			reconcile(t.Context(), newHerdrClient(), o, p, st, &digest{})

			landed := map[string]bool{}
			for _, u := range landable(p, st) {
				landed[u.ID] = true
			}
			if tc.promoted {
				if got := st.UnitStatus("polish-fix-1"); got != "done" {
					t.Fatalf("fix-1 is %q, want done after a passing review", got)
				}
				if note := st.Units["polish-fix-1"].Note; note != "proved by polish-review-2" {
					t.Fatalf("fix-1 note %q, want it to name the review", note)
				}
				if !landed["polish-fix-1"] {
					t.Fatalf("a promoted fix must be landable: %v", landed)
				}
			} else {
				if got := st.UnitStatus("polish-fix-1"); got != "unproven" {
					t.Fatalf("fix-1 is %q, a failing review must not promote it", got)
				}
				if landed["polish-fix-1"] {
					t.Fatalf("an unproven fix must not be landable: %v", landed)
				}
			}
		})
	}
}
