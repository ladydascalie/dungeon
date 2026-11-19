package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// depOtherPlan is a valid plan with a loop, so a dep may name a generated unit.
const depOtherPlan = `encounter: other
repo: /other
minions:
  - {id: w1, role: worker, worktree: w1}
  - {id: r1, role: reviewer, of: w1}
units:
  - id: rates
    minion: w1
    paths: [rates/**]
    goal: Rates.
loops:
  - {id: polish, reviewer: r1, fixer: w1, rounds: 2, focus: edge cases,
     pass: "Merge verdict: PASS", paths: [rates/**]}
`

// depLocalPlan is one unit with one dep, written however the case needs it.
func depLocalPlan(dep string) string {
	return `encounter: local
repo: /repo
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: A
    minion: w1
    paths: [a/**]
    goal: A.
    deps:
      - ` + dep + "\n"
}

func writeDepFile(t *testing.T, path, src string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// depCase writes the other plan and the local plan, then validates the local
// one. The dep is the single line the local plan's unit waits on.
func depCase(t *testing.T, dep string) []Finding {
	t.Helper()
	dir := t.TempDir()
	writeDepFile(t, filepath.Join(dir, "other", "plan.yaml"), depOtherPlan)
	src := depLocalPlan(dep)
	planPath := writeDepFile(t, filepath.Join(dir, "plan.yaml"), src)
	p, f := Decode([]byte(src))
	if len(f) > 0 {
		t.Fatalf("decode: %v", f)
	}
	return ValidateDeps(planPath, p)
}

func TestDepDecodesBothForms(t *testing.T) {
	src := depLocalPlan("{plan: ../../api/.dungeon/billing/plan.yaml, unit: rates-endpoint}")
	p := decodeOK(t, strings.Replace(src, "      - ", "      - recon\n      - ", 1))
	deps := p.Units[0].Deps
	if len(deps) != 2 {
		t.Fatalf("deps %v, want two", deps)
	}
	if !deps[0].Local() || deps[0].Unit != "recon" {
		t.Fatalf("a scalar dep must be local: %+v", deps[0])
	}
	if deps[1].Local() {
		t.Fatalf("a mapping dep must be cross-plan: %+v", deps[1])
	}
	if deps[1].Plan != "../../api/.dungeon/billing/plan.yaml" || deps[1].Unit != "rates-endpoint" {
		t.Fatalf("dep %+v", deps[1])
	}
	if got := deps[0].String(); got != "recon" {
		t.Fatalf("local dep label %q, want recon", got)
	}
	if got := deps[1].String(); got != "../../api/.dungeon/billing/plan.yaml#rates-endpoint" {
		t.Fatalf("cross dep label %q", got)
	}
}

func TestDepMappingRefusesAnUnknownKey(t *testing.T) {
	_, f := Decode([]byte(depLocalPlan("{plan: ../p.yaml, unit: rates, paln: typo}")))
	wantCodes(t, f, EParse)
	if !strings.Contains(f[0].Message, "paln") {
		t.Fatalf("the error must name the key: %q", f[0].Message)
	}
}

func TestDepNeedsAUnit(t *testing.T) {
	for _, dep := range []string{`""`, `{plan: ../p.yaml}`, `{plan: ../p.yaml, unit: ""}`} {
		_, f := Decode([]byte(depLocalPlan(dep)))
		wantCodes(t, f, EParse)
	}
}

func TestDepMarshalKeepsBothForms(t *testing.T) {
	deps := []Dep{{Unit: "recon"}, {Plan: "../../api/plan.yaml", Unit: "rates"}}
	out, err := yaml.Marshal(deps)
	if err != nil {
		t.Fatal(err)
	}
	want := "- recon\n- plan: ../../api/plan.yaml\n  unit: rates\n"
	if string(out) != want {
		t.Fatalf("marshalled\n%s\nwant\n%s", out, want)
	}
	var back []Dep
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, deps) {
		t.Fatalf("round trip gave %+v, want %+v", back, deps)
	}
}

