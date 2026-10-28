package main

import (
	"os"
	"os/exec"
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
	// The agent name carries the run's token, so only the prefix is exact.
	if !strings.Contains(string(calls), "agent start w1-") ||
		!strings.Contains(string(calls), "--kind pi --pane w1P:p9") {
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
	if !strings.Contains(string(calls), "workspace create") {
		t.Fatalf("a gone pin should fall back to the encounter workspace:\n%s", calls)
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

func TestOneTabPerRole(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	plan := strings.Replace(twoWorkerPlan, "%REPO%", repo, 1)
	plan = strings.Replace(plan, `  - {id: w2, role: worker, worktree: w2}`,
		`  - {id: w2, role: worker, worktree: w2}
  - {id: r1, role: reviewer, of: w1}`, 1)
	plan = strings.Replace(plan, `  - id: B
    minion: w2
    paths: [b.txt]
    goal: Second change.
    accept: test -f b.txt`, `  - id: B
    minion: w2
    paths: [b.txt]
    goal: Second change.
    accept: test -f b.txt
  - id: R
    minion: r1
    deps: [A]
    goal: Review both.`, 1)
	planPath, _, _ := encounter(t, repo, plan)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	calls, _ := os.ReadFile(log)
	s := string(calls)
	if strings.Count(s, "workspace create") != 1 {
		t.Fatalf("one workspace per encounter:\n%s", s)
	}
	if !strings.Contains(s, "tab rename w9:t1 workers") {
		t.Fatalf("the first tab holds the two workers:\n%s", s)
	}
	if !strings.Contains(s, "tab create --workspace w9 --label reviewer") {
		t.Fatalf("the reviewer gets its own tab, singular:\n%s", s)
	}
	if strings.Count(s, "pane split") != 1 {
		t.Fatalf("two workers share one tab with one split:\n%s", s)
	}
}

func TestPinnedMinionStaysOutOfTheWorkspace(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	plan := strings.Replace(planFor(repo),
		"  - {id: w1, role: worker, worktree: w1}",
		"  - {id: w1, role: worker, worktree: w1, pane: \"w1P:p9\"}", 1)
	planPath, _, _ := encounter(t, repo, plan)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "workspace create") {
		t.Fatalf("a fully pinned encounter needs no workspace:\n%s", calls)
	}
}

func TestPanesGrowRightByDefault(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	planPath, _, _ := encounter(t, repo, strings.Replace(twoWorkerPlan, "%REPO%", repo, 1))

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "pane split w9:p1 --direction right") {
		t.Fatalf("two workers should split to the right:\n%s", calls)
	}
}

func TestSplitDirectionComesFromThePlanOrTheFlag(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	plan := strings.Replace(twoWorkerPlan, "%REPO%", repo, 1)
	plan = strings.Replace(plan, "base: main", "base: main\nsplit: down", 1)
	planPath, _, _ := encounter(t, repo, plan)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "--direction down") {
		t.Fatalf("the plan asked for down:\n%s", calls)
	}

	// The flag wins over the plan.
	log2 := fakeHerdr(t, "")
	if code, _, stderr := runCLI("up", "--no-dispatch", "--split", "left", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	calls, _ = os.ReadFile(log2)
	if !strings.Contains(string(calls), "--direction left") {
		t.Fatalf("--split should override the plan:\n%s", calls)
	}
}

func TestSplitDirectionIsValidated(t *testing.T) {
	src := strings.Replace(goodPlan, "base: main", "base: main\nsplit: sideways", 1)
	wantCodes(t, Validate(decodeOK(t, src)), EUnknownSplit)
}

func TestPanesGetEqualShares(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	plan := strings.Replace(twoWorkerPlan, "%REPO%", repo, 1)
	plan = strings.Replace(plan, `  - {id: w2, role: worker, worktree: w2}`,
		`  - {id: w2, role: worker, worktree: w2}
  - {id: w3, role: worker, worktree: w3}`, 1)
	planPath, _, _ := encounter(t, repo, plan)

	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	callsRead, _ := os.ReadFile(log)
	calls := string(callsRead)
	// herdr's ratio is what the pane being split keeps: 2 of 3 for the second
	// lane, 1 of 2 for the third, so all three end up a third of the tab.
	if !strings.Contains(calls, "--ratio 0.6667") {
		t.Fatalf("second lane should keep 2/3 of the root:\n%s", calls)
	}
	if !strings.Contains(calls, "--ratio 0.5000") {
		t.Fatalf("third lane should keep 1/2 of the root:\n%s", calls)
	}
}

// The argv pi receives is the contract: a minion's tools list replaces the role
// default, and a minion with no list keeps the default.
func TestLaneToolsOverrideAndFallBack(t *testing.T) {
	repo := gitRepo(t)
	log := fakeHerdr(t, "")
	plan := strings.Replace(planFor(repo),
		"  - {id: w1, role: worker, worktree: w1}",
		"  - {id: s1, role: scout, tools: \"read,grep,find,ls\"}\n  - {id: w1, role: worker, worktree: w1}", 1)
	planPath, _, _ := encounter(t, repo, plan)

	code, stdout, stderr := runCLI("up", "--no-dispatch", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var scout, worker string
	for _, line := range strings.Split(string(calls), "\n") {
		switch {
		case strings.HasPrefix(line, "agent start s1-"):
			scout = line
		case strings.HasPrefix(line, "agent start w1-"):
			worker = line
		}
	}
	if scout == "" || worker == "" {
		t.Fatalf("both lanes should start:\n%s", calls)
	}
	if !strings.Contains(scout, "--tools read,grep,find,ls") {
		t.Fatalf("a scout must get its own tools, got %q", scout)
	}
	if strings.Contains(scout, "bash") {
		t.Fatalf("a scout that did not list bash must not get it: %q", scout)
	}
	if !strings.Contains(worker, "--tools read,bash,edit,write") {
		t.Fatalf("a worker with no tools must get the role default, got %q", worker)
	}
}

// requirePaneCwdExists wraps the herdr already on PATH so any pane-creating call
// whose --cwd is missing fails, which is what a real herdr refuses to do.
func requirePaneCwdExists(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("herdr")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprev=\nfor a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--cwd\" ] && [ ! -d \"$a\" ]; then\n" +
		"    echo \"missing cwd: $a\" >&2\n    exit 1\n  fi\n" +
		"  prev=\"$a\"\ndone\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A reviewer's pane opens in the worktree it reads, not the base checkout. Its
// accept already runs there; the process starts there too. The paired worktree
// must exist first, so both declaration orders work.
func TestReviewerPaneUsesThePairedWorktree(t *testing.T) {
	worker := "  - {id: w1, role: worker, worktree: w1}"
	reviewer := "  - {id: r1, role: reviewer, of: w1}"
	cases := []struct {
		name    string
		minions string
		// The first minion's role tab is the workspace's root tab; later roles
		// get a tab of their own. The reviewer pane is created by whichever
		// line matches.
		want string
	}{
		{
			name:    "writer declared first",
			minions: worker + "\n" + reviewer,
			want:    "--label reviewer --cwd ",
		},
		{
			name:    "reviewer declared first",
			minions: reviewer + "\n" + worker,
			want:    "workspace create --label enc --cwd ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := gitRepo(t)
			log := fakeHerdr(t, "")
			requirePaneCwdExists(t)
			plan := `encounter: enc
repo: ` + repo + `
base: main
minions:
` + tc.minions + `
units:
  - id: A
    minion: w1
    paths: [a.txt]
    goal: Do A.
    accept: "true"
`
			planPath, _, _ := encounter(t, repo, plan)
			if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
				t.Fatalf("up: %s", stderr)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			wt := worktreePath(repo, "enc", "w1")
			if _, err := os.Stat(wt); err != nil {
				t.Fatalf("the reviewer's worktree must exist: %v", err)
			}
			if want := tc.want + wt; !strings.Contains(string(calls), want) {
				t.Fatalf("the reviewer pane should open in w1's worktree (%q):\n%s", want, calls)
			}
		})
	}
}
