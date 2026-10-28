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

	for _, want := range []string{"dungeon", "st", "READY 1", "WAITING 2", "space actions", "deps 0/0"} {
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

	m = press(t, m, " ", "s")
	if got := m.selectedID(); got != want {
		t.Fatalf("after space s the cursor is on %q, want %q", got, want)
	}
	m = press(t, m, " ", "t")
	if got := m.selectedID(); got != want {
		t.Fatalf("after space t the cursor is on %q, want %q", got, want)
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
	if !strings.Contains(stdout, "dungeon") || !strings.Contains(stdout, "space actions") {
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
	if !strings.Contains(stderr, "no usable plan") {
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
	if !strings.Contains(m.View(), "⛧ A") {
		t.Fatalf("a branch should be marked expandable:\n%s", m.View())
	}

	m = press(t, m, " ", " ")
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
	if !strings.Contains(folded, "⛧ A") {
		t.Fatalf("a folded row should keep the fold mark:\n%s", folded)
	}
	if !strings.Contains(folded, "1 folded") {
		t.Fatalf("the header should count folds:\n%s", folded)
	}

	m = press(t, m, " ", " ")
	if len(m.order) != 3 {
		t.Fatalf("unfolding should restore the branch, got %v", m.order)
	}
}

func TestCollapseAllAndExpandAll(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	m = press(t, m, " ", "c")
	if len(m.order) != 1 {
		t.Fatalf("collapse all should leave only roots, got %v", m.order)
	}
	m = press(t, m, " ", "e")
	if len(m.order) != 3 {
		t.Fatalf("expand all should restore every unit, got %v", m.order)
	}
}

func TestHelpExplainsTheView(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	if strings.Contains(m.View(), "STATUS") {
		t.Fatal("the legend should be hidden by default")
	}
	m = press(t, m, " ", "?")
	out := m.View()
	for _, want := range []string{"STATUS", "bold", "enter / d  detail", "∧N"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	m = press(t, m, " ", "?")
	if strings.Contains(m.View(), "STATUS") {
		t.Fatal("? should close the legend")
	}
}

func TestPrefixShowsTheActionPaneAndCancels(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	m = press(t, m, " ")
	out := m.View()
	for _, want := range []string{"PREFIX", "fold selected", "focus lane", "open link", "collapse all", "quit", "esc cancels"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prefix should show %q:\n%s", want, out)
		}
	}
	if !m.prefix {
		t.Fatal("space should open the prefix")
	}

	m = press(t, m, "esc")
	if m.prefix {
		t.Fatal("esc should cancel the prefix")
	}
	if strings.Contains(m.View(), "focus lane") {
		t.Fatal("the pane should close with the prefix")
	}
}

func TestPrefixKeysDoNotActWithoutThePrefix(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	m = press(t, m, "s")
	if m.mode != "tree" {
		t.Fatalf("s alone changed the mode to %q", m.mode)
	}
	m = press(t, m, " ", "s")
	if m.mode != "sections" {
		t.Fatalf("space s should switch to sections, mode %q", m.mode)
	}

	m = press(t, m, " ", "z")
	if !strings.Contains(m.View(), "no action on z") {
		t.Fatalf("an unknown prefix key should say so:\n%s", m.View())
	}
	if m.prefix {
		t.Fatal("an unknown key should close the prefix")
	}

	m = press(t, m, " ", " ")
	if len(m.folded) != 1 {
		t.Fatalf("space space should fold the selected branch, folded=%v", m.folded)
	}
}

func TestEscapeNeverQuitsAndClosesWhatIsOpen(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)

	// In the default view esc does nothing and returns no command.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatal("esc must not quit the app")
	}
	m = next.(viewModel)

	// The legend closes on esc.
	m = press(t, m, " ", "?")
	if !m.help {
		t.Fatal("help should be open")
	}
	m = press(t, m, "esc")
	if m.help {
		t.Fatal("esc should close the legend")
	}

	// The detail pane closes on esc, and a second esc is harmless.
	m = press(t, m, "d")
	if m.overlay != "detail" {
		t.Fatalf("overlay %q, want detail", m.overlay)
	}
	m = press(t, m, "esc")
	if m.overlay != "" {
		t.Fatalf("overlay %q, want closed", m.overlay)
	}
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatal("a second esc must not quit the app")
	}
	_ = next
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
	for _, want := range []string{"FILES", "reports/A.md", "j/k file"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// j/k moves the file list, not the unit cursor behind the pane.
	selected := m.selectedID()
	m = press(t, m, "j")
	if m.artIdx != 1 {
		t.Fatalf("j moved the file cursor to %d, want 1", m.artIdx)
	}
	if m.selectedID() != selected {
		t.Fatalf("j moved the unit cursor to %q, want %q", m.selectedID(), selected)
	}
	m = press(t, m, "k")
	if m.artIdx != 0 {
		t.Fatalf("k moved the file cursor to %d, want 0", m.artIdx)
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

func TestCardCollapsesMultilineGoalAndNote(t *testing.T) {
	plan := writePlan(t, strings.Replace(statusPlan, "    goal: first\n",
		"    goal: |\n      first line\n        second line\n", 1))
	state := filepath.Join(t.TempDir(), "state.json")
	st, _ := LoadState(state)
	if err := st.SetUnit("A", "blocked", "needs you\n\n  pick a name"); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(state, st); err != nil {
		t.Fatal(err)
	}
	m := newTestView(t, plan, state)
	m.cursor = 0
	m.rebuildOrder("")

	box := m.detailBox(100, 8)
	if n := len(strings.Split(box, "\n")); n != 8 {
		t.Fatalf("card is %d lines, want exactly 8:\n%s", n, box)
	}
	if !strings.Contains(box, "first line second line") {
		t.Fatalf("a multi-line goal should collapse to one line:\n%s", box)
	}
	if !strings.Contains(box, "note: needs you pick a name") {
		t.Fatalf("a multi-line note should collapse to one line:\n%s", box)
	}
}

// The list is a full-width table: the status column ends one column inside the
// frame at every width, and the left column absorbs the slack.
func TestRowsFillTheFrameAndPinTheStatus(t *testing.T) {
	short := writePlan(t, statusPlan)
	long := writePlan(t, strings.Replace(statusPlan,
		"goal: first", "goal: "+strings.Repeat("a long goal ", 30), 1))
	loop := writePlan(t, loopForestPlan)

	for _, tc := range []struct {
		name  string
		plan  string
		width int
	}{
		{"short goals at 80", short, 80},
		{"short goals at 120", short, 120},
		{"short goals at 200", short, 200},
		{"long goals at 80", long, 80},
		{"long goals at 120", long, 120},
		{"long goals at 200", long, 200},
		{"loop plan at 80", loop, 80},
		{"loop plan at 120", loop, 120},
		{"loop plan at 200", loop, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state.json")
			m := newTestView(t, tc.plan, state)
			m.width, m.height = tc.width, 40

			lines, _ := m.listLines(tc.width)
			if len(lines) == 0 {
				t.Fatal("no rows")
			}
			for i, line := range lines {
				if strings.TrimSpace(line) == "" {
					continue
				}
				if got := runeLen(strings.TrimRight(line, " ")); got != tc.width-1 {
					t.Fatalf("row %d ends at %d, want %d: %q",
						i, got, tc.width-1, line)
				}
			}
		})
	}
}

// A goal wider than the left column is ellipsised there; it never moves or
// truncates the status at the right edge.
func TestLongGoalTruncatesInsteadOfPushingTheStatus(t *testing.T) {
	const goal = "a long goal "
	plan := writePlan(t, strings.Replace(statusPlan, "goal: first", "goal: "+strings.Repeat(goal, 30), 1))
	state := filepath.Join(t.TempDir(), "state.json")
	m := newTestView(t, plan, state)
	m.width, m.height = 80, 40

	lines, _ := m.listLines(80)
	var found bool
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " ")
		if !strings.HasSuffix(trimmed, "ready") {
			continue
		}
		found = true
		if !strings.Contains(line, "…") {
			t.Fatalf("the long goal should be ellipsised: %q", line)
		}
		if strings.Contains(line, strings.Repeat(goal, 30)) {
			t.Fatalf("the goal pushed the status out: %q", line)
		}
	}
	if !found {
		t.Fatalf("no row ends with the intact status:\n%s", strings.Join(lines, "\n"))
	}
}

