package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
    # The prompt names the brief, so any encounter's lane can answer: the
    # report sits in the encounter directory, one level above briefs/.
    brief=$(printf '%s' "$4" | sed -n 's/^Read \(.*\) and do it\..*$/\1/p')
    unit=$(basename "$brief" .md)
    enc=$(dirname "$(dirname "$brief")")
    if [ "$FAKE_REPORT" = "1" ]; then
      mkdir -p "$enc/reports"
      echo "did the thing" > "$enc/reports/$unit.md"
    fi
    if [ -n "$FAKE_LANE_SH" ]; then
      # A lane that does git work of its own: merge a landed branch, fast-forward,
      # cherry-pick. The script runs where the lane is.
      ( cd "$FAKE_WT" && sh "$FAKE_LANE_SH" )
    fi
    if [ -n "$FAKE_COMMITS" ] && [ -f "$FAKE_COMMITS" ]; then
      # One line per unit: unit:path:message. The lane writes the file and
      # commits it, the way a lane that ignores the rule would.
      line=$(sed -n "s/^$unit://p" "$FAKE_COMMITS" | head -1)
      if [ -n "$line" ]; then
        file=${line%%:*}
        msg=${line#*:}
        mkdir -p "$(dirname "$FAKE_WT/$file")"
        echo "lane work" > "$FAKE_WT/$file"
        git -C "$FAKE_WT" add -A
        git -C "$FAKE_WT" -c user.name=test -c user.email=test@example.com \
          commit -q -m "$msg"
      fi
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
	if strings.Contains(text, "Notify the parent") {
		t.Fatalf("no parent pane means no ping:\n%s", text)
	}
}

