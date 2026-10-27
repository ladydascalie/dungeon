package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReaderRendersMarkdown(t *testing.T) {
	plan := writePlan(t, statusPlan)
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	report := filepath.Join(dir, "reports", "A.md")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Report\n\nWhat changed:\n\n- created NOTES.md\n- ran the build\n\n```sh\nmake test\n```\n"
	if err := os.WriteFile(report, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Encounter: "st", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	st.Attach("A", report)
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}

	m := newTestView(t, plan, state)
	m = press(t, m, "d")
	m = press(t, m, "enter")

	read := m.View()
	for _, want := range []string{"Report", "What changed", "created NOTES.md", "make test"} {
		if !strings.Contains(read, want) {
			t.Fatalf("reader should show %q:\n%s", want, read)
		}
	}
	// A bullet proves the markdown path ran: the source has "- created", the
	// render has a bullet glyph. Headings keep their marker under glamour's
	// no-colour style, which is what a piped frame gets, so it is not asserted.
	if !strings.Contains(read, "• created NOTES.md") {
		t.Fatalf("the list should be rendered, not shown raw:\n%s", read)
	}
	if !strings.Contains(read, "A.md") {
		t.Fatalf("the title should name the file:\n%s", read)
	}
}

func TestReaderLeavesPlainTextAlone(t *testing.T) {
	plan := writePlan(t, statusPlan)
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	log := filepath.Join(dir, "build.log")
	if err := os.WriteFile(log, []byte("# not a heading\nplain line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Encounter: "st", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	st.Attach("A", log)
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}

	m := newTestView(t, plan, state)
	m = press(t, m, "d")
	m = press(t, m, "enter")
	if out := m.View(); !strings.Contains(out, "# not a heading") {
		t.Fatalf("a .log must not be rendered as markdown:\n%s", out)
	}
}
