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
	prefix      bool // ctrl+d is held: the next key names an action

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
	markPlanDrift(&snap, planPath, st)
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
		if m.prefix {
			m.prefix = false
			return m, m.runPrefix(key)
		}
		if key == "ctrl+d" {
			m.prefix = true
			return m, nil
		}
		if m.overlay != "" && m.handleOverlayKey(key) {
			return m, nil
		}
		switch key {
		case "esc", "ctrl+c":
			return m, tea.Quit
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

// prefixMenu is the set of actions behind ctrl+d. Motion, fold, detail and the
// overlays stay direct; everything that changes the view or leaves it is here.
var prefixMenu = []struct{ key, label string }{
	{"f", "focus lane"},
	{"o", "open link"},
	{"r", "reload"},
	{"t", "tree"},
	{"s", "sections"},
	{"h", "hide settled"},
	{"c", "collapse all"},
	{"e", "expand all"},
	{"?", "help"},
	{"q", "quit"},
}

// runPrefix executes one prefixed action. esc and ctrl+d cancel; an unknown
// key says so and returns, so a stray press never changes the run.
func (m *viewModel) runPrefix(key string) tea.Cmd {
	switch key {
	case "esc", "ctrl+d":
		return nil
	case "ctrl+c":
		return tea.Quit
	case "o":
		u, ok := m.selected()
		if !ok || u.Link == "" {
			m.notice = "no link on this unit"
			return nil
		}
		if err := openURL(u.Link); err != nil {
			m.notice = "cannot open: " + err.Error()
		}
		return nil
	case "f":
		return m.focusCmd()
	case "r":
		return m.refresh()
	case "t":
		m.mode = "tree"
		m.rebuildOrder(m.selectedID())
	case "s":
		m.mode = "sections"
		m.rebuildOrder(m.selectedID())
	case "h":
		m.hideSettled = !m.hideSettled
		m.rebuildOrder(m.selectedID())
	case "c":
		for _, id := range foldable(pruneForest(buildForest(m.snap), m.hidden)) {
			m.folded[id] = true
		}
		m.rebuildOrder(m.selectedID())
	case "e":
		m.folded = map[string]bool{}
		m.rebuildOrder(m.selectedID())
	case "?":
		m.help = !m.help
	case "q":
		return tea.Quit
	default:
		m.notice = "no action on " + key
	}
	m.clampCursor()
	return nil
}

// prefixBox is the which-key pane: every action the prefix can take, drawn
// bottom right while ctrl+d is held.
func (m viewModel) prefixBox() string {
	labelW := 0
	for _, a := range prefixMenu {
		labelW = max(labelW, runeLen(a.label))
	}
	var rows []string
	for i := 0; i < len(prefixMenu); i += 2 {
		row := fmt.Sprintf("%-2s %-*s", prefixMenu[i].key, labelW, prefixMenu[i].label)
		if i+1 < len(prefixMenu) {
			row += "   " + fmt.Sprintf("%-2s %-*s", prefixMenu[i+1].key, labelW, prefixMenu[i+1].label)
		}
		rows = append(rows, row)
	}
	head := dimStyle.Render("ctrl+d") + "   " + dimStyle.Render("esc cancels")
	body := head + "\n" + strings.Join(rows, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63")).
		Padding(0, 1).
		Render(body)
}

// overlayBottomRight draws box over the last rows of body, flush right. Each
// covered row is replaced whole: splicing styled text by column would cut
// escape sequences.
func overlayBottomRight(body, box string, width int) string {
	lines := strings.Split(body, "\n")
	boxLines := strings.Split(box, "\n")
	pad := width - lipgloss.Width(box)
	if pad < 0 {
		pad = 0
	}
	offset := len(lines) - len(boxLines)
	for i, line := range boxLines {
		row := offset + i
		if row < 0 || row >= len(lines) {
			continue
		}
		lines[row] = strings.Repeat(" ", pad) + line
	}
	return strings.Join(lines, "\n")
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
		return titleStyle.Render("dungeon") + "\n\n" + m.err.Error() + "\n\n" + dimStyle.Render("ctrl+d r to retry, ctrl+d q to quit") + "\n"
	}

	var body string
	switch {
	case m.help:
		body = m.helpView()
	case m.overlay == "detail":
		body = m.detailOverlay()
	case m.overlay == "reader":
		body = m.readerView()
	default:
		body = m.bodyView()
	}
	if m.prefix {
		body = overlayBottomRight(body, m.prefixBox(), m.width)
	}
	return m.headerView() + "\n" + body + "\n" + m.footerView()
}

// helpView is the legend. The glyphs mean nothing without one, and a screen of
// coloured marks is not a substitute for naming them.
func (m viewModel) helpView() string {
	status := []string{
		"The id takes the state's colour. The right column names the state in words,",
		"and says what the unit waits for, which pane it is in, or that it needs you.",
	}
	keys := []string{
		"j / k      move",
		"g / G      first and last",
		"space      fold the selected branch",
		"d          detail pane for the selected unit",
		"esc        quit",
		"",
		"ctrl+d     then an action:",
		"  f        jump to the selected unit's pane",
		"  o        open the unit's link (a pull request, a review)",
		"  r        re-read plan, state and herdr",
		"  t / s    tree and sections layouts",
		"  h        hide done and canceled",
		"  c / e    collapse and expand every branch",
		"  ?        this legend",
		"  q        quit",
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
	if s.PlanDrift {
		status += " · plan drift"
	}
	if m.prefix {
		status += " · PREFIX"
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

// contentWidth caps the text on the left of a row at two thirds of the frame.
// The status column stays right aligned at the frame edge, so on a wide terminal
// the goal stops early and the gap between them is what keeps the row readable.
func (m viewModel) contentWidth() int {
	return min(m.width, max(40, m.width*2/3))
}

func (m viewModel) bodyView() string {
	detailH := 0
	if m.height >= 22 {
		detailH = 7
	}
	rows := max(3, m.height-5-detailH)
	width := m.width
	lines, cursorLine := m.listLines(width)
	lines = window(lines, cursorLine, rows)
	body := strings.Join(lines, "\n")
	if detailH == 0 {
		return body
	}
	return lipgloss.NewStyle().Height(rows).Render(body) + "\n" + m.detailBox(width, detailH)
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
	type row struct {
		u                    UnitRow
		prefix, suffix, fold string
	}
	var rows []row
	tree, counts, branches := m.tree()
	for _, line := range tree {
		// A leaf keeps an empty fold cell so the ids stay aligned.
		suffix, fold := "", " "
		switch {
		case counts[line.ID] > 0:
			suffix, fold = fmt.Sprintf("  [+%d]", counts[line.ID]), "⛧"
		case branches[line.ID]:
			fold = "⛧"
		}
		rows = append(rows, row{m.rowByID(line.ID), line.Prefix, suffix, fold})
	}

	parts := make([][5]string, 0, len(rows))
	for _, r := range rows {
		h, l, i, rest, right := m.rowParts(r.u, r.prefix, r.suffix, r.fold)
		parts = append(parts, [5]string{h, l, i, rest, right})
	}
	mt := m.metricsOf(parts, max(10, width-1))

	var lines []string
	cursorLine := 0
	cursor := m.selectedID()
	for _, r := range rows {
		if r.u.ID == cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, m.rowLine(r.u, width, r.prefix, r.suffix, r.fold, mt))
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
		var parts [][5]string
		for _, u := range rows {
			h, l, i, rest, right := m.rowParts(u, "", "", "")
			parts = append(parts, [5]string{h, l, i, rest, right})
		}
		mt := m.metricsOf(parts, max(10, width-1))

		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, sectionStyle.Render(fmt.Sprintf("%s %d", sec.label, len(rows))))
		for _, u := range rows {
			if u.ID == cursor {
				cursorLine = len(lines)
			}
			lines = append(lines, m.rowLine(u, width, "", "", "", mt))
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

// rowParts splits a row into the two columns' worth of text. The left column is
// the connectors, the fold mark, the id, the join count, the lane and the goal;
// the right column is the status.
func (m viewModel) rowParts(u UnitRow, prefix, suffix, fold string) (head, lead, id, rest, right string) {
	join := ""
	if len(u.Inputs) > 1 {
		join = fmt.Sprintf(" ∧%d", len(u.Inputs))
	}
	where := u.Minion
	if u.Pane != "" {
		where += " " + u.Pane
	}
	head = prefix + fold
	if head != "" {
		lead = " " // air between the fold mark and the id
	}
	id = pad(u.ID, idWidth(m.snap))
	rest = strings.TrimLeft(join+" "+pad(truncate(where, 22), whereWidth(m.snap))+" "+u.Goal, " ")
	right = linkMark(u) + m.statusText(u) + suffix
	return head, lead, id, rest, right
}

// rowMetrics sizes the two columns for the rows that are about to be drawn: the
// left column as wide as its longest line, the right column as wide as its
// longest status, and a small fixed gap between them. The pair then sits at the
// left of the frame with nothing stretched to reach the far edge, which is what
// makes a wide terminal readable.
type rowMetrics struct {
	left  int
	right int
	pad   int
}

func (m viewModel) metricsOf(rows [][5]string, avail int) rowMetrics {
	const pad = 2
	left, right := 0, 0
	for _, parts := range rows {
		head, lead, id, rest, status := parts[0], parts[1], parts[2], parts[3], parts[4]
		left = max(left, runeLen(head+lead+id+" "+rest))
		right = max(right, runeLen(status))
	}
	// Keep the gap at pad and let the left column shrink instead.
	left = min(left, max(20, avail-right-pad))
	return rowMetrics{left: left, right: right, pad: pad}
}

// rowLine draws one row against those columns: the left text padded to the
// column, the gap, then the status right aligned inside its own column.
func (m viewModel) rowLine(u UnitRow, width int, prefix, suffix, fold string, mt rowMetrics) string {
	head, lead, id, rest, right := m.rowParts(u, prefix, suffix, fold)

	fixed := runeLen(head) + runeLen(lead) + runeLen(id) + 1
	if mt.left < fixed {
		plain := truncate(head+lead+id, max(4, mt.left))
		return m.paintRow(u, plain, "", "", "")
	}
	// The padding belongs to the rest of the row, not to the line: paintRow
	// rebuilds the row from its parts, so spaces added to the assembled string
	// would be dropped and the status would drift left on short rows.
	rest = truncate(rest, max(0, mt.left-fixed))
	rest += strings.Repeat(" ", max(0, mt.left-fixed-runeLen(rest)))
	plain := head + lead + id + " " + rest

	right = truncate(right, mt.right)
	if mt.right > runeLen(right) {
		right = strings.Repeat(" ", mt.right-runeLen(right)) + right
	}
	return m.paintRow(u, plain, head+lead, id, rest) + strings.Repeat(" ", mt.pad) + m.paintRight(u, right)
}

// paintRow styles the left side. The selected row is plain text inside reverse
// video, so nesting colours inside the highlight cannot fight it.
func (m viewModel) paintRow(u UnitRow, plain, head, id, rest string) string {
	if u.ID == m.selectedID() {
		return selectedStyle.Render(plain)
	}
	if head == "" && id == "" {
		return plain
	}
	style := statusStyles[u.Status]
	settled := u.Status == "done" || u.Status == "canceled"
	idStyle := style.Bold(true)
	restStyle := lipgloss.NewStyle()
	if settled {
		idStyle, restStyle = dimStyle, dimStyle
	}
	return dimStyle.Render(head) + idStyle.Render(id) + " " + restStyle.Render(rest)
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
		tail += " " + u.Pane
		if u.Agent != "" {
			tail += " (" + u.Agent + ")"
		}
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
	keys := "j/k move · d detail · space fold · ctrl+d actions"
	if m.prefix {
		keys = "ctrl+d: choose an action · esc cancels"
	}
	if m.state.UpdatedAt == "" && !m.prefix {
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
