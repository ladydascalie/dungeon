package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// fleetRootTwo writes two encounters with their own accept commands, for a test
// that needs the plans to disagree.
func fleetRootTwo(t *testing.T, fileA, acceptA, fileB, acceptB string) (root, repo string) {
	t.Helper()
	repo = gitRepo(t)
	root = t.TempDir()
	fakeHerdr(t, "")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	writeFleetPlan(t, root, filepath.Join(".dungeon", "a", "plan.yaml"), base, map[string]string{
		"%ENC%": "a", "%REPO%": repo, "%BASE%": "main", "%FILE%": fileA, "%ACCEPT%": acceptA,
	})
	writeFleetPlan(t, root, filepath.Join(".dungeon", "b", "plan.yaml"), base.Add(-time.Hour), map[string]string{
		"%ENC%": "b", "%REPO%": repo, "%BASE%": "main", "%FILE%": fileB, "%ACCEPT%": acceptB,
	})
	t.Chdir(root)
	return root, repo
}

func fleetStatePath(root, name string) string {
	return filepath.Join(root, ".dungeon", name, "state.json")
}

func fleetPlanPath(root, name string) string {
	return filepath.Join(root, ".dungeon", name, "plan.yaml")
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

// breakUnitMinion makes a plan that parses and does not validate: its unit names
// a minion the plan does not declare.
func breakUnitMinion(t *testing.T, path string) {
	t.Helper()
	editAFile(t, path, "minion: w1\n    paths", "minion: w9\n    paths")
}

func stateExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// loadFleet resolves zero, one and many encounters, and leaves a plan that does
// not validate out of the sweep while naming it.
func TestLoadFleetResolvesZeroOneAndMany(t *testing.T) {
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
				path := writeFleetPlan(t, root, filepath.Join(".dungeon", "broken", "plan.yaml"), time.Now(), map[string]string{
					"%ENC%": "broken", "%REPO%": repo, "%BASE%": "main", "%FILE%": "broken.txt",
				})
				breakUnitMinion(t, path)
			}

			var buf bytes.Buffer
			load := loadFleet(false, &buf)
			var got []string
			for _, target := range load.targets {
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
			if load.skipped != 1 || load.total != 2 {
				t.Fatalf("load counted %d skipped of %d, want 1 of 2", load.skipped, load.total)
			}
		})
	}
}

// F1: loading a fleet writes nothing. A preview must not create a state file,
// arm the edit guard or reorder discovery.
func TestLoadingAFleetWritesNoState(t *testing.T) {
	root, _ := fleetRoot(t, "one", "two")

	var buf bytes.Buffer
	load := loadFleet(false, &buf)
	if len(load.targets) != 2 {
		t.Fatalf("targets %d, want 2: %s", len(load.targets), buf.String())
	}
	for _, name := range []string{"one", "two"} {
		if stateExists(t, fleetStatePath(root, name)) {
			t.Fatalf("loading created %s", fleetStatePath(root, name))
		}
	}

	if code, _, stderr := runCLI("next", "--all", "--dry-run"); code != 0 {
		t.Fatalf("dry run exit %d: %s", code, stderr)
	}
	for _, name := range []string{"one", "two"} {
		if stateExists(t, fleetStatePath(root, name)) {
			t.Fatalf("a dry run created %s", fleetStatePath(root, name))
		}
	}

	// The edit is legal: nothing was dispatched, so nothing may refuse it.
	editAFile(t, fleetPlanPath(root, "one"), "goal: Touch one.txt.", "goal: Touch one.txt, and more.")
	code, stdout, stderr := runCLI("up", "--all", "--no-dispatch")
	if code != 0 {
		t.Fatalf("up --all exit %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "skipped") {
		t.Fatalf("a preview armed the edit guard:\n%s", stdout)
	}
	for _, want := range []string{"one: 1 spawned", "two: 1 spawned"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output missing %q:\n%s", want, stdout)
		}
	}
}

