package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The TUI is a renderer over the same Snapshot the plain table uses. It adds no
// truth: everything on screen comes from the plan, the state, and herdr.

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
	sectionStyle  = lipgloss.NewStyle().Bold(true).Faint(true)
	selectedStyle = lipgloss.NewStyle().Bold(true).Reverse(true)
	panelStyle    = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).PaddingLeft(1)
	ruleStyle     = lipgloss.NewStyle().Faint(true)

	statusStyles = map[string]lipgloss.Style{
		"blocked":  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203")),
		"working":  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42")),
		"ready":    lipgloss.NewStyle().Foreground(lipgloss.Color("220")),
		"waiting":  lipgloss.NewStyle().Faint(true),
		"unproven": lipgloss.NewStyle().Foreground(lipgloss.Color("213")),
		"rejected": lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		"done":     lipgloss.NewStyle().Foreground(lipgloss.Color("71")),
		"canceled": lipgloss.NewStyle().Faint(true),
	}

	statusGlyphs = map[string]string{
		"blocked":  "●",
		"working":  "◐",
		"ready":    "○",
		"waiting":  "·",
		"unproven": "?",
		"rejected": "✗",
		"done":     "✓",
		"canceled": "–",
	}
)

type tickMsg time.Time

type refreshedMsg struct {
	plan  Plan
	state State
	snap  Snapshot
	err   error
}

type focusMsg struct{ err error }

// focusCmd jumps the operator to the selected unit's lane. A live agent is
// exact; with none, the pane hint recorded at spawn is used.
func (m viewModel) focusCmd() tea.Cmd {
	u, ok := m.selected()
	if !ok {
		return nil
	}
	minion, hint := u.Minion, m.state.Minions[u.Minion].Pane
	return func() tea.Msg {
		return focusMsg{err: newHerdrClient().focus(context.Background(), minion, hint)}
	}
}

type viewModel struct {
	planPath  string
	statePath string
	interval  time.Duration

	plan  Plan
	state State
	snap  Snapshot
	err   error

	mode        string // "tree" or "sections"
	hideSettled bool
	help        bool

	notice         string // the last thing that went wrong, shown in the footer until the next key
	overlay        string // "", "detail" or "reader"
	artIdx         int
	reader         string
	readerLines    []string
	readerTop      int
	readerRendered []string // markdown, rendered once per width, never per frame
	readerWidth    int
	folded         map[string]bool
	order          []string // unit ids in display order
	cursor         int

	width  int
	height int
}

func newViewModel(planPath, statePath string, interval time.Duration) viewModel {
	return viewModel{
		planPath:  planPath,
		statePath: statePath,
		interval:  interval,
		mode:      "tree",
		folded:    map[string]bool{},
		width:     100,
		height:    30,
	}
}

func (m viewModel) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(m.interval))
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// loadSnapshot re-reads all three sources: the plan, the state, and herdr.
func loadSnapshot(planPath, statePath string) refreshedMsg {
	p, code := loadValidPlan(planPath, io.Discard)
	if code != 0 {
		return refreshedMsg{err: fmt.Errorf("no usable plan at %s: run dungeon init or dungeon check", planPath)}
	}
	st, err := LoadState(statePath)
	if err != nil {
		return refreshedMsg{err: err}
	}
	snap := buildSnapshot(p, planPath, st, probeAgents(context.Background()))
	return refreshedMsg{plan: p, state: st, snap: snap}
}

// refresh is a command so the UI never blocks on a file read or a herdr call.
func (m viewModel) refresh() tea.Cmd {
	planPath, statePath := m.planPath, m.statePath
	return func() tea.Msg { return loadSnapshot(planPath, statePath) }
}

