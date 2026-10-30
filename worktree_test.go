package main

import (
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

	if err := ensureWorktree(repo, "main", "enc", "w1", path); err != nil {
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

	if err := ensureWorktree(repo, "main", "enc", "w1", path); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"switch", "--create", "--base main", "enc/w1"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("wt was not asked for %q:\n%s", want, calls)
		}
	}
	// Worktrunk owns the layout. An override here would ignore the project's
	// own worktree-path and break the <repo>.worktrees convention.
	if strings.Contains(string(calls), "--config-set") {
		t.Fatalf("wt was given a layout override:\n%s", calls)
	}
	if _, err := os.Stat(filepath.Join(path, "README.md")); err != nil {
		t.Fatalf("worktree not at %s: %v", path, err)
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