// F1: the hash is recorded when the lanes are told the plan, and --force is the
// only thing that records an edit the operator accepted instead.
func TestNextAllRecordsTheHashOnlyUnderForce(t *testing.T) {
	root, _ := fleetRoot(t, "one", "two")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	editAFile(t, fleetPlanPath(root, "one"), "goal: Touch one.txt.", "goal: Touch one.txt, and more.")

	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 1 {
		t.Fatalf("a plan edited after dispatch must be skipped: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"plan changed since dispatch", "1 of 2 encounters skipped"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output missing %q:\n%s", want, stdout)
		}
	}

	if code, _, stderr := runCLI("next", "--all", "--dry-run", "--force"); code != 0 {
		t.Fatalf("--force exit %d: %s", code, stderr)
	}
	want, err := PlanHash(fleetPlanPath(root, "one"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(fleetStatePath(root, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if st.PlanHash != want {
		t.Fatalf("--force must record the new hash, got %s want %s", shortHash(st.PlanHash), shortHash(want))
	}
}

// An empty sweep, or one with only a broken plan, could not run: exit 2.
func TestNextAllRefusesAnEmptySweep(t *testing.T) {
	root := t.TempDir()
	fakeHerdr(t, "")
	t.Chdir(root)
	code, _, stderr := runCLI("next", "--all", "--dry-run")
	if code != 2 {
		t.Fatalf("no encounter: exit %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no encounter to sweep") {
		t.Fatalf("stderr %q", stderr)
	}

	repo := gitRepo(t)
	path := writeFleetPlan(t, root, filepath.Join(".dungeon", "broken", "plan.yaml"), time.Now(), map[string]string{
		"%ENC%": "broken", "%REPO%": repo, "%BASE%": "main", "%FILE%": "x.txt",
	})
	breakUnitMinion(t, path)
	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 2 {
		t.Fatalf("only a broken plan: exit %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stdout, "skipped .dungeon/broken/plan.yaml") {
		t.Fatalf("the broken plan must be named:\n%s", stdout)
	}
}

// A broken plan is named and skipped while the rest of the sweep still runs,
// and the exit code says the sweep was not whole.
func TestNextAllSkipsABrokenPlanAndSweepsTheRest(t *testing.T) {
	root, _ := fleetRoot(t, "one")
	breakUnitMinion(t, writeFleetPlan(t, root, filepath.Join(".dungeon", "broken", "plan.yaml"), time.Now(), map[string]string{
		"%ENC%": "broken", "%REPO%": filepath.Join(root, "repo"), "%BASE%": "main", "%FILE%": "broken.txt",
	}))

	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr)
	}
	if !strings.Contains(stdout, "skipped .dungeon/broken/plan.yaml: units[0].minion") {
		t.Fatalf("the broken plan must be named:\n%s", stdout)
	}
	if !strings.Contains(stdout, "one\n") || !strings.Contains(stdout, "no lane: A") {
		t.Fatalf("the sweep must still run the readable plan:\n%s", stdout)
	}
	if !strings.Contains(stdout, "1 of 2 encounters skipped") {
		t.Fatalf("the summary must say how much was left out:\n%s", stdout)
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

// F3, and F11 for the forking case: --timeout bounds the accept commands too,
// not only the sleep between passes. `sh` forks for this gate, so killing only
// sh would leave `sleep` holding the output pipe.
func TestNextAllTimeoutBoundsAnAccept(t *testing.T) {
	root, _ := fleetRootWith(t, map[string]string{"%ACCEPT%": "sleep 3; true"}, "one", "two")
	t.Setenv("FAKE_REPORT", "1")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}

	const limit = time.Second
	start := time.Now()
	code, stdout, stderr := runCLI("next", "--all", "--poll", "20ms",
		"--timeout", limit.String(), "--accept-timeout", "30s")
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "timeout: lanes still working, run dungeon next --all again") {
		t.Fatalf("the sweep must report the timeout:\n%s", stdout)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("the sweep took %v for a %v timeout: the deadline must bound the accepts", elapsed, limit)
	}
	// A cut accept is not a rejection: the unit is still working.
	for _, name := range []string{"one", "two"} {
		st, err := LoadState(fleetStatePath(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.UnitStatus("A"); got != "working" {
			t.Fatalf("%s/A is %q, want working: %s", name, got, st.Units["A"].Note)
		}
	}
}

// F8: a dry run names the plan each dispatch belongs to, inside its block.
func TestNextAllDryRunNamesThePlan(t *testing.T) {
	fleetRoot(t, "one", "two")
	t.Setenv("FAKE_REPORT", "1")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}

	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, name := range []string{"one", "two"} {
		if !strings.Contains(stdout, name+"\n  would dispatch: A to w1\n") {
			t.Fatalf("the dry run must name %s in its block:\n%s", name, stdout)
		}
	}
	if strings.Contains(stdout, "would dispatch A ") {
		t.Fatalf("a dry-run line outside the blocks names no plan:\n%s", stdout)
	}
	if strings.Contains(stdout, "nothing to do") {
		t.Fatalf("a block that would dispatch must not say nothing to do:\n%s", stdout)
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

// F5: two plan files that claim one encounter name would share a worktree, a
// branch and a box key, so the sweep refuses before it touches anything.
func TestFleetRefusesTwoPlansWithOneEncounterName(t *testing.T) {
	repo := gitRepo(t)
	root := t.TempDir()
	fakeHerdr(t, "")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	writeFleetPlan(t, root, filepath.Join(".dungeon", "a", "plan.yaml"), base, map[string]string{
		"%ENC%": "dup", "%REPO%": repo, "%BASE%": "main", "%FILE%": "a.txt",
	})
	writeFleetPlan(t, root, filepath.Join(".dungeon", "b", "plan.yaml"), base.Add(-time.Hour), map[string]string{
		"%ENC%": "dup", "%REPO%": repo, "%BASE%": "main", "%FILE%": "b.txt",
	})
	t.Chdir(root)

	cases := map[string][]string{
		"up":   {"up", "--all", "--no-dispatch"},
		"next": {"next", "--all", "--dry-run"},
		"land": {"land", "--all"},
	}
	for name, args := range cases {
		code, _, stderr := runCLI(args...)
		if code != 2 {
			t.Fatalf("%s exit %d, want 2: %s", name, code, stderr)
		}
		for _, want := range []string{`declare encounter "dup"`, ".dungeon/a/plan.yaml", ".dungeon/b/plan.yaml", "rename one"} {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%s stderr missing %q:\n%s", name, want, stderr)
			}
		}
	}
	if _, err := os.Stat(worktreePath(repo, "dup", "w1")); err == nil {
		t.Fatal("a duplicate encounter must not cut a worktree")
	}
}

// land --all refuses what one checkout cannot do: a sweep over two repos, two
// bases, or a --branch that is already a plan's branch.
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
			name:  "a colliding integration branch",
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

// F2: --branch must not name a lane branch. The lane's worktree already exists
// on it, so the combined merge would run inside a live lane.
func TestLandAllRefusesALaneBranch(t *testing.T) {
	root, repo := fleetRoot(t, "one", "two")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	_ = root

	code, stdout, stderr := runCLI("land", "--all", "--branch", "one/w1")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stderr, "is w1's lane branch") || !strings.Contains(stderr, "pass --branch") {
		t.Fatalf("stderr must name the lane branch and the way out:\n%s", stderr)
	}
	if strings.Contains(stdout, "fleet branch") {
		t.Fatalf("nothing may be merged onto a lane branch:\n%s", stdout)
	}
	if head, err := gitRun(worktreePath(repo, "one", "w1"), "rev-parse", "--abbrev-ref", "HEAD"); err != nil || head != "one/w1" {
		t.Fatalf("the lane worktree moved: %q %v", head, err)
	}
}

// F7: every spelling the flag package accepts routes to the sweep, and
// --all=false stays on the single-plan path.
func TestLandAllAcceptsEveryAllSpelling(t *testing.T) {
	fleetRoot(t, "one")
	for _, spell := range []string{"-all", "--all", "--all=true"} {
		code, stdout, stderr := runCLI("land", spell)
		if code != 0 {
			t.Fatalf("land %s exit %d: %s", spell, code, stderr)
		}
		if !strings.Contains(stdout, "one: nothing to land") {
			t.Fatalf("land %s did not take the sweep path:\n%s", spell, stdout)
		}
	}

	code, stdout, stderr := runCLI("land", "--all=false")
	if code != 0 {
		t.Fatalf("land --all=false exit %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "one: nothing to land") {
		t.Fatalf("--all=false must not sweep:\n%s", stdout)
	}
	if !strings.Contains(stdout, "nothing to land") {
		t.Fatalf("land --all=false should land the one encounter:\n%s", stdout)
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
	if strings.Contains(stdout, "nothing was pushed") {
		t.Fatalf("a conflicted fleet branch must not print the success line:\n%s", stdout)
	}
}

// F4: a plan whose own gate fails must not stop the other plans from landing.
func TestLandAllLandsEveryPlanWhenOneFails(t *testing.T) {
	root, repo := fleetRootTwo(t, "a.txt", "false", "b.txt", "test -f b.txt")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	commitIn(t, worktreePath(repo, "a", "w1"), "a.txt", "a\n", "a")
	commitIn(t, worktreePath(repo, "b", "w1"), "b.txt", "b\n", "b")
	for _, name := range []string{"a", "b"} {
		if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", fleetPlanPath(root, name)); code != 0 {
			t.Fatalf("set %s: %s", name, stderr)
		}
	}

	code, stdout, stderr := runCLI("land", "--all")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"a: merged 1 units on a/integration",
		"a:   FAIL A: false",
		"b: merged 1 units on b/integration",
		"b:   ok   A: test -f b.txt",
		"did not land: a",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "fleet branch") {
		t.Fatalf("a fleet branch must not be cut when a plan did not land:\n%s", stdout)
	}
	if _, err := gitRun(repo, "rev-parse", "--verify", "refs/heads/b/integration"); err != nil {
		t.Fatalf("the other plan must still land: %v", err)
	}
}

// F6: a gate that fails on the combined tree still leaves the branch on disk,
// so it is reported as not good, never as a branch to trust.
func TestLandAllReportsAFailedCombinedGate(t *testing.T) {
	root, repo := fleetRootTwo(t, "a.txt", "test ! -f b.txt", "b.txt", "test -f b.txt")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	commitIn(t, worktreePath(repo, "a", "w1"), "a.txt", "a\n", "a")
	commitIn(t, worktreePath(repo, "b", "w1"), "b.txt", "b\n", "b")
	for _, name := range []string{"a", "b"} {
		if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", fleetPlanPath(root, name)); code != 0 {
			t.Fatalf("set %s: %s", name, stderr)
		}
	}

	code, stdout, stderr := runCLI("land", "--all")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "fleet branch fleet/integration") {
		t.Fatalf("the combined branch should be reported:\n%s", stdout)
	}
	if !strings.Contains(stdout, "FAIL a/A: test ! -f b.txt") {
		t.Fatalf("the failing unit must be named:\n%s", stdout)
	}
	if !strings.Contains(stdout, "fleet/integration is not good: a/A") {
		t.Fatalf("a branch that failed its gate is not good:\n%s", stdout)
	}
	if strings.Contains(stdout, "nothing was pushed") {
		t.Fatalf("a branch that failed its gate must not print the success line:\n%s", stdout)
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

// F10: a branch that already exists is someone's work. A sweep must not adopt it
// and move it, so the guard refuses before anything is merged.
func TestLandAllRefusesAPreExistingBranch(t *testing.T) {
	root, repo := fleetRoot(t, "one", "two")
	if _, err := gitRun(repo, "branch", "staging", "main"); err != nil {
		t.Fatal(err)
	}
	tip, err := gitRun(repo, "rev-parse", "staging")
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("land", "--all", "--branch", "staging")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stderr, "branch staging already exists") || !strings.Contains(stderr, "pass --branch") {
		t.Fatalf("stderr must name the branch and the way out:\n%s", stderr)
	}
	after, err := gitRun(repo, "rev-parse", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if after != tip {
		t.Fatalf("staging moved from %s to %s", tip, after)
	}
	_ = root
}

// F10: --branch must never be a plan's base. Land does not touch the base branch.
func TestLandAllRefusesTheBaseBranch(t *testing.T) {
	fleetRoot(t, "one", "two")
	repo, err := gitRepoAt(t)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := gitRun(repo, "rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("land", "--all", "--branch", "main")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stderr, "base branch") || !strings.Contains(stderr, "never moves a base branch") {
		t.Fatalf("stderr must say the base branch is out of bounds:\n%s", stderr)
	}
	after, err := gitRun(repo, "rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != tip {
		t.Fatalf("main moved from %s to %s", tip, after)
	}
}

// gitRepoAt reads the repo a fleet fixture points at.
func gitRepoAt(t *testing.T) (string, error) {
	t.Helper()
	plan, findings, err := loadPlan(fleetPlanPath(mustWd(t), "one"))
	if err != nil {
		return "", err
	}
	if len(findings) > 0 {
		t.Fatalf("plan findings: %v", findings)
	}
	return plan.Repo, nil
}

func mustWd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// F10: the branch a previous land cut is the one exception. Landing again must
// still work, or a fleet could never be re-landed.
func TestLandAllReLandsOnItsOwnFleetBranch(t *testing.T) {
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

	if code, stdout, stderr := runCLI("land", "--all"); code != 0 {
		t.Fatalf("first land exit %d: %s\n%s", code, stderr, stdout)
	}
	code, stdout, stderr := runCLI("land", "--all")
	if code != 0 {
		t.Fatalf("second land exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "fleet branch fleet/integration") {
		t.Fatalf("the second land must reuse the branch it cut:\n%s", stdout)
	}
}

// F12: a dry run settles nothing. It does not run the accept, write the state or
// mark the unit; it says what it would settle.
func TestNextAllDryRunSettlesNothing(t *testing.T) {
	repo := gitRepo(t)
	root := t.TempDir()
	fakeHerdr(t, "")
	plan := `encounter: one
repo: ` + repo + `
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: A
    minion: w1
    paths: [one.txt, settled.txt]
    goal: Touch one.txt.
    accept: touch settled.txt
`
	planPath := filepath.Join(root, ".dungeon", "one", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("FAKE_STATUS", "busy") // unknown: held until a report lands
	t.Setenv("FAKE_REPORT", "0")

	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	if code, _, stderr := runCLI("next", "--all", "--poll", "10ms", "--timeout", "80ms"); code != 0 {
		t.Fatalf("first next --all: %s", stderr)
	}
	// The lane writes the report a real lane would write.
	report := reportPath(planPath, "A")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("did the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := StatePath(planPath)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "would settle: A") {
		t.Fatalf("the preview must say what it would settle:\n%s", stdout)
	}
	if strings.Contains(stdout, "rejected:") || strings.Contains(stdout, "done:") {
		t.Fatalf("a preview must not settle:\n%s", stdout)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("a dry run wrote the state:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(worktreePath(repo, "one", "w1"), "settled.txt")); err == nil {
		t.Fatal("a dry run ran the accept command")
	}
}

// F13: a dry run predicts one pass, so it takes the same slots a real pass would.
func TestNextAllDryRunPredictsOnePass(t *testing.T) {
	repo := gitRepo(t)
	root := t.TempDir()
	fakeHerdr(t, "")
	plan := `encounter: one
repo: ` + repo + `
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: U1
    minion: w1
    paths: [one.txt]
    goal: One.
    accept: test -f one.txt
  - id: U2
    minion: w1
    paths: [two.txt]
    goal: Two.
    accept: test -f two.txt
`
	planPath := filepath.Join(root, ".dungeon", "one", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	code, stdout, stderr := runCLI("next", "--all", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "would dispatch: U1 to w1") {
		t.Fatalf("the first ready unit is what one pass dispatches:\n%s", stdout)
	}
	if strings.Contains(stdout, "U2 to w1") {
		t.Fatalf("one minion takes one unit per pass:\n%s", stdout)
	}
}

// F17: land --all keeps --all when a dispatch-level flag is present. The router
// takes its flags from the same set the command does, so it cannot drop the
// sweep on a flag it does not know.
func TestLandAllKeepsAllWithADispatchFlag(t *testing.T) {
	fleetRoot(t, "one")
	for _, args := range [][]string{
		{"land", "--all"},
		{"land", "--all", "--accept-timeout", "1s"},
		{"land", "--all", "--mux", "herdr"},
		{"land", "--all", "--poll", "1s"},
		{"land", "--all", "--timeout", "5m"},
		{"land", "--all", "--no-gate"},
		{"land", "--all=true", "--accept-timeout", "2s"},
	} {
		code, stdout, stderr := runCLI(args...)
		if code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, stderr)
		}
		if !strings.Contains(stdout, "one: nothing to land") {
			t.Fatalf("%v did not take the sweep path:\n%s", args, stdout)
		}
	}
}

// buildDungeon builds the binary a signal test needs: the in-process run()
// cannot be signalled, and the finding is about what a real process does.
func buildDungeon(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go tool is needed to build a binary a test can signal")
	}
	bin := filepath.Join(t.TempDir(), "dungeon")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// waitForPid waits for a file a gate writes when it starts.
func waitForPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gate never started: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// F15: an interrupt must reach the gate. The gate runs in its own process group,
// so a signal that killed dungeon outright would leave that group alive, writing
// into the worktree after the sweep is gone.
func TestAnInterruptStopsTheGateWithTheSweep(t *testing.T) {
	bin := buildDungeon(t)
	repo := gitRepo(t)
	root := t.TempDir()
	fakeHerdr(t, "")
	t.Setenv("FAKE_REPORT", "1")

	gateDir := t.TempDir()
	gate := filepath.Join(gateDir, "gate.sh")
	pidFile := filepath.Join(gateDir, "gate.pid")
	script := "#!/bin/sh\necho $$ > '" + pidFile + "'\nsleep 1\ntouch a.txt\n"
	if err := os.WriteFile(gate, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	plan := `encounter: one
repo: ` + repo + `
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: A
    minion: w1
    paths: [a.txt]
    goal: Touch a.txt.
    accept: ` + gate + "\n"
	planPath := filepath.Join(root, ".dungeon", "one", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}

	cmd := exec.Command(bin, "next", "--all", "--poll", "20ms", "--timeout", "60s")
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	pid := waitForPid(t, pidFile) // the gate is running now
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signal the sweep: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- cmd.Wait() }()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatalf("the sweep did not stop after SIGTERM:\n%s", out.String())
	}

	// The gate must be gone, not merely re-parented.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gate %d survived the interrupt (kill: %v):\n%s", pid, err, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// And it must not write into the worktree after the sweep is dead.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(worktreePath(repo, "one", "w1"), "a.txt")); err == nil {
		t.Fatalf("the gate wrote into the worktree after the sweep died:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "interrupted") {
		t.Fatalf("the sweep should say it was interrupted:\n%s", out.String())
	}
}

// F19: an interrupted land --all must not fabricate gate failures. A gate
// killed by the interrupt is not a FAIL gate: the sweep stops, says it was
// interrupted, withholds the fleet branch, could not run and leaves the state
// alone.
func TestAnInterruptedLandDoesNotFabricateGateFailures(t *testing.T) {
	bin := buildDungeon(t)
	gateDir := t.TempDir()
	gate := filepath.Join(gateDir, "gate.sh")
	pidFile := filepath.Join(gateDir, "gate.pid")
	script := "#!/bin/sh\necho $$ > '" + pidFile + "'\nsleep 8\ntrue\n"
	if err := os.WriteFile(gate, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root, repo := fleetRootWith(t, map[string]string{"%ACCEPT%": gate}, "one", "two")

	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	for _, name := range []string{"one", "two"} {
		commitIn(t, worktreePath(repo, name, "w1"), name+".txt", name+"\n", name)
		if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", fleetPlanPath(root, name)); code != 0 {
			t.Fatalf("set %s: %s", name, stderr)
		}
	}

	cmd := exec.Command(bin, "land", "--all", "--accept-timeout", "60s")
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	waitForPid(t, pidFile) // a plan's gate is running
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signal the land: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- cmd.Wait() }()
	select {
	case err := <-stopped:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("land exit %v, want 2:\n%s", err, out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the land did not stop after SIGTERM:\n%s", out.String())
	}

	text := out.String()
	if strings.Contains(text, "FAIL") {
		t.Fatalf("a gate cut by the interrupt is not a failed gate:\n%s", text)
	}
	if !strings.Contains(text, "interrupted: lanes still working, run dungeon land --all again") {
		t.Fatalf("the land must say it was interrupted:\n%s", text)
	}
	if strings.Contains(text, "fleet branch") {
		t.Fatalf("the fleet branch must be withheld:\n%s", text)
	}
	for _, name := range []string{"one", "two"} {
		st, err := LoadState(fleetStatePath(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if u := st.Units["A"]; u.Landed != nil {
			t.Fatalf("%s/A recorded a landing for a gate that was never judged: %+v", name, u.Landed)
		}
	}
}

// waitForFileContains waits until path holds substr: a gate that logs where it
// runs tells the test which gate is now running.
func waitForFileContains(t *testing.T, path, substr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(raw), substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never held %q: %v\n%s", path, substr, err, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// F20: an interrupt during the combined gate must not fabricate a failure. That
// gate was not judged, the fleet branch is left where it is, and the land says
// it was interrupted instead of calling the branch not good.
func TestAnInterruptedCombinedGateIsNotAFailure(t *testing.T) {
	bin := buildDungeon(t)
	gateDir := t.TempDir()
	gate := filepath.Join(gateDir, "gate.sh")
	ran := filepath.Join(gateDir, "ran.log")
	script := "#!/bin/sh\necho \"$PWD\" >> '" + ran + "'\nsleep 2\ntrue\n"
	if err := os.WriteFile(gate, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root, repo := fleetRootWith(t, map[string]string{"%ACCEPT%": gate}, "one", "two")

	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}
	for _, name := range []string{"one", "two"} {
		commitIn(t, worktreePath(repo, name, "w1"), name+".txt", name+"\n", name)
		if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", fleetPlanPath(root, name)); code != 0 {
			t.Fatalf("set %s: %s", name, stderr)
		}
	}

	cmd := exec.Command(bin, "land", "--all", "--accept-timeout", "60s")
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait until the combined worktree's gate is the one running.
	waitForFileContains(t, ran, "fleet-integration")
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signal the land: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- cmd.Wait() }()
	select {
	case err := <-stopped:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("land exit %v, want 2:\n%s", err, out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the land did not stop after SIGTERM:\n%s", out.String())
	}

	text := out.String()
	if strings.Contains(text, "FAIL") {
		t.Fatalf("a gate cut by the interrupt is not a failed gate:\n%s", text)
	}
	if strings.Contains(text, "is not good") {
		t.Fatalf("an unjudged gate must not condemn the fleet branch:\n%s", text)
	}
	if !strings.Contains(text, "interrupted: lanes still working, run dungeon land --all again") {
		t.Fatalf("the land must say it was interrupted:\n%s", text)
	}
	if !strings.Contains(text, "fleet branch fleet/integration") {
		t.Fatalf("the fleet branch stays where it is:\n%s", text)
	}
	if _, err := gitRun(repo, "rev-parse", "--verify", "refs/heads/fleet/integration"); err != nil {
		t.Fatalf("the fleet branch must be left in place: %v", err)
	}
}

// The poll sleep watches the context. A sweep with an 8s poll and a large
// accept timeout must stop the moment it is interrupted, not sleep the rest of
// the interval out.
func TestAnInterruptDuringThePollSleepExitsAtOnce(t *testing.T) {
	bin := buildDungeon(t)
	fleetRootWith(t, map[string]string{"%ACCEPT%": "true"}, "one")
	// A status word dungeon does not know: the lane is held until a report
	// lands, so the sweep stays in the loop with the unit still working.
	t.Setenv("FAKE_STATUS", "busy")
	t.Setenv("FAKE_REPORT", "0")
	if code, _, stderr := runCLI("up", "--all", "--no-dispatch"); code != 0 {
		t.Fatalf("up --all: %s", stderr)
	}

	cmd := exec.Command(bin, "next", "--all", "--poll", "8s", "--timeout", "60s", "--accept-timeout", "30m")
	cmd.Dir = mustGetwd(t)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// The brief has been pasted, so the pass is over and the sweep is sleeping.
	waitForFileContains(t, os.Getenv("FAKE_LOG"), "agent prompt")
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signal the sweep: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- cmd.Wait() }()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatalf("the sweep did not stop after SIGTERM:\n%s", out.String())
	}
	elapsed := time.Since(start)
	if !strings.Contains(out.String(), "interrupted: lanes still working, run dungeon next --all again") {
		t.Fatalf("the sweep must say it was interrupted:\n%s", out.String())
	}
	if elapsed >= time.Second {
		t.Fatalf("the sweep took %v to exit an 8s poll: an interrupt must not wait out the sleep\n%s",
			elapsed, out.String())
	}
}

// mustGetwd is the current directory, which a test has already moved into the
// fixture root.
func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// up --all leaves a settled encounter alone: its units are all done or
// canceled, so it spawns no lane, writes no state and says so on one line. A
// settled target is not a failure of the sweep.
func TestUpAllLeavesASettledEncounterAlone(t *testing.T) {
	root, _ := fleetRoot(t, "finished", "live")
	if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", fleetPlanPath(root, "finished")); code != 0 {
		t.Fatalf("set finished/A done: %s", stderr)
	}
	settledPath := fleetStatePath(root, "finished")
	before, err := os.ReadFile(settledPath)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("up", "--all", "--no-dispatch")
	if code != 0 {
		t.Fatalf("a settled encounter must not fail the sweep: exit %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "finished: settled, no lanes spawned\n") {
		t.Fatalf("the sweep must name the settled encounter:\n%s", stdout)
	}
	if !strings.Contains(stdout, "live: 1 spawned") {
		t.Fatalf("the live encounter is still spawned:\n%s", stdout)
	}
	after, err := os.ReadFile(settledPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a settled encounter must not have its state written:\nbefore %s\nafter %s", before, after)
	}
	calls, err := os.ReadFile(os.Getenv("FAKE_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(calls), "agent start"); n != 1 {
		t.Fatalf("one lane was spawned, want 1 agent start:\n%s", calls)
	}
}
