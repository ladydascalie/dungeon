package main

import (
	"sort"
	"strings"
	"testing"
)

const goodPlan = `encounter: fix-auth-timeouts
repo: ~/Code/Personal/api
base: main
concurrency: 3
minions:
  - {id: scout, role: scout}
  - {id: w1, role: worker, worktree: fix-auth-w1}
  - {id: w2, role: worker, worktree: fix-auth-w2}
  - {id: r1, role: reviewer, of: w1}
  - {id: r2, role: reviewer, of: w2}
  - {id: land, role: integrator, worktree: fix-auth-int}
units:
  - id: recon
    minion: scout
    goal: Where is the read timeout set? Cite file:line.
  - id: api-timeout
    minion: w1
    paths: [apps/api/**]
    deps: [recon]
    goal: Read timeout comes from config.Default. Make it injectable.
    accept: go test ./apps/api/...
  - id: web-timeout
    minion: w2
    paths: [apps/web/**]
    deps: [recon]
    goal: Send the injected timeout on every request.
    accept: bun test
  - {id: review-api, minion: r1, deps: [api-timeout]}
  - {id: review-web, minion: r2, deps: [web-timeout]}
  - id: land
    minion: land
    deps: [review-api, review-web]
    accept: mise run gate
`

func decodeOK(t *testing.T, src string) Plan {
	t.Helper()
	p, f := Decode([]byte(src))
	if len(f) > 0 {
		t.Fatalf("decode failed: %v", f)
	}
	return p
}

// edit applies old/new pairs to the good plan source. A pair that does not
// match is a broken test, not a broken plan.
func edit(t *testing.T, pairs ...string) Plan {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatal("edit takes old/new pairs")
	}
	src := goodPlan
	for i := 0; i < len(pairs); i += 2 {
		old, new := pairs[i], pairs[i+1]
		if !strings.Contains(src, old) {
			t.Fatalf("test edit did not apply: %q", old)
		}
		src = strings.Replace(src, old, new, 1)
	}
	return decodeOK(t, src)
}

func wantCodes(t *testing.T, got []Finding, want ...Code) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", codes(got), want)
	}
	for i := range want {
		if got[i].Code != want[i] {
			t.Fatalf("got %v, want %v", codes(got), want)
		}
	}
}

func codes(fs []Finding) []Code {
	out := make([]Code, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return out
}

func TestGoodPlanIsClean(t *testing.T) {
	if f := Validate(decodeOK(t, goodPlan)); len(f) != 0 {
		t.Fatalf("unexpected findings: %v", f)
	}
}

func TestFindings(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want []Code
	}{
		{
			name: "the DM does not play a minion",
			plan: edit(t,
				"    minion: w1\n    paths: [apps/api/**]",
				"    minion: scout\n    paths: [apps/api/**]"),
			want: []Code{EDMPlaysMinion},
		},
		{
			name: "a writing unit must declare paths",
			plan: edit(t, "    paths: [apps/api/**]\n", ""),
			want: []Code{ENoPaths},
		},
		{
			name: "unknown minion on a unit",
			plan: edit(t, "    minion: w1\n", "    minion: w9\n"),
			want: []Code{EUnknownMinion},
		},
		{
			name: "unit names no minion",
			plan: edit(t, "    minion: w1\n", "    minion: \"\"\n"),
			want: []Code{ENoMinion},
		},
		{
			name: "unknown role",
			plan: edit(t, "role: worker, worktree: fix-auth-w1", "role: minon, worktree: fix-auth-w1"),
			want: []Code{EUnknownRole, EDMPlaysMinion},
		},
		{
			name: "a writer needs a worktree",
			plan: edit(t, "{id: w1, role: worker, worktree: fix-auth-w1}", "{id: w1, role: worker}"),
			want: []Code{ENoWorktree},
		},
		{
			name: "duplicate unit id",
			plan: edit(t, "{id: review-api, minion: r1", "{id: api-timeout, minion: r1"),
			want: []Code{EDuplicateID, ECycle, EMissingDep},
		},
		{
			name: "minion id is not a herdr name",
			plan: edit(t, "{id: w1, role: worker", "{id: W1, role: worker"),
			want: []Code{EBadMinionID, EUnknownMinion, EUnknownMinion},
		},
		{
			name: "a minion cannot be its own pair",
			plan: edit(t, "{id: r1, role: reviewer, of: w1}", "{id: r1, role: reviewer, of: r1}"),
			want: []Code{ECycle},
		},
		{
			name: "missing dep",
			plan: edit(t, "deps: [recon]", "deps: [nope]"),
			want: []Code{EMissingDep},
		},
		{
			name: "self dep",
			plan: edit(t, "    deps: [recon]\n", "    deps: [api-timeout]\n"),
			want: []Code{ECycle},
		},
		{
			name: "split with a staff role",
			plan: edit(t, "    minion: scout\n    goal:",
				"    split: {role: scout, count: 1}\n    goal:"),
			want: []Code{ESplitInvalid},
		},
		{
			name: "split without enough minions",
			plan: edit(t, "    minion: scout\n    goal:",
				"    split: {role: worker, count: 3}\n    goal:"),
			want: []Code{ESplitNoMinion},
		},
		{
			name: "split names a minion too",
			plan: edit(t, "    minion: w1\n    paths: [apps/api/**]",
				"    minion: w1\n    split: {role: worker, count: 2}\n    paths: [apps/api/**]"),
			want: []Code{ESplitInvalid},
		},
		{
			name: "no encounter",
			plan: edit(t, "encounter: fix-auth-timeouts\n", ""),
			want: []Code{ENoEncounter},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantCodes(t, Validate(c.plan), c.want...)
		})
	}
}

