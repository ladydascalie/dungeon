package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func downSetup(t *testing.T, started string) (planPath, statePath, log string) {
	t.Helper()
	repo := gitRepo(t)
	log = fakeHerdr(t, started)
	// A previous up would have created the encounter workspace.
	if err := os.WriteFile(os.Getenv("FAKE_WS"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	planPath, statePath, _ = encounter(t, repo, strings.Replace(twoWorkerPlan, "%REPO%", repo, 1))
	t.Setenv("HERDR_WORKSPACE_ID", "w9")
	return planPath, statePath, log
}

func TestDownStopsIdleLanesAndLeavesTheRun(t *testing.T) {
	planPath, statePath, log := downSetup(t, "w1:idle\nw2:idle\n")

	code, stdout, stderr := runCLI("down", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "stopped 2 minions, 0 still running") {
		t.Fatalf("output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "still in") {
		t.Fatalf("down should say what it left behind:\n%s", stdout)
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "pane close") != 2 {
		t.Fatalf("both panes should be closed:\n%s", calls)
	}
	if !strings.Contains(string(calls), "workspace close") {
		t.Fatalf("the encounter workspace should be closed:\n%s", calls)
	}
	st, _ := LoadState(statePath)
	if st.MinionStatus("w1") != "absent" {
		t.Fatalf("minion %q, want absent", st.MinionStatus("w1"))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(statePath), "plan.yaml")); err != nil {
		t.Fatalf("the plan must survive: %v", err)
	}
}

func TestDownRefusesToStopAWorkingLane(t *testing.T) {
	planPath, statePath, log := downSetup(t, "w1:idle\nw2:working\n")

	code, stdout, stderr := runCLI("down", "--plan", planPath)
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr)
	}
	if !strings.Contains(stdout, "w2 is working") {
		t.Fatalf("output:\n%s", stdout)
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "workspace close") {
		t.Fatalf("the workspace must stay while a lane is working:\n%s", calls)
	}
	st, _ := LoadState(statePath)
	if st.MinionStatus("w2") != "absent" {
		// w2 was never recorded as spawned in this test, so absent is the default
		t.Fatalf("w2 %q", st.MinionStatus("w2"))
	}
}

func TestDownForceStopsEverything(t *testing.T) {
	planPath, _, log := downSetup(t, "w1:working\nw2:working\n")
	code, stdout, stderr := runCLI("down", "--force", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "stopped 2 minions") {
		t.Fatalf("output:\n%s", stdout)
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "pane close") != 2 {
		t.Fatalf("both panes should be closed:\n%s", calls)
	}
}

// setToken stamps the run's state with a token, as a first up would have.
func setToken(t *testing.T, statePath, token string) {
	t.Helper()
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	st.Token = token
	if err := SaveState(statePath, st); err != nil {
		t.Fatal(err)
	}
}

func TestDownStopsATokenizedLane(t *testing.T) {
	planPath, statePath, log := downSetup(t, "w1-7f:idle\nw2-7f:idle\n")
	setToken(t, statePath, "7f")

	code, stdout, stderr := runCLI("down", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "stopped 2 minions, 0 still running") {
		t.Fatalf("down missed the tokenized lanes:\n%s", stdout)
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "pane close") != 2 {
		t.Fatalf("both tokenized panes should be closed:\n%s", calls)
	}
	if strings.Contains(string(calls), "w1 ") || strings.Contains(string(calls), "w2 ") {
		t.Fatalf("down must not address a bare minion id:\n%s", calls)
	}
}

// A live bare lane belongs to another run. A tokenized state must leave it be.
func TestDownLeavesAnotherRunsBareLaneAlone(t *testing.T) {
	planPath, statePath, log := downSetup(t, "w1:idle\nw2:idle\n")
	setToken(t, statePath, "7f")

	code, stdout, stderr := runCLI("down", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "stopped 0 minions") {
		t.Fatalf("down touched another run's lane:\n%s", stdout)
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "pane close") {
		t.Fatalf("another run's panes must survive:\n%s", calls)
	}
}

// A pinned lane is interrupted, and the interrupt must name the derived agent.
func TestDownInterruptsATokenizedPinnedLane(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "w1-7f:idle\n")
	if err := os.WriteFile(os.Getenv("FAKE_WS"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	plan := strings.Replace(twoWorkerPlan, "%REPO%", repo, 1)
	plan = strings.Replace(plan,
		"{id: w1, role: worker, worktree: w1}",
		"{id: w1, role: worker, worktree: w1, pane: \"w9:p2\"}", 1)
	planPath, statePath, _ := encounter(t, repo, plan)
	setToken(t, statePath, "7f")
	t.Setenv("HERDR_WORKSPACE_ID", "w9")

	code, stdout, stderr := runCLI("down", "--plan", planPath)
	if code != 0 {
		t.Fatalf("down exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "stopped 1 minions") {
		t.Fatalf("the pinned lane was not stopped:\n%s", stdout)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "agent send-keys w1-7f ctrl+c") {
		t.Fatalf("a pinned lane needs send-keys under the derived name:\n%s", calls)
	}
	if strings.Contains(string(calls), "agent send-keys w1 ctrl+c") {
		t.Fatalf("a pinned lane must not be addressed by the bare id:\n%s", calls)
	}
}
