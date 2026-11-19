package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planWithRoom is written the way an operator writes one: comments, blank lines
// between units, folded goals, a flow list and a flow minion. Every edit test
// asserts the whole file, so a reformatted plan fails the test.
const planWithRoom = `# Two writers, one door.
encounter: mount-the-door
repo: ~/src/api
base: main

minions:
  # A writer owns a worktree.
  - {id: w1, role: worker, worktree: w1}
  - {id: r1, role: reviewer, of: w1}

units:
  - id: admin-config
    minion: w1
    paths: [admin/**]
    goal: >
      Hold the admin-side login configuration.

  - id: mount-login-surface
    minion: w1
    deps: [admin-config]
    paths:
      - api/**
    goal: >
      Mount the login surface.
    accept: go test ./api/...

  - id: contract-confirm-403
    minion: w1
    deps: [mount-login-surface]
    paths: [api/publisherauthhttp/**]
    goal: >
      A disabled login answers 403, and never says whether the account exists.
    accept: go test ./api/publisherauthhttp/...
`

func runEdit(t *testing.T, src string, args ...string) (file string, code int, stdout, stderr string) {
	t.Helper()
	path := writePlan(t, src)
	code, stdout, stderr = runCLI(append([]string{"edit", "--plan", path}, args...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), code, stdout, stderr
}

func TestEditAddsAPathToAFlowList(t *testing.T) {
	after, code, stdout, stderr := runEdit(t, planWithRoom, "unit", "contract-confirm-403", "--paths-add", "services/auth/**")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := strings.Replace(planWithRoom,
		"    paths: [api/publisherauthhttp/**]",
		"    paths: [api/publisherauthhttp/**, services/auth/**]", 1)
	if after != want {
		t.Fatalf("plan changed beyond the edit:\n%s",
			unifiedDiff("plan", planLines([]byte(want)), planLines([]byte(after)), 3))
	}
	if !strings.Contains(stdout, "+    paths: [api/publisherauthhttp/**, services/auth/**]") {
		t.Fatalf("the diff should show the new path: %s", stdout)
	}
	if !strings.Contains(stdout, "ok: mount-the-door, 2 minions, 3 units") {
		t.Fatalf("the edit should report the plan is clean: %s", stdout)
	}
}

func TestEditAddsAPathToABlockList(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "mount-login-surface", "--paths-add", "config/**")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := strings.Replace(planWithRoom, "      - api/**\n", "      - api/**\n      - config/**\n", 1)
	if after != want {
		t.Fatalf("got:\n%s\nwant:\n%s", after, want)
	}
}

func TestEditInsertsAFieldTheUnitLacks(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "admin-config", "--accept", "go test ./admin/...")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := strings.Replace(planWithRoom,
		"      Hold the admin-side login configuration.\n\n  - id: mount-login-surface",
		"      Hold the admin-side login configuration.\n    accept: go test ./admin/...\n\n  - id: mount-login-surface", 1)
	if after != want {
		t.Fatalf("the new field should go last, before the blank line:\n%s", after)
	}
}

func TestEditRefusesAVerdictOnAWorker(t *testing.T) {
	after, code, stdout, stderr := runEdit(t, planWithRoom, "unit", "admin-config", "--verdict", "Verdict: PASS")
	if code != 1 {
		t.Fatalf("exit %d, want 1 (a refused edit)", code)
	}
	if !strings.Contains(stderr, "E_VERDICT_ROLE") || !strings.Contains(stderr, "unchanged") {
		t.Fatalf("the refusal should name the finding: %q", stderr)
	}
	if after != planWithRoom || stdout != "" {
		t.Fatal("a refused edit writes nothing and prints no diff")
	}
}

func TestEditReplacesAScalar(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "mount-login-surface",
		"--goal", "Mount the login surface and gate it on the configuration.")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := strings.Replace(planWithRoom,
		"    goal: >\n      Mount the login surface.\n",
		"    goal: Mount the login surface and gate it on the configuration.\n", 1)
	if after != want {
		t.Fatalf("got:\n%s\nwant:\n%s", after, want)
	}
}

