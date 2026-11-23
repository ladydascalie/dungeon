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
		// The exemption is a prefix, not a component: a lane that puts code
		// under a .dungeon directory of its own chooses the directory, not the
		// rule, so the file is a change outside the paths.
		{"nested .dungeon is code", planPath, "pkg/.dungeon/inject.go", false},
		{"nested .dungeon, deeper", planPath, "a/b/.dungeon/x.go", false},
		{"nested .dungeon, from a subdirectory", planPath, "../../pkg/.dungeon/x.go", false},
		{"absolute nested .dungeon", planPath, filepath.Join(enc, "pkg", ".dungeon", "inject.go"), false},
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
// head recorded at dispatch still sees it, the note names the file, and the note
// carries the plan edit that would have covered it.
func TestCommittedWorkOutsideThePathsBlocks(t *testing.T) {
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
	for _, want := range []string{"changed files outside your paths", "elsewhere.txt",
		"dungeon edit unit A --paths-add '<glob>'"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note %q must carry %q", note, want)
		}
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

// A rename is the deletion it is plus the addition it makes. Without
// --no-renames git reports only the destination, so a file renamed out of the
// unit's paths would leave its deletion unseen; no pattern can hide a path it
// deleted.
func TestChangedSinceSeesBothSidesOfARename(t *testing.T) {
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "fleet.go"), []byte("dep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "dep work")
	head, err := gitRun(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "mv", "fleet.go", "src/fleet.go")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v\n%s", err, out)
	}

	changed, uncommitted, err := changedSince(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 || changed[0] != "fleet.go" || changed[1] != "src/fleet.go" {
		t.Fatalf("changedSince = %q, want the deletion and the addition", changed)
	}
	// The rename is staged and not committed, so both halves are dungeon's to
	// commit when the source is inside the paths.
	if len(uncommitted) != 1 || uncommitted[0] != "src/fleet.go" {
		t.Fatalf("uncommitted = %q, want the destination git status reports", uncommitted)
	}
}

// An untracked file is a change like any other, and git diff names only tracked
// paths, so the uncommitted list is what makes it count.
func TestChangedSinceCountsAnUntrackedFile(t *testing.T) {
	repo := gitRepo(t)
	head, err := gitRun(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "cmd", "tool", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, uncommitted, err := changedSince(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "cmd/tool/main.go" {
		t.Fatalf("changedSince = %q, want the untracked file", changed)
	}
	if len(uncommitted) != 1 || uncommitted[0] != "cmd/tool/main.go" {
		t.Fatalf("uncommitted = %q, want the untracked file", uncommitted)
	}
}

// A check that cannot read the recorded head has proved nothing, so it reports
// the error rather than an empty change set. The caller blocks on it.
func TestChangedSinceReportsAGitError(t *testing.T) {
	repo := gitRepo(t)
	changed, _, err := changedSince(repo, "0123456789012345678901234567890123456789")
	if err == nil {
		t.Fatalf("a head that is not a commit returned %q and no error", changed)
	}
	if !strings.Contains(err.Error(), "0123456789012345678901234567890123456789") {
		t.Fatalf("error %q must name what git could not read", err)
	}
}

// A normal repository is not refused. The check names specific ways git reports
// a tree it does not hold, not the presence of a feature a repo happens to use.
func TestBlindStateAllowsANormalRepo(t *testing.T) {
	repo := gitRepo(t)
	note, err := blindState(repo)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("note %q, want none on a normal repo", note)
	}
}

// A filter configured with no attribute driving it is inert: no file in the
// tree goes through it, so it says nothing false. git-lfs installs filter
// commands in a machine's global config, and every repo that never names them
// must stay usable.
func TestBlindStateAllowsAConfiguredFilterThatDrivesNoAttribute(t *testing.T) {
	repo := gitRepo(t)
	for key, value := range map[string]string{
		"filter.hide.clean":  "printf 'base\n'",
		"filter.hide.smudge": "cat",
	} {
		cmd := exec.Command("git", "-C", repo, "config", key, value)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", key, err, out)
		}
	}
	note, err := blindState(repo)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("note %q, want none for an unused filter", note)
	}
}

// The repository dungeon itself lives in is not refused either: the guard must
// not block the checkout an operator runs it from.
func TestBlindStateAllowsTheDungeonRepo(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	root := strings.TrimSpace(string(out))
	note, err := blindState(root)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("note %q, want none on the dungeon repo", note)
	}
}
