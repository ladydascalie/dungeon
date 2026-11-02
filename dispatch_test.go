package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
  "tab list") echo '{"result":{"tabs":[{"tab_id":"w9:t1","label":"enc"}]}}' ;;
  "tab close") echo '{"result":{"type":"ok"}}' ;;
  "pane get")
    case "$3" in gone*) echo '{"error":{"code":"pane_not_found"}}' >&2; exit 1 ;; esac
    echo '{"result":{"pane":{"pane_id":"'"$3"'","tab_id":"w9:t1"}}}' ;;
  "agent focus")
    if grep -q "^$3:" "$FAKE_STARTED"; then echo '{"result":{"type":"ok"}}'
    else echo '{"error":{"code":"agent_not_found"}}' >&2; exit 1; fi ;;
  "tab focus") echo '{"result":{"type":"ok"}}' ;;
    "pane close") : > "$FAKE_STARTED"; echo '{"result":{"type":"ok"}}' ;;  # closing the pane kills the agent in it
  "pane split") echo '{"result":{"pane":{"pane_id":"w9:p2"}}}' ;;
    "workspace list") if [ -f "$FAKE_WS" ]; then echo '{"result":{"workspaces":[{"workspace_id":"w9","label":"enc"}]}}'; else echo '{"result":{"workspaces":[]}}'; fi ;;
  "workspace create") touch "$FAKE_WS"; echo '{"result":{"workspace":{"workspace_id":"w9"},"tab":{"tab_id":"w9:t1"},"root_pane":{"pane_id":"w9:p1"}}}' ;;
  "workspace close") rm -f "$FAKE_WS"; echo '{"result":{"type":"ok"}}' ;;
  "tab rename") echo '{"result":{"type":"ok"}}' ;;
  "tab create") echo '{"result":{"tab":{"tab_id":"w9:t9"},"root_pane":{"pane_id":"w9:p9"}}}' ;;
  "agent start") echo "$3:${FAKE_STATUS:-idle}" >> "$FAKE_STARTED"
                 echo '{"result":{"type":"ok"}}' ;;
  "agent send-keys")
    if [ "$4" = "ctrl+c" ]; then grep -v "^$3:" "$FAKE_STARTED" > "$FAKE_STARTED.tmp"; mv "$FAKE_STARTED.tmp" "$FAKE_STARTED"; fi
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
	// The backend is chosen from the environment, and this is a herdr session.
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_STARTED", startedFile)
	t.Setenv("FAKE_WS", filepath.Join(dir, "workspace"))
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
	for _, want := range []string{"workspace create", "tab rename", "agent start w1"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("herdr was not asked to %q:\n%s", want, calls)
		}
	}
	if !strings.Contains(string(calls), "--no-skills") {
		t.Fatalf("a lane must not load the operator's skills:\n%s", calls)
	}
}