func TestBriefNamesTheBoxWhenTheBackendPosts(t *testing.T) {
	m := Minion{ID: "w1", Role: RoleWorker}
	u := Unit{ID: "A", Goal: "Do it.", Paths: []string{"src/**"}}
	text := briefText(briefData{
		minion:   m,
		unit:     u,
		dir:      "/repo",
		report:   "/enc/reports/A.md",
		ping:     "dm msg post --key lane/w1 --state done --msg done",
		pingNote: "The parent reads the session box. Do not wait for a reply.",
	})
	if !strings.Contains(text, "Notify the parent") {
		t.Fatalf("brief has no notify section:\n%s", text)
	}
	if !strings.Contains(text, "The parent reads the session box") {
		t.Fatalf("brief does not name the box:\n%s", text)
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

// The brief asks a lane to verify before it settles, and to say so when it
// cannot. A confident summary is not evidence: most failed runs end with one.
func TestBriefAsksTheLaneToVerifyBeforeSettling(t *testing.T) {
	u := Unit{ID: "A", Goal: "Change it.", Accept: "go test ./..."}
	m := Minion{ID: "a1", Role: RoleWorker}
	text := briefText(briefData{minion: m, unit: u, dir: "/repo", report: "/enc/reports/A.md"})
	for _, want := range []string{"## Before you settle", "write \"unverified\" in the report"} {
		if !strings.Contains(text, want) {
			t.Fatalf("brief should carry %q:\n%s", want, text)
		}
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
	// Uncommitted work inside the paths is still dungeon's to commit, so the
	// note reads exactly as it did before the guard.
	note := st.Units["A"].Note
	if !strings.Contains(note, "accept: true ok") {
		t.Fatalf("note %q lost the accept receipt", note)
	}
	if strings.Contains(note, "lane committed") {
		t.Fatalf("note %q must not claim the lane committed: dungeon did", note)
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
	if !strings.Contains(stdout, "plan fix: A: widen the unit's paths") {
		t.Fatalf("next must name the plan edit:\n%s", stdout)
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

// A blocked or unproven unit whose reason lives in the plan gets a plan fix
// hint. A loop fix is unproven on purpose, so it never gets one.
func TestPlanFixesNameThePlanEdit(t *testing.T) {
	p := expanded(t)
	st := &State{Units: map[string]UnitState{}}
	st.Units["polish-fix-1"] = UnitState{Status: "unproven", Note: "no accept command, so nothing can prove it"}
	st.Units["A"] = UnitState{Status: "unproven", Note: "no accept command, so nothing can prove it"}
	st.Units["A2"] = UnitState{Status: "blocked", Note: "changed files outside your paths: b/other.go"}
	st.Units["A3"] = UnitState{Status: "blocked", Note: "lane is waiting on you in w9p1"}

	fixes := planFixes(p, st, digest{Unproven: []string{"polish-fix-1", "A"}, Blocked: []string{"A2", "A3"}})
	if len(fixes) != 2 {
		t.Fatalf("plan fixes %v", fixes)
	}
	if !strings.Contains(fixes[0], "A: add an accept command") {
		t.Fatalf("unproven hint %q", fixes[0])
	}
	if !strings.Contains(fixes[1], "A2: widen the unit's paths") {
		t.Fatalf("blocked hint %q", fixes[1])
	}
}

// reapSpy wraps a mux and records the key the reap notice was posted under.
type reapSpy struct {
	mux
	key string
}

func (r *reapSpy) MarkReadyToReap(ctx context.Context, key string) error {
	r.key = key
	return nil
}

// F14: a settled lane tells the multiplexer it can be reaped under the lane's
// box key, so a parent watching the lane/ prefix sees it.
func TestReconcileMarksTheLaneReadyToReapUnderItsKey(t *testing.T) {
	p, o, st, _ := reconcileFixture(t, "w1:idle\n")
	if err := os.MkdirAll(lanePath(p, "w1"), 0o755); err != nil {
		t.Fatal(err)
	}
	report := reportPath(o.planPath, "A")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("did it\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spy := &reapSpy{mux: newHerdr()}
	reconcile(t.Context(), spy, o, p, st, &digest{})
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit %q, want done: %s", got, st.Units["A"].Note)
	}
	if want := laneBoxKey(p.Encounter, "w1"); spy.key != want {
		t.Fatalf("reap notice key %q, want %q", spy.key, want)
	}
}

// F16: a gate is proven by its exit status. One that exits 0 and leaves a child
// holding its output still passed, even though the wait is bounded.
func TestAGateThatExitsZeroIsProvenEvenIfItLeavesAChild(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	plan := strings.Replace(planFor(repo), "accept: true", "accept: (sleep 3; touch leak.txt) & true", 1)
	planPath, statePath, _ := encounter(t, repo, plan)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "30s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "done: A") {
		t.Fatalf("a gate that exited 0 must be proven:\n%s", stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit %q, want done: %s", got, st.Units["A"].Note)
	}
}

// waitGone blocks until pid is no longer a live process.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for running(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d survived the gate kill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An interrupted accept must reach the gate's group and nothing else. The gate
// is a shell that forks, so the kill has to take the fork with it, and it must
// not reach a sibling in this test's own group: that is what a wrong pgid, a
// kill(0) or a kill of the gate's own pid followed by its parent would do. All
// three leave the caller with no output and a non-zero exit.
func TestAnInterruptedAcceptKillsOnlyTheGateGroup(t *testing.T) {
	sibling := exec.Command("sleep", "60")
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sibling.Process.Kill()
		_, _ = sibling.Process.Wait()
	})

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "gate.pid")
	childFile := filepath.Join(dir, "child.pid")
	script := "echo $$ > '" + pidFile + "'\nsleep 60 &\necho $! > '" + childFile + "'\nwait"

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := acceptCommand(ctx, dir, script)
		done <- err
	}()

	gate := waitForPid(t, pidFile)
	forked := waitForPid(t, childFile)
	group, err := syscall.Getpgid(gate)
	if err != nil {
		t.Fatalf("pgid of the gate %d: %v", gate, err)
	}
	if group == syscall.Getpgrp() {
		t.Fatalf("the gate runs in this test's group %d: a group kill would take the harness", group)
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an interrupted accept reported success")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the accept did not stop after cancel")
	}

	waitGone(t, gate)
	waitGone(t, forked)
	if err := syscall.Kill(sibling.Process.Pid, 0); err != nil {
		t.Fatalf("the accept killed a process outside the gate's group: %v", err)
	}
}

// fakeLaneCommits tells the fake herdr to make the lane commit one file per
// unit: unit:path:message, one line each. The commit lands when the prompt
// arrives, which is inside the dispatch, so it is the lane's own work.
func fakeLaneCommits(t *testing.T, lane string, lines ...string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lane-commits")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_COMMITS", path)
	t.Setenv("FAKE_WT", lane)
}