func TestEditLongGoalFoldsAndRoundTrips(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 30) + "end")
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "mount-login-surface", "--goal", long)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(after, "    goal: >-\n") {
		t.Fatalf("a long goal should be folded:\n%s", after)
	}
	for _, line := range strings.Split(after, "\n") {
		if len(line) > 100 {
			t.Fatalf("a folded goal keeps lines short, got %d: %q", len(line), line)
		}
	}
	p, findings := Decode([]byte(after))
	if len(findings) > 0 {
		t.Fatalf("decode: %v", findings)
	}
	unit, ok := findUnit(p, "mount-login-surface")
	if !ok {
		t.Fatal("unit is gone")
	}
	if unit.Goal != long {
		t.Fatalf("goal round trip: got %q, want %q", unit.Goal, long)
	}
}

func TestEditClearsAScalar(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "contract-confirm-403", "--goal", "")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(after, "A disabled login answers 403") {
		t.Fatalf("the goal survived:\n%s", after)
	}
	if !strings.Contains(after, "    paths: [api/publisherauthhttp/**]\n    accept: go test ./api/publisherauthhttp/...\n") {
		t.Fatalf("the field after the removed one moved:\n%s", after)
	}
}

func TestEditFlowMinionStaysOnOneLine(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "minion", "r1", "--tools", "read,bash,grep")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := strings.Replace(planWithRoom,
		"  - {id: r1, role: reviewer, of: w1}\n",
		"  - {id: r1, role: reviewer, of: w1, tools: 'read,bash,grep'}\n", 1)
	if after != want {
		t.Fatalf("got:\n%s\nwant:\n%s", after, want)
	}
}

func TestEditRefusesAnEditThatAddsAFinding(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "mount-login-surface", "--deps-add", "ghost")
	if code != 1 {
		t.Fatalf("exit %d, want 1 (a refused edit)", code)
	}
	if !strings.Contains(stderr, "E_MISSING_DEP") {
		t.Fatalf("the refusal should name the finding: %q", stderr)
	}
	if after != planWithRoom {
		t.Fatalf("a refused edit must not touch the plan:\n%s", after)
	}
}

func TestEditNamesAnUnknownUnit(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "ghost", "--paths-add", "x/**")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "unit ghost is not in the plan") {
		t.Fatalf("stderr %q", stderr)
	}
	if after != planWithRoom {
		t.Fatal("the plan changed")
	}
}

func TestEditRefusesToRemoveAnEntryTheUnitLacks(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "mount-login-surface", "--paths-rm", "db/**")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, `paths does not list "db/**"`) {
		t.Fatalf("stderr %q", stderr)
	}
	if after != planWithRoom {
		t.Fatal("the plan changed")
	}
}

func TestEditRefusesAFlagForTheOtherKind(t *testing.T) {
	for _, c := range [][]string{
		{"unit", "mount-login-surface", "--role", "worker"},
		{"minion", "w1", "--paths-add", "api/**"},
	} {
		_, code, _, stderr := runEdit(t, planWithRoom, c...)
		if code != 2 || !strings.Contains(stderr, "not a ") {
			t.Fatalf("%v: exit %d, stderr %q", c, code, stderr)
		}
	}
}

