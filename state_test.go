package main

import (
	"os"
	"path/filepath"
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