func TestNoMinionsAtAll(t *testing.T) {
	p := decodeOK(t, "encounter: empty\nminions: []\nunits: []\n")
	wantCodes(t, Validate(p), ENoMinion)
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	_, f := Decode([]byte(strings.Replace(goodPlan, "minions:", "minons:", 1)))
	wantCodes(t, f, EParse)
	if !strings.Contains(f[0].Message, "minons") {
		t.Fatalf("parse error should name the field: %q", f[0].Message)
	}
}

func TestDecodeEmptyPlan(t *testing.T) {
	_, f := Decode(nil)
	wantCodes(t, f, EParse)
}

func TestCycleIsReportedOnce(t *testing.T) {
	p := edit(t,
		"    deps: [recon]\n    goal: Read timeout",
		"    deps: [recon, web-timeout]\n    goal: Read timeout",
		"    deps: [recon]\n    goal: Send",
		"    deps: [recon, api-timeout]\n    goal: Send",
	)
	f := Validate(p)
	if len(f) != 1 || f[0].Code != ECycle {
		t.Fatalf("one loop should yield one finding, got %v", f)
	}
	if !strings.Contains(f[0].Message, "api-timeout") || !strings.Contains(f[0].Message, "web-timeout") {
		t.Fatalf("cycle should name its members: %q", f[0].Message)
	}
}

