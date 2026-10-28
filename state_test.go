package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dungeon", "state.json")
	st := State{Encounter: "x", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	if err := st.SetUnit("A", "working", "brief written"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMinion("w1", "busy", ""); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(path, st); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Encounter != "x" {
		t.Fatalf("encounter %q", got.Encounter)
	}
	if got.UnitStatus("A") != "working" || got.Units["A"].Note != "brief written" {
		t.Fatalf("unit %+v", got.Units["A"])
	}
	if got.MinionStatus("w1") != "busy" {
		t.Fatalf("minion %+v", got.Minions["w1"])
	}
	if got.UpdatedAt == "" {
		t.Fatal("updated_at should be stamped")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temporary file should be renamed away")
	}
}

func TestStateHistoryAppends(t *testing.T) {
	st := State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	for _, s := range []string{"planned", "working", "blocked", "working", "done"} {
		if err := st.SetUnit("A", s, ""); err != nil {
			t.Fatal(err)
		}
	}
	h := st.Units["A"].History
	if len(h) != 5 {
		t.Fatalf("history %v", h)
	}
	if h[2].From != "working" || h[2].To != "blocked" {
		t.Fatalf("history entry %+v", h[2])
	}
}

func TestStateRejectsUnknownStatus(t *testing.T) {
	st := State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	err := st.SetUnit("A", "complete", "")
	if err == nil || !strings.Contains(err.Error(), "done") {
		t.Fatalf("want a refusal that lists the valid statuses, got %v", err)
	}
	if err := st.SetMinion("w1", "sleeping", ""); err == nil {
		t.Fatal("want a refusal for an unknown minion status")
	}
}

func TestDefaultsArePlannedAndAbsent(t *testing.T) {
	st := State{}
	if got := st.UnitStatus("nope"); got != "planned" {
		t.Fatalf("unit default %q", got)
	}
	if got := st.MinionStatus("nope"); got != "absent" {
		t.Fatalf("minion default %q", got)
	}
}

func TestStatePathSitsBesideThePlan(t *testing.T) {
	if got := StatePath(".dungeon/plan.yaml"); got != ".dungeon/state.json" {
		t.Fatalf("got %q", got)
	}
}

func TestTokenFormat(t *testing.T) {
	valid := regexp.MustCompile(`^[0-9a-z]{2}$`)
	for i := 0; i < 500; i++ {
		if tok := newToken(); !valid.MatchString(tok) {
			t.Fatalf("token %q is not two characters from [0-9a-z]", tok)
		}
	}
}

func TestAgentNameUsesTheToken(t *testing.T) {
	if got := (State{Token: "7f"}).AgentName("w1"); got != "w1-7f" {
		t.Fatalf("agent name %q, want w1-7f", got)
	}
	// A state with no token keeps the bare id, so an in-flight run is unchanged.
	if got := (State{}).AgentName("w1"); got != "w1" {
		t.Fatalf("a tokenless state must use the bare id, got %q", got)
	}
}

// Two runs of the same plan declare the same minion ids. The token is what
// keeps them off one agent.
func TestTwoRunsOfOnePlanGetDifferentNames(t *testing.T) {
	a, b := State{Token: "aa"}, State{Token: "bb"}
	if a.AgentName("w1") == b.AgentName("w1") {
		t.Fatalf("both runs derived %q", a.AgentName("w1"))
	}
}

func TestAgentNameTruncatesToTheHerdrLimit(t *testing.T) {
	long := "w" + strings.Repeat("x", 31) // 32, the longest minion id a plan allows
	name := (State{Token: "7f"}).AgentName(long)
	if len(name) != 32 {
		t.Fatalf("agent name %q is %d bytes, want 32", name, len(name))
	}
	if !strings.HasSuffix(name, "-7f") {
		t.Fatalf("the token must survive truncation, got %q", name)
	}
}

func TestPickTokenRerollsALiveCandidate(t *testing.T) {
	agents := map[string]Agent{"w1-aa": {Name: "w1-aa", Status: "idle"}}
	seq := []string{"aa", "bb"}
	draws := 0
	gen := func() string {
		tok := seq[draws]
		draws++
		return tok
	}
	tok, err := pickToken(agents, []Minion{{ID: "w1"}}, gen)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if tok != "bb" || draws != 2 {
		t.Fatalf("token %q after %d draws, want bb after 2", tok, draws)
	}
}

func TestPickTokenGivesUpAfter64Tries(t *testing.T) {
	agents := map[string]Agent{"w1-aa": {Name: "w1-aa", Status: "idle"}}
	draws := 0
	gen := func() string { draws++; return "aa" }
	_, err := pickToken(agents, []Minion{{ID: "w1"}}, gen)
	if err == nil {
		t.Fatal("a token that always collides must fail rather than return a name")
	}
	if draws != 64 {
		t.Fatalf("drew %d tokens, want 64", draws)
	}
	if !strings.Contains(err.Error(), "w1-aa") {
		t.Fatalf("the finding must name the live agent, got %v", err)
	}
}

func TestTokenSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := State{Encounter: "x", Token: "7f", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	if err := SaveState(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentName("w1") != "w1-7f" {
		t.Fatalf("a reloaded state lost the token: %q", got.AgentName("w1"))
	}
}

// A tokenless state owns a bare lane only when it recorded that lane's pane.
func TestLegacyRunLiveNeedsItsOwnPaneRecord(t *testing.T) {
	minions := []Minion{{ID: "w1"}}
	agents := map[string]Agent{"w1": {Name: "w1", Pane: "w9:p2"}}

	st := &State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	if legacyRunLive(st, agents, minions) {
		t.Fatal("a state with no pane record must not adopt a live bare lane")
	}

	st.PinPane("w1", "w9:p9")
	if legacyRunLive(st, agents, minions) {
		t.Fatal("a live bare lane in another pane is another run's")
	}

	st.PinPane("w1", "w9:p2")
	if !legacyRunLive(st, agents, minions) {
		t.Fatal("a live bare lane at the recorded pane is this run's")
	}
}

// A fresh run that finds a bare w1 from another run must take a token and
// spawn its own lane, not attach to the stranger.
func TestEnsureTokenRefusesAForeignBareLane(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "w1:working\n")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))

	code, stdout, stderr := runCLI("up", "--no-dispatch", "--plan", planPath)
	if code != 0 {
		t.Fatalf("up exit %d: %s", code, stderr)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Token == "" {
		t.Fatal("a foreign bare lane must not stop the run taking a token")
	}
	if !strings.Contains(stdout, "1 spawned") {
		t.Fatalf("the run must spawn its own lane:\n%s", stdout)
	}
}

// A legacy run is live under its bare ids, and its state remembers the pane.
// up keeps the bare id so the fleet is not renamed under it.
func TestEnsureTokenKeepsALegacyRunWithItsPane(t *testing.T) {
	repo := gitRepo(t)
	fakeHerdr(t, "w1:idle\n")
	planPath, statePath, _ := encounter(t, repo, planFor(repo))
	st := State{Encounter: "enc", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	st.PinPane("w1", "w9:p2") // the fake reports every agent at w9:p2
	if err := SaveState(statePath, st); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("up", "--no-dispatch", "--plan", planPath)
	if code != 0 {
		t.Fatalf("up exit %d: %s", code, stderr)
	}
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Token != "" {
		t.Fatalf("a legacy run must keep the bare id, got token %q", st.Token)
	}
	if !strings.Contains(stdout, "0 spawned, 1 already live") {
		t.Fatalf("the legacy lane should be adopted:\n%s", stdout)
	}
}