// recordedLaneHead reads the dispatch head straight from the state file. The
// test that proves the hole is closed must run against the code before the
// guard, which had no such field, so it parses the JSON itself.
func recordedLaneHead(t *testing.T, statePath, unit string) string {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Units map[string]struct {
			LaneHead string `json:"lane_head"`
		} `json:"units"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw.Units[unit].LaneHead
}

// A lane that commits inside its own paths leaves nothing uncommitted. The work
// is there and gated, so the unit settles done, and the note names the work it
// found committed so the parent can see it happened.
func TestLaneCommitInsidePathsSettlesDone(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))
	wt := worktreePath(repo, "enc", "w1")
	fakeLaneCommits(t, wt, "A:src/lane.go:lane works")

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	base, err := gitRun(repo, "rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "done: A") {
		t.Fatalf("output:\n%s", stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit %q, want done: %s", got, st.Units["A"].Note)
	}
	note := st.Units["A"].Note
	if !strings.Contains(note, "already committed in your paths") || !strings.Contains(note, "src/lane.go") {
		t.Fatalf("note %q must say the work was already committed", note)
	}
	if !strings.Contains(note, "accept: true ok") {
		t.Fatalf("note %q lost the accept receipt", note)
	}

	if got := recordedLaneHead(t, statePath, "A"); got != base {
		t.Fatalf("recorded head %q, want the dispatch head %q", got, base)
	}
	// The lane's commit is on the branch: dungeon did not add a second one.
	if subject, err := gitRun(repo, "log", "-1", "--format=%s", "enc/w1"); err != nil || subject != "lane works" {
		t.Fatalf("lane branch head %q: %v", subject, err)
	}
	if clean, _ := changedFiles(wt); len(clean) != 0 {
		t.Fatalf("worktree still dirty: %v", clean)
	}
}

// The regression this unit exists to prevent: a lane that commits work outside
// its declared paths leaves git status empty, so the settle check used to see
// nothing, commit nothing, and settle the unit green. It must block, name the
// file and carry the plan edit that covers it.
func TestLaneCommitOutsidePathsBlocks(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))
	wt := worktreePath(repo, "enc", "w1")
	fakeLaneCommits(t, wt, "A:fleet.go:lane wanders")

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
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "blocked" {
		t.Fatalf("unit %q, want blocked: %s", got, st.Units["A"].Note)
	}
	note := st.Units["A"].Note
	for _, want := range []string{"changed files outside your paths", "fleet.go",
		"dungeon edit unit A --paths-add '<glob>'"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note %q must carry %q", note, want)
		}
	}
}

const twoUnitPlan = `encounter: enc
repo: %REPO%
base: main
concurrency: 1
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: A
    minion: w1
    paths: [a/**]
    goal: Do the first thing.
    accept: true
  - id: B
    minion: w1
    paths: [b/**]
    goal: Do the second thing.
    accept: true
`

// The second unit on a minion counts from its own dispatch head. The first
// unit's commits are already there when it starts, and they are not its work.
func TestSecondUnitIsNotBlockedByTheFirstsCommits(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, strings.Replace(twoUnitPlan, "%REPO%", repo, 1))
	fakeLaneCommits(t, worktreePath(repo, "enc", "w1"),
		"A:a/first.go:first lane", "B:b/second.go:second lane")

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"A", "B"} {
		if got := st.UnitStatus(id); got != "done" {
			t.Fatalf("unit %q = %q, want done: %s", id, got, st.Units[id].Note)
		}
		if note := st.Units[id].Note; !strings.Contains(note, "already committed in your paths") {
			t.Fatalf("unit %q note %q must say the work was already committed", id, note)
		}
	}
	if strings.Contains(stdout, "blocked:") {
		t.Fatalf("the second unit was blocked by the first unit's commits:\n%s", stdout)
	}
}

// And the second unit's own commit outside its paths is still caught.
func TestSecondUnitCommitOutsidePathsStillBlocks(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, strings.Replace(twoUnitPlan, "%REPO%", repo, 1))
	fakeLaneCommits(t, worktreePath(repo, "enc", "w1"),
		"A:a/first.go:first lane", "B:elsewhere.go:second lane wanders")

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	if code, _, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath); code != 0 {
		t.Fatalf("next: %s", stderr)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit A %q, want done: %s", got, st.Units["A"].Note)
	}
	if got := st.UnitStatus("B"); got != "blocked" {
		t.Fatalf("unit B %q, want blocked: %s", got, st.Units["B"].Note)
	}
	note := st.Units["B"].Note
	if !strings.Contains(note, "elsewhere.go") || strings.Contains(note, "a/first.go") {
		t.Fatalf("note %q must name only the second unit's own change", note)
	}
}

// A state with no recorded head is judged as before: the guard is new, not
// retroactive, so a unit already in flight settles on git status alone.
func TestStateWithNoRecordedHeadBehavesAsBefore(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))
	wt := worktreePath(repo, "enc", "w1")

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	// The lane committed outside its paths before the guard existed.
	if err := os.WriteFile(filepath.Join(wt, "fleet.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, wt, "lane wanders")
	report := reportPath(planPath, "A")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("did the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetUnit("A", "working", "brief "+briefPath(planPath, "A")); err != nil {
		t.Fatal(err)
	}
	if got := recordedLaneHead(t, statePath, "A"); got != "" {
		t.Fatalf("this fixture needs a state with no recorded head, got %q", got)
	}
	if err := SaveState(statePath, st); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath); code != 0 {
		t.Fatalf("next: %s", stderr)
	}
	st, err = LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "done" {
		t.Fatalf("unit %q, want done as before: %s", got, st.Units["A"].Note)
	}
	if note := st.Units["A"].Note; strings.Contains(note, "lane committed") {
		t.Fatalf("note %q must read as it did before the guard", note)
	}
}

// fakeLaneAction tells the fake herdr to run a shell script in the lane's
// worktree when the brief arrives, so a lane can merge, fast-forward or
// cherry-pick the way a real one does before it reports.
func fakeLaneAction(t *testing.T, lane, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lane.sh")
	if err := os.WriteFile(path, []byte("set -e\n"+script+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LANE_SH", path)
	t.Setenv("FAKE_WT", lane)
}

// laneGitIdentity gives the lane's own git commands an author, so a commit it
// makes in a script needs no -c flags.
func laneGitIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

// depBranch cuts a landed branch from main with one file on it, the way another
// unit's landed work arrives, and leaves main where it was.
func depBranch(t *testing.T, repo, file, content string) {
	t.Helper()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("checkout", "-q", "-b", "dep/landed")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, file)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "dep work")
	git("checkout", "-q", "main")
}

// landDep merges dep/landed into main, so a lane's worktree is cut with the
// dep's file already in the tree it starts from, the way a landed branch is.
func landDep(t *testing.T, repo string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "merge", "--no-ff", "dep/landed", "-m", "merge dep/landed")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git merge dep/landed: %v\n%s", err, out)
	}
}

// mergeCase runs one fake lane on a real worktree and returns the settled unit,
// the run's stdout and the lane's worktree, so each shape of change is told the
// same way. paths overrides the unit's declared paths when it is not empty.
func mergeCase(t *testing.T, paths, script string, dep func(t *testing.T, repo string)) (UnitState, string, string) {
	t.Helper()
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	laneGitIdentity(t)
	plan := planFor(repo)
	if paths != "" {
		plan = strings.Replace(plan, "paths: [src/**]", "paths: ["+paths+"]", 1)
	}
	planPath, statePath, _ := encounter(t, repo, plan)
	if dep != nil {
		dep(t, repo)
	}
	wt := worktreePath(repo, "enc", "w1")
	fakeLaneAction(t, wt, script)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath)
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	return st.Units["A"], stdout, wt
}

// blockedOutside fails the test unless the unit blocked and its note names the
// files and the plan edit that would have covered them.
func blockedOutside(t *testing.T, u UnitState, stdout string, files ...string) {
	t.Helper()
	if u.Status != "blocked" {
		t.Fatalf("unit %q, want blocked: %s\n%s", u.Status, u.Note, stdout)
	}
	want := append([]string{"changed files outside your paths",
		"dungeon edit unit A --paths-add '<glob>'"}, files...)
	for _, w := range want {
		if !strings.Contains(u.Note, w) {
			t.Fatalf("note %q must carry %q", u.Note, w)
		}
	}
}

// B1: a lane asks git to merge its own out-of-paths commit from a side branch.
// The commit is on a second parent, so no first-parent walk and no note can
// blame it, but the tree holds the file and the recorded head does not.
func TestLaneCommitMergedFromASideBranchBlocks(t *testing.T) {
	u, stdout, _ := mergeCase(t, "", `git checkout -q -b lane/side
mkdir -p demo
printf 'lane\n' > demo/a.go
git add demo/a.go
git commit -q -m 'lane wanders'
git checkout -q -
git merge --no-ff lane/side -m 'merge lane/side'`, nil)
	blockedOutside(t, u, stdout, "demo/a.go")
}

// B2: the lane commits outside, parks the commit on a side branch and
// fast-forwards to it. The branch moves, the tree holds the file, the recorded
// head does not.
func TestLaneFastForwardToAParkedCommitBlocks(t *testing.T) {
	u, stdout, _ := mergeCase(t, "", `git checkout -q -b lane/parked
printf 'lane\n' > fleet.go
git add fleet.go
git commit -q -m 'lane parks'
git checkout -q -
git merge --ff-only lane/parked`, nil)
	blockedOutside(t, u, stdout, "fleet.go")
}

// B3: the lane rebases onto a landed dep instead of merging it. Every dep commit
// is now on the first-parent chain, and no reflog entry calls it a
// fast-forward, so the dep's file is in the tree and outside the paths: block.
func TestLaneRebaseOntoALandedBranchBlocks(t *testing.T) {
	u, stdout, _ := mergeCase(t, "", `mkdir -p src
printf 'lane\n' > src/lane.go
git add src/lane.go
git commit -q -m 'lane work'
git rebase dep/landed`, func(t *testing.T, repo string) { depBranch(t, repo, "fleet.go", "dep\n") })
	blockedOutside(t, u, stdout, "fleet.go")
}

// A merge is the normal way to hit the guard now: the dep's file is in the tree,
// outside the unit's paths, and the note says how to widen them.
func TestMergeOfAnUndeclaredBranchBlocks(t *testing.T) {
	u, stdout, _ := mergeCase(t, "", "git merge --no-ff dep/landed -m 'merge dep/landed'",
		func(t *testing.T, repo string) { depBranch(t, repo, "fleet.go", "dep\n") })
	blockedOutside(t, u, stdout, "fleet.go")
}

// B4: the lane renames a landed dep file into its own paths. The destination is
// inside the paths and the deletion is outside, so the block names the deletion:
// --no-renames keeps the two halves apart instead of reporting the destination
// alone.
func TestRenameOfALandedFileIntoThePathsBlocks(t *testing.T) {
	u, stdout, _ := mergeCase(t, "", `mkdir -p src
git mv fleet.go src/fleet.go
git commit -q -m 'rename the dep file'`,
		func(t *testing.T, repo string) {
			depBranch(t, repo, "fleet.go", "dep\n")
			landDep(t, repo)
		})
	blockedOutside(t, u, stdout, "fleet.go")
	if strings.Contains(u.Note, "outside your paths: src/fleet.go") {
		t.Fatalf("note %q blames the destination, not the deletion", u.Note)
	}
}

// The other half of B4: the dep branch is not on the base, so its file is not in
// the dispatch head either. The lane merges it and renames it into its own paths,
// and the tree it leaves holds nothing outside those paths, so it is contained:
// the lane deleted nothing that was there when it started, and the content now
// lives in a file it owns.
func TestMergeThenRenameIntoThePathsLeavesAContainedTree(t *testing.T) {
	u, stdout, _ := mergeCase(t, "", `mkdir -p src
git merge --no-ff dep/landed -m 'merge dep/landed'
git mv fleet.go src/fleet.go
git commit -q -m 'rename the dep file'`,
		func(t *testing.T, repo string) { depBranch(t, repo, "fleet.go", "dep\n") })
	if u.Status != "done" {
		t.Fatalf("unit %q, want done: %s\n%s", u.Status, u.Note, stdout)
	}
}

// A cherry-pick writes the file into the tree as the lane's own commit, which is
// outside its paths, so it blocks like any other change.
func TestCherryPickOutsideThePathsBlocks(t *testing.T) {
	u, stdout, _ := mergeCase(t, "", "git cherry-pick dep/landed",
		func(t *testing.T, repo string) { depBranch(t, repo, "fleet.go", "dep\n") })
	blockedOutside(t, u, stdout, "fleet.go")
}

// B5: a check that cannot run has proved nothing. A head git cannot read blocks
// with the error, rather than reporting an empty change set and passing.
func TestAGitErrorBlocksAndNamesIt(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))
	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	report := reportPath(planPath, "A")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("did the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	const unreadable = "0123456789012345678901234567890123456789"
	if err := st.SetUnit("A", "working", "brief "+briefPath(planPath, "A")); err != nil {
		t.Fatal(err)
	}
	st.RecordLaneHead("A", unreadable)
	if err := SaveState(statePath, st); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runCLI("next", "--poll", "10ms", "--timeout", "5s", "--plan", planPath); code != 0 {
		t.Fatalf("next: %s", stderr)
	}
	st, err = LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.UnitStatus("A"); got != "blocked" {
		t.Fatalf("unit %q, want blocked: %s", got, st.Units["A"].Note)
	}
	note := st.Units["A"].Note
	if !strings.Contains(note, "could not check the unit's changes") || !strings.Contains(note, unreadable) {
		t.Fatalf("note %q must name the error", note)
	}
}

// A lane that merges a landed branch into a unit whose paths cover it is inside
// its paths: the merge is a change like any other and the unit settles done.
func TestMergeOfADeclaredBranchSettlesDone(t *testing.T) {
	u, stdout, _ := mergeCase(t, "src/**, fleet.go", "git merge --no-ff dep/landed -m 'merge dep/landed'",
		func(t *testing.T, repo string) { depBranch(t, repo, "fleet.go", "dep\n") })
	if u.Status != "done" {
		t.Fatalf("unit %q, want done: %s\n%s", u.Status, u.Note, stdout)
	}
	if !strings.Contains(u.Note, "already committed in your paths") || !strings.Contains(u.Note, "fleet.go") {
		t.Fatalf("note %q must name the declared file as committed", u.Note)
	}
	if !strings.Contains(stdout, "done: A") {
		t.Fatalf("output:\n%s", stdout)
	}
}

// A conflict resolved inside the unit's own paths is its own work and inside its
// paths: done.
func TestConflictResolvedInsideThePathsSettlesDone(t *testing.T) {
	u, stdout, _ := mergeCase(t, "", `mkdir -p src
printf 'lane\n' > src/lane.go
git add src/lane.go
git commit -q -m 'lane work'
git merge --no-ff dep/landed -m 'merge dep/landed' || true
printf 'resolved\n' > src/lane.go
git add src/lane.go
git commit -q --no-edit`,
		func(t *testing.T, repo string) { depBranch(t, repo, "src/lane.go", "dep\n") })
	if u.Status != "done" {
		t.Fatalf("unit %q, want done: %s\n%s", u.Status, u.Note, stdout)
	}
	if !strings.Contains(u.Note, "already committed in your paths") || !strings.Contains(u.Note, "src/lane.go") {
		t.Fatalf("note %q must name the resolution", u.Note)
	}
}
