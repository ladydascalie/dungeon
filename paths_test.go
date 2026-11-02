package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPlanIsSearchedUpwards(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".dungeon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(plan, []byte("encounter: x\nminions: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(deep); err != nil {
		t.Fatal(err)
	}
	if got := defaultPlan(); got != plan {
		t.Fatalf("got %q, want %q", got, plan)
	}
}

func TestDefaultPlanFallsBackWhenThereIsNone(t *testing.T) {
	empty := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	// /tmp has no ancestor plan in a normal checkout, but the fallback must still
	// be the relative default rather than an error.
	if got := defaultPlan(); got == "" {
		t.Fatal("want a path, got empty")
	}
}

// A lane keeps its screenshots and notes under .dungeon/ in its own worktree.
// That is encounter scratch, not work, even though the plan's encounter lives
// somewhere else. A genuine change outside the declared paths still blocks.
func TestChangedFileUnderTheEncounterIsNotOutside(t *testing.T) {
	repo := gitRepo(t)
	enc := t.TempDir()
	planPath := filepath.Join(enc, ".dungeon", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte("encounter: enc\nminions: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := Plan{Encounter: "enc", Repo: repo}
	wt := worktreePath(repo, p.Encounter, "w1")
	if err := ensureIntegrationWorktree(repo, "main", wt, "enc/w1"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, ".dungeon", "shots"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".dungeon", "shots", "a.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "elsewhere.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, _ := LoadState(filepath.Join(enc, ".dungeon", "state.json"))
	u := Unit{ID: "A", Paths: []string{"src/**"}}
	o := dispatchOpts{planPath: planPath}
	if !blockedByWorktree(o, p, &st, u, Minion{ID: "w1", Role: RoleWorker}) {
		t.Fatal("want the outside change to block")
	}
	if got := st.Units["A"].Note; strings.Contains(got, "a.png") || strings.Contains(got, ".dungeon") {
		t.Fatalf("encounter change reported as outside work: %q", got)
	}
	if got := st.Units["A"].Note; !strings.Contains(got, "elsewhere.txt") {
		t.Fatalf("genuine outside change not named: %q", got)
	}
}

func TestUnderEncounterAcceptsAnySpelling(t *testing.T) {
	enc := t.TempDir()
	planPath := filepath.Join(enc, ".dungeon", "plan.yaml")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relPlan, err := filepath.Rel(wd, planPath)
	if err != nil {
		t.Fatal(err)
	}
	absState := filepath.Join(enc, ".dungeon", "state.json")
	cases := []struct {
		name string
		plan string
		file string
		want bool
	}{
		{"worktree .dungeon, root relative", planPath, ".dungeon/shots/a.png", true},
		{"worktree .dungeon, subdirectory relative", planPath, "../../.dungeon/shots/a.png", true},
		{"absolute encounter file", planPath, absState, true},
		{"relative plan path", relPlan, absState, true},
		{"relative plan path, root relative file", relPlan, ".dungeon/state.json", true},
		{"code", planPath, "src/main.go", false},
		{"lookalike", planPath, "docs/.dungeonish/x", false},
	}
	for _, c := range cases {
		if got := underEncounter(c.plan, c.file); got != c.want {
			t.Errorf("%s: underEncounter(%q, %q) = %v, want %v", c.name, c.plan, c.file, got, c.want)
		}
	}
}

// commitAll stages everything and commits in repo with a fixed identity, so a
// porcelain test starts from a clean worktree.
func commitAll(t *testing.T, repo, message string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", message}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// Two modified files must both be reported, including one with a space: the
// first entry starts with a leading space status, the byte that used to be lost.
func TestChangedFilesReportsEveryModifiedFile(t *testing.T) {
	repo := gitRepo(t)
	for _, name := range []string{"tmux.go", "paths.go", "notes with space.md"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitAll(t, repo, "tracked")
	for _, name := range []string{"tmux.go", "paths.go", "notes with space.md"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := changedFiles(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"notes with space.md", "paths.go", "tmux.go"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("changedFiles = %q, want %q", got, want)
	}
}

// The first porcelain entry is owned and the second is not. Both must be
// classified against their own paths, not shifted by a lost leading space.
func TestChangedFilesFirstOwnedSecondOutside(t *testing.T) {
	repo := gitRepo(t)
	for _, name := range []string{"aaa.go", "zzz.txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitAll(t, repo, "tracked")
	for _, name := range []string{"aaa.go", "zzz.txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := changedFiles(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "aaa.go" || got[1] != "zzz.txt" {
		t.Fatalf("changedFiles = %q, want [aaa.go zzz.txt]", got)
	}
	paths := []string{"aaa.go"}
	if !ownedBy(paths, got[0]) {
		t.Errorf("%q classified outside its owner", got[0])
	}
	if ownedBy(paths, got[1]) {
		t.Errorf("%q classified as owned", got[1])
	}
}

// A rename reports its destination and carries the source as the next NUL
// entry. Only the destination is a changed file.
func TestChangedFilesRenameTakesDestination(t *testing.T) {
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "old name.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "tracked")
	cmd := exec.Command("git", "-C", repo, "mv", "old name.go", "new name.go")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v\n%s", err, out)
	}

	got, err := changedFiles(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "new name.go" {
		t.Fatalf("changedFiles = %q, want [new name.go]", got)
	}
}