// A plan with only local deps must marshal exactly as the []string it used to
// be, or every edit rewrites the file.
func TestLocalDepsMarshalAsBefore(t *testing.T) {
	old, err := yaml.Marshal([]string{"recon", "rates"})
	if err != nil {
		t.Fatal(err)
	}
	now, err := yaml.Marshal([]Dep{{Unit: "recon"}, {Unit: "rates"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(now) != string(old) {
		t.Fatalf("local deps marshal as\n%s\nwant\n%s", now, old)
	}
}

func TestValidateDepsMissingPlan(t *testing.T) {
	f := depCase(t, "{plan: nope/plan.yaml, unit: rates}")
	wantCodes(t, f, EDepPlan)
}

func TestValidateDepsMissingUnit(t *testing.T) {
	f := depCase(t, "{plan: other/plan.yaml, unit: nope}")
	wantCodes(t, f, EDepUnit)
	if !strings.Contains(f[0].Message, "nope") || !strings.Contains(f[0].Message, "other") {
		t.Fatalf("the finding must name the plan and the unit: %q", f[0].Message)
	}
}

func TestValidateDepsSelfReference(t *testing.T) {
	f := depCase(t, "{plan: plan.yaml, unit: A}")
	wantCodes(t, f, EDepSelf)
}

// Expanding the other plan is what lets a dep name a generated unit.
func TestValidateDepsAcceptsAGeneratedUnit(t *testing.T) {
	if f := depCase(t, "{plan: other/plan.yaml, unit: polish-review-3}"); len(f) > 0 {
		t.Fatalf("a generated unit of the other plan is a real dep: %v", f)
	}
}

func TestValidateDepsCatchesACycleAcrossPlans(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "local", "plan.yaml")
	otherPath := filepath.Join(dir, "other", "plan.yaml")
	other := strings.Replace(depOtherPlan, "    goal: Rates.\n",
		fmt.Sprintf("    goal: Rates.\n    deps:\n      - {plan: %s, unit: A}\n", planPath), 1)
	writeDepFile(t, otherPath, other)
	src := depLocalPlan(fmt.Sprintf("{plan: %s, unit: rates}", otherPath))
	writeDepFile(t, planPath, src)
	p, findings := Decode([]byte(src))
	if len(findings) > 0 {
		t.Fatalf("decode: %v", findings)
	}

	f := ValidateDeps(planPath, p)
	cycles := 0
	for _, c := range codes(f) {
		if c == ECycle {
			cycles++
		}
	}
	if cycles != 1 {
		t.Fatalf("want one cycle finding, got %v", codes(f))
	}
}

func crossDepState(t *testing.T) (planPath string, dep Dep, other State) {
	t.Helper()
	dir := t.TempDir()
	planPath = filepath.Join(dir, "plan.yaml")
	dep = Dep{Plan: "other/plan.yaml", Unit: "rates"}
	other = State{
		Encounter: "other",
		Units:     map[string]UnitState{"rates": {Status: "done", At: now()}},
		Minions:   map[string]MinionState{},
	}
	return planPath, dep, other
}

func TestCrossPlanDepNeedsALandedUnit(t *testing.T) {
	planPath, dep, other := crossDepState(t)
	p, st := Plan{}, &State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}

	// No state file at all: not satisfied, and not an error.
	if depSatisfied(planPath, p, st, dep) {
		t.Fatal("a missing other state must not satisfy a dep")
	}

	statePath := StatePath(resolveDepPlan(planPath, dep))
	if err := SaveState(statePath, other); err != nil {
		t.Fatal(err)
	}
	if depSatisfied(planPath, p, st, dep) {
		t.Fatal("done without landed must not satisfy a cross-plan dep")
	}

	other.SetLanded("rates", "api/integration", "abc1234")
	if err := SaveState(statePath, other); err != nil {
		t.Fatal(err)
	}
	if !depSatisfied(planPath, p, st, dep) {
		t.Fatal("done and landed must satisfy a cross-plan dep")
	}
}

func TestLandRecordsLanded(t *testing.T) {
	planPath, statePath, repo := landSetup(t)
	commitIn(t, worktreePath(repo, "enc", "w1"), "a.txt", "one\n", "A")
	if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", planPath); code != 0 {
		t.Fatalf("set: %s", stderr)
	}

	code, stdout, stderr := runCLI("land", "--plan", planPath)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	l := st.Units["A"].Landed
	if l == nil {
		t.Fatalf("a merged, gated unit must record where it landed: %+v", st.Units["A"])
	}
	if l.Ref != "enc/integration" {
		t.Fatalf("ref %q, want enc/integration", l.Ref)
	}
	if len(l.Commit) == 0 {
		t.Fatal("landed must record the commit it merged at")
	}
	if l.At == "" {
		t.Fatal("landed must record when")
	}
}

