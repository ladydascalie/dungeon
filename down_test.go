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
