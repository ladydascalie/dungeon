package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeEncounter(t *testing.T, path, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := strings.Replace(goodPlan, "encounter: fix-auth-timeouts", "encounter: "+name, 1)
	if err := os.WriteFile(path, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
}

// touchEncounter sets a plan's mtime so ordering by last touch is not left to
// the filesystem's timestamp resolution.
func touchEncounter(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverPlansListsNewestTouchFirst(t *testing.T) {
	root := t.TempDir()
	writeEncounter(t, filepath.Join(root, ".dungeon", "plan.yaml"), "root")
	writeEncounter(t, filepath.Join(root, ".dungeon", "auth", "plan.yaml"), "auth")
	writeEncounter(t, filepath.Join(root, ".dungeon", "web", "plan.yaml"), "web")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	touchEncounter(t, filepath.Join(root, ".dungeon", "auth", "plan.yaml"), base.Add(2*time.Hour))
	touchEncounter(t, filepath.Join(root, ".dungeon", "web", "plan.yaml"), base.Add(time.Hour))
	touchEncounter(t, filepath.Join(root, ".dungeon", "plan.yaml"), base)
	t.Chdir(root)

	found := discoverPlans()
	if len(found) != 3 {
		t.Fatalf("found %d plans, want 3: %+v", len(found), found)
	}
	for i, want := range []string{"auth", "web", "root"} {
		if found[i].Name != want {
			t.Fatalf("plan %d is %q, want %q", i, found[i].Name, want)
		}
		if found[i].Err != nil {
			t.Fatalf("plan %d: %v", i, found[i].Err)
		}
	}
}

func TestDiscoverPlansListsAnUnreadablePlan(t *testing.T) {
	root := t.TempDir()
	writeEncounter(t, filepath.Join(root, ".dungeon", "plan.yaml"), "root")
	bad := filepath.Join(root, ".dungeon", "bad", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(":::"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	found := discoverPlans()
	if len(found) != 2 {
		t.Fatalf("found %d plans, want 2", len(found))
	}
	badPlan := found[0]
	if badPlan.Name != "bad" {
		badPlan = found[1]
	}
	if badPlan.Err == nil {
		t.Fatal("a broken plan should be listed with its error")
	}
	if badPlan.Name != "bad" {
		t.Fatalf("unreadable plan name %q, want bad", badPlan.Name)
	}
}

func TestDiscoverPlansRanksAStateWriteAsATouch(t *testing.T) {
	root := t.TempDir()
	writeEncounter(t, filepath.Join(root, ".dungeon", "plan.yaml"), "root")
	writeEncounter(t, filepath.Join(root, ".dungeon", "auth", "plan.yaml"), "auth")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	touchEncounter(t, filepath.Join(root, ".dungeon", "plan.yaml"), base)
	touchEncounter(t, filepath.Join(root, ".dungeon", "auth", "plan.yaml"), base)
	state := filepath.Join(root, ".dungeon", "auth", "state.json")
	if err := os.WriteFile(state, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	touchEncounter(t, state, base.Add(time.Hour))
	t.Chdir(root)

	found := discoverPlans()
	if len(found) != 2 || found[0].Name != "auth" {
		t.Fatalf("a state write should lift the encounter: %+v", found)
	}
}

func TestAmbiguousEncounterIsRefused(t *testing.T) {
	root := t.TempDir()
	writeEncounter(t, filepath.Join(root, ".dungeon", "plan.yaml"), "root")
	writeEncounter(t, filepath.Join(root, ".dungeon", "auth", "plan.yaml"), "auth")
	t.Chdir(root)

	for _, args := range [][]string{{"check"}, {"status"}, {"next", "--dry-run"}, {"land"}, {"down"}} {
		code, _, stderr := runCLI(args...)
		if code != 2 {
			t.Fatalf("%v exit %d, want 2: %s", args, code, stderr)
		}
		if !strings.Contains(stderr, "2 encounters") || !strings.Contains(stderr, "pass --plan") {
			t.Fatalf("%v should name the ambiguity:\n%s", args, stderr)
		}
		if !strings.Contains(stderr, ".dungeon/plan.yaml") || !strings.Contains(stderr, ".dungeon/auth/plan.yaml") {
			t.Fatalf("%v should list both plans:\n%s", args, stderr)
		}
	}

	code, stdout, stderr := runCLI("check", filepath.Join(root, ".dungeon", "auth", "plan.yaml"))
	if code != 0 {
		t.Fatalf("explicit plan exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "ok: auth") {
		t.Fatalf("explicit plan output:\n%s", stdout)
	}
}

func TestSingleNestedPlanIsResolved(t *testing.T) {
	root := t.TempDir()
	writeEncounter(t, filepath.Join(root, ".dungeon", "auth", "plan.yaml"), "auth")
	t.Chdir(root)

	code, stdout, stderr := runCLI("check")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "ok: auth") {
		t.Fatalf("output:\n%s", stdout)
	}
}

func TestExplicitMissingDefaultIsNotRedirected(t *testing.T) {
	root := t.TempDir()
	writeEncounter(t, filepath.Join(root, ".dungeon", "auth", "plan.yaml"), "auth")
	t.Chdir(root)

	code, stdout, stderr := runCLI("check", "--plan", ".dungeon/plan.yaml")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s\n%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "ok: auth") {
		t.Fatal("an explicit path must not be replaced by the single nested plan")
	}
}

func TestViewOnceRefusesAmbiguity(t *testing.T) {
	root := t.TempDir()
	writeEncounter(t, filepath.Join(root, ".dungeon", "plan.yaml"), "root")
	writeEncounter(t, filepath.Join(root, ".dungeon", "auth", "plan.yaml"), "auth")
	t.Chdir(root)

	code, _, stderr := runCLI("view", "--once")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stderr, "choose one") {
		t.Fatalf("view should refuse and name --plan:\n%s", stderr)
	}
}

func TestPickerChoosesAnEncounter(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, ".dungeon", "plan.yaml")
	second := filepath.Join(root, ".dungeon", "auth", "plan.yaml")
	writeEncounter(t, first, "root")
	writeEncounter(t, second, "auth")
	t.Chdir(root)

	m := viewModel{
		width:    100,
		height:   30,
		choosing: []Encounter{{Path: first, Name: "root"}, {Path: second, Name: "auth"}},
	}
	out := m.View()
	for _, want := range []string{"choose an encounter", "root", "auth", "enter open"} {
		if !strings.Contains(out, want) {
			t.Fatalf("picker missing %q:\n%s", want, out)
		}
	}

	m = press(t, m, "j")
	if m.choice != 1 {
		t.Fatalf("choice %d, want 1", m.choice)
	}
	m = press(t, m, "enter")
	if len(m.choosing) != 0 {
		t.Fatal("the picker should close on enter")
	}
	if m.planPath != second {
		t.Fatalf("planPath %q, want %q", m.planPath, second)
	}
	if m.statePath != StatePath(second) {
		t.Fatalf("statePath %q, want %q", m.statePath, StatePath(second))
	}
}