func TestLandLeavesAFailedGateUnlanded(t *testing.T) {
	planPath, statePath, repo := landSetup(t)
	commitIn(t, worktreePath(repo, "enc", "w1"), "a.txt", "one\n", "A")
	if code, _, stderr := runCLI("set", "unit", "A", "done", "--plan", planPath); code != 0 {
		t.Fatalf("set: %s", stderr)
	}
	plan := strings.Replace(twoWorkerPlan, "accept: test -f a.txt", "accept: false", 1)
	plan = strings.Replace(plan, "%REPO%", repo, 1)
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, stdout, _ := runCLI("land", "--force", "--plan", planPath); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Units["A"].Landed != nil {
		t.Fatalf("a failed gate must claim nothing: %+v", st.Units["A"].Landed)
	}
}

// A failure leaves that unit and every unit after it unlanded.
func TestLandStopsLandingAtAFailedGate(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "")
	planPath, statePath, _ := encounter(t, repo, strings.Replace(oneWorkerTwoUnits, "%REPO%", repo, 1))
	if code, _, stderr := runCLI("up", "--no-dispatch", "--plan", planPath); code != 0 {
		t.Fatalf("up: %s", stderr)
	}
	// A's file is committed, C's is not, so C's gate fails after A's passes.
	commitIn(t, worktreePath(repo, "enc", "w1"), "a.txt", "one\n", "A")
	for _, u := range []string{"A", "C"} {
		if code, _, stderr := runCLI("set", "unit", u, "done", "--plan", planPath); code != 0 {
			t.Fatalf("set %s: %s", u, stderr)
		}
	}

	if code, stdout, _ := runCLI("land", "--plan", planPath); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, stdout)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Units["A"].Landed == nil {
		t.Fatal("the unit before the failed gate did land")
	}
	if st.Units["C"].Landed != nil {
		t.Fatal("the unit that failed its gate must not be landed")
	}
}

// The brief is the lane's contract: a cross-plan dep must point at the other
// plan's report and say the ref and commit it landed on.
func TestBriefNamesACrossPlanDep(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "local", "plan.yaml")
	otherPath := filepath.Join(dir, "other", "plan.yaml")
	src := depLocalPlan("{plan: ../other/plan.yaml, unit: rates}")
	writeDepFile(t, planPath, src)
	p := decodeOK(t, src)
	m, _ := findMinion(p, "w1")

	other := State{
		Encounter: "other",
		Units:     map[string]UnitState{"rates": {Status: "done", At: now()}},
		Minions:   map[string]MinionState{},
	}
	other.SetLanded("rates", "other/integration", "abc1234")
	if err := SaveState(StatePath(otherPath), other); err != nil {
		t.Fatal(err)
	}

	path, err := writeBrief(newHerdr(), briefInput{
		opts: dispatchOpts{planPath: planPath}, plan: p, minion: m, unit: p.Units[0], state: &State{},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)

	if want := "deps that landed: ../other/plan.yaml#rates"; !strings.Contains(text, want) {
		t.Fatalf("brief missing %q:\n%s", want, text)
	}
	abs, err := filepath.Abs(reportPath(otherPath, "rates"))
	if err != nil {
		t.Fatal(err)
	}
	want := "- ../other/plan.yaml#rates: " + abs + " (landed on other/integration@abc1234)"
	if !strings.Contains(text, want) {
		t.Fatalf("brief missing %q:\n%s", want, text)
	}
}

func TestSnapshotShowsWhatLanded(t *testing.T) {
	p := decodeOK(t, depLocalPlan("recon"))
	st := State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	st.SetLanded("A", "enc/integration", "abc1234")
	s := buildSnapshot(p, "plan.yaml", st, nil)
	if got := s.Units[0].Landed; got != "enc/integration@abc1234" {
		t.Fatalf("landed %q, want enc/integration@abc1234", got)
	}
}