// The real dagr run in ll-frontend/.dagr/run.json, converted mechanically.
// It cannot pass: dagr records no paths, no roles, and 14 of 32 tasks carry no
// owner. Those counts are the finding, so pin them.
func TestDagrRunConvertedRawFails(t *testing.T) {
	p, f, err := DecodeFile("testdata/dagr-ll-frontend-raw.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(f) > 0 {
		t.Fatalf("decode: %v", f)
	}
	got := map[Code]int{}
	for _, x := range Validate(p) {
		got[x.Code]++
	}
	want := map[Code]int{ENoMinion: 14, ENoPaths: 8}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for code, n := range want {
		if got[code] != n {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The same run, set up correctly: every unit names a minion, every worker unit
// declares paths. Gates become reviewer verdicts, the orchestrator owner
// becomes a gatekeeper plus an integrator.
func TestDagrRunMappedIsClean(t *testing.T) {
	p, f, err := DecodeFile("testdata/dagr-ll-frontend-mapped.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(f) > 0 {
		t.Fatalf("decode: %v", f)
	}
	if got := Validate(p); len(got) != 0 {
		t.Fatalf("unexpected findings: %v", got)
	}
}

// The mapped plan must carry the same dependency graph as the run it came from.
// Only canceled work and the operator question may be missing.
func TestMappedKeepsTheSameGraph(t *testing.T) {
	raw, _, err := DecodeFile("testdata/dagr-ll-frontend-raw.json")
	if err != nil {
		t.Fatal(err)
	}
	mapped, _, err := DecodeFile("testdata/dagr-ll-frontend-mapped.yaml")
	if err != nil {
		t.Fatal(err)
	}
	omitted := map[string]bool{"TYPE1": true, "SHIP1": true, "QBG": true}

	rawDeps := map[string][]string{}
	for _, u := range raw.Units {
		rawDeps[u.ID] = u.Deps
	}
	for _, u := range mapped.Units {
		want, ok := rawDeps[u.ID]
		if !ok {
			if u.ID != "land" {
				t.Fatalf("unit %q is not in the dagr run", u.ID)
			}
			continue
		}
		if got := strings.Join(sorted(u.Deps), ","); got != strings.Join(sorted(want), ",") {
			t.Fatalf("unit %q deps %s, want %s", u.ID, got, strings.Join(sorted(want), ","))
		}
	}
	seen := map[string]bool{}
	for _, u := range mapped.Units {
		seen[u.ID] = true
	}
	for id := range rawDeps {
		if !seen[id] && !omitted[id] {
			t.Fatalf("unit %q was dropped without a reason", id)
		}
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestEncounterMustBeBranchSafe(t *testing.T) {
	p := decodeOK(t, strings.Replace(goodPlan, "encounter: fix-auth-timeouts", "encounter: Fix Auth", 1))
	wantCodes(t, Validate(p), EBadEncounter)
}

// aLoop is a valid loop statement, parked after the last unit of goodPlan.
const aLoop = `loops:
  - {id: polish, reviewer: r1, fixer: w1, rounds: 2, focus: error handling, pass: all tests pass, paths: [lib/**]}
`

func withLoop(t *testing.T, loop string) Plan {
	t.Helper()
	return edit(t, "    accept: mise run gate\n", "    accept: mise run gate\n"+loop)
}

func TestGoodLoopIsClean(t *testing.T) {
	p := withLoop(t, aLoop)
	if f := Validate(p); len(f) != 0 {
		t.Fatalf("unexpected findings: %v", f)
	}
}

func TestLoopFindings(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want []Code
	}{
		{
			name: "loop id collides with a unit",
			plan: withLoop(t, strings.Replace(aLoop, "id: polish", "id: api-timeout", 1)),
			want: []Code{EDuplicateID},
		},
		{
			name: "loop id is empty",
			plan: withLoop(t, strings.Replace(aLoop, "id: polish", `id: ""`, 1)),
			want: []Code{ENoID},
		},
		{
			name: "two loops share an id",
			plan: withLoop(t, aLoop+strings.Replace(aLoop, "loops:\n", "", 1)),
			want: []Code{EDuplicateID},
		},
		{
			name: "loop reviewer is not declared",
			plan: withLoop(t, strings.Replace(aLoop, "reviewer: r1", "reviewer: nope", 1)),
			want: []Code{EUnknownMinion},
		},
		{
			name: "loop reviewer does not review",
			plan: withLoop(t, strings.Replace(aLoop, "reviewer: r1", "reviewer: w1", 1)),
			want: []Code{ELoopRole},
		},
		{
			name: "loop reviewer is empty",
			plan: withLoop(t, strings.Replace(aLoop, "reviewer: r1", `reviewer: ""`, 1)),
			want: []Code{ENoMinion},
		},
		{
			name: "loop fixer cannot write",
			plan: withLoop(t, strings.Replace(aLoop, "fixer: w1", "fixer: r1", 1)),
			want: []Code{ELoopRole},
		},
		{
			name: "loop fixer is not declared",
			plan: withLoop(t, strings.Replace(aLoop, "fixer: w1", "fixer: nope", 1)),
			want: []Code{EUnknownMinion},
		},
		{
			name: "loop rounds below one",
			plan: withLoop(t, strings.Replace(aLoop, "rounds: 2", "rounds: 0", 1)),
			want: []Code{ELoopRounds},
		},
		{
			name: "loop rounds above nine",
			plan: withLoop(t, strings.Replace(aLoop, "rounds: 2", "rounds: 10", 1)),
			want: []Code{ELoopRounds},
		},
		{
			name: "loop has no pass",
			plan: withLoop(t, strings.Replace(aLoop, "pass: all tests pass", `pass: ""`, 1)),
			want: []Code{ELoopPass},
		},
		{
			name: "loop has no focus",
			plan: withLoop(t, strings.Replace(aLoop, "focus: error handling", `focus: ""`, 1)),
			want: []Code{ELoopFocus},
		},
		{
			name: "loop has no paths",
			plan: withLoop(t, strings.Replace(aLoop, ", paths: [lib/**]", "", 1)),
			want: []Code{ENoPaths},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantCodes(t, Validate(c.plan), c.want...)
		})
	}
}

func TestLoopFindingsAreAtTheLoop(t *testing.T) {
	p := withLoop(t, strings.Replace(aLoop, "rounds: 2", "rounds: 0", 1))
	f := Validate(p)
	if len(f) != 1 || !strings.HasPrefix(f[0].Path, "loops[0]") {
		t.Fatalf("loop finding should be at loops[0]: %v", f)
	}
}

func TestLoopReviewGoalIsOptional(t *testing.T) {
	p := withLoop(t, strings.Replace(aLoop, "pass: all tests pass",
		"pass: all tests pass, review_goal: find one real bug, fix_goal: fix it", 1))
	if f := Validate(p); len(f) != 0 {
		t.Fatalf("goals are optional, got %v", f)
	}
}

func TestDecodeRejectsUnknownLoopField(t *testing.T) {
	_, f := Decode([]byte(strings.Replace(goodPlan, "    accept: mise run gate\n",
		"    accept: mise run gate\n"+strings.Replace(aLoop, "rounds: 2", "round: 2", 1), 1)))
	wantCodes(t, f, EParse)
	if !strings.Contains(f[0].Message, "round") {
		t.Fatalf("parse error should name the key: %q", f[0].Message)
	}
}

func TestToolsReplaceTheRoleDefault(t *testing.T) {
	p := edit(t, "{id: w1, role: worker, worktree: fix-auth-w1}",
		"{id: w1, role: worker, worktree: fix-auth-w1, tools: \"read, bash, edit, write\"}")
	if f := Validate(p); len(f) != 0 {
		t.Fatalf("unexpected findings: %v", f)
	}
}

func TestToolsFindings(t *testing.T) {
	cases := []struct {
		name  string
		tools string
		want  []Code
	}{
		{name: "a name that is not a tool name", tools: "read,Bash", want: []Code{EBadTools}},
		{name: "an empty item", tools: "read,,bash", want: []Code{EBadTools}},
		{name: "a trailing comma", tools: "read,", want: []Code{EBadTools}},
		{name: "a duplicate", tools: "read,read", want: []Code{EBadTools}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := edit(t, "{id: w1, role: worker, worktree: fix-auth-w1}",
				"{id: w1, role: worker, worktree: fix-auth-w1, tools: \""+c.tools+"\"}")
			wantCodes(t, Validate(p), c.want...)
		})
	}
}

func TestReviewerVerdictIsClean(t *testing.T) {
	p := edit(t, "{id: review-api, minion: r1, deps: [api-timeout]}",
		`{id: review-api, minion: r1, deps: [api-timeout], verdict: "Verdict: shipped"}`)
	if f := Validate(p); len(f) != 0 {
		t.Fatalf("unexpected findings: %v", f)
	}
}

func TestVerdictFindings(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want []Code
	}{
		{
			name: "a worker cannot give a verdict",
			plan: edit(t, "    goal: Where is the read timeout set? Cite file:line.",
				"    goal: Where is the read timeout set? Cite file:line.\n    verdict: done"),
			want: []Code{EVerdictRole},
		},
		{
			name: "a verdict unit cannot also accept",
			plan: edit(t, "{id: review-api, minion: r1, deps: [api-timeout]}",
				`{id: review-api, minion: r1, deps: [api-timeout], verdict: "Verdict: shipped", accept: go test ./apps/api/...}`),
			want: []Code{EVerdictAccept},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantCodes(t, Validate(c.plan), c.want...)
		})
	}
}