func (m viewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.overlay == "reader" {
			m.renderReader()
		}
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.refresh(), tick(m.interval))

	case refreshedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		keep := m.selectedID()
		m.plan, m.state, m.snap = msg.plan, msg.state, msg.snap
		m.rebuildOrder(keep)
		return m, nil

	case focusMsg:
		if msg.err != nil {
			m.notice = "cannot focus: " + msg.err.Error()
		}
		return m, nil

	case tea.KeyMsg:
		m.notice = ""
		key := msg.String()
		if m.overlay != "" && m.handleOverlayKey(key) {
			return m, nil
		}
		switch key {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "o":
			u, ok := m.selected()
			if !ok || u.Link == "" {
				m.notice = "no link on this unit"
				return m, nil
			}
			if err := openURL(u.Link); err != nil {
				m.notice = "cannot open: " + err.Error()
			}
			return m, nil
		case "f":
			return m, m.focusCmd()
		case "r":
			return m, m.refresh()
		case "t":
			m.mode = "tree"
			m.rebuildOrder(m.selectedID())
		case "s":
			m.mode = "sections"
			m.rebuildOrder(m.selectedID())
		case "h":
			m.hideSettled = !m.hideSettled
			m.rebuildOrder(m.selectedID())
		case "?":
			m.help = !m.help
		case "d":
			m.overlay, m.artIdx = "detail", 0
		case " ", "enter":
			id := m.selectedID()
			if id != "" {
				if m.folded[id] {
					delete(m.folded, id)
				} else {
					m.folded[id] = true
				}
				m.rebuildOrder(id)
			}
		case "c":
			for _, id := range foldable(pruneForest(buildForest(m.snap), m.hidden)) {
				m.folded[id] = true
			}
			m.rebuildOrder(m.selectedID())
		case "e":
			m.folded = map[string]bool{}
			m.rebuildOrder(m.selectedID())
		case "j", "down":
			m.cursor++
		case "k", "up":
			m.cursor--
		case "g", "home":
			m.cursor = 0
		case "G", "end":
			m.cursor = len(m.order) - 1
		}
		m.clampCursor()
		return m, nil
	}
	return m, nil
}

// rebuildOrder recomputes the display list and keeps the cursor on the same
// unit, not the same row: a refresh reorders everything.
func (m *viewModel) rebuildOrder(keep string) {
	m.order = m.order[:0]
	if m.mode == "tree" {
		lines, _, _ := m.tree()
		for _, line := range lines {
			m.order = append(m.order, line.ID)
		}
	} else {
		for _, u := range m.snap.Units {
			if !m.hidden(u.ID) {
				m.order = append(m.order, u.ID)
			}
		}
	}
	m.cursor = 0
	for i, id := range m.order {
		if id == keep {
			m.cursor = i
		}
	}
	m.clampCursor()
}

// tree builds the display forest once: hide settled work, then fold, then walk.
// It also reports which units have children, so a row can show whether it folds.
func (m viewModel) tree() ([]treeLine, map[string]int, map[string]bool) {
	roots := pruneForest(buildForest(m.snap), m.hidden)
	branches := map[string]bool{}
	for _, id := range foldable(roots) {
		branches[id] = true
	}
	roots, counts := applyFolds(roots, m.folded)
	return flattenForest(roots), counts, branches
}

func (m viewModel) hidden(id string) bool {
	if !m.hideSettled {
		return false
	}
	st := m.rowByID(id).Status
	return st == "done" || st == "canceled"
}

func (m viewModel) rowByID(id string) UnitRow {
	for _, u := range m.snap.Units {
		if u.ID == id {
			return u
		}
	}
	return UnitRow{}
}

func (m *viewModel) clampCursor() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if n := len(m.order); n > 0 && m.cursor >= n {
		m.cursor = n - 1
	}
}

func (m viewModel) selectedID() string {
	if m.cursor < 0 || m.cursor >= len(m.order) {
		return ""
	}
	return m.order[m.cursor]
}

func (m viewModel) selected() (UnitRow, bool) {
	id := m.selectedID()
	if id == "" {
		return UnitRow{}, false
	}
	return m.rowByID(id), true
}

func (m viewModel) render() string {
	if m.err != nil {
		return titleStyle.Render("dungeon") + "\n\n" + m.err.Error() + "\n\n" + dimStyle.Render("press r to retry, q to quit") + "\n"
	}
	if m.help {
		return m.headerView() + "\n" + m.helpView() + "\n" + m.footerView()
	}
	switch m.overlay {
	case "detail":
		return m.headerView() + "\n" + m.detailOverlay() + "\n" + m.footerView()
	case "reader":
		return m.headerView() + "\n" + m.readerView() + "\n" + m.footerView()
	}
	return m.headerView() + "\n" + m.bodyView() + "\n" + m.footerView()
}

