package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHerdr puts a herdr on PATH that records what it was asked, tracks the
// lanes it started, and can write the report a real lane would write. One test
// can therefore run a whole dispatch without a pane.
func fakeHerdr(t *testing.T, started string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	startedFile := filepath.Join(dir, "started")
	if err := os.WriteFile(startedFile, []byte(started), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$1 $2" in
  "agent list")
    printf '{"result":{"agents":['
    first=1
    while IFS=: read -r name status; do
      [ -z "$name" ] && continue
      [ "$first" = "0" ] && printf ','
      first=0
      printf '{"name":"%s","pane_id":"w9:p2","agent_status":"%s","cwd":"/x"}' "$name" "${status:-idle}"
    done < "$FAKE_STARTED"
    printf ']}}'
    ;;
  "tab create") echo '{"result":{"root_pane":{"pane_id":"w9:p1"}}}' ;;
  "tab list") echo '{"result":{"tabs":[{"tab_id":"w9:t1","label":"enc"}]}}' ;;
  "tab close") echo '{"result":{"type":"ok"}}' ;;
  "pane close") echo '{"result":{"type":"ok"}}' ;;
  "pane split") echo '{"result":{"pane":{"pane_id":"w9:p2"}}}' ;;
  "agent start") echo "$3:${FAKE_STATUS:-idle}" >> "$FAKE_STARTED"
                 echo '{"result":{"type":"ok"}}' ;;
  "agent prompt")
    if [ "$FAKE_REPORT" = "1" ]; then
      mkdir -p "$FAKE_ENCOUNTER/reports"
      echo "did the thing" > "$FAKE_ENCOUNTER/reports/$FAKE_UNIT.md"
    fi
    echo '{"result":{"type":"ok"}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_STARTED", startedFile)
	return log
}

// gitRepo builds a real repository so worktree creation is exercised, not mocked.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "root")
	return dir
}

func encounter(t *testing.T, repo, plan string) (planPath, statePath, dir string) {
	t.Helper()
	dir = t.TempDir()
	planPath = filepath.Join(dir, ".dungeon", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath = StatePath(planPath)
	t.Setenv("FAKE_ENCOUNTER", filepath.Dir(planPath))
	t.Setenv("FAKE_UNIT", "A")
	return planPath, statePath, dir
}

const dispatchPlan = `encounter: enc
repo: %REPO%
base: main
concurrency: 1
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: A
    minion: w1
    paths: [src/**]
    goal: Do the thing.
    accept: true
`

func planFor(repo string) string {
	return strings.Replace(dispatchPlan, "%REPO%", repo, 1)
}

func TestUpSpawnsOneLaneAndCutsAWorktree(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	code, stdout, stderr := runCLI("up", "--no-dispatch", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "1 spawned") {
		t.Fatalf("output %q", stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.MinionStatus("w1"); got != "spawned" {
		t.Fatalf("minion %q, want spawned", got)
	}
	wt := worktreePath(repo, "enc", "w1")
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Fatalf("worktree not cut at %s: %v", wt, err)
	}
	calls, _ := os.ReadFile(log)
	for _, want := range []string{"tab create", "pane split", "agent start w1"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("herdr was not asked to %q:\n%s", want, calls)
		}
	}
	if !strings.Contains(string(calls), "--no-skills") {
		t.Fatalf("a lane must not load the operator's skills:\n%s", calls)
	}
}

func TestUpIsIdempotent(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	planPath, _, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("first up: %s", stderr)
	}
	code, stdout, stderr := runCLI("up", "--no-dispatch", "--plan", planPath)
	if code != 0 {
		t.Fatalf("second up: %s", stderr)
	}
	if !strings.Contains(stdout, "0 spawned, 1 already live") {
		t.Fatalf("a live minion must be left alone, got %q", stdout)
	}
}

func TestNextDispatchesAndSettlesOnTheAcceptCommand(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "dispatched: A") || !strings.Contains(stdout, "done: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit %q, want done", got)
	}
	if !strings.Contains(st.Units["A"].Note, "accept: true ok") {
		t.Fatalf("receipt %q", st.Units["A"].Note)
	}

	brief, err := os.ReadFile(briefPath(planPath, "A"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Do the thing.", "src/**", "accept: true", "What you may not do"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("brief missing %q:\n%s", want, brief)
		}
	}
}

func TestNextRejectsWhenTheAcceptCommandFails(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	plan := strings.Replace(planFor(repo), "accept: true", "accept: false", 1)
	planPath, statePath, _ := encounter(t, repo, plan)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "rejected: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, _ := LoadState(statePath)
	if got := st.UnitStatus("A"); got != "rejected" {
		t.Fatalf("unit %q, want rejected", got)
	}
}

func TestNextBlocksWhenTheLaneLeavesNoReport(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "0")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "blocked: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, _ := LoadState(statePath)
	if got := st.Units["A"].Note; !strings.Contains(got, "no report") {
		t.Fatalf("note %q", got)
	}
}

func TestNextRefusesToDispatchWithNoLane(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "2s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "no lane: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, _ := LoadState(statePath)
	if got := st.UnitStatus("A"); got != "planned" {
		t.Fatalf("unit %q, want planned", got)
	}
}

func TestNextRespectsConcurrency(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "w1:working\nw2:idle\n")
	plan := strings.Replace(planFor(repo), "concurrency: 1", "concurrency: 1", 1)
	plan = strings.Replace(plan, `  - {id: w1, role: worker, worktree: w1}`, `  - {id: w1, role: worker, worktree: w1}
  - {id: w2, role: worker, worktree: w2}`, 1)
	plan = strings.Replace(plan, `  - id: A
    minion: w1
    paths: [src/**]
    goal: Do the thing.
    accept: true`, `  - id: A
    minion: w1
    paths: [src/**]
    goal: One.
    accept: true
  - id: B
    minion: w2
    paths: [lib/**]
    goal: Two.
    accept: true`, 1)
	planPath, statePath, _ := encounter(t, repo, plan)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	// w1 is already working and takes the only slot, so B must wait.
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "2s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	st, _ := LoadState(statePath)
	if got := st.UnitStatus("B"); got != "planned" {
		t.Fatalf("B is %q, want planned while the cap is full\n%s", got, stdout)
	}
}