func TestEnterOpensTheDetailPane(t *testing.T) {
	plan := writePlan(t, statusPlan)
	m := newTestView(t, plan, filepath.Join(t.TempDir(), "state.json"))

	m = press(t, m, "enter")
	if m.overlay != "detail" {
		t.Fatalf("enter should open the detail pane, overlay %q", m.overlay)
	}
	// Folding stays on the prefix and never fires on a bare enter.
	m = press(t, m, "esc")
	if len(m.folded) != 0 {
		t.Fatalf("enter must not fold, folded=%v", m.folded)
	}
}

func TestFailedHasAStyle(t *testing.T) {
	if _, ok := statusStyles["failed"]; !ok {
		t.Fatal("the view needs a colour for a failed unit")
	}
	var found bool
	for _, sec := range sections {
		if sec.status == "failed" {
			found = true
		}
	}
	if !found {
		t.Fatal("the view draws sections from status.go, which must list failed")
	}
}

// loopViewWith builds the view against a snapshot made in memory, so a test
// controls the state and the live panes without touching herdr.
func loopViewWith(t *testing.T, set map[string]string, agents map[string]Agent) viewModel {
	t.Helper()
	st := State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	for id, status := range set {
		if err := st.SetUnit(id, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	return loopViewFromState(t, st, agents)
}

func loopViewFromState(t *testing.T, st State, agents map[string]Agent) viewModel {
	t.Helper()
	p := decodeOK(t, loopForestPlan)
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("plan is not valid: %v", f)
	}
	e, f := ExpandLoops(p)
	if len(f) > 0 {
		t.Fatalf("expand: %v", f)
	}
	m := newViewModel("plan.yaml", "state.json", time.Second)
	m.plan, m.state = e, st
	m.snap = buildSnapshot(e, "plan.yaml", st, agents)
	m.width, m.height = 120, 40
	m.rebuildOrder("")
	return m
}

func emptyLoopState() State {
	return State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
}

func selectID(t *testing.T, m *viewModel, id string) {
	t.Helper()
	for i, got := range m.order {
		if got == id {
			m.cursor = i
			return
		}
	}
	t.Fatalf("%q is not in the order %v", id, m.order)
}

func loopLine(t *testing.T, m viewModel, id string) string {
	t.Helper()
	lines, _ := m.treeLines(m.width)
	for _, line := range lines {
		if strings.Contains(line, id) {
			return line
		}
	}
	t.Fatalf("no line for %q in:\n%s", id, strings.Join(lines, "\n"))
	return ""
}

func TestLoopSummaryRow(t *testing.T) {
	m := loopViewWith(t,
		map[string]string{"A": "done", "polish-review-1": "working"},
		map[string]Agent{"r1": {Name: "r1", Pane: "w9:p3", Status: "working"}},
	)
	lines := strings.Join(mustTreeLines(t, m), "\n")

	for _, want := range []string{
		"⛧ polish", "⟳ 1/2 correctness", "working w9:p3",
		"review-1", "review, round 1", "fix-1", "fix, round 1",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %q in:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "polish-review-1") {
		t.Fatalf("a child row should drop the loop prefix:\n%s", lines)
	}
	if strings.Contains(lines, "Review round 1 of loop polish") {
		t.Fatalf("a child row should show the short label, not the long goal:\n%s", lines)
	}
}

func mustTreeLines(t *testing.T, m viewModel) []string {
	t.Helper()
	lines, _ := m.treeLines(m.width)
	return lines
}

// The child row is short, and the card still holds the full generated goal.
func TestLoopChildCardKeepsTheFullGoal(t *testing.T) {
	m := loopViewWith(t, map[string]string{"A": "done", "polish-review-1": "working"}, nil)
	selectID(t, &m, "polish-review-1")
	card := m.detailBox(120, 7)
	if !strings.Contains(card, "Review round 1 of loop polish") {
		t.Fatalf("the card should show the full goal:\n%s", card)
	}
	if !strings.Contains(card, "polish-review-1") {
		t.Fatalf("the card should name the real unit id:\n%s", card)
	}
}

// The status column reads the loop's aggregate state, not one unit's state.
func TestLoopAggregateStateInTheStatusColumn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   map[string]string
		agent map[string]Agent
		want  string
	}{
		{
			name: "working wins",
			set:  map[string]string{"A": "done", "polish-review-1": "working"},
			agent: map[string]Agent{
				"r1": {Name: "r1", Pane: "w9:p3", Status: "working"},
			},
			want: "working w9:p3",
		},
		{
			name: "a failed review names the loop",
			set:  map[string]string{"A": "done", "polish-review-1": "failed"},
			want: "failed",
		},
		{
			name: "a ready review makes the loop ready",
			set:  map[string]string{"A": "done"},
			want: "ready",
		},
		{
			name: "an unsatisfied entry dep keeps it waiting",
			set:  map[string]string{},
			want: "waiting",
		},
		{
			name: "a passed review makes it done",
			set: map[string]string{
				"A": "done", "polish-review-1": "done", "polish-fix-1": "canceled",
				"polish-review-2": "canceled", "polish-fix-2": "canceled",
				"polish-review-3": "canceled",
			},
			want: "done",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := loopViewWith(t, tc.set, tc.agent)
			l := m.snapLoop("polish")
			if l == nil {
				t.Fatal("no loop row in the snapshot")
			}
			c := m.rowParts(m.loopDisplay(*l), "", "", "")
			if c.right != tc.want {
				t.Fatalf("status column %q, want %q", c.right, tc.want)
			}
		})
	}
}

