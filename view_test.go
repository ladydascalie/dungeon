package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func newTestView(t *testing.T, plan, state string) viewModel {
	t.Helper()
	m := newViewModel(plan, state, time.Second)
	m = m.applyRefresh(loadSnapshot(plan, state))
	m.width, m.height = 120, 30
	return m
}

func TestViewSectionsFrame(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(state, State{Encounter: "st", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}); err != nil {
		t.Fatal(err)
	}

	m := newTestView(t, plan, state)
	m.mode = "sections"
	m.rebuildOrder("")
	out := m.View()

	for _, want := range []string{"dungeon", "st", "READY 1", "WAITING 2", "q quit", "deps 0/0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestViewDetailBoxShowsTheSelectedUnit(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	st, _ := LoadState(state)
	if err := st.SetUnit("A", "blocked", "needs you: pick a name"); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}
	m := newTestView(t, plan, state)
	m.cursor = 0
	out := m.View()
	for _, want := range []string{"A  blocked", "needs you: pick a name", "worker w1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "╭") || !strings.Contains(out, "╰") {
		t.Fatalf("the detail strip should be boxed:\n%s", out)
	}
}

func TestViewRowShowsTheRightStatusColumn(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)
	out := m.View()
	if !strings.Contains(out, "waits A") {
		t.Fatalf("a waiting row should name the dep it waits for:\n%s", out)
	}
	if !strings.Contains(out, "ready") {
		t.Fatalf("a ready row should say ready:\n%s", out)
	}
}

func TestViewTreeFrame(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	out := m.View()
	if !strings.Contains(out, "tree ·") {
		t.Fatalf("the header should name the mode:\n%s", out)
	}
	if !strings.Contains(out, "└─") {
		t.Fatalf("a child should hang under its parent:\n%s", out)
	}
	if strings.Contains(out, "READY 1") {
		t.Fatalf("tree mode has no section headers:\n%s", out)
	}
	// The nested rows must still be reachable with the cursor.
	if len(m.order) != len(m.snap.Units) {
		t.Fatalf("order has %d ids, want %d", len(m.order), len(m.snap.Units))
	}
}

func TestViewModeToggleKeepsTheCursor(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)
	m.cursor = 1
	want := m.selectedID()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(viewModel)
	if got := m.selectedID(); got != want {
		t.Fatalf("after s the cursor is on %q, want %q", got, want)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = next.(viewModel)
	if got := m.selectedID(); got != want {
		t.Fatalf("after t the cursor is on %q, want %q", got, want)
	}
}

func TestViewCursorFollowsTheUnitAcrossRefresh(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(state, State{Encounter: "st", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}); err != nil {
		t.Fatal(err)
	}
	m := newTestView(t, plan, state)
	if m.cursor != 0 {
		t.Fatalf("cursor starts at %d", m.cursor)
	}

	// Move to the last unit, then let a unit ahead of it settle. The cursor must
	// stay on the same unit, not the same row.
	m.cursor = len(m.order) - 1
	want := m.selectedID()

	st, err := LoadState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetUnit("A", "working", ""); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}

	m = m.applyRefresh(loadSnapshot(plan, state))
	got, ok := m.selected()
	if !ok || got.ID != want {
		t.Fatalf("cursor landed on %q, want %q", got.ID, want)
	}
}

func TestViewClampsTheCursor(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	m.cursor = 99
	m.clampCursor()
	if m.cursor != len(m.order)-1 {
		t.Fatalf("cursor %d", m.cursor)
	}
	m.cursor = -5
	m.clampCursor()
	if m.cursor != 0 {
		t.Fatalf("cursor %d", m.cursor)
	}
}

func TestViewPrintsOneFrameWhenPiped(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	code, stdout, stderr := runCLI("view", "--plan", plan, "--state", state)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "dungeon") || !strings.Contains(stdout, "q quit") {
		t.Fatalf("output:\n%s", stdout)
	}
}

func TestViewRefusesABrokenPlan(t *testing.T) {
	broken := strings.Replace(statusPlan, "    paths: [a/**]\n", "", 1)
	plan := writePlan(t, broken)
	code, _, stderr := runCLI("view", "--once", "--plan", plan)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "does not validate") {
		t.Fatalf("stderr %q", stderr)
	}
}

func press(t *testing.T, m viewModel, keys ...string) viewModel {
	t.Helper()
	for _, k := range keys {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		if k == " " {
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
		}
		next, _ := m.Update(msg)
		m = next.(viewModel)
	}
	return m
}

