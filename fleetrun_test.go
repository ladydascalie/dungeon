package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fleetUnitPlan is one encounter of a fleet: one worker, one unit, one file. The
// file differs per encounter, so two plans can merge without a conflict.
const fleetUnitPlan = `encounter: %ENC%
repo: %REPO%
base: %BASE%
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: A
    minion: w1
    paths: [%FILE%]
    goal: Touch %FILE%.
    accept: %ACCEPT%
`

// writeFleetPlan writes one encounter under root and pins its mtime, so
// discovery order is the order the test wrote them and not the clock's.
func writeFleetPlan(t *testing.T, root, rel string, at time.Time, repl map[string]string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := fleetUnitPlan
	for k, v := range repl {
		plan = strings.ReplaceAll(plan, k, v)
	}
	if _, ok := repl["%ACCEPT%"]; !ok {
		// A plan that names no accept gets the file check, so a dispatch-only
		// test does not have to write one.
		plan = strings.ReplaceAll(plan, "%ACCEPT%", "test -f "+repl["%FILE%"])
	}
	if err := os.WriteFile(path, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	touchEncounter(t, path, at)
	return path
}

// fleetRoot builds one repo and one encounter per name under one .dungeon root
// and puts the working directory there. Names are given newest touch first, so
// discovery order is the argument order.
func fleetRoot(t *testing.T, names ...string) (root, repo string) {
	t.Helper()
	return fleetRootWith(t, nil, names...)
}

// fleetRootWith is fleetRoot with extra substitutions applied to every plan, so
// a test can hand its lanes a different accept command.
func fleetRootWith(t *testing.T, overrides map[string]string, names ...string) (root, repo string) {
	t.Helper()
	repo = gitRepo(t)
	root = t.TempDir()
	fakeHerdr(t, "")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i, name := range names {
		repl := map[string]string{
			"%ENC%":    name,
			"%REPO%":   repo,
			"%BASE%":   "main",
			"%FILE%":   name + ".txt",
			"%ACCEPT%": "test -f " + name + ".txt",
		}
		for k, v := range overrides {
			repl[k] = v
		}
		writeFleetPlan(t, root, filepath.Join(".dungeon", name, "plan.yaml"), base.Add(time.Duration(len(names)-i)*time.Hour), repl)
	}
	t.Chdir(root)
	return root, repo
}

func fleetStatePath(root, name string) string {
	return filepath.Join(root, ".dungeon", name, "state.json")
}

func fleetPlanPath(root, name string) string {
	return filepath.Join(root, ".dungeon", name, "plan.yaml")
}

// fleetTargets resolves zero, one and many encounters, and leaves a plan that
// does not validate out of the sweep while naming it.
func TestFleetTargetsResolvesZeroOneAndMany(t *testing.T) {
	cases := []struct {
		name        string
		encounters  []string
		broken      bool
		want        []string
		wantSkipped string
	}{
		{name: "zero", want: nil},
		{name: "one encounter", encounters: []string{"one"}, want: []string{"one"}},
		{name: "many encounters", encounters: []string{"one", "two"}, want: []string{"one", "two"}},
		{
			name:        "a broken plan is skipped and named",
			encounters:  []string{"one"},
			broken:      true,
			want:        []string{"one"},
			wantSkipped: "skipped .dungeon/broken/plan.yaml: units[0].minion E_UNKNOWN_MINION",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, repo := fleetRoot(t, tc.encounters...)
			if tc.broken {
				writeFleetPlan(t, root, filepath.Join(".dungeon", "broken", "plan.yaml"), time.Now(), map[string]string{
					"%ENC%":  "broken",
					"%REPO%": repo,
					"%BASE%": "main",
					"%FILE%": "broken.txt",
				})
				// A unit naming a minion the plan does not declare is the
				// shortest plan that parses and does not validate.
				path := filepath.Join(root, ".dungeon", "broken", "plan.yaml")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				fixed := strings.Replace(string(raw), "minion: w1\n    paths", "minion: w9\n    paths", 1)
				if err := os.WriteFile(path, []byte(fixed), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var buf bytes.Buffer
			targets := fleetTargets(false, &buf)
			var got []string
			for _, target := range targets {
				got = append(got, target.encounter())
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("targets %v, want %v", got, tc.want)
			}
			if tc.wantSkipped == "" {
				if buf.Len() != 0 {
					t.Fatalf("nothing should be skipped: %q", buf.String())
				}
				return
			}
			if !strings.Contains(buf.String(), tc.wantSkipped) {
				t.Fatalf("skipped line missing %q:\n%s", tc.wantSkipped, buf.String())
			}
		})
	}
}

// A plan edited after dispatch is refused by the sweep, the same way a single
// plan refuses it, and --force records the new hash instead.
func TestFleetTargetsAppliesThePlanHashGuard(t *testing.T) {
	root, _ := fleetRoot(t, "one")
	statePath := fleetStatePath(root, "one")
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	h, err := PlanHash(fleetPlanPath(root, "one"))
	if err != nil {
		t.Fatal(err)
	}
	st.Encounter, st.PlanHash = "one", h
	if err := SaveState(statePath, st); err != nil {
		t.Fatal(err)
	}
	editAFile(t, fleetPlanPath(root, "one"), "goal: Touch one.txt.", "goal: Touch one.txt, edited.")

	var buf bytes.Buffer
	if targets := fleetTargets(false, &buf); len(targets) != 0 {
		t.Fatalf("a plan edited after dispatch must be skipped, got %d targets", len(targets))
	}
	if !strings.Contains(buf.String(), "plan changed since dispatch") {
		t.Fatalf("the skip must name the guard:\n%s", buf.String())
	}

	buf.Reset()
	targets := fleetTargets(true, &buf)
	if len(targets) != 1 {
		t.Fatalf("--force accepts the edit, got %d targets: %s", len(targets), buf.String())
	}
	after, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if after.PlanHash == h {
		t.Fatal("--force must record the new hash")
	}
}

func editAFile(t *testing.T, path, old, new string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), old) {
		t.Fatalf("%s does not hold %q", path, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An empty sweep, or one with only a broken plan, could not run: exit 2.
func TestNextAllRefusesAnEmptySweep(t *testing.T) {
	root := t.TempDir()
	fakeHerdr(t, "")
	t.Chdir(root)
	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 2 {
		t.Fatalf("no encounter: exit %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no encounter to sweep") {
		t.Fatalf("stderr %q", stderr)
	}

	repo := gitRepo(t)
	writeFleetPlan(t, root, filepath.Join(".dungeon", "broken", "plan.yaml"), time.Now(), map[string]string{
		"%ENC%": "broken", "%REPO%": repo, "%BASE%": "main", "%FILE%": "x.txt",
	})
	editAFile(t, filepath.Join(root, ".dungeon", "broken", "plan.yaml"), "minion: w1\n    paths", "minion: w9\n    paths")
	code, stdout, stderr = runCLI("next", "--all", "--dry-run")
	if code != 2 {
		t.Fatalf("only a broken plan: exit %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stdout, "skipped .dungeon/broken/plan.yaml") {
		t.Fatalf("the broken plan must be named:\n%s", stdout)
	}
}

// next --all dispatches the ready unit of every plan in one round and settles
// both states on their own accept command.
func TestNextAllDispatchesAndSettlesBothPlans(t *testing.T) {
	root, _ := fleetRootWith(t, map[string]string{"%ACCEPT%": "true"}, "one", "two")
	t.Setenv("FAKE_REPORT", "1")

	if code, stdout, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all exit %d: %s\n%s", code, stderr, stdout)
	}
	code, stdout, stderr := runCLI("next", "--all", "--poll", "10ms", "--timeout", "5s")
	if code != 0 {
		t.Fatalf("next --all exit %d: %s\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"one\n  dispatched: A\n  done: A\n",
		"two\n  dispatched: A\n  done: A\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("digest missing %q:\n%s", want, stdout)
		}
	}
	for _, name := range []string{"one", "two"} {
		st, err := LoadState(fleetStatePath(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.UnitStatus("A"); got != "done" {
			t.Fatalf("%s/A is %q, want done: %s", name, got, st.Units["A"].Note)
		}
		if !strings.Contains(st.Units["A"].Note, "accept: true ok") {
			t.Fatalf("%s/A receipt %q", name, st.Units["A"].Note)
		}
	}
}

// --timeout covers the whole sweep: two held plans must not double the wait.
func TestNextAllTimeoutCoversTheWholeSweep(t *testing.T) {
	fleetRoot(t, "one", "two")
	t.Setenv("FAKE_STATUS", "busy") // a word dungeon does not know: held until a report lands
	t.Setenv("FAKE_REPORT", "0")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}

	const limit = 300 * time.Millisecond
	start := time.Now()
	code, stdout, stderr := runCLI("next", "--all", "--poll", "25ms", "--timeout", limit.String())
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "timeout: lanes still working, run dungeon next --all again") {
		t.Fatalf("the sweep must say what to run again:\n%s", stdout)
	}
	if n := strings.Count(stdout, "timeout:"); n != 1 {
		t.Fatalf("the sweep timed out %d times, want one:\n%s", n, stdout)
	}
	// Two plans each getting their own timeout would take at least twice as
	// long as the one deadline the sweep is given.
	if elapsed >= 2*limit {
		t.Fatalf("the sweep took %v for a %v timeout: --timeout must cover the whole sweep", elapsed, limit)
	}
}

// A broken plan is named and skipped while the rest of the sweep still runs.
func TestNextAllSkipsABrokenPlanAndSweepsTheRest(t *testing.T) {
	root, repo := fleetRoot(t, "one")
	writeFleetPlan(t, root, filepath.Join(".dungeon", "broken", "plan.yaml"), time.Now(), map[string]string{
		"%ENC%": "broken", "%REPO%": repo, "%BASE%": "main", "%FILE%": "broken.txt",
	})
	editAFile(t, filepath.Join(root, ".dungeon", "broken", "plan.yaml"), "minion: w1\n    paths", "minion: w9\n    paths")

	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "skipped .dungeon/broken/plan.yaml: units[0].minion") {
		t.Fatalf("the broken plan must be named first:\n%s", stdout)
	}
	if !strings.Contains(stdout, "one\n") || !strings.Contains(stdout, "no lane: A") {
		t.Fatalf("the sweep must still run the readable plan:\n%s", stdout)
	}
}

// --dry-run prints the digest and writes nothing.
func TestNextAllDryRunDispatchesNothing(t *testing.T) {
	root, _ := fleetRoot(t, "one", "two")
	t.Setenv("FAKE_REPORT", "1")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}

	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if n := strings.Count(stdout, "would dispatch A to w1"); n != 2 {
		t.Fatalf("dry run should name both plans' dispatch, got %d:\n%s", n, stdout)
	}
	for _, name := range []string{"one", "two"} {
		st, err := LoadState(fleetStatePath(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.UnitStatus("A"); got != "planned" {
			t.Fatalf("%s/A is %q, a dry run must not dispatch", name, got)
		}
		if _, err := os.Stat(reportPath(fleetPlanPath(root, name), "A")); err == nil {
			t.Fatalf("%s wrote a report on a dry run", name)
		}
	}
}

// --all with --plan names one encounter and sweeps every one: exit 2.
func TestAllWithPlanIsRefused(t *testing.T) {
	fakeHerdr(t, "")
	t.Chdir(t.TempDir())
	plan := filepath.Join(t.TempDir(), "plan.yaml")
	for _, cmd := range []string{"up", "next", "land"} {
		code, _, stderr := runCLI(cmd, "--all", "--plan", plan)
		if code != 2 {
			t.Fatalf("%s --all --plan exit %d, want 2: %s", cmd, code, stderr)
		}
		if !strings.Contains(stderr, "--all") || !strings.Contains(stderr, "--plan") {
			t.Fatalf("%s should name both flags: %q", cmd, stderr)
		}
	}
}

// A bare command still refuses to guess between encounters, and now names the
// sweep as the other answer.
func TestABareCommandNamesAll(t *testing.T) {
	fleetRoot(t, "one", "two")
	for _, cmd := range []string{"up", "next", "land"} {
		code, _, stderr := runCLI(cmd)
		if code != 2 {
			t.Fatalf("bare %s exit %d, want 2: %s", cmd, code, stderr)
		}
		if !strings.Contains(stderr, "pass --plan to choose one") {
			t.Fatalf("bare %s should still name --plan: %q", cmd, stderr)
		}
		if !strings.Contains(stderr, "--all to sweep every encounter") {
			t.Fatalf("bare %s should name --all: %q", cmd, stderr)
		}
	}
}

// land --all refuses what one checkout cannot do: a sweep over two repos, two
// bases, or a --branch that is already a plan's integration branch.
func TestLandAllRefusesMixedRepoMixedBaseAndACollidingBranch(t *testing.T) {
	other := func(t *testing.T, repo string) (string, string) { return gitRepo(t), "main" }
	same := func(t *testing.T, repo string) (string, string) { return repo, "main" }
	bases := func(t *testing.T, repo string) (string, string) { return repo, "develop" }

	cases := []struct {
		name  string
		build func(t *testing.T, repo string) (repo2, base2 string)
		args  []string
		want  []string
	}{
		{
			name:  "mixed repos",
			build: other,
			want:  []string{"land merges branches in one checkout", "one repo root and one base"},
		},
		{
			name:  "mixed bases",
			build: bases,
			want:  []string{"lands on main", "lands on develop", "one repo root and one base"},
		},
		{
			name:  "a colliding branch",
			build: same,
			args:  []string{"--branch", "two/integration"},
			want:  []string{"is two's own integration branch", "pass --branch"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, repo := fleetRoot(t, "one")
			repo2, base2 := tc.build(t, repo)
			writeFleetPlan(t, root, filepath.Join(".dungeon", "two", "plan.yaml"), time.Now(), map[string]string{
				"%ENC%": "two", "%REPO%": repo2, "%BASE%": base2, "%FILE%": "two.txt",
			})

			code, stdout, stderr := runCLI(append([]string{"land", "--all"}, tc.args...)...)
			if code != 2 {
				t.Fatalf("exit %d, want 2: %s\n%s", code, stderr, stdout)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr missing %q:\n%s", want, stderr)
				}
			}
		})
	}
}

// land --all lands each plan, then combines the two integration branches on one
// branch and re-runs every merged unit's accept against the combined tree.
func TestLandAllCutsACombinedBranch(t *testing.T) {
	root, repo := fleetRoot(t, "one", "two")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	for _, name := range []string{"one", "two"} {
		commitIn(t, worktreePath(repo, name, "w1"), name+".txt", name+"\n", name)
		if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", fleetPlanPath(root, name)); code != 0 {
			t.Fatalf("set %s: %s", name, stderr)
		}
	}

	code, stdout, stderr := runCLI("land", "--all")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"one: merged 1 units on one/integration",
		"two: merged 1 units on two/integration",
		"fleet branch fleet/integration",
		"merged: one/A two/A",
		"ok   one/A: test -f one.txt",
		"ok   two/A: test -f two.txt",
		"nothing was pushed",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output missing %q:\n%s", want, stdout)
		}
	}
	fleet := branchWorktreePath(repo, "fleet/integration")
	for _, f := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(filepath.Join(fleet, f)); err != nil {
			t.Fatalf("%s should be on the combined branch: %v", f, err)
		}
	}
	if _, err := gitRun(repo, "rev-parse", "--verify", "refs/heads/fleet/integration"); err != nil {
		t.Fatalf("the combined branch is not a branch: %v", err)
	}
}