// Fold rules: unstarted folds, started unfolds, an explicit choice wins, and a
// loop that starts while the view is open unfolds unless the operator folded it.
func TestLoopFoldRules(t *testing.T) {
	t.Run("an unstarted loop is folded", func(t *testing.T) {
		m := loopViewWith(t, map[string]string{}, nil)
		if got := strings.Join(m.order, ","); got != "A,polish" {
			t.Fatalf("order %q, want A,polish", got)
		}
		if !strings.Contains(mustTreeLines(t, m)[1], "[+5]") {
			t.Fatalf("a folded loop should say how much it hides:\n%s", strings.Join(mustTreeLines(t, m), "\n"))
		}
	})

	t.Run("a started loop unfolds", func(t *testing.T) {
		m := loopViewWith(t, map[string]string{"A": "done", "polish-review-1": "working"}, nil)
		if got := strings.Join(m.order, ","); got != "A,polish,polish-review-1,polish-fix-1,polish-review-2,polish-fix-2,polish-review-3" {
			t.Fatalf("order %q", got)
		}
	})

	t.Run("an explicit fold wins", func(t *testing.T) {
		m := loopViewWith(t, map[string]string{"A": "done", "polish-review-1": "working"}, nil)
		selectID(t, &m, "polish")
		m = press(t, m, " ", " ")
		if got := strings.Join(m.order, ","); got != "A,polish" {
			t.Fatalf("explicit fold left %q", got)
		}
	})

	t.Run("an explicit unfold wins", func(t *testing.T) {
		m := loopViewWith(t, map[string]string{}, nil)
		selectID(t, &m, "polish")
		m = press(t, m, " ", " ")
		if len(m.order) != 7 {
			t.Fatalf("explicit unfold left %v", m.order)
		}
	})

	t.Run("expand all beats the auto fold", func(t *testing.T) {
		m := loopViewWith(t, map[string]string{}, nil)
		m = press(t, m, " ", "e")
		if len(m.order) != 7 {
			t.Fatalf("expand all left %v", m.order)
		}
	})
}

