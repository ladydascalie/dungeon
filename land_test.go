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