func TestProvisionalIsGeneratedOnly(t *testing.T) {
	src := strings.Replace(goodPlan, "    accept: go test ./apps/api/...",
		"    accept: go test ./apps/api/...\n    provisional: true", 1)
	_, f := Decode([]byte(src))
	wantCodes(t, f, EParse)
	if !strings.Contains(f[0].Message, "provisional") {
		t.Fatalf("the decode error should name the key: %q", f[0].Message)
	}
}

// Two minion ids that share a prefix collide once the name is truncated, so a
// plan that declares them is refused rather than left to race one agent.
func TestTruncatedMinionIDsCollide(t *testing.T) {
	base := strings.Repeat("a", 29)
	src := "encounter: enc\nminions:\n" +
		"  - {id: " + base + "aa, role: worker, worktree: w1}\n" +
		"  - {id: " + base + "bb, role: worker, worktree: w2}\n"
	wantCodes(t, Validate(decodeOK(t, src)), EDuplicateAgent)
}

// A long id that stays distinct in its first 29 characters derives its own name.
func TestLongMinionIDsStayDistinct(t *testing.T) {
	base := strings.Repeat("a", 28)
	src := "encounter: enc\nminions:\n" +
		"  - {id: " + base + "aa, role: worker, worktree: w1}\n" +
		"  - {id: " + base + "bb, role: worker, worktree: w2}\n"
	wantCodes(t, Validate(decodeOK(t, src)))
}