func saveLoopState(t *testing.T, path string, set map[string]string) {
	t.Helper()
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Encounter == "" {
		st.Encounter = "lf"
	}
	for id, status := range set {
		if err := st.SetUnit(id, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveState(path, st); err != nil {
		t.Fatal(err)
	}
}

// An explicit fold and an explicit unfold both survive a refresh. A loop that
// starts with no operator choice unfolds by itself.
func TestLoopFoldsSurviveRefresh(t *testing.T) {
	plan := writePlan(t, loopForestPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(state, State{Encounter: "lf", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}); err != nil {
		t.Fatal(err)
	}
	m := newTestView(t, plan, state)
	m.width, m.height = 120, 40
	if got := strings.Join(m.order, ","); got != "A,polish" {
		t.Fatalf("fresh loop order %q", got)
	}

	// Starting while the view is open unfolds the loop by itself.
	saveLoopState(t, state, map[string]string{"A": "done", "polish-review-1": "working"})
	m = m.applyRefresh(loadSnapshot(plan, state))
	if len(m.order) != 7 {
		t.Fatalf("a started loop should unfold on refresh, order %v", m.order)
	}

	// An explicit fold survives both the refresh and the start.
	selectID(t, &m, "polish")
	m = press(t, m, " ", " ")
	if got := strings.Join(m.order, ","); got != "A,polish" {
		t.Fatalf("explicit fold left %q", got)
	}
	saveLoopState(t, state, map[string]string{"polish-review-1": "done", "polish-review-2": "working"})
	m = m.applyRefresh(loadSnapshot(plan, state))
	if got := strings.Join(m.order, ","); got != "A,polish" {
		t.Fatalf("a refresh lost the operator fold: %q", got)
	}

	// An explicit unfold on an unstarted loop also survives the refresh.
	other := writePlan(t, loopForestPlan)
	otherState := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(otherState, State{Encounter: "lf", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}); err != nil {
		t.Fatal(err)
	}
	n := newTestView(t, other, otherState)
	n.width, n.height = 120, 40
	selectID(t, &n, "polish")
	n = press(t, n, " ", " ")
	if len(n.order) != 7 {
		t.Fatalf("explicit unfold left %v", n.order)
	}
	n = n.applyRefresh(loadSnapshot(other, otherState))
	if len(n.order) != 7 {
		t.Fatalf("a refresh lost the operator unfold: %v", n.order)
	}
}

func openLoopTimeline(t *testing.T, m viewModel) viewModel {
	t.Helper()
	selectID(t, &m, "polish")
	return press(t, m, "enter")
}

func TestLoopTimelineOpensOnTheSummaryRow(t *testing.T) {
	for _, key := range []string{"d", "enter"} {
		t.Run(key, func(t *testing.T) {
			m := loopViewWith(t, map[string]string{
				"A": "done", "polish-review-1": "working",
			}, nil)
			selectID(t, &m, "polish")
			m = press(t, m, key)
			if m.overlay != "loop" {
				t.Fatalf("%s opened overlay %q, want loop", key, m.overlay)
			}
			out := m.View()
			for _, want := range []string{
				"polish", "focus:", "correctness", "cap: 2",
				"current round: 1 of 2", "Merge verdict: PASS",
				"ROUNDS 3", "round 1", "round 2", "final",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in:\n%s", want, out)
				}
			}
		})
	}
}

func TestLoopTimelineShowsBothStatesAndTheFailureNote(t *testing.T) {
	st := emptyLoopState()
	if err := st.SetUnit("A", "done", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUnit("polish-review-1", "failed",
		`wanted verdict "Merge verdict: PASS", last Merge verdict: Merge verdict: FAIL`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUnit("polish-fix-1", "working", ""); err != nil {
		t.Fatal(err)
	}
	m := loopViewFromState(t, st, nil)
	m = openLoopTimeline(t, m)
	out := m.View()

	if !strings.Contains(out, "review failed") {
		t.Fatalf("a round row should name the review state:\n%s", out)
	}
	if !strings.Contains(out, "fix working") {
		t.Fatalf("a round row should name the fix state:\n%s", out)
	}
	if !strings.Contains(out, "Merge verdict: FAIL") {
		t.Fatalf("a failed verdict should name its note:\n%s", out)
	}
}

func TestLoopTimelineMovesBetweenRounds(t *testing.T) {
	m := loopViewWith(t, map[string]string{"A": "done", "polish-review-1": "working"}, nil)
	m = openLoopTimeline(t, m)
	if m.roundIdx != 0 {
		t.Fatalf("timeline opened on round index %d, want 0", m.roundIdx)
	}
	m = press(t, m, "j")
	if m.roundIdx != 1 {
		t.Fatalf("j moved to %d, want 1", m.roundIdx)
	}
	m = press(t, m, "j")
	if m.roundIdx != 2 {
		t.Fatalf("j moved to %d, want 2", m.roundIdx)
	}
	m = press(t, m, "j")
	if m.roundIdx != 0 {
		t.Fatalf("j should wrap to 0, got %d", m.roundIdx)
	}
	m = press(t, m, "k")
	if m.roundIdx != 2 {
		t.Fatalf("k should wrap to 2, got %d", m.roundIdx)
	}
	m = press(t, m, "g")
	if m.roundIdx != 0 {
		t.Fatalf("g moved to %d, want 0", m.roundIdx)
	}
	m = press(t, m, "G")
	if m.roundIdx != 2 {
		t.Fatalf("G moved to %d, want 2", m.roundIdx)
	}
}

func TestLoopRoundCardOpensAndEscGoesBack(t *testing.T) {
	m := loopViewWith(t, map[string]string{"A": "done", "polish-review-1": "working"}, nil)
	m = openLoopTimeline(t, m)
	m = press(t, m, "enter")
	if m.overlay != "detail" {
		t.Fatalf("enter opened overlay %q, want detail", m.overlay)
	}
	if m.cardID != "polish-review-1" {
		t.Fatalf("card unit %q, want polish-review-1", m.cardID)
	}
	out := m.View()
	if !strings.Contains(out, "polish-review-1") || !strings.Contains(out, "Review round 1 of loop polish") {
		t.Fatalf("the round card should show the full unit:\n%s", out)
	}

	m = press(t, m, "esc")
	if m.overlay != "loop" || m.cardID != "" {
		t.Fatalf("esc left overlay %q card %q, want back on the timeline", m.overlay, m.cardID)
	}
	m = press(t, m, "esc")
	if m.overlay != "" {
		t.Fatalf("a second esc should close the timeline, overlay %q", m.overlay)
	}
}

func TestLoopRoundCardPicksTheLiveUnit(t *testing.T) {
	m := loopViewWith(t, map[string]string{
		"A": "done", "polish-review-1": "done", "polish-fix-1": "unproven",
		"polish-review-2": "done", "polish-fix-2": "working",
	}, nil)
	m = openLoopTimeline(t, m) // the timeline opens on the current round, 2
	m = press(t, m, "enter")
	if m.cardID != "polish-fix-2" {
		t.Fatalf("a round with a working fix should open it, got %q", m.cardID)
	}
}

func TestLoopTimelineFocusTargetsTheSelectedRound(t *testing.T) {
	m := loopViewWith(t, map[string]string{
		"A": "done", "polish-review-1": "done", "polish-fix-1": "unproven",
		"polish-review-2": "working",
	}, map[string]Agent{"r1": {Name: "r1", Pane: "w9:p3", Status: "working"}})
	m = openLoopTimeline(t, m) // the timeline opens on the current round, 2
	u, ok := m.focusTarget()
	if !ok || u.ID != "polish-review-2" || u.Minion != "r1" || u.Pane != "w9:p3" {
		t.Fatalf("focus target %+v, want the live review of round 2", u)
	}

	// A working fix in the same round wins over the review.
	m = loopViewWith(t, map[string]string{
		"A": "done", "polish-review-1": "done", "polish-fix-1": "unproven",
		"polish-review-2": "done", "polish-fix-2": "working",
	}, map[string]Agent{"w1": {Name: "w1", Pane: "w9:p4", Status: "working"}})
	m = openLoopTimeline(t, m)
	u, ok = m.focusTarget()
	if !ok || u.ID != "polish-fix-2" || u.Minion != "w1" {
		t.Fatalf("focus target %+v, want the live fix of round 2", u)
	}
}

func TestLoopTimelineIsEmptyBeforeExpansion(t *testing.T) {
	p := decodeOK(t, loopForestPlan)
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("plan is not valid: %v", f)
	}
	st := emptyLoopState()
	m := newViewModel("plan.yaml", "state.json", time.Second)
	m.plan, m.state = p, st
	m.snap = buildSnapshot(p, "plan.yaml", st, nil)
	m.width, m.height = 120, 40
	m.rebuildOrder("")

	m = openLoopTimeline(t, m)
	if m.overlay != "loop" {
		t.Fatalf("overlay %q, want loop", m.overlay)
	}
	out := m.View()
	if !strings.Contains(out, "no rounds yet") {
		t.Fatalf("an unexpanded loop should show an empty timeline:\n%s", out)
	}
	// Keys on an empty timeline are harmless.
	m = press(t, m, "j", "k", "enter")
	if m.overlay != "loop" {
		t.Fatalf("enter on an empty timeline changed the overlay to %q", m.overlay)
	}
}

func TestViewDetailFlagOpensTheLoopTimeline(t *testing.T) {
	plan := writePlan(t, loopForestPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	code, stdout, stderr := runCLI("view", "--once", "--plan", plan, "--state", state, "--detail", "polish")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"polish  ", "current round:", "ROUNDS", "round 1"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("missing %q in:\n%s", want, stdout)
		}
	}
}

// The always-visible strip shows the loop when the cursor is on its summary
// row, not a blank unit card.
func TestDetailBoxShowsTheLoop(t *testing.T) {
	m := loopViewWith(t, map[string]string{"A": "done", "polish-review-1": "working"}, nil)
	selectID(t, &m, "polish")

	box := m.detailBox(120, 7)
	if n := len(strings.Split(box, "\n")); n != 7 {
		t.Fatalf("loop detail box is %d lines, want 7:\n%s", n, box)
	}
	for _, want := range []string{
		"polish", "working", "focus: correctness", "round 1/2",
		"now: review, round 1", "Review round 1 of loop polish",
		"d opens the round timeline",
	} {
		if !strings.Contains(box, want) {
			t.Fatalf("missing %q in the loop detail box:\n%s", want, box)
		}
	}

	// The same strip is what the first frame draws under the tree.
	if out := m.View(); !strings.Contains(out, "d opens the round timeline") {
		t.Fatalf("the first frame should show the loop strip:\n%s", out)
	}
}

// An unexpanded loop has no round to name, so the strip shows its focus and
// cap only.
func TestDetailBoxForAnUnexpandedLoop(t *testing.T) {
	p := decodeOK(t, loopForestPlan)
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("plan is not valid: %v", f)
	}
	st := emptyLoopState()
	m := newViewModel("plan.yaml", "state.json", time.Second)
	m.plan, m.state = p, st
	m.snap = buildSnapshot(p, "plan.yaml", st, nil)
	m.width, m.height = 120, 40
	m.rebuildOrder("")
	selectID(t, &m, "polish")

	box := m.detailBox(120, 7)
	for _, want := range []string{"focus: correctness", "cap: 2", "d opens the round timeline"} {
		if !strings.Contains(box, want) {
			t.Fatalf("missing %q in the loop detail box:\n%s", want, box)
		}
	}
	for _, unwanted := range []string{"round 1/2", "now:"} {
		if strings.Contains(box, unwanted) {
			t.Fatalf("an unexpanded loop should not show %q:\n%s", unwanted, box)
		}
	}
}

const authCheckPlan = `encounter: ap
minions:
  - {id: w1, role: worker, worktree: wt1}
  - {id: r1, role: reviewer, of: w1}
units:
  - {id: auth-check, minion: w1, paths: [a/**], goal: holds the check}
  - {id: B, minion: w1, paths: [b/**], deps: [auth-check], goal: waits}
loops:
  - {id: auth, reviewer: r1, fixer: w1, rounds: 1, focus: correctness,
     pass: "Merge verdict: PASS", paths: [a/**]}
`

// Only a generated unit loses its loop prefix. A declared unit that starts
// with a loop id keeps its full id, including in a waits label.
func TestDisplayUnitIDOnlyStripsGeneratedIDs(t *testing.T) {
	p := decodeOK(t, authCheckPlan)
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("plan is not valid: %v", f)
	}
	e, f := ExpandLoops(p)
	if len(f) > 0 {
		t.Fatalf("expand: %v", f)
	}
	st := emptyLoopState()
	m := newViewModel("plan.yaml", "state.json", time.Second)
	m.plan, m.state = e, st
	m.snap = buildSnapshot(e, "plan.yaml", st, nil)
	m.width, m.height = 120, 40
	m.rebuildOrder("")

	if got := displayUnitID(m.snap, "auth-check"); got != "auth-check" {
		t.Fatalf("a declared unit became %q, want auth-check", got)
	}
	if got := displayUnitID(m.snap, "auth-review-1"); got != "review-1" {
		t.Fatalf("a generated unit became %q, want review-1", got)
	}

	lines := strings.Join(mustTreeLines(t, m), "\n")
	if !strings.Contains(lines, "waits auth-check") {
		t.Fatalf("a declared dep should keep its full id:\n%s", lines)
	}
	if strings.Contains(lines, "waits check") {
		t.Fatalf("a declared dep lost its loop prefix:\n%s", lines)
	}
	if strings.Contains(lines, "auth-review-1") {
		t.Fatalf("a generated unit should show its stripped id:\n%s", lines)
	}
}