// helpView is the legend. The glyphs mean nothing without one, and a screen of
// coloured marks is not a substitute for naming them.
func (m viewModel) helpView() string {
	status := []string{
		"● blocked     needs you, or an input failed",
		"◐ working     a lane is on it",
		"○ ready       deps met, no lane on it",
		"· waiting     deps not met yet",
		"? unproven    finished, and nothing can prove it",
		"✗ rejected    sent back to the lane",
		"✓ done        the accept command exited 0, dependents may start",
		"– canceled    withdrawn from the plan",
	}
	keys := []string{
		"j / k      move",
		"g / G      first and last",
		"f          jump to the selected unit's pane",
		"o          open the unit's link (a pull request, a review)",
		"space      fold the selected branch",
		"c / e      collapse and expand every branch",
		"t / s      tree and sections layouts",
		"d          detail pane for the selected unit",
		"h          hide done and canceled",
		"r          re-read plan, state and herdr",
		"?          this legend",
		"q          quit",
	}
	var b strings.Builder
	b.WriteString(sectionStyle.Render("STATUS") + "\n")
	for _, line := range status {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + sectionStyle.Render("KEYS") + "\n")
	for _, line := range keys {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("∧N on a row: it joins N inputs. [+N] on a folded row: N units hidden."))
	width := min(max(60, m.width/2), m.width-2)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(width).
		Render(b.String())
	return lipgloss.Place(m.width, max(4, m.height-5), lipgloss.Center, lipgloss.Center, box)
}

func (m viewModel) headerView() string {
	s := m.snap
	left := titleStyle.Render("dungeon") + "  " + s.Encounter
	_, counts, _ := m.tree()
	status := m.mode + " · state " + s.StateAge
	if n := len(counts); n > 0 {
		status = fmt.Sprintf("%s · %d folded", m.mode, n) + " · state " + s.StateAge
	}
	right := dimStyle.Render(status)
	chips := ""
	for _, sec := range sections {
		if n := s.Counts[sec.status]; n > 0 {
			chips += "  " + statusStyles[sec.status].Render(fmt.Sprintf("%d %s", n, strings.ToLower(sec.label)))
		}
	}
	return spread(left, right, m.width) + "\n" + chips + "\n" +
		ruleStyle.Render(strings.Repeat("─", max(0, m.width)))
}

func (m viewModel) bodyView() string {
	detailH := 0
	if m.height >= 22 {
		detailH = 7
	}
	rows := max(3, m.height-5-detailH)
	lines, cursorLine := m.listLines(m.width)
	lines = window(lines, cursorLine, rows)
	body := strings.Join(lines, "\n")
	if detailH == 0 {
		return body
	}
	return lipgloss.NewStyle().Height(rows).Render(body) + "\n" + m.detailBox(m.width, detailH)
}

// listLines renders the left column and returns the line the cursor is on, so
// the viewport can follow it.
func (m viewModel) listLines(width int) ([]string, int) {
	if m.mode == "tree" {
		return m.treeLines(width)
	}
	return m.sectionLines(width)
}

func (m viewModel) treeLines(width int) ([]string, int) {
	var lines []string
	cursorLine := 0
	cursor := m.selectedID()
	tree, counts, branches := m.tree()
	for _, line := range tree {
		suffix, fold := "", " "
		switch {
		case counts[line.ID] > 0:
			suffix, fold = fmt.Sprintf("  [+%d]", counts[line.ID]), "▸"
		case branches[line.ID]:
			fold = "▾"
		}
		if line.ID == cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, m.rowLine(m.rowByID(line.ID), width, line.Prefix, suffix, fold))
	}
	return lines, cursorLine
}

func (m viewModel) sectionLines(width int) ([]string, int) {
	var lines []string
	cursorLine := 0
	cursor := m.selectedID()
	for _, sec := range sections {
		var rows []UnitRow
		for _, u := range m.snap.Units {
			if u.Status == sec.status && !m.hidden(u.ID) {
				rows = append(rows, u)
			}
		}
		if len(rows) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, sectionStyle.Render(fmt.Sprintf("%s %d", sec.label, len(rows))))
		for _, u := range rows {
			if u.ID == cursor {
				cursorLine = len(lines)
			}
			if u.ID == cursor {
				cursorLine = len(lines)
			}
			lines = append(lines, m.rowLine(u, width, "", "", ""))
		}
	}
	return lines, cursorLine
}