// A mapping is the cross-plan form: it must name a plan, and every value must
// be a plain one. Otherwise the dep silently becomes a local one.
func TestDepMappingNeedsAPlanAndPlainValues(t *testing.T) {
	for _, dep := range []string{
		`{unit: recon}`,
		`{plan: "", unit: recon}`,
		`{plan: [other/plan.yaml], unit: rates}`,
		`{plan: other/plan.yaml, unit: [rates]}`,
	} {
		_, f := Decode([]byte(depLocalPlan(dep)))
		wantCodes(t, f, EParse)
	}
}

// yaml.v3 refuses a duplicate key everywhere else in the plan.
func TestDepMappingRefusesADuplicateKey(t *testing.T) {
	_, f := Decode([]byte(depLocalPlan("{plan: a.yaml, plan: b.yaml, unit: u}")))
	wantCodes(t, f, EParse)
	if !strings.Contains(f[0].Message, "already defined") {
		t.Fatalf("the error must name the duplicate: %q", f[0].Message)
	}
}

// A review has no branch to merge, so done is the whole proof. A worker still
// needs Landed. The review here is generated by the other plan's loop, so this
// also proves the role is read from the expanded plan.
func TestCrossPlanDepOnAReviewNeedsOnlyDone(t *testing.T) {
	dir := t.TempDir()
	otherPath := filepath.Join(dir, "other", "plan.yaml")
	writeDepFile(t, otherPath, depOtherPlan)
	planPath := filepath.Join(dir, "plan.yaml")
	review := Dep{Plan: "other/plan.yaml", Unit: "polish-review-3"}
	rates := Dep{Plan: "other/plan.yaml", Unit: "rates"}

	other := State{
		Encounter: "other",
		Units: map[string]UnitState{
			"rates":           {Status: "done", At: now()},
			"polish-review-3": {Status: "done", At: now()},
		},
		Minions: map[string]MinionState{},
	}
	if err := SaveState(StatePath(otherPath), other); err != nil {
		t.Fatal(err)
	}
	p := Plan{}
	st := &State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}

	if !depSatisfied(planPath, p, st, review) {
		t.Fatal("a done review has no branch to merge, so it releases its dependants")
	}
	if depSatisfied(planPath, p, st, rates) {
		t.Fatal("a done worker that has not landed must not release its dependants")
	}
}

// A walk cut short by the depth cap must not mark its node done, or the next
// start skips the node and misses the cycle below it. A cross-plan dep is the
// mapping form: a bare scalar is a local unit id.
func TestValidateDepsFindsACyclePastTheDepthCap(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 10)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("p%d.yaml", i))
	}
	plan := func(name, unit, dep string) string {
		return fmt.Sprintf("encounter: %s\nrepo: /r\nunits:\n  - {id: %s, deps: [%s]}\n", name, unit, dep)
	}
	dep := func(path, unit string) string {
		return fmt.Sprintf("{plan: %s, unit: %s}", path, unit)
	}

	// u0 sits eight hops from p8's y, so the walk from u0 is cut at y's child.
	writeDepFile(t, paths[0], fmt.Sprintf("encounter: p0\nrepo: /r\nunits:\n  - {id: u0, deps: [%s]}\n  - {id: a2, deps: [%s]}\n",
		dep(paths[1], "c1"), dep(paths[8], "y")))
	for i := 1; i <= 7; i++ {
		target := dep(paths[i+1], fmt.Sprintf("c%d", i+1))
		if i == 7 {
			target = dep(paths[8], "y")
		}
		writeDepFile(t, paths[i], plan(fmt.Sprintf("p%d", i), fmt.Sprintf("c%d", i), target))
	}
	writeDepFile(t, paths[8], plan("p8", "y", dep(paths[9], "z")))
	writeDepFile(t, paths[9], plan("p9", "z", dep(paths[8], "y")))

	src, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	p, findings := Decode(src)
	if len(findings) > 0 {
		t.Fatalf("decode: %v", findings)
	}

	cycles := 0
	for _, c := range codes(ValidateDeps(paths[0], p)) {
		if c == ECycle {
			cycles++
		}
	}
	if cycles != 1 {
		t.Fatalf("want the y/z cycle reported once, got %v", codes(ValidateDeps(paths[0], p)))
	}
}