func TestEditNamesNoField(t *testing.T) {
	_, code, _, stderr := runEdit(t, planWithRoom, "unit", "mount-login-surface")
	if code != 2 || !strings.Contains(stderr, "names no field to change") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestEditDryRunWritesNothing(t *testing.T) {
	after, code, stdout, stderr := runEdit(t, planWithRoom, "unit", "mount-login-surface", "--paths-add", "db/**", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if after != planWithRoom {
		t.Fatalf("--dry-run wrote the plan:\n%s", after)
	}
	if !strings.Contains(stdout, "+      - db/**") || !strings.Contains(stdout, "dry run: nothing written") {
		t.Fatalf("stdout %q", stdout)
	}
}

func TestEditIsIdempotent(t *testing.T) {
	after, code, stdout, stderr := runEdit(t, planWithRoom, "unit", "mount-login-surface", "--paths-add", "api/**")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if after != planWithRoom {
		t.Fatalf("an add that changes nothing must write nothing:\n%s", after)
	}
	if !strings.Contains(stdout, "nothing to change") {
		t.Fatalf("stdout %q", stdout)
	}
}

func TestEditKeepsCommentsAndBlankLines(t *testing.T) {
	after, code, _, stderr := runEdit(t, planWithRoom, "unit", "contract-confirm-403", "--paths-add", "db/**")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, keep := range []string{
		"# Two writers, one door.\n",
		"  # A writer owns a worktree.\n",
		"\n  - id: mount-login-surface\n",
		"\nunits:\n",
	} {
		if !strings.Contains(after, keep) {
			t.Fatalf("%q was lost:\n%s", keep, after)
		}
	}
}

func TestEditWarnsWhenThePlanWasDispatched(t *testing.T) {
	path := writePlan(t, planWithRoom)
	state := State{
		Encounter: "mount-the-door",
		PlanHash:  strings.Repeat("ab", 32),
		Units:     map[string]UnitState{},
		Minions:   map[string]MinionState{},
	}
	if err := SaveState(StatePath(path), state); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI("edit", "--plan", path, "unit", "mount-login-surface", "--paths-add", "db/**")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "so next and land need --force") {
		t.Fatalf("the drift warning is missing: %q", stdout)
	}
}

func TestEditFindsTheOnlyEncounter(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, ".dungeon", "mount-the-door", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte(planWithRoom), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	code, _, stderr := runCLI("edit", "unit", "mount-login-surface", "--paths-add", "db/**")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- db/**") {
		t.Fatal("the discovered encounter was not edited")
	}
}

func TestUnifiedDiffGroupsHunks(t *testing.T) {
	before := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	after := []string{"a", "B", "c", "d", "e", "f", "g", "h", "I", "j"}
	got := unifiedDiff("plan.yaml", before, after, 1)
	want := `--- a/plan.yaml
+++ b/plan.yaml
@@ -1,3 +1,3 @@
 a
-b
+B
 c
@@ -8,3 +8,3 @@
 h
-i
+I
 j
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// depEditPlan takes deps in both list forms. The plan it names is written beside
// it, so a cross-plan dep resolves.
const depEditPlan = `encounter: cross
repo: ~/src/api
base: main

minions:
  - {id: w1, role: worker, worktree: w1}

units:
  - id: block-unit
    minion: w1
    deps:
      - local-one
    paths: [a/**]
    goal: Block deps.

  - id: flow-unit
    minion: w1
    deps: [local-one]
    paths: [b/**]
    goal: Flow deps.

  - id: local-one
    minion: w1
    paths: [c/**]
    goal: Local.
`

const depEditOtherPlan = `encounter: other
repo: ~/src/other
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: rates
    minion: w1
    paths: [rates/**]
    goal: Rates.
`

// depPlanFile writes the dep plan and the plan it names into one directory, so
// a cross-plan dep has a file to resolve. A second edit reads what the first
// wrote.
func depPlanFile(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	other := filepath.Join(dir, "other", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(depEditOtherPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func editDepFile(t *testing.T, path string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runCLI(append([]string{"edit", "--plan", path}, args...)...)
}

func readPlan(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// withDep is depEditPlan with one unit's deps line replaced, so a test states the
// file it starts from.
func withDep(old, next string) string {
	return strings.Replace(depEditPlan, old, next, 1)
}

const crossDepFlag = "other/plan.yaml#rates"

func TestEditAddsACrossPlanDepToABlockList(t *testing.T) {
	path := depPlanFile(t, depEditPlan)
	code, stdout, stderr := editDepFile(t, path, "unit", "block-unit", "--deps-add", crossDepFlag)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := withDep("    deps:\n      - local-one\n",
		"    deps:\n      - local-one\n      - {plan: other/plan.yaml, unit: rates}\n")
	if got := readPlan(t, path); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(stdout, "+      - {plan: other/plan.yaml, unit: rates}") {
		t.Fatalf("the diff should show the new dep: %s", stdout)
	}
}

func TestEditAddsACrossPlanDepToAFlowList(t *testing.T) {
	path := depPlanFile(t, depEditPlan)
	code, _, stderr := editDepFile(t, path, "unit", "flow-unit", "--deps-add", crossDepFlag)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := withDep("    deps: [local-one]",
		"    deps: [local-one, {plan: other/plan.yaml, unit: rates}]")
	if got := readPlan(t, path); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestEditRemovesACrossPlanDep(t *testing.T) {
	for _, c := range []struct {
		name, unit, src string
	}{
		{
			name: "block",
			unit: "block-unit",
			src: withDep("    deps:\n      - local-one\n",
				"    deps:\n      - local-one\n      - {plan: other/plan.yaml, unit: rates}\n"),
		},
		{
			name: "flow",
			unit: "flow-unit",
			src: withDep("    deps: [local-one]",
				"    deps: [local-one, {plan: other/plan.yaml, unit: rates}]"),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := depPlanFile(t, c.src)
			code, _, stderr := editDepFile(t, path, "unit", c.unit, "--deps-rm", crossDepFlag)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if got := readPlan(t, path); got != depEditPlan {
				t.Fatalf("removing the dep should restore the plan:\n%s", got)
			}
		})
	}
}

// An add that changes nothing writes nothing: the flag value and the mapping
// entry must compare equal.
func TestEditAddIsIdempotentForADep(t *testing.T) {
	src := withDep("    deps: [local-one]",
		"    deps: [local-one, {plan: other/plan.yaml, unit: rates}]")
	path := depPlanFile(t, src)
	code, stdout, stderr := editDepFile(t, path, "unit", "flow-unit", "--deps-add", crossDepFlag)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to change") {
		t.Fatalf("stdout %q", stdout)
	}
	if got := readPlan(t, path); got != src {
		t.Fatalf("an add that changes nothing must not write:\n%s", got)
	}
}

// A flow list that holds a mapping entry survives a set.
func TestEditSetsADepListThatHoldsAMapping(t *testing.T) {
	src := withDep("    deps: [local-one]",
		"    deps: [local-one, {plan: other/plan.yaml, unit: rates}]")
	path := depPlanFile(t, src)
	code, _, stderr := editDepFile(t, path, "unit", "flow-unit", "--deps-set", "local-one")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := readPlan(t, path); got != depEditPlan {
		t.Fatalf("got:\n%s\nwant:\n%s", got, depEditPlan)
	}
}

func TestEditRefusesToRemoveADepTheUnitLacks(t *testing.T) {
	path := depPlanFile(t, depEditPlan)
	code, _, stderr := editDepFile(t, path, "unit", "block-unit", "--deps-rm", crossDepFlag)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, `deps does not list "other/plan.yaml#rates"`) {
		t.Fatalf("stderr %q", stderr)
	}
	if got := readPlan(t, path); got != depEditPlan {
		t.Fatal("the plan changed")
	}
}

func TestEditRefusesACrossPlanDepOnAMissingPlan(t *testing.T) {
	path := depPlanFile(t, depEditPlan)
	code, _, stderr := editDepFile(t, path, "unit", "block-unit", "--deps-add", "nope/plan.yaml#rates")
	if code != 1 {
		t.Fatalf("exit %d, want 1 (a refused edit)", code)
	}
	if !strings.Contains(stderr, "E_DEP_PLAN") || !strings.Contains(stderr, "unchanged") {
		t.Fatalf("the refusal should name the finding: %q", stderr)
	}
	if got := readPlan(t, path); got != depEditPlan {
		t.Fatal("a refused edit must not touch the plan")
	}
}

func TestEditRefusesACrossPlanDepOnAMissingUnit(t *testing.T) {
	path := depPlanFile(t, depEditPlan)
	code, _, stderr := editDepFile(t, path, "unit", "block-unit", "--deps-add", "other/plan.yaml#nope")
	if code != 1 {
		t.Fatalf("exit %d, want 1 (a refused edit)", code)
	}
	if !strings.Contains(stderr, "E_DEP_UNIT") {
		t.Fatalf("the refusal should name the finding: %q", stderr)
	}
}

func TestEditRefusesADepWithAnEmptySide(t *testing.T) {
	for _, v := range []string{"other/plan.yaml#", "#rates"} {
		path := depPlanFile(t, depEditPlan)
		code, _, stderr := editDepFile(t, path, "unit", "block-unit", "--deps-add", v)
		if code != 2 {
			t.Fatalf("%q: exit %d, want 2", v, code)
		}
		if !strings.Contains(stderr, "both sides") {
			t.Fatalf("%q: stderr %q", v, stderr)
		}
		if got := readPlan(t, path); got != depEditPlan {
			t.Fatalf("%q: the plan changed", v)
		}
	}
}
