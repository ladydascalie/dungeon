package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hideWorktrunk puts only git on PATH, so the fallback is exercised even on a
// machine where wt is installed.
func hideWorktrunk(t *testing.T) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestWorktreeFallsBackToGitWhenWtIsMissing(t *testing.T) {
	repo := gitRepo(t)
	hideWorktrunk(t)
	path := filepath.Join(t.TempDir(), "enc-w1")

	if _, err := ensureWorktree(repo, "main", "enc", "w1", path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "README.md")); err != nil {
		t.Fatalf("git fallback did not cut the worktree: %v", err)
	}
}

func TestWorktreeUsesWorktrunkWhenPresent(t *testing.T) {
	repo := gitRepo(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	path := filepath.Join(t.TempDir(), "enc-w1")

	// A fake wt that records the call and cuts the worktree the same way the
	// real one would, so the test proves the branch was taken, not that
	// worktrunk works.
	script := `#!/bin/sh
echo "$@" >> "$FAKE_WT_LOG"
git -C "$FAKE_WT_REPO" worktree add -b "$FAKE_WT_BRANCH" "$FAKE_WT_PATH" "$FAKE_WT_BASE"
`
	if err := os.WriteFile(filepath.Join(bin, "wt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_WT_LOG", log)
	t.Setenv("FAKE_WT_PATH", path)
	t.Setenv("FAKE_WT_BRANCH", "enc/w1")
	t.Setenv("FAKE_WT_REPO", repo)
	t.Setenv("FAKE_WT_BASE", "main")

	if _, err := ensureWorktree(repo, "main", "enc", "w1", path); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--config-set", "worktree-path=", "switch", "--create", "--base main", "enc/w1"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("wt was not asked for %q:\n%s", want, calls)
		}
	}
	if _, err := os.Stat(filepath.Join(path, "README.md")); err != nil {
		t.Fatalf("worktree not at %s: %v", path, err)
	}
}

// A worktree that moved after it was cut is still the lane. worktrunk reports
// the actual path, so dungeon must not keep using the creation convention.
func TestResolveWorktreePathUsesWorktrunk(t *testing.T) {
	repo := gitRepo(t)
	bin := t.TempDir()
	moved := filepath.Join(t.TempDir(), "moved-enc-w1")
	script := fmt.Sprintf(`#!/bin/sh
cat <<'EOF'
{"items":[{"branch":"enc/w1","worktree":{"path":%q}},{"branch":"other","worktree":{"path":"/elsewhere"}}]}
EOF
`, moved)
	if err := os.WriteFile(filepath.Join(bin, "wt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, ok := resolveWorktreePath(repo, "enc/w1")
	if !ok || got != moved {
		t.Fatalf("got %q, %v; want %q, true", got, ok, moved)
	}
	if got := lanePath(Plan{Repo: repo, Encounter: "enc"}, "w1"); got != moved {
		t.Fatalf("lanePath got %q, want %q", got, moved)
	}
	if _, ok := resolveWorktreePath(repo, "enc/nope"); ok {
		t.Fatal("a branch with no worktree must not resolve")
	}
}

// git is the fallback when wt is missing, and it reports a moved worktree too.
func TestResolveWorktreePathFollowsAGitWorktreeMove(t *testing.T) {
	repo := gitRepo(t)
	hideWorktrunk(t)
	old := filepath.Join(t.TempDir(), "enc-w1")
	moved := filepath.Join(t.TempDir(), "moved-enc-w1")
	if _, err := gitRun(repo, "worktree", "add", "-b", "enc/w1", old, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitRun(repo, "worktree", "move", old, moved); err != nil {
		t.Fatal(err)
	}
	got, ok := resolveWorktreePath(repo, "enc/w1")
	if !ok || got != moved {
		t.Fatalf("got %q, %v; want %q, true", got, ok, moved)
	}
}

// A branch with unique commits and no worktree is the lane's work. Attach it
// instead of failing with Branch already exists.
func TestEnsureWorktreeAttachesABranchWithUniqueCommits(t *testing.T) {
	repo := gitRepo(t)
	hideWorktrunk(t)
	work := filepath.Join(t.TempDir(), "work")
	if _, err := gitRun(repo, "worktree", "add", "-b", "enc/w1", work, "main"); err != nil {
		t.Fatal(err)
	}
	commitIn(t, work, "work.txt", "one\n", "w1 work")
	if _, err := gitRun(repo, "worktree", "remove", work); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "enc-w1")
	lane, err := ensureWorktree(repo, "main", "enc", "w1", path)
	if err != nil {
		t.Fatal(err)
	}
	if !lane.Resumed || lane.Recreated {
		t.Fatalf("lane %+v: want resumed, not recreated", lane)
	}
	if lane.Ahead != 1 || lane.Head == "" || lane.Subject != "w1 work" {
		t.Fatalf("lane facts %+v", lane)
	}
	if _, err := os.Stat(filepath.Join(path, "work.txt")); err != nil {
		t.Fatalf("the reused branch must keep its commit: %v", err)
	}
}

// A branch with no unique commits holds no work. Replace it from base.
func TestEnsureWorktreeRecreatesABranchWithNoUniqueCommits(t *testing.T) {
	repo := gitRepo(t)
	hideWorktrunk(t)
	if _, err := gitRun(repo, "branch", "enc/w1", "main"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "enc-w1")
	lane, err := ensureWorktree(repo, "main", "enc", "w1", path)
	if err != nil {
		t.Fatal(err)
	}
	if !lane.Recreated || lane.Resumed {
		t.Fatalf("lane %+v: want recreated, not resumed", lane)
	}
	if lane.Ahead != 0 {
		t.Fatalf("recreated lane is %d commits ahead, want 0", lane.Ahead)
	}
	got, err := gitRun(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got != "enc/w1" {
		t.Fatalf("branch %q, want enc/w1", got)
	}
}

func TestWorktreePathFollowsTheProjectConvention(t *testing.T) {
	got := worktreePath("/code/acme", "enc", "w1")
	want := filepath.Join("/code", "acme.worktrees", "enc-w1")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestIntegrationWorktreeAttachesAnExistingBranch(t *testing.T) {
	repo := gitRepo(t)
	hideWorktrunk(t)
	path := filepath.Join(t.TempDir(), "enc-integration")
	if _, err := gitRun(repo, "branch", "enc/int", "main"); err != nil {
		t.Fatal(err)
	}

	if err := ensureIntegrationWorktree(repo, "main", path, "enc/int"); err != nil {
		t.Fatal(err)
	}
	got, err := gitRun(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got != "enc/int" {
		t.Fatalf("branch %q, want enc/int", got)
	}
}
