package main

import (
	"os"
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
	if err := ensureIntegrationWorktree(repo, "main", p.Encounter, wt, "w1"); err != nil {
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
