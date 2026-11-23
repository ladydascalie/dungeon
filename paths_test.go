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
	if blocked, _ := blockedByWorktree(o, p, &st, u, Minion{ID: "w1", Role: RoleWorker}); !blocked {
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

// commitAll stages everything and commits in repo with a fixed identity and a
// fixed date, so a porcelain test starts from a clean worktree and every commit
// it makes has its own hash.
func commitAll(t *testing.T, repo, message string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", message}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_AUTHOR_DATE=2000-01-01T00:00:00 +0000",
			"GIT_COMMITTER_DATE=2000-01-01T00:00:00 +0000")
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

// A lane that creates a new directory leaves it untracked, and git collapses an
// untracked tree to one "dir/" entry unless it is asked for every file. That
// collapsed name matches no per-file path pattern, so a unit that wrote only
// inside its own paths was blocked with "outside your paths: dir/".
func TestUntrackedDirectoryIsMatchedFileByFile(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(wt, "cmd", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "cmd", "tool", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, _ := LoadState(filepath.Join(enc, ".dungeon", "state.json"))
	u := Unit{ID: "A", Paths: []string{"cmd/**"}}
	o := dispatchOpts{planPath: planPath}
	if blocked, note := blockedByWorktree(o, p, &st, u, Minion{ID: "w1", Role: RoleWorker}); blocked {
		t.Fatalf("work inside cmd/** blocked: %q", note)
	}
}

// A lane that commits its own work leaves git status empty. Counting from the
// head recorded at dispatch still sees it, and the note names both the file and
// the short commit that carried it.
func TestCommittedWorkOutsideThePathsBlocksWithItsCommit(t *testing.T) {
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
	head, err := gitRun(wt, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "elsewhere.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, wt, "lane wanders")
	short, err := gitRun(wt, "log", "-1", "--format=%h")
	if err != nil {
		t.Fatal(err)
	}

	u := Unit{ID: "A", Paths: []string{"src/**"}}
	o := dispatchOpts{planPath: planPath}
	m := Minion{ID: "w1", Role: RoleWorker}

	guarded, _ := LoadState(filepath.Join(enc, ".dungeon", "state.json"))
	guarded.RecordLaneHead("A", head)
	blocked, _ := blockedByWorktree(o, p, &guarded, u, m)
	if !blocked {
		t.Fatal("a commit outside the paths must block")
	}
	note := guarded.Units["A"].Note
	if !strings.Contains(note, "changed files outside your paths") ||
		!strings.Contains(note, "elsewhere.txt") || !strings.Contains(note, short) {
		t.Fatalf("note %q must name the file and commit %q", note, short)
	}

	// The guard is not retroactive: a state with no recorded head reads git
	// status alone, as it did before, so the same commit goes unseen.
	legacy, _ := LoadState(filepath.Join(enc, ".dungeon", "state.json"))
	if blocked, note := blockedByWorktree(o, p, &legacy, u, m); blocked {
		t.Fatalf("a state with no recorded head blocked: %q", note)
	}
}

// A file the lane committed and then modified again is named once: the commit
// list and the status list both hold it, and it is one outside change.
func TestCommittedThenModifiedFileIsNamedOnce(t *testing.T) {
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
	head, err := gitRun(wt, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "fleet.go"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, wt, "lane wanders")
	if err := os.WriteFile(filepath.Join(wt, "fleet.go"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, _ := LoadState(filepath.Join(enc, ".dungeon", "state.json"))
	st.RecordLaneHead("A", head)
	u := Unit{ID: "A", Paths: []string{"src/**"}}
	if blocked, _ := blockedByWorktree(dispatchOpts{planPath: planPath}, p, &st, u, Minion{ID: "w1", Role: RoleWorker}); !blocked {
		t.Fatal("want the outside change to block")
	}
	note := st.Units["A"].Note
	if got := strings.Count(note, "fleet.go"); got != 1 {
		t.Fatalf("note %q names fleet.go %d times, want once", note, got)
	}
}

// A merge commit's difference from its first parent also names every file the
// other branch changed, which the lane did not write. Only what differs from
// every parent is the lane's resolution, so a file that arrived untouched is
// never counted as written.
func TestMergeWritesOnlyTheResolution(t *testing.T) {
	repo := gitRepo(t)
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00 +0000",
		"GIT_COMMITTER_DATE=2000-01-01T00:00:00 +0000")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("a.txt", "base\n")
	write("b.txt", "base\n")
	commitAll(t, repo, "files")
	head, err := gitRun(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	git("checkout", "-q", "-b", "dep/landed")
	write("a.txt", "dep\n")
	write("b.txt", "dep\n")
	commitAll(t, repo, "dep work")
	git("checkout", "-q", "main")

	write("a.txt", "lane\n")
	commitAll(t, repo, "lane work")
	merge := exec.Command("git", "-C", repo, "merge", "--no-ff", "dep/landed", "-m", "merge dep/landed")
	merge.Env = env
	if err := merge.Run(); err == nil {
		t.Fatal("the fixture needs a conflict on a.txt")
	}
	write("a.txt", "resolved\n")
	commitAll(t, repo, "merge dep/landed")

	written, err := writtenFiles(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	short, err := gitRun(repo, "rev-parse", "--short", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := written["a.txt"]; got != short {
		t.Fatalf("a.txt named by %q, want the merge %q: %v", got, short, written)
	}
	if _, ok := written["b.txt"]; ok {
		t.Fatalf("a file that arrived through the merge was counted as written: %v", written)
	}
	commits, err := laneCommits(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("laneCommits = %v, want the lane's own two commits", commits)
	}
}