func TestFoldHidesTheBranchAndSaysHowMuch(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)
	if len(m.order) != 3 {
		t.Fatalf("order %v", m.order)
	}

	// An expandable row says so before anything is folded.
	if !strings.Contains(m.View(), "▾○ A") {
		t.Fatalf("a branch should be marked expandable:\n%s", m.View())
	}

	m = press(t, m, " ")
	if len(m.order) != 1 || m.order[0] != "A" {
		t.Fatalf("folding the root should leave one row, got %v", m.order)
	}
	if got := m.selectedID(); got != "A" {
		t.Fatalf("cursor moved to %q", got)
	}
	folded := m.View()
	if !strings.Contains(folded, "[+2]") {
		t.Fatalf("a folded row should say how much it hides:\n%s", folded)
	}
	if !strings.Contains(folded, "▸○ A") {
		t.Fatalf("a folded row should be marked as folded:\n%s", folded)
	}
	if !strings.Contains(folded, "1 folded") {
		t.Fatalf("the header should count folds:\n%s", folded)
	}

	m = press(t, m, " ")
	if len(m.order) != 3 {
		t.Fatalf("unfolding should restore the branch, got %v", m.order)
	}
}

func TestCollapseAllAndExpandAll(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	m = press(t, m, "c")
	if len(m.order) != 1 {
		t.Fatalf("collapse all should leave only roots, got %v", m.order)
	}
	m = press(t, m, "e")
	if len(m.order) != 3 {
		t.Fatalf("expand all should restore every unit, got %v", m.order)
	}
}

func TestHelpExplainsTheGlyphs(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	if strings.Contains(m.View(), "STATUS") {
		t.Fatal("the legend should be hidden by default")
	}
	m = press(t, m, "?")
	out := m.View()
	for _, want := range []string{"STATUS", "done", "blocked", "space      fold", "∧N"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	m = press(t, m, "?")
	if strings.Contains(m.View(), "STATUS") {
		t.Fatal("? should close the legend")
	}
}

func TestDetailOverlayListsAndReadsFiles(t *testing.T) {
	plan := writePlan(t, statusPlan)
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	report := filepath.Join(dir, "reports", "A.md")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := State{Encounter: "st", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	if err := st.Attach("A", report); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}

	m := newTestView(t, plan, state)
	m = press(t, m, "d")
	out := m.View()
	for _, want := range []string{"FILES", "reports/A.md", "tab file"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// The attached file is first, so enter reads it.
	m = press(t, m, "enter")
	read := m.View()
	if !strings.Contains(read, "line one") || !strings.Contains(read, "A.md") {
		t.Fatalf("the reader should show the file:\n%s", read)
	}

	// esc goes back to the detail pane, not straight out.
	m = press(t, m, "esc")
	if m.overlay != "detail" {
		t.Fatalf("overlay %q, want detail", m.overlay)
	}
	m = press(t, m, "d")
	if m.overlay != "" {
		t.Fatalf("overlay %q, want closed", m.overlay)
	}
}

func TestAttachRefusesAFileThatIsNotThere(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	code, _, _ := runCLI("attach", "A", "/nope/nothing.md", "--plan", plan, "--state", state)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if code, _, _ := runCLI("attach", "nope", plan, "--plan", plan, "--state", state); code != 2 {
		t.Fatalf("unknown unit: exit %d, want 2", code)
	}
}

func TestFrameNeverExceedsTheTerminal(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	st, _ := LoadState(state)
	// A long goal and a long note are exactly what used to grow the card.
	long := strings.Repeat("a very long goal that must not wrap the card ", 6)
	if err := st.SetUnit("A", "blocked", long); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}
	m := newTestView(t, plan, state)
	if err := os.WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, size := range [][2]int{{100, 30}, {80, 24}, {60, 20}, {120, 40}, {40, 12}} {
		m.width, m.height = size[0], size[1]
		lines := strings.Split(m.View(), "\n")
		if len(lines) > m.height {
			t.Fatalf("%dx%d: frame has %d lines, terminal has %d", m.width, m.height, len(lines), m.height)
		}
		for _, line := range lines {
			if runeLen(line) > m.width {
				t.Fatalf("%dx%d: line is %d wide: %q", m.width, m.height, runeLen(line), line)
			}
		}
	}
}

func TestCardIsOneLinePerField(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	st, _ := LoadState(state)
	long := strings.Repeat("a long goal that would wrap ", 8)
	p, _ := Decode([]byte(strings.Replace(statusPlan, "goal: first", "goal: "+long, 1)))
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}
	m := newTestView(t, plan, state)
	m.plan, m.snap = p, buildSnapshot(p, plan, st, nil)
	m.rebuildOrder("")

	box := m.detailBox(100, 7)
	if n := len(strings.Split(box, "\n")); n != 7 {
		t.Fatalf("card is %d lines, want exactly 7:\n%s", n, box)
	}
	if !strings.Contains(box, "…") {
		t.Fatalf("a long goal should be ellipsised, not wrapped:\n%s", box)
	}
}
