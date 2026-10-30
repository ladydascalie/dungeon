package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	boldStyle     = lipgloss.NewStyle().Bold(true)
	sectionStyle  = lipgloss.NewStyle().Bold(true).Faint(true)
	selectedStyle = lipgloss.NewStyle().Bold(true).Reverse(true)
	panelStyle    = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).PaddingLeft(1)
	ruleStyle     = lipgloss.NewStyle().Faint(true)

	statusStyles = map[string]lipgloss.Style{
		"blocked":  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203")),
		"failed":   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")),
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

// focusCmd jumps the operator to the chosen unit's lane. A live agent is
// exact; with none, the pane hint recorded at spawn is used.
func (m viewModel) focusCmd() tea.Cmd {
	u, ok := m.focusTarget()
	if !ok {
		return nil
	}
	// The derived name is what herdr knows the lane by; the plan keeps the id.
	agent, hint := m.state.AgentName(u.Minion), m.state.Minions[u.Minion].Pane
	if u.Pane != "" {
		hint = u.Pane
	}
	return func() tea.Msg {
		return focusMsg{err: newHerdrClient().focus(context.Background(), agent, hint)}
	}
}

// focusTarget is the unit the operator means when they press space f. In the
// loop timeline that is the selected round's live unit; in a card opened from
// one it is that card's unit; on a loop summary row it is the loop's live unit.
func (m viewModel) focusTarget() (UnitRow, bool) {
	if m.overlay == "loop" {
		return m.roundUnit()
	}
	if m.cardID != "" {
		return m.rowByID(m.cardID), true
	}
	u, ok := m.selected()
	if !ok {
		return u, false
	}
	if l := m.snapLoop(u.ID); l != nil {
		if live, ok := m.liveLoopUnit(*l); ok {
			return live, true
		}
	}
	return u, true
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
	prefix      bool        // the prefix is open: the next key names an action
	choosing    []Encounter // a picker is open: choose an encounter before loading
	choice      int

	notice         string // the last thing that went wrong, shown in the footer until the next key
	overlay        string // "", "detail", "reader" or "loop"
	artIdx         int
	cardID         string // the unit shown in a detail card opened from a loop round
	roundIdx       int    // the selected round in the loop timeline
	reader         string
	readerLines    []string
	readerTop      int
	readerRendered []string // markdown, rendered once per width, never per frame
	readerWidth    int
	folded         map[string]bool // explicit operator folds
	foldExplicit   map[string]bool // the operator set this fold, so auto-fold must not win
	order          []string        // unit and loop ids in display order
	cursor         int

	width  int
	height int
}

func newViewModel(planPath, statePath string, interval time.Duration) viewModel {
	return viewModel{
		planPath:     planPath,
		statePath:    statePath,
		interval:     interval,
		mode:         "tree",
		folded:       map[string]bool{},
		foldExplicit: map[string]bool{},
		width:        100,
		height:       30,
	}
}

func (m viewModel) Init() tea.Cmd {
	if m.choiceMode() {
		return nil
	}
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
		if m.choiceMode() {
			return m, m.choose(key)
		}
		if m.prefix {
			m.prefix = false
			return m, m.runPrefix(key)
		}
		if key == " " {
			m.prefix = true
			return m, nil
		}
		if key == "esc" && m.help {
			m.help = false
			return m, nil
		}
		if m.overlay != "" && m.handleOverlayKey(key) {
			return m, nil
		}
		switch key {
		case "esc":
			return m, nil
		case "ctrl+c":
			return m, tea.Quit
		case "d", "enter":
			if l := m.snapLoop(m.selectedID()); l != nil {
				m.overlay, m.cardID = "loop", ""
				m.roundIdx = m.currentRoundIndex(*l)
			} else {
				m.overlay, m.cardID, m.artIdx = "detail", "", 0
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

// prefixMenu is the set of actions behind space. Motion, fold, detail and the
// overlays stay direct; everything that changes the view or leaves it is here.
var prefixMenu = []struct{ key, label string }{
	{" ", "fold selected"},
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

// runPrefix executes one prefixed action. esc cancels; an unknown
// key says so and returns, so a stray press never changes the run.
func (m *viewModel) runPrefix(key string) tea.Cmd {
	switch key {
	case "esc":
		return nil
	case "ctrl+c":
		return tea.Quit
	case " ":
		m.toggleFold()
	case "o":
		u, ok := m.cardRow()
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
			m.foldExplicit[id] = true
		}
		m.rebuildOrder(m.selectedID())
	case "e":
		m.folded = map[string]bool{}
		m.foldExplicit = map[string]bool{}
		for _, l := range m.snap.Loops {
			m.foldExplicit[l.ID] = true // an explicit unfold beats an unstarted loop's auto fold
		}
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
// bottom right while the prefix is open.
func (m viewModel) prefixBox() string {
	keyLabel := func(k string) string {
		if k == " " {
			return "space"
		}
		return k
	}
	keyW, labelW := 0, 0
	for _, a := range prefixMenu {
		keyW = max(keyW, runeLen(keyLabel(a.key)))
		labelW = max(labelW, runeLen(a.label))
	}
	var rows []string
	for i := 0; i < len(prefixMenu); i += 2 {
		row := fmt.Sprintf("%-*s %-*s", keyW, keyLabel(prefixMenu[i].key), labelW, prefixMenu[i].label)
		if i+1 < len(prefixMenu) {
			row += "   " + fmt.Sprintf("%-*s %-*s", keyW, keyLabel(prefixMenu[i+1].key), labelW, prefixMenu[i+1].label)
		}
		rows = append(rows, row)
	}
	head := dimStyle.Render("space") + "   " + dimStyle.Render("esc cancels")
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

func (m viewModel) choiceMode() bool { return len(m.choosing) > 0 }

// choose moves the picker and opens the selected encounter. No plan is loaded
// until a choice is made.
func (m *viewModel) choose(key string) tea.Cmd {
	switch key {
	case "j", "down":
		m.choice++
	case "k", "up":
		m.choice--
	case "enter":
		e := m.choosing[m.choice]
		m.choosing = nil
		if e.Err != nil {
			m.err = fmt.Errorf("%s: %w", shortPath(e.Path), e.Err)
			return nil
		}
		m.planPath, m.statePath = e.Path, StatePath(e.Path)
		return tea.Batch(m.refresh(), tick(m.interval))
	case "q", "esc", "ctrl+c":
		return tea.Quit
	}
	if m.choice < 0 {
		m.choice = 0
	}
	if m.choice >= len(m.choosing) {
		m.choice = len(m.choosing) - 1
	}
	return nil
}

// choiceView is the encounter picker: one line per plan, most recently touched first.
func (m viewModel) choiceView() string {
	width := 0
	for _, e := range m.choosing {
		width = max(width, runeLen(shortPath(e.Path)))
	}
	var b strings.Builder
	for i, e := range m.choosing {
		cursor := "  "
		if i == m.choice {
			cursor = "❯ "
		}
		name := e.Name
		if e.Err != nil {
			name = "unreadable"
		}
		fmt.Fprintf(&b, "%s%s  %s", cursor, pad(shortPath(e.Path), width), name)
		if i+1 < len(m.choosing) {
			b.WriteString("\n")
		}
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Render(b.String())
	content := titleStyle.Render("dungeon") + "  choose an encounter\n\n" + box + "\n\n" +
		dimStyle.Render("j/k move · enter open · q quit")
	return lipgloss.Place(max(40, m.width), max(12, m.height), lipgloss.Center, lipgloss.Center, content)
}

// toggleFold folds or unfolds the selected branch. It sits behind the prefix on space.
// The choice is explicit, so it survives a refresh and beats the auto fold an
// unstarted loop gets.
func (m *viewModel) toggleFold() {
	id := m.selectedID()
	if id == "" {
		return
	}
	m.folded[id] = !m.foldsNow()[id]
	m.foldExplicit[id] = true
	m.rebuildOrder(id)
}

// foldsNow is the fold map the view draws. An operator choice wins. Otherwise a
// loop follows its own rule: unstarted folds, started unfolds.
func (m viewModel) foldsNow() map[string]bool {
	folded := map[string]bool{}
	for id, on := range m.folded {
		if on {
			folded[id] = true
		}
	}
	for _, l := range m.snap.Loops {
		if m.foldExplicit[l.ID] {
			continue
		}
		if !l.Started {
			folded[l.ID] = true
		}
	}
	return folded
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
	roots, counts := applyFolds(roots, m.foldsNow())
	return flattenForest(roots), counts, branches
}

func (m viewModel) hidden(id string) bool {
	if !m.hideSettled {
		return false
	}
	if m.snapLoop(id) != nil {
		// A loop is the summary of its children. Hiding it would splice its
		// generated units back into the forest as free nodes.
		return false
	}
	st := m.rowByID(id).Status
	return st == "done" || st == "canceled"
}

func (m viewModel) snapLoop(id string) *LoopRow {
	for i := range m.snap.Loops {
		if m.snap.Loops[i].ID == id {
			return &m.snap.Loops[i]
		}
	}
	return nil
}

// loopRow is the summary of a loop: its aggregate state and the pane of the
// unit that is live now.
func (m viewModel) loopRow(l LoopRow) UnitRow {
	r := UnitRow{ID: l.ID, Status: l.State}
	for i, id := range l.Units {
		c := m.unitByID(id)
		if i == 0 {
			r.Inputs = c.Inputs // the loop's entry deps
		}
		if c.Status == "working" && c.Pane != "" {
			r.Pane = c.Pane
			break
		}
		if r.Pane == "" && c.Pane != "" {
			r.Pane = c.Pane
		}
	}
	return r
}

func (m viewModel) unitByID(id string) UnitRow {
	for _, u := range m.snap.Units {
		if u.ID == id {
			return u
		}
	}
	return UnitRow{}
}

func (m viewModel) rowByID(id string) UnitRow {
	if u := m.unitByID(id); u.ID != "" {
		return u
	}
	if l := m.snapLoop(id); l != nil {
		return m.loopRow(*l)
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

// cardRow is the unit a detail card shows. A card opened from a loop round
// names its unit; every other card follows the cursor.
func (m viewModel) cardRow() (UnitRow, bool) {
	if m.cardID != "" {
		u := m.rowByID(m.cardID)
		return u, u.ID != ""
	}
	return m.selected()
}

func (m viewModel) render() string {
	if m.choiceMode() {
		return m.choiceView()
	}
	if m.err != nil {
		return titleStyle.Render("dungeon") + "\n\n" + m.err.Error() + "\n\n" + dimStyle.Render("space r to retry, space q to quit") + "\n"
	}

	var body string
	switch {
	case m.help:
		body = m.helpView()
	case m.overlay == "loop":
		body = m.loopOverlay()
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
		"The id and lane are bold. The right column names the state in words, and says",
		"what the unit waits for, which pane it is in, or that it needs you.",
	}
	keys := []string{
		"j / k      move",
		"g / G      first and last",
		"enter / d  detail pane for the selected unit or loop",
		"esc        close the overlay or legend",
		"ctrl+c     quit",
		"",
		"space      then an action:",
		"  space    fold the selected branch",
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
		d                    displayRow
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
		var d displayRow
		switch {
		case line.Loop:
			l := m.snapLoop(line.ID)
			if l == nil {
				continue
			}
			// The loop glyph is its own mark, folded or not: a summary row must
			// never read as a unit.
			fold = "⛧"
			d = m.loopDisplay(*l)
		case line.Label != "":
			d = unitRow(m.rowByID(line.ID))
			d.id = line.Label
			d.goal = loopChildGoal(line.Label)
		default:
			d = unitRow(m.rowByID(line.ID))
		}
		rows = append(rows, row{d, line.Prefix, suffix, fold})
	}

	parts := make([]rowCells, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, m.rowParts(r.d, r.prefix, r.suffix, r.fold))
	}
	mt := m.metricsOf(parts, max(10, width-1))

	var lines []string
	cursorLine := 0
	cursor := m.selectedID()
	for _, r := range rows {
		if r.d.u.ID == cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, m.rowLine(r.d, width, r.prefix, r.suffix, r.fold, mt))
	}
	return lines, cursorLine
}

// loopDisplay is the loop summary row: the id, its round progress and focus in
// the goal column, its aggregate state in the status column, and no lane.
func (m viewModel) loopDisplay(l LoopRow) displayRow {
	u := m.loopRow(l)
	return displayRow{
		u:    u,
		id:   l.ID,
		goal: fmt.Sprintf("⟳ %d/%d %s", l.Round, l.Rounds, collapseSpace(l.Focus)),
		lane: "",
	}
}

// loopChildGoal is the short label a generated row shows. The full generated
// goal stays in the card, which reads the unit itself.
func loopChildGoal(label string) string {
	i := strings.LastIndex(label, "-")
	if i < 0 {
		return label
	}
	kind, round := label[:i], label[i+1:]
	if kind != "review" && kind != "fix" {
		return label
	}
	return kind + ", round " + round
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
		var parts []rowCells
		for _, u := range rows {
			parts = append(parts, m.rowParts(unitRow(u), "", "", ""))
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
			lines = append(lines, m.rowLine(unitRow(u), width, "", "", "", mt))
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
			return "waits " + displayUnitID(m.snap, u.WaitsOn[0])
		}
		if len(u.WaitsOn) > 1 {
			return fmt.Sprintf("waits %d", len(u.WaitsOn))
		}
		return "waiting"
	}
	return u.Status
}

// rowMetrics sizes the two columns for the rows that are about to be drawn: the
// right column as wide as its longest status, a small fixed gap, and the left
// column taking the rest of the frame.
type rowMetrics struct {
	left  int
	right int
	pad   int
}

func (m viewModel) paintRight(u UnitRow, right string) string {
	if u.ID == m.selectedID() {
		return selectedStyle.Render(right)
	}
	// The right column uses the same palette as the header counts and the
	// detail bar, so a done unit is green everywhere it is named.
	return statusStyles[u.Status].Render(right)
}

func (m viewModel) detailBox(width, height int) string {
	u, ok := m.selected()
	if !ok {
		return ""
	}
	inner := width - 4
	var lines []string
	if l := m.snapLoop(u.ID); l != nil {
		lines = m.loopDetailLines(*l, inner)
	} else {
		lines = m.unitDetailLines(u, inner)
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

// unitDetailLines is the bottom strip for a unit: what it is, what it waits
// for, and its latest note or change.
func (m viewModel) unitDetailLines(u UnitRow, inner int) []string {
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

	lines = append(lines, truncate(collapseSpace(u.Goal), inner))

	deps := fmt.Sprintf("deps %d/%d", u.DepsDone, u.DepsTotal)
	if len(u.WaitsOn) > 0 {
		deps += " · waits " + strings.Join(u.WaitsOn, " ")
	} else if len(u.Inputs) > 0 {
		deps += " · all inputs landed"
	}
	lines = append(lines, dimStyle.Render(truncate(deps, inner)))

	if u.Note != "" {
		lines = append(lines, truncate("note: "+collapseSpace(u.Note), inner))
	} else if us, ok := m.state.Units[u.ID]; ok && len(us.History) > 0 {
		last := us.History[len(us.History)-1]
		when := last.At
		if t, err := time.Parse(time.RFC3339, last.At); err == nil {
			when = t.Local().Format("15:04")
		}
		line := fmt.Sprintf("%s %s → %s", when, orDash(last.From), last.To)
		if last.Note != "" {
			line += "  " + collapseSpace(last.Note)
		}
		lines = append(lines, dimStyle.Render(truncate(line, inner)))
	}

	if u.Link != "" {
		lines = append(lines, dimStyle.Render(truncate("link: "+u.Link, inner)))
	}
	return lines
}

// loopDetailLines is the bottom strip for a loop summary row. The row carries
// the loop, not a unit, so the strip names the round the loop is on and the
// unit that round is working.
func (m viewModel) loopDetailLines(l LoopRow, inner int) []string {
	head := titleStyle.Render(l.ID) + "  " + statusStyles[l.State].Render(l.State) + dimStyle.Render("  loop")
	lines := []string{head}
	lines = append(lines, truncate("focus: "+collapseSpace(l.Focus), inner))

	rows := m.loopRounds(l)
	if len(rows) == 0 {
		lines = append(lines, dimStyle.Render(truncate(fmt.Sprintf("cap: %d", l.Rounds), inner)))
	} else {
		lines = append(lines, dimStyle.Render(truncate(fmt.Sprintf("round %d/%d", l.Round, l.Rounds), inner)))
		if u, ok := m.roundPick(rows[m.currentRoundIndex(l)]); ok {
			kind, round, _ := loopChildKind(l.ID, u.ID)
			now := fmt.Sprintf("%s, round %d", kind, round)
			lines = append(lines, truncate("now: "+now+" · "+collapseSpace(u.Goal), inner))
		}
	}
	lines = append(lines, dimStyle.Render(truncate("d opens the round timeline", inner)))
	return lines
}

func (m viewModel) footerView() string {
	keys := "j/k move · enter detail · space actions"
	if m.prefix {
		keys = "space: choose an action · esc cancels"
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
		w = max(w, runeLen(displayUnitID(s, u.ID)))
	}
	for _, l := range s.Loops {
		w = max(w, runeLen(l.ID))
	}
	return w
}

// displayUnitID drops the loop prefix a generated unit's parent already names.
// A declared unit keeps its full id even when it starts with a loop id, because
// only a generated unit is listed under its loop.
func displayUnitID(s Snapshot, id string) string {
	for _, l := range s.Loops {
		for _, generated := range l.Units {
			if generated == id {
				return strings.TrimPrefix(id, l.ID+"-")
			}
		}
	}
	return id
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
	explicit := fs.NArg() > 0 || *planPath != ""
	var found []Encounter
	if !explicit {
		found = discoverPlans()
		switch len(found) {
		case 0:
			*planPath = defaultPlan()
		case 1:
			*planPath = found[0].Path
			found = nil
		}
	}
	if *statePath == "" {
		*statePath = StatePath(*planPath)
	}
	if *mode != "tree" && *mode != "sections" {
		fmt.Fprintf(stderr, "dungeon: mode must be tree or sections, not %q\n", *mode)
		return 2
	}
	if len(found) > 1 {
		if *once || !isTerminal(stdout) || *detail != "" {
			fmt.Fprintf(stderr, "dungeon: %d encounters under %s\n", len(found), shortPath(filepath.Dir(found[0].Path)))
			printEncounters(stderr, found)
			fmt.Fprintln(stderr, "pass --plan to choose one")
			return 2
		}
		pick := newViewModel(*planPath, *statePath, *interval)
		pick.mode, pick.hideSettled = *mode, *hide
		pick.width, pick.height = *width, *height
		pick.choosing = found
		prog := tea.NewProgram(pick, tea.WithAltScreen(), tea.WithContext(ctx))
		if _, err := prog.Run(); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		return 0
	}

	m := newViewModel(*planPath, *statePath, *interval)
	m.mode, m.hideSettled = *mode, *hide
	m.width, m.height = *width, *height
	m = m.applyRefresh(loadSnapshot(*planPath, *statePath))
	if *detail != "" {
		for i, id := range m.order {
			if id != *detail {
				continue
			}
			m.cursor, m.artIdx, m.cardID = i, 0, ""
			if l := m.snapLoop(id); l != nil {
				m.overlay, m.roundIdx = "loop", m.currentRoundIndex(*l)
			} else {
				m.overlay = "detail"
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
