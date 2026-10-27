package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// editPlan appends a comment: the smallest edit a plan can take.
func editPlan(t *testing.T, planPath string) {
	t.Helper()
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, append(data, []byte("# edited after dispatch\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func upForDrift(t *testing.T, repo string) (planPath, statePath string) {
	t.Helper()
	planPath, statePath, _ = encounter(t, repo, planFor(repo))
	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	return planPath, statePath
}

func TestUpRecordsThePlanHash(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	planPath, statePath := upForDrift(t, repo)

	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := PlanHash(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.PlanHash != want {
		t.Fatalf("state hash %q, want %q", st.PlanHash, want)
	}
	if len(want) != 64 {
		t.Fatalf("hash %q is not sha256 hex", want)
	}
}

func TestNextRefusesAPlanEditedAfterDispatch(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath := upForDrift(t, repo)
	editPlan(t, planPath)

	code, _, stderr := runCLI("next", "--poll", "10ms", "--timeout", "1s", "--plan", planPath)
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr)
	}
	if !strings.Contains(stderr, "plan changed since dispatch") || !strings.Contains(stderr, "--force") {
		t.Fatalf("refusal should name the drift and the way out:\n%s", stderr)
	}
	if _, err := os.Stat(briefPath(planPath, "A")); !os.IsNotExist(err) {
		t.Fatalf("a refused plan must not write a brief: %v", err)
	}
	st, _ := LoadState(statePath)
	if got := st.UnitStatus("A"); got != "planned" {
		t.Fatalf("unit %q, want planned", got)
	}

	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--force", "--plan", planPath)
	if code != 0 {
		t.Fatalf("forced next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "done: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, _ = LoadState(statePath)
	want, _ := PlanHash(planPath)
	if st.PlanHash != want {
		t.Fatalf("force must record the new hash: state %q, plan %q", st.PlanHash, want)
	}
}

func TestLandRefusesAPlanEditedAfterDispatch(t *testing.T) {
	planPath, _, repo := landSetup(t)
	commitIn(t, worktreePath(repo, "enc", "w1"), "a.txt", "one\n", "A")
	if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", planPath); code != 0 {
		t.Fatalf("set: %s", stderr)
	}
	editPlan(t, planPath)

	code, _, stderr := runCLI("land", "--plan", planPath)
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr)
	}
	if !strings.Contains(stderr, "plan changed since dispatch") {
		t.Fatalf("refusal should name the drift:\n%s", stderr)
	}
	if _, err := os.Stat(worktreePath(repo, "enc", "integration")); !os.IsNotExist(err) {
		t.Fatalf("a refused land must not cut the integration worktree: %v", err)
	}

	code, stdout, stderr := runCLI("land", "--force", "--plan", planPath)
	if code != 0 {
		t.Fatalf("forced land exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "merged 1 unit") {
		t.Fatalf("output:\n%s", stdout)
	}
}

func TestStatusShowsPlanDrift(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	planPath, _ := upForDrift(t, repo)

	code, stdout, stderr := runCLI("status", "--plan", planPath)
	if code != 0 {
		t.Fatalf("status exit %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "plan changed since dispatch") {
		t.Fatalf("an unedited plan must not drift:\n%s", stdout)
	}

	editPlan(t, planPath)
	code, stdout, stderr = runCLI("status", "--plan", planPath)
	if code != 0 {
		t.Fatalf("status exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "plan changed since dispatch") {
		t.Fatalf("status should flag the drift:\n%s", stdout)
	}

	code, stdout, stderr = runCLI("status", "--json", "--plan", planPath)
	if code != 0 {
		t.Fatalf("json status exit %d: %s", code, stderr)
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(stdout), &snap); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !snap.PlanDrift || snap.PlanHash == "" {
		t.Fatalf("snapshot should carry the drift and the current hash: %+v", snap)
	}
}
