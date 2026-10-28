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

func TestColumnsAreContentSizedWithASmallGap(t *testing.T) {
	short := writePlan(t, statusPlan)
	long := writePlan(t, strings.Replace(statusPlan,
		"goal: first", "goal: "+strings.Repeat("a long goal ", 30), 1))

	for _, tc := range []struct {
		name  string
		plan  string
		width int
		// narrow is true when the content should not reach the frame edge.
		narrow bool
	}{
		{"short goals, wide frame", short, 284, true},
		{"long goals, wide frame", long, 284, false},
		{"long goals, narrow frame", long, 120, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state.json")
			m := newTestView(t, tc.plan, state)
			m.width, m.height = tc.width, 40

			lines, _ := m.listLines(tc.width)
			var widths []int
			for _, line := range lines {
				if strings.Contains(line, "ready") || strings.Contains(line, "waits") {
					widths = append(widths, runeLen(strings.TrimRight(line, " ")))
				}
			}
			if len(widths) == 0 {
				t.Fatal("no rows")
			}
			// Every status ends at the same column: the two columns are a table.
			for _, w := range widths {
				if w != widths[0] {
					t.Fatalf("statuses are not aligned: %v", widths)
				}
			}
			if widths[0] > tc.width {
				t.Fatalf("block is %d wide, frame is %d", widths[0], tc.width)
			}
			if tc.narrow && widths[0] >= tc.width-1 {
				t.Fatalf("short content should not reach the edge: %d of %d", widths[0], tc.width)
			}
			if !tc.narrow && widths[0] != tc.width-1 {
				t.Fatalf("long content should fill to the edge: %d of %d", widths[0], tc.width)
			}
			// The gap between the two columns is the pad, so the row's text ends
			// two columns before the status column begins. Short rows get more
			// whitespace than that; the column is sized by the longest row.
			if !tc.narrow && widths[0] != tc.width-1 {
				t.Fatalf("long content should fill to the edge")
			}
		})
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
	p := decodeOK(t, loopForestPlan)
	if f := Validate(p); len(f) > 0 {
		t.Fatalf("plan is not valid: %v", f)
	}
	e, f := ExpandLoops(p)
	if len(f) > 0 {
		t.Fatalf("expand: %v", f)
	}
	st := State{Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	for id, status := range set {
		if err := st.SetUnit(id, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	m := newViewModel("plan.yaml", "state.json", time.Second)
	m.plan, m.state = e, st
	m.snap = buildSnapshot(e, "plan.yaml", st, agents)
	m.width, m.height = 120, 40
	m.rebuildOrder("")
	return m
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