// statusText is the right hand column: what this unit is doing, or waiting for.
func (m viewModel) statusText(u UnitRow) string {
	switch u.Status {
	case "blocked":
		return "needs you"
	case "working":
		if u.Pane != "" {
			return "working " + u.Pane
		}
		return "working"
	case "waiting":
		if len(u.WaitsOn) == 1 {
			return "waits " + u.WaitsOn[0]
		}
		if len(u.WaitsOn) > 1 {
			return fmt.Sprintf("waits %d", len(u.WaitsOn))
		}
		return "waiting"
	}
	return u.Status
}

// rowLine builds one unit row: connectors, fold marker, status glyph, id, join
// count, where the work is, the goal, and a right aligned status.
//
// Only the glyph, the id and the right column carry the status colour. Colouring
// the whole line made a full screen of work read as a wall of colour, and the
// status is already in two places.
func (m viewModel) rowLine(u UnitRow, width int, prefix, suffix, fold string) string {
	join := ""
	if len(u.Inputs) > 1 {
		join = fmt.Sprintf(" ∧%d", len(u.Inputs))
	}
	where := u.Minion
	if u.Pane != "" {
		where += " " + u.Pane
	}
	head := prefix + fold
	glyph := statusGlyphs[u.Status]
	id := pad(u.ID, idWidth(m.snap))
	rest := strings.TrimLeft(join+" "+pad(truncate(where, 22), whereWidth(m.snap))+" "+u.Goal, " ")

	avail := max(10, width-1)
	right := truncate(linkMark(u)+m.statusText(u)+suffix, max(6, avail/3))

	budget := avail - runeLen(right) - 2
	fixed := runeLen(head) + 1 + runeLen(id) + 1
	if budget < fixed {
		plain := truncate(head+glyph+" "+id, max(4, budget))
		return m.paintRow(u, plain, "", "", "", "")
	}
	rest = truncate(rest, max(0, budget-fixed))
	plain := head + glyph + " " + id + " " + rest
	gap := max(1, avail-runeLen(plain)-runeLen(right))
	return m.paintRow(u, plain, head, glyph, id, rest) + strings.Repeat(" ", gap) + m.paintRight(u, right)
}

// paintRow styles the left side. The selected row is plain text inside reverse
// video, so nesting colours inside the highlight cannot fight it.
func (m viewModel) paintRow(u UnitRow, plain, head, glyph, id, rest string) string {
	if u.ID == m.selectedID() {
		return selectedStyle.Render(plain)
	}
	if head == "" && glyph == "" {
		return plain
	}
	style := statusStyles[u.Status]
	settled := u.Status == "done" || u.Status == "canceled"
	idStyle := style.Bold(true)
	restStyle := lipgloss.NewStyle()
	if settled {
		idStyle, restStyle = dimStyle, dimStyle
	}
	return dimStyle.Render(head) + style.Render(glyph) + " " +
		idStyle.Render(id) + " " + restStyle.Render(rest)
}

func (m viewModel) paintRight(u UnitRow, right string) string {
	if u.ID == m.selectedID() {
		return selectedStyle.Render(right)
	}
	switch u.Status {
	case "blocked", "working":
		return statusStyles[u.Status].Render(right)
	case "done", "canceled":
		return dimStyle.Render(right)
	}
	return right
}