// land --all names the plan that conflicted on the combined branch and never
// reports that branch as good.
func TestLandAllStopsOnACombinedConflict(t *testing.T) {
	root, repo := fleetRoot(t, "one", "two")
	// Both plans own the same file: their integration branches merge cleanly on
	// their own and collide on the combined branch.
	for _, name := range []string{"one", "two"} {
		editAFile(t, fleetPlanPath(root, name), "paths: ["+name+".txt]", "paths: [shared.txt]")
		editAFile(t, fleetPlanPath(root, name), "accept: test -f "+name+".txt", "accept: test -f shared.txt")
	}
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	for _, name := range []string{"one", "two"} {
		commitIn(t, worktreePath(repo, name, "w1"), "shared.txt", name+"\n", name)
		if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", fleetPlanPath(root, name)); code != 0 {
			t.Fatalf("set %s: %s", name, stderr)
		}
	}

	code, stdout, stderr := runCLI("land", "--all", "--no-gate")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "conflict landing two on fleet/integration") {
		t.Fatalf("the conflict must name the plan:\n%s", stdout)
	}
	if !strings.Contains(stdout, "fleet/integration is not good") {
		t.Fatalf("a conflicted fleet branch is never good:\n%s", stdout)
	}
}

// One plan that merged something has no combined branch to prove: the plan's own
// integration branch is the whole result.
func TestLandAllWithOneMergedPlanHasNoCombinedBranch(t *testing.T) {
	root, repo := fleetRoot(t, "one", "two")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	commitIn(t, worktreePath(repo, "one", "w1"), "one.txt", "one\n", "one")
	if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", fleetPlanPath(root, "one")); code != 0 {
		t.Fatalf("set: %s", stderr)
	}

	code, stdout, stderr := runCLI("land", "--all")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "one: merged 1 units on one/integration") {
		t.Fatalf("output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "two: nothing to land") {
		t.Fatalf("output:\n%s", stdout)
	}
	if strings.Contains(stdout, "fleet branch") {
		t.Fatalf("one merged plan needs no combined branch:\n%s", stdout)
	}
}

// The lane's box key carries the encounter, so two plans that both declare w1
// post to two places and one parent watching lane/ sees both.
func TestTheBoxKeyCarriesTheEncounter(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, ".dungeon", "plan.yaml")
	p := decodeOK(t, goodPlan)
	m, ok := findMinion(p, "w1")
	if !ok {
		t.Fatal("the fixture plan has no w1")
	}
	u, ok := findUnit(p, "api-timeout")
	if !ok {
		t.Fatal("the fixture plan has no api-timeout")
	}

	path, err := writeBrief(newDM(), briefInput{
		opts:   dispatchOpts{planPath: planPath, parentPane: "w9:p9"},
		plan:   p,
		minion: m, unit: u, state: &State{},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "--key lane/" + p.Encounter + "/w1"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("brief missing %q:\n%s", want, raw)
	}
	if strings.Contains(string(raw), "--key lane/w1 ") {
		t.Fatalf("the bare lane/w1 key collides across a fleet:\n%s", raw)
	}
}
