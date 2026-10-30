package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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

func writeReport(t *testing.T, body string) (plan, state string) {
	t.Helper()
	plan = writePlan(t, statusPlan)
	dir := t.TempDir()
	state = filepath.Join(dir, "state.json")
	report := filepath.Join(dir, "reports", "A.md")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Encounter: "st", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	st.Attach("A", report)
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}
	return plan, state
}

func TestReaderSearchJumpsAndCycles(t *testing.T) {
	plan, state := writeReport(t, "# Report\n\nalpha one\n\nbeta\n\nalpha two\n")
	m := newTestView(t, plan, state)
	m = press(t, m, "d", "enter")
	m.height = 10 // a short viewport, so a jump has to move the top

	m = press(t, m, "/", "A", "L", "P", "H", "A")
	if len(m.matches) != 2 {
		t.Fatalf("want 2 matches, got %v in %q", m.matches, m.readerDisplay())
	}
	if m.matchIdx != 0 || m.readerTop != m.matches[0] {
		t.Fatalf("the search should sit on the first match, idx %d top %d matches %v", m.matchIdx, m.readerTop, m.matches)
	}
	if out := m.View(); !strings.Contains(out, "/ALPHA▏  1/2") {
		t.Fatalf("the footer should count the matches:\n%s", out)
	}

	m = press(t, m, "enter")
	if m.searching {
		t.Fatal("enter should keep the search and close the prompt")
	}
	m = press(t, m, "n")
	if m.matchIdx != 1 || m.readerTop != m.matches[1] {
		t.Fatalf("n should move to the second match, idx %d top %d", m.matchIdx, m.readerTop)
	}
	m = press(t, m, "n")
	if m.matchIdx != 0 {
		t.Fatalf("n should wrap, idx %d", m.matchIdx)
	}
	m = press(t, m, "N")
	if m.matchIdx != 1 {
		t.Fatalf("N should wrap backwards, idx %d", m.matchIdx)
	}
}

func TestReaderSearchEscRestoresTheTop(t *testing.T) {
	plan, state := writeReport(t, "# Report\n\none\n\ntwo\n\nthree\n")
	m := newTestView(t, plan, state)
	m = press(t, m, "d", "enter")
	m.height = 10

	m = press(t, m, "/", "t", "h", "r")
	top := m.readerTop
	m = press(t, m, "esc")
	if m.searching || m.query != "" || len(m.matches) != 0 {
		t.Fatalf("esc should drop the search: %+v", m)
	}
	if m.readerTop != 0 {
		t.Fatalf("esc should restore the top, want 0 got %d (search had moved to %d)", m.readerTop, top)
	}
	if out := m.View(); !strings.Contains(out, "j/k scroll") {
		t.Fatalf("the footer should return to the plain hint:\n%s", out)
	}
}

// forceColor pins the colour profile so a rendered style always carries its
// escape codes, whatever terminal the tests run in.
func forceColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func TestHighlightANSIIgnoresEscapeSequences(t *testing.T) {
	forceColor(t)
	line := "\x1b[31mred\x1b[0m"
	if got := highlightANSI(line, "31", matchStyle); got != line {
		t.Fatalf("an escape parameter must not be a hit: %q", got)
	}
	got := highlightANSI(line, "RED", matchStyle)
	if !strings.Contains(got, matchStyle.Render("red")) {
		t.Fatalf("the visible text should be painted case-insensitively: %q", got)
	}
}

func TestReaderSearchPaintsTheMatches(t *testing.T) {
	forceColor(t)
	plan, state := writeReport(t, "# Report\n\nalpha one\n\nbeta\n\nalpha two\n")
	m := newTestView(t, plan, state)
	m = press(t, m, "d", "enter")
	m.height = 20

	m = press(t, m, "/", "a", "l", "p", "h", "a")
	out := m.View()
	want := currentMatchStyle.Render("alpha")
	other := matchStyle.Render("alpha")
	if strings.Index(out, want) > strings.Index(out, other) {
		t.Fatalf("the first hit should be the current one:\n%s", out)
	}

	m = press(t, m, "enter", "n")
	out = m.View()
	if strings.Index(out, other) > strings.Index(out, want) {
		t.Fatalf("n should make the second hit current:\n%s", out)
	}
}

func TestReaderSearchEscUnpaints(t *testing.T) {
	forceColor(t)
	plan, state := writeReport(t, "# Report\n\nalpha one\n")
	m := newTestView(t, plan, state)
	m = press(t, m, "d", "enter")
	m.height = 20

	m = press(t, m, "/", "a", "l", "p", "h", "a", "esc")
	if out := m.View(); strings.Contains(out, matchStyle.Render("alpha")) || strings.Contains(out, currentMatchStyle.Render("alpha")) {
		t.Fatalf("esc should drop the paint:\n%s", out)
	}
}