func (m viewModel) detailBox(width, height int) string {
	u, ok := m.selected()
	if !ok {
		return ""
	}
	inner := width - 4
	var lines []string

	head := titleStyle.Render(u.ID) + "  " + statusStyles[u.Status].Render(u.Status)
	tail := u.Role + " " + u.Minion
	if u.Pane != "" {
		tail += " " + u.Pane + " (" + u.Agent + ")"
	}
	head += dimStyle.Render(truncate("  "+tail, max(0, inner-runeLen(u.ID)-runeLen(u.Status)-4)))
	lines = append(lines, head)

	lines = append(lines, truncate(u.Goal, inner))

	deps := fmt.Sprintf("deps %d/%d", u.DepsDone, u.DepsTotal)
	if len(u.WaitsOn) > 0 {
		deps += " · waits " + strings.Join(u.WaitsOn, " ")
	} else if len(u.Inputs) > 0 {
		deps += " · all inputs landed"
	}
	lines = append(lines, dimStyle.Render(truncate(deps, inner)))

	if u.Note != "" {
		lines = append(lines, truncate("note: "+u.Note, inner))
	} else if us, ok := m.state.Units[u.ID]; ok && len(us.History) > 0 {
		last := us.History[len(us.History)-1]
		when := last.At
		if t, err := time.Parse(time.RFC3339, last.At); err == nil {
			when = t.Local().Format("15:04")
		}
		line := fmt.Sprintf("%s %s → %s", when, orDash(last.From), last.To)
		if last.Note != "" {
			line += "  " + last.Note
		}
		lines = append(lines, dimStyle.Render(truncate(line, inner)))
	}

	if u.Link != "" {
		lines = append(lines, dimStyle.Render(truncate("link: "+u.Link, inner)))
	}
	for len(lines) < height-2 {
		lines = append(lines, "")
	}
	lines = lines[:max(0, height-2)]

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(max(10, inner+2)). // lipgloss counts padding inside Width; +2 keeps the box at the frame width
		Render(strings.Join(lines, "\n"))
}

func (m viewModel) footerView() string {
	keys := "j/k · f focus · o link · d detail · space fold · h hide · t/s · ? help · q quit"
	if m.state.UpdatedAt == "" {
		keys = "no state yet · " + keys
	}
	if m.notice != "" {
		keys = m.notice
	}
	return ruleStyle.Render(strings.Repeat("─", max(0, m.width))) + "\n" + dimStyle.Render(keys)
}

func idWidth(s Snapshot) int {
	w := 4
	for _, u := range s.Units {
		w = max(w, len(u.ID))
	}
	return w
}

func whereWidth(s Snapshot) int {
	w := 4
	for _, u := range s.Units {
		n := len(u.Minion)
		if u.Pane != "" {
			n += 1 + len(u.Pane)
		}
		w = max(w, min(n, 22))
	}
	return w
}

func spread(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// window keeps the cursor visible in a fixed number of rows.
func window(lines []string, cursor, rows int) []string {
	if len(lines) <= rows {
		return lines
	}
	start := cursor - rows/2
	if start < 0 {
		start = 0
	}
	if start+rows > len(lines) {
		start = len(lines) - rows
	}
	return lines[start : start+rows]
}

func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		if line == "" {
			line = word
			continue
		}
		if len(line)+1+len(word) > width {
			out = append(out, line)
			line = word
			continue
		}
		line += " " + word
	}
	if line != "" {
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// isTerminal reports whether w is a character device, so view can fall back to
// a single frame when output is piped.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func viewCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("view", stderr)
	planPath, statePath := planFlags(fs)
	interval := fs.Duration("interval", 2*time.Second, "how often to re-read the plan, the state and herdr")
	once := fs.Bool("once", false, "print one frame and exit")
	width := fs.Int("width", 100, "frame width for --once")
	height := fs.Int("height", 30, "frame height for --once")
	mode := fs.String("mode", "tree", "tree or sections")
	detail := fs.String("detail", "", "open the detail pane for this unit (with --once)")
	hide := fs.Bool("hide-settled", false, "hide done and canceled units")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		*planPath = fs.Arg(0)
	}
	if *statePath == "" {
		*statePath = StatePath(*planPath)
	}
	if *mode != "tree" && *mode != "sections" {
		fmt.Fprintf(stderr, "dungeon: mode must be tree or sections, not %q\n", *mode)
		return 2
	}

	m := newViewModel(*planPath, *statePath, *interval)
	m.mode, m.hideSettled = *mode, *hide
	m.width, m.height = *width, *height
	m = m.applyRefresh(loadSnapshot(*planPath, *statePath))
	if *detail != "" {
		for i, id := range m.order {
			if id == *detail {
				m.cursor, m.overlay, m.artIdx = i, "detail", 0
			}
		}
		if m.overlay == "" {
			fmt.Fprintf(stderr, "dungeon: unit %q is not visible\n", *detail)
			return 2
		}
	}
	if m.err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", m.err)
		return 1
	}

	if *once || !isTerminal(stdout) {
		fmt.Fprint(stdout, m.View())
		return 0
	}
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	return 0
}

func (m viewModel) applyRefresh(r refreshedMsg) viewModel {
	next, _ := m.Update(r)
	return next.(viewModel)
}