// A retry must not die with Branch already exists. When the branch has work
// but no worktree, up attaches it and tells the agent what it inherited.
func TestUpResumesABranchWithNoWorktree(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	work := filepath.Join(t.TempDir(), "work")
	if _, err := gitRun(repo, "worktree", "add", "-b", "enc/w1", work, "main"); err != nil {
		t.Fatal(err)
	}
	commitIn(t, work, "work.txt", "one\n", "w1 work")
	if _, err := gitRun(repo, "worktree", "remove", work); err != nil {
		t.Fatal(err)
	}
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	code, stdout, stderr := runCLI("up", "--no-dispatch", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "w1: resumed enc/w1") || !strings.Contains(stdout, "ahead of main: 1") {
		t.Fatalf("output %q", stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	ms := st.Minions["w1"]
	if !ms.Resumed || ms.Recreated {
		t.Fatalf("minion %+v: want resumed", ms)
	}
	if ms.Branch != "enc/w1" || ms.Ahead != 1 || ms.Head == "" || ms.Worktree == "" {
		t.Fatalf("minion %+v", ms)
	}
	if _, err := os.Stat(filepath.Join(ms.Worktree, "work.txt")); err != nil {
		t.Fatalf("the reused branch must keep its commit: %v", err)
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
	for _, want := range []string{"Do the thing.", "src/**", "accept: true", "herdr pane run w9:p9", "A: w1 finished", "What you may not do"} {
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
	fakeHerdr(t, "w1-aa:working\nw2-aa:idle\n")
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
	// The live lanes belong to this run: the state's token names them, so up
	// does not mistake another run's bare w1 for its own.
	own := State{Encounter: "enc", Token: "aa", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	if err := SaveState(statePath, own); err != nil {
		t.Fatal(err)
	}

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

func TestBriefHasNoPingWithoutAParentPane(t *testing.T) {
	m := Minion{ID: "w1", Role: RoleWorker}
	u := Unit{ID: "A", Goal: "Do it.", Paths: []string{"src/**"}}
	text := briefText(briefData{minion: m, unit: u, dir: "/repo", report: "/enc/reports/A.md"})
	if strings.Contains(text, "Ping the parent") {
		t.Fatalf("no parent pane means no ping:\n%s", text)
	}
}

// A reviewer must read the branch it reviews: its cwd is its writer's worktree,
// not the base checkout. A staff minion with no pair keeps the base checkout.
func TestMinionDirReadsThePairedWorktree(t *testing.T) {
	p := decodeOK(t, goodPlan)
	w1, _ := findMinion(p, "w1")
	r1, _ := findMinion(p, "r1")
	scout, _ := findMinion(p, "scout")
	writerDir := worktreePath(p.Repo, p.Encounter, "w1")

	if got := minionDir(p, w1); got != writerDir {
		t.Fatalf("a writer works in its own worktree, got %q", got)
	}
	if got := minionDir(p, r1); got != writerDir {
		t.Fatalf("a reviewer of w1 must read w1's worktree, got %q", got)
	}
	if got := minionDir(p, scout); got != p.Repo {
		t.Fatalf("a scout with no pair keeps the base checkout, got %q", got)
	}
}

func TestBriefLeadsWithAReusedBranch(t *testing.T) {
	p := decodeOK(t, goodPlan)
	m, _ := findMinion(p, "w1")
	var u Unit
	for _, x := range p.Units {
		if x.ID == "api-timeout" {
			u = x
		}
	}
	text := briefText(briefData{
		minion: m,
		unit:   u,
		dir:    "/lane",
		report: "/enc/reports/api-timeout.md",
		base:   p.Base,
		lane: MinionState{
			Branch: "enc/w1", Head: "1a2b3c4", Subject: "Fix auth",
			Ahead: 3, Dirty: 2, Resumed: true,
		},
	})
	reused := strings.Index(text, "## Lane branch reused")
	goal := strings.Index(text, u.Goal)
	if reused < 0 || goal < 0 || reused > goal {
		t.Fatalf("the reused branch must lead the brief:\n%s", text)
	}
	for _, want := range []string{
		"branch: enc/w1",
		"head: 1a2b3c4 Fix auth",
		"ahead of main: 3",
		"uncommitted files: 2",
		"Do not reset or delete them.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("brief missing %q:\n%s", want, text)
		}
	}
}

func TestBriefNamesThePairedWorktreeAndDepReports(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, ".dungeon", "plan.yaml")
	p := decodeOK(t, goodPlan)
	m, _ := findMinion(p, "r1")
	var u Unit
	for _, x := range p.Units {
		if x.ID == "review-api" {
			u = x
		}
	}

	path, err := writeBrief(newHerdr(), briefInput{
		opts: dispatchOpts{planPath: planPath}, plan: p, minion: m, unit: u, state: &State{},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	if want := "worktree: " + worktreePath(p.Repo, p.Encounter, "w1"); !strings.Contains(text, want) {
		t.Fatalf("brief missing %q:\n%s", want, text)
	}
	depReport, err := filepath.Abs(reportPath(planPath, "api-timeout"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "## reports from deps") {
		t.Fatalf("brief has no dep reports section:\n%s", text)
	}
	if want := "- api-timeout: " + depReport; !strings.Contains(text, want) {
		t.Fatalf("brief missing %q:\n%s", want, text)
	}
}

// TestBriefTellsALaneToResumeWhenTheDispatchArrivesAgain is the duplicate
// dispatch guard. The brief is all the lane reads, so it must say that a
// second arrival is the same unit in the same worktree, and that the report is
// written once.
func TestBriefTellsALaneToResumeWhenTheDispatchArrivesAgain(t *testing.T) {
	p := decodeOK(t, goodPlan)
	m, _ := findMinion(p, "w1")
	var u Unit
	for _, x := range p.Units {
		if x.ID == "api-timeout" {
			u = x
		}
	}
	text := briefText(briefData{
		minion: m,
		unit:   u,
		dir:    "/lane",
		report: "/enc/reports/api-timeout.md",
	})

	again := strings.Index(text, "## If this dispatch arrives again")
	if again < 0 {
		t.Fatalf("brief does not say what to do when it arrives again:\n%s", text)
	}
	if accept := strings.Index(text, "accept: "+u.Accept); accept < 0 || again > accept {
		t.Fatalf("the repeated-dispatch section must come before the accept line:\n%s", text)
	}
	for _, want := range []string{
		"same unit and the same worktree",
		"continue from what is already there",
		"do not restart",
		"write the report once",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("brief missing %q:\n%s", want, text)
		}
	}
	// The instruction is a short note, not a second contract.
	section := text[again:strings.Index(text, "accept: "+u.Accept)]
	if lines := strings.Count(section, "\n"); lines > 4 {
		t.Fatalf("the repeated-dispatch section is %d lines, want a short one:\n%s", lines, section)
	}

	// The rest of the brief still names what the lane owns and how it passes.
	if !strings.Contains(text, "worktree: /lane") {
		t.Fatalf("brief lost its worktree:\n%s", text)
	}
	for _, path := range u.Paths {
		if !strings.Contains(text, "  - "+path) {
			t.Fatalf("brief lost owned path %q:\n%s", path, text)
		}
	}
}

func TestOfMustNameAWriter(t *testing.T) {
	plan := strings.Replace(goodPlan, "{id: r1, role: reviewer, of: w1}",
		"{id: r1, role: reviewer, of: scout}", 1)
	wantCodes(t, Validate(decodeOK(t, plan)), EOfNotWriter)

	plan = strings.Replace(goodPlan, "{id: r1, role: reviewer, of: w1}",
		"{id: r1, role: reviewer, of: nope}", 1)
	wantCodes(t, Validate(decodeOK(t, plan)), EUnknownMinion)
}

// verdictCase builds a reviewer unit and, when report is not empty, writes that
// report. The paired worktree exists so a no-verdict unit can still run its
// accept command there.
func verdictCase(t *testing.T, verdict, report string) (dispatchOpts, Plan, *State, Unit, Minion) {
	t.Helper()
	dir := t.TempDir()
	planPath := filepath.Join(dir, ".dungeon", "plan.yaml")
	p := Plan{Encounter: "enc", Repo: dir, Minions: []Minion{
		{ID: "w1", Role: RoleWorker, Worktree: "w1"},
		{ID: "r1", Role: RoleReviewer, Of: "w1"},
	}}
	u := Unit{ID: "R", Minion: "r1", Verdict: verdict, Accept: "true"}
	if verdict == "" {
		u.Minion = "w1"
	}
	m, ok := findMinion(p, u.Minion)
	if !ok {
		t.Fatalf("minion %q not found", u.Minion)
	}
	if err := os.MkdirAll(worktreePath(p.Repo, p.Encounter, "w1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if report != "" {
		path := reportPath(planPath, u.ID)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := &State{Encounter: "enc", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	o := dispatchOpts{planPath: planPath, acceptTimeout: time.Minute}
	return o, p, st, u, m
}

func TestVerdictPassMarksDone(t *testing.T) {
	o, p, st, u, m := verdictCase(t, "Merge verdict: PASS",
		"# Report\n\n  Merge verdict: PASS  \n\nDone.\n")
	if got := settle(t.Context(), o, p, st, u, m); got != "done" {
		t.Fatalf("settle %q, want done: %s", got, st.Units[u.ID].Note)
	}
	if want := "verdict: Merge verdict: PASS"; st.Units[u.ID].Note != want {
		t.Fatalf("receipt %q, want %q", st.Units[u.ID].Note, want)
	}
}

func TestVerdictFailIsFailedAndNamesWhatItSaw(t *testing.T) {
	o, p, st, u, m := verdictCase(t, "Merge verdict: PASS",
		"Merge verdict: FAIL\nthen some detail\n")
	if got := settle(t.Context(), o, p, st, u, m); got != "failed" {
		t.Fatalf("settle %q, want failed", got)
	}
	note := st.Units[u.ID].Note
	for _, want := range []string{"Merge verdict: PASS", "Merge verdict: FAIL"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note %q missing %q", note, want)
		}
	}
}

func TestVerdictFailWithNoVerdictLineSaysNone(t *testing.T) {
	o, p, st, u, m := verdictCase(t, "Merge verdict: PASS", "# Report\n\nNothing shipped.\n")
	if got := settle(t.Context(), o, p, st, u, m); got != "failed" {
		t.Fatalf("settle %q, want failed", got)
	}
	note := st.Units[u.ID].Note
	if !strings.Contains(note, "Merge verdict: PASS") || !strings.Contains(note, "none") {
		t.Fatalf("note %q should name the wanted verdict and none", note)
	}
}

func TestVerdictMissingReportBlocks(t *testing.T) {
	o, p, st, u, m := verdictCase(t, "Merge verdict: PASS", "")
	if got := settle(t.Context(), o, p, st, u, m); got != "blocked" {
		t.Fatalf("settle %q, want blocked", got)
	}
	if !strings.Contains(st.Units[u.ID].Note, "no report") {
		t.Fatalf("note %q should name the missing report", st.Units[u.ID].Note)
	}
}

func TestNoVerdictStillRunsTheAcceptCommand(t *testing.T) {
	o, p, st, u, m := verdictCase(t, "", "did it\n")
	if got := settle(t.Context(), o, p, st, u, m); got != "done" {
		t.Fatalf("settle %q, want done: %s", got, st.Units[u.ID].Note)
	}
	if !strings.Contains(st.Units[u.ID].Note, "accept: true ok") {
		t.Fatalf("receipt %q should be the accept command's", st.Units[u.ID].Note)
	}
}

func TestBriefStatesTheVerdictInsteadOfAccept(t *testing.T) {
	u := Unit{ID: "R", Goal: "Review it.", Verdict: "Merge verdict: PASS", Accept: "should not appear"}
	m := Minion{ID: "r1", Role: RoleReviewer}
	text := briefText(briefData{minion: m, unit: u, dir: "/repo", report: "/enc/reports/R.md"})
	if !strings.Contains(text, "verdict: Merge verdict: PASS") {
		t.Fatalf("brief should state the verdict line:\n%s", text)
	}
	if strings.Contains(text, "should not appear") {
		t.Fatalf("a verdict unit must not get an accept command:\n%s", text)
	}
}

// A backend without a status protocol still settles a unit: when the lane
// state is unknown, the accept command is the judge.
func TestNextSettlesAnUnknownLaneByAccept(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	t.Setenv("FAKE_STATUS", "busy") // a word dungeon does not know: unknown
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "3s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "done: A") {
		t.Fatalf("an unknown lane should settle by accept, got:\n%s", stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit %q, want done", got)
	}
}

// An unknown lane is held until its report lands, then judged by accept. The
// report is the only finish signal a backend with no status protocol has.
func TestNextLeavesAnUnknownLaneUntilItsReportLands(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_STATUS", "busy") // a word dungeon does not know: unknown
	t.Setenv("FAKE_REPORT", "0")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "60ms", "--plan", planPath)
	if code != 0 {
		t.Fatalf("first next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "timeout: lanes still working") {
		t.Fatalf("an unknown lane with no report must be held, got:\n%s", stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "working" {
		t.Fatalf("unit %q, want working until the report lands", got)
	}

	// The report lands: the accept command is the judge now.
	report := reportPath(planPath, "A")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("did the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCLI("next", "--poll", "10ms", "--timeout", "2s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("second next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "done: A") {
		t.Fatalf("the report should release the accept command, got:\n%s", stdout)
	}
	st, err = LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit %q, want done", got)
	}
}

// A lane leaves its work in the worktree. Dungeon commits it on the lane
// branch, names the unit, then runs the accept. The lane never commits.
func TestDungeonCommitsLaneWorkOnTheLaneBranch(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	wt := worktreePath(repo, "enc", "w1")
	if err := os.WriteFile(filepath.Join(wt, "NOTES.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := strings.Replace(planFor(repo), "paths: [src/**]", "paths: [NOTES.md]", 1)
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--force", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "done: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, _ := LoadState(statePath)
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit %q, want done: %s", got, st.Units["A"].Note)
	}

	subject, err := gitRun(repo, "log", "-1", "--format=%s", "enc/w1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(subject, "A") {
		t.Fatalf("commit message %q does not name the unit", subject)
	}
	files, err := gitRun(repo, "show", "--name-only", "--format=", "enc/w1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files, "NOTES.md") {
		t.Fatalf("lane branch does not hold NOTES.md:\n%s", files)
	}
	if clean, _ := changedFiles(wt); len(clean) != 0 {
		t.Fatalf("worktree still dirty after the commit: %v", clean)
	}
}

// A change outside the unit's paths still blocks the unit and names the files.
func TestAChangeOutsideThePathsStillBlocks(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	wt := worktreePath(repo, "enc", "w1")
	if err := os.WriteFile(filepath.Join(wt, "elsewhere.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--force", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "blocked: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, _ := LoadState(statePath)
	if got := st.Units["A"].Note; !strings.Contains(got, "outside your paths: elsewhere.txt") {
		t.Fatalf("note %q", got)
	}
	if subject, _ := gitRun(repo, "log", "-1", "--format=%s", "enc/w1"); subject != "root" {
		t.Fatalf("an outside change must not be committed, head %q", subject)
	}
}

// reconcileFixture builds one working unit beside a fake herdr. The plan is
// built in memory: reconcile and dispatchUnit read the plan value, not the file.
func reconcileFixture(t *testing.T, started string) (Plan, dispatchOpts, *State, string) {
	t.Helper()
	logPath := fakeHerdr(t, started)
	dir := t.TempDir()
	planPath := filepath.Join(dir, ".dungeon", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte("encounter: enc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Plan{
		Encounter: "enc",
		Repo:      filepath.Join(dir, "repo"),
		Base:      "main",
		Minions:   []Minion{{ID: "w1", Role: RoleWorker}},
		Units: []Unit{{
			ID: "A", Minion: "w1", Paths: []string{"src/**"},
			Goal: "Do the thing.", Accept: "true",
		}},
	}
	st := &State{Encounter: "enc", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	if err := st.SetUnit("A", "working", "brief "+briefPath(planPath, "A")); err != nil {
		t.Fatal(err)
	}
	o := dispatchOpts{
		planPath:      planPath,
		statePath:     StatePath(planPath),
		timeout:       time.Second,
		acceptTimeout: time.Second,
		poll:          time.Millisecond,
		out:           io.Discard,
	}
	return p, o, st, logPath
}

func promptCalls(t *testing.T, logPath string) int {
	t.Helper()
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(calls), "agent prompt")
}

// A working unit whose prompt was never confirmed is told its brief again.
// The successful retry clears the marker, and a later pass leaves it alone.
func TestReconcileRepromptsAnUnconfirmedWorkingUnit(t *testing.T) {
	p, o, st, logPath := reconcileFixture(t, "w1:working\n")
	st.SetPromptPending("A", true)

	d := &digest{}
	if !reconcile(t.Context(), newHerdr(), o, p, st, d) {
		t.Fatal("a re-prompt is progress")
	}
	if st.PromptIsPending("A") {
		t.Fatal("the marker must clear after Prompt succeeds")
	}
	if got := promptCalls(t, logPath); got != 1 {
		t.Fatalf("prompted %d times, want once", got)
	}
	if len(d.Dispatched) != 1 || d.Dispatched[0] != "A" {
		t.Fatalf("digest dispatched %v, want [A]", d.Dispatched)
	}

	reconcile(t.Context(), newHerdr(), o, p, st, &digest{})
	if got := promptCalls(t, logPath); got != 1 {
		t.Fatalf("a confirmed unit was prompted again: %d calls", got)
	}
}

// A confirmed working unit is still working, not a retry candidate.
func TestReconcileLeavesAConfirmedWorkingUnitAlone(t *testing.T) {
	p, o, st, logPath := reconcileFixture(t, "w1:working\n")
	st.SetPromptPending("A", false)

	reconcile(t.Context(), newHerdr(), o, p, st, &digest{})

	if got := promptCalls(t, logPath); got != 0 {
		t.Fatalf("a confirmed unit was prompted %d times", got)
	}
	if got := st.UnitStatus("A"); got != "working" {
		t.Fatalf("unit %q, want working", got)
	}
}

// A report proves the lane got its brief, so the stale marker is cleared
// without sending the prompt a second time.
func TestReconcileDoesNotRepromptALaneThatReported(t *testing.T) {
	p, o, st, logPath := reconcileFixture(t, "w1:working\n")
	st.SetPromptPending("A", true)
	if err := os.MkdirAll(filepath.Dir(reportPath(o.planPath, "A")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath(o.planPath, "A"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reconcile(t.Context(), newHerdr(), o, p, st, &digest{})

	if got := promptCalls(t, logPath); got != 0 {
		t.Fatalf("a lane with a report was prompted %d times", got)
	}
	if st.PromptIsPending("A") {
		t.Fatal("a report must clear the marker")
	}
}

// A state written before prompt_pending existed reads as confirmed, so an old
// encounter does not re-prompt a fleet that is already working.
func TestReconcileLeavesALegacyWorkingStateUntouched(t *testing.T) {
	p, o, _, logPath := reconcileFixture(t, "w1:working\n")
	legacy := `{
  "encounter": "enc",
  "units": {"A": {"status": "working", "note": "old brief", "at": "2025-01-01T00:00:00Z"}},
  "minions": {}
}
`
	if err := os.WriteFile(o.statePath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(o.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.PromptIsPending("A") {
		t.Fatal("a state without the field must read as confirmed")
	}

	reconcile(t.Context(), newHerdr(), o, p, &st, &digest{})

	if got := promptCalls(t, logPath); got != 0 {
		t.Fatalf("a legacy working unit was prompted %d times", got)
	}
	after, err := os.ReadFile(o.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != legacy {
		t.Fatalf("a legacy state was rewritten:\n%s", after)
	}
}
