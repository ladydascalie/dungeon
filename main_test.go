package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain isolates the suite from the operator's worktrunk config. A
// post-start hook would run inside a test repo and race the temp directory
// cleanup. dungeon passes the worktree path explicitly, so an empty config is
// the whole setting a test needs.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dungeon-wt")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Setenv("WORKTRUNK_CONFIG_PATH", cfg)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func writePlan(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCheckClean(t *testing.T) {
	code, stdout, _ := runCLI("check", writePlan(t, goodPlan))
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "ok: fix-auth-timeouts, 6 minions, 6 units") {
		t.Fatalf("unexpected output: %q", stdout)
	}
}

func TestCheckFindings(t *testing.T) {
	broken := strings.Replace(goodPlan, "    minion: w1\n    paths: [apps/api/**]",
		"    minion: scout\n    paths: [apps/api/**]", 1)
	code, stdout, _ := runCLI("check", writePlan(t, broken))
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stdout, "E_DM_PLAYS_MINION") {
		t.Fatalf("output should name the code: %q", stdout)
	}
}

func TestCheckJSON(t *testing.T) {
	code, stdout, _ := runCLI("check", "--json", writePlan(t, goodPlan))
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	var got []Finding
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v in %q", err, stdout)
	}
	if len(got) != 0 {
		t.Fatalf("want no findings, got %v", got)
	}
}

func TestCheckMissingFile(t *testing.T) {
	code, stdout, stderr := runCLI("check", filepath.Join(t.TempDir(), "nope.yaml"))
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("nothing should go to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "nope.yaml") {
		t.Fatalf("stderr should name the file: %q", stderr)
	}
}

func TestCheckPositionalBeatsDefault(t *testing.T) {
	code, _, _ := runCLI("check", writePlan(t, goodPlan))
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
}

func TestUsage(t *testing.T) {
	if code, _, _ := runCLI(); code != 2 {
		t.Fatalf("no args: exit %d, want 2", code)
	}
	if code, _, _ := runCLI("dance"); code != 2 {
		t.Fatalf("unknown command: exit %d, want 2", code)
	}
	if code, stdout, _ := runCLI("help"); code != 0 || !strings.Contains(stdout, "dungeon check") {
		t.Fatalf("help: exit %d, output %q", code, stdout)
	}
}

func TestSetWritesStateAndStatusReadsIt(t *testing.T) {
	path := writePlan(t, goodPlan)
	state := filepath.Join(t.TempDir(), "state.json")

	code, stdout, stderr := runCLI("set", "unit", "api-timeout", "working",
		"--note", "brief written", "--plan", path, "--state", state)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "api-timeout working (was planned)") {
		t.Fatalf("output %q", stdout)
	}

	code, stdout, _ = runCLI("status", "--plan", path, "--state", state)
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if !strings.Contains(stdout, "WORKING (1)") || !strings.Contains(stdout, "brief written") {
		t.Fatalf("status output:\n%s", stdout)
	}
}

func TestSetRefusesUnknownIDsAndStatuses(t *testing.T) {
	path := writePlan(t, goodPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	if code, _, _ := runCLI("set", "unit", "nope", "working", "--plan", path, "--state", state); code != 2 {
		t.Fatalf("unknown unit: exit %d, want 2", code)
	}
	if code, _, _ := runCLI("set", "unit", "recon", "complete", "--plan", path, "--state", state); code != 2 {
		t.Fatalf("unknown status: exit %d, want 2", code)
	}
	if code, _, _ := runCLI("set", "minion", "nope", "busy", "--plan", path, "--state", state); code != 2 {
		t.Fatalf("unknown minion: exit %d, want 2", code)
	}
	if code, _, _ := runCLI("set", "cat", "recon", "busy", "--plan", path, "--state", state); code != 2 {
		t.Fatalf("unknown kind: exit %d, want 2", code)
	}
}

func TestSetWarnsWhenNoAgentIsInThePane(t *testing.T) {
	path := writePlan(t, goodPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	fakeHerdr(t, "")

	code, _, stderr := runCLI("set", "unit", "api-timeout", "working", "--plan", path, "--state", state)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "no live agent") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestStatusJSONShowsPanes(t *testing.T) {
	path := writePlan(t, goodPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	fakeHerdr(t, "w1:working\n")
	if code, _, stderr := runCLI("set", "unit", "api-timeout", "working", "--plan", path, "--state", state); code != 0 {
		t.Fatalf("set: %s", stderr)
	}

	code, stdout, _ := runCLI("status", "--json", "--plan", path, "--state", state)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(stdout), &snap); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	var found UnitRow
	for _, u := range snap.Units {
		if u.ID == "api-timeout" {
			found = u
		}
	}
	if found.Pane != "w9:p2" || found.Agent != "working" {
		t.Fatalf("row %+v", found)
	}
}

func TestStatusRefusesABrokenPlan(t *testing.T) {
	broken := strings.Replace(goodPlan, "    minion: w1\n    paths: [apps/api/**]",
		"    minion: scout\n    paths: [apps/api/**]", 1)
	code, stdout, stderr := runCLI("status", "--plan", writePlan(t, broken))
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("no table for a broken plan: %q", stdout)
	}
	if !strings.Contains(stderr, "E_DM_PLAYS_MINION") {
		t.Fatalf("stderr should name the finding: %q", stderr)
	}
}

func TestStateEncounterMismatch(t *testing.T) {
	path := writePlan(t, goodPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	other := State{Encounter: "something-else", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	if err := SaveState(state, other); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI("set", "unit", "recon", "working", "--plan", path, "--state", state)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "something-else") {
		t.Fatalf("stderr %q", stderr)
	}
}
