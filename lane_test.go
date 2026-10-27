package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedPaneIsUsedWhenItExists(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "0")
	plan := strings.Replace(planFor(repo),
		"  - {id: w1, role: worker, worktree: w1}",
		"  - {id: w1, role: worker, worktree: w1, pane: \"w1P:p9\"}", 1)
	planPath, statePath, _ := encounter(t, repo, plan)

	code, stdout, stderr := runCLI("up", "--no-dispatch", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	calls, _ := os.ReadFile(log)
	// The fake answers pane get, so the pinned pane wins and no split happens.
	if strings.Contains(string(calls), "pane split") {
		t.Fatalf("a live pin should not split a pane:\n%s", calls)
	}
	if !strings.Contains(string(calls), "agent start w1 --kind pi --pane w1P:p9") {
		t.Fatalf("the agent should start in the pinned pane:\n%s", calls)
	}
	st, _ := LoadState(statePath)
	if got := st.Minions["w1"].Pane; got != "w1P:p9" {
		t.Fatalf("pane hint %q, want w1P:p9", got)
	}
}

func TestPinnedPaneIsRecordedAsAHint(t *testing.T) {
	plan := decodeOK(t, strings.Replace(goodPlan, "{id: w1, role: worker, worktree: fix-auth-w1}",
		"{id: w1, role: worker, worktree: fix-auth-w1, pane: w1P:p9}", 1))
	if got := plan.Minions[1].Pane; got != "w1P:p9" {
		t.Fatalf("pane %q", got)
	}
}

func TestTwoMinionsCannotShareAPane(t *testing.T) {
	src := strings.Replace(goodPlan, "{id: w1, role: worker, worktree: fix-auth-w1}",
		"{id: w1, role: worker, worktree: fix-auth-w1, pane: same:pane}", 1)
	src = strings.Replace(src, "{id: w2, role: worker, worktree: fix-auth-w2}",
		"{id: w2, role: worker, worktree: fix-auth-w2, pane: same:pane}", 1)
	wantCodes(t, Validate(decodeOK(t, src)), EDuplicatePane)
}

func TestFocusUsesTheAgentThenThePaneHint(t *testing.T) {
	log := fakeHerdr(t, "w1:idle\n")
	h := newHerdrClient()
	if err := h.focus(t.Context(), "w1", "w1P:p9"); err != nil {
		t.Fatalf("focus: %v", err)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "agent focus w1") {
		t.Fatalf("a live agent should be focused directly:\n%s", calls)
	}

	// No live agent: the hint's tab is the best available move.
	log2 := fakeHerdr(t, "")
	if err := h.focus(t.Context(), "w1", "w1P:p9"); err != nil {
		t.Fatalf("focus with a hint: %v", err)
	}
	calls, _ = os.ReadFile(log2)
	if !strings.Contains(string(calls), "tab focus") {
		t.Fatalf("with no agent the pane's tab should be focused:\n%s", calls)
	}

	if err := h.focus(t.Context(), "w1", ""); err == nil {
		t.Fatal("with no agent and no hint, focus must fail rather than guess")
	}
}

func TestStatusShowsThePaneHintWhenTheLaneIsGone(t *testing.T) {
	plan := writePlan(t, statusPlan)
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	fakeHerdr(t, "")
	st := State{Encounter: "st", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	st.PinPane("w1", "w1P:p9")
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}
	m := newTestView(t, plan, state)
	out := m.View()
	if !strings.Contains(out, "w1P:p9") {
		t.Fatalf("the pane hint should survive a dead lane:\n%s", out)
	}
}

func TestAStaleGoneFallsBackToASplit(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	plan := strings.Replace(planFor(repo),
		"  - {id: w1, role: worker, worktree: w1}",
		"  - {id: w1, role: worker, worktree: w1, pane: \"gone:p1\"}", 1)
	planPath, statePath, _ := encounter(t, repo, plan)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "pane split") {
		t.Fatalf("a gone pin should fall back to a split:\n%s", calls)
	}
	st, _ := LoadState(statePath)
	if got := st.Minions["w1"].Pane; got == "gone:p1" || got == "" {
		t.Fatalf("the hint should be the new pane, got %q", got)
	}
}

func TestDownInterruptsAPinnedLaneInsteadOfClosingItsPane(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "w1:idle\n")
	plan := strings.Replace(planFor(repo),
		"  - {id: w1, role: worker, worktree: w1}",
		"  - {id: w1, role: worker, worktree: w1, pane: \"w9:p2\"}", 1)
	planPath, _, _ := encounter(t, repo, plan)
	t.Setenv("HERDR_WORKSPACE_ID", "w9")

	if code, _, stderr := runCLI("down", "--plan", planPath); code != 0 {
		t.Fatalf("down exit %d: %s", code, stderr)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "agent send-keys w1 ctrl+c") {
		t.Fatalf("a pinned lane should be interrupted, not closed:\n%s", calls)
	}
	if strings.Contains(string(calls), "pane close") {
		t.Fatalf("a pinned pane belongs to the operator:\n%s", calls)
	}
}
