package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The overlay is the per-unit detail pane: what the unit is, what it waits for,
// and the files kept with it. The list can only show a row; a brief or a report
// needs somewhere to be read.

var (
	// matchStyle is every other hit on screen: the reader must show why a line
	// matched without hiding the current one.
	matchStyle = lipgloss.NewStyle().Background(lipgloss.Color("220")).Foreground(lipgloss.Color("0"))
	// currentMatchStyle is the hit n and N sit on, so a cycle lands somewhere
	// the eye can find.
	currentMatchStyle = lipgloss.NewStyle().Background(lipgloss.Color("39")).Foreground(lipgloss.Color("0")).Bold(true)
)

type artifact struct {
	Path  string
	Label string
	Size  int64
	Mod   time.Time
	Err   string
}

// artifacts resolves the files a unit keeps: the ones attached by hand, then the
// brief and report a dispatch writes under the encounter directory, which is
// the plan's own .dungeon/ so they live as long as the plan does.
func (m viewModel) artifacts(u UnitRow) []artifact {
	var out []artifact
	seen := map[string]bool{}

	dir := filepath.Dir(m.statePath)
	// A label is the shortest name that is unambiguous on screen: relative to
	// the state directory when the file lives there, else to home.
	label := func(path string) string {
		if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
		if home, err := os.UserHomeDir(); err == nil {
			if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
				return "~/" + rel
			}
		}
		return path
	}

	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		a := artifact{Path: path, Label: label(path)}
		if info, err := os.Stat(path); err != nil {
			a.Err = "missing"
		} else if info.IsDir() {
			a.Err = "directory"
		} else {
			a.Size, a.Mod = info.Size(), info.ModTime()
		}
		out = append(out, a)
	}

	if us, ok := m.state.Units[u.ID]; ok {
		for _, f := range us.Files {
			add(f)
		}
	}
	add(filepath.Join(dir, "briefs", u.ID+".md"))
	add(filepath.Join(dir, "reports", u.ID+".md"))
	return out
}

func (m viewModel) detailOverlay() string {
	u, ok := m.cardRow()
	if !ok {
		return dimStyle.Render("no units")
	}
	inner := max(20, min(m.width-6, 110))
	var b strings.Builder

	b.WriteString(titleStyle.Render(u.ID) + "  " + statusStyles[u.Status].Render(u.Status) +
		dimStyle.Render(fmt.Sprintf("  %s %s", u.Role, u.Minion)) + "\n\n")
	b.WriteString(wrap(u.Goal, inner) + "\n\n")

	deps := fmt.Sprintf("deps %d/%d", u.DepsDone, u.DepsTotal)
	if len(u.WaitsOn) > 0 {
		deps += " · waits " + strings.Join(u.WaitsOn, " ")
	}
	if u.Pane != "" {
		deps += " · pane " + u.Pane
		if u.Agent != "" {
			deps += " (" + u.Agent + ")"
		}
	}
	b.WriteString(dimStyle.Render(truncate(deps, inner)) + "\n")
	if u.Note != "" {
		b.WriteString(wrap("note: "+u.Note, inner) + "\n")
	}

	files := m.artifacts(u)
	if u.Link != "" {
		b.WriteString("\n" + sectionStyle.Render("LINK") + "\n" + truncate(u.Link, inner) + "\n")
	}
	b.WriteString("\n" + sectionStyle.Render(fmt.Sprintf("FILES %d", len(files))) + "\n")
	for i, a := range files {
		mark := " "
		if i == m.artIdx {
			mark = "▸"
		}
		line := fmt.Sprintf("%s %s", mark, a.Label)
		meta := a.Err
		if meta == "" {
			meta = fmt.Sprintf("%s  %s", humanSize(a.Size), age(a.Mod.UTC().Format(time.RFC3339)))
		}
		line += "  " + dimStyle.Render(meta)
		if i == m.artIdx {
			line = selectedStyle.Render(truncate(fmt.Sprintf("%s %s  %s", mark, a.Label, meta), inner))
		}
		b.WriteString(line + "\n")
	}
	if len(files) == 0 {
		b.WriteString(dimStyle.Render("none. attach one with: dungeon attach "+u.ID+" <path>") + "\n")
	}

	if us, ok := m.state.Units[u.ID]; ok && len(us.History) > 0 {
		b.WriteString("\n" + sectionStyle.Render("HISTORY") + "\n")
		start := max(0, len(us.History)-5)
		for _, c := range us.History[start:] {
			when := c.At
			if t, err := time.Parse(time.RFC3339, c.At); err == nil {
				when = t.Local().Format("15:04")
			}
			line := fmt.Sprintf("%s %s → %s", when, orDash(c.From), c.To)
			if c.Note != "" {
				line += "  " + c.Note
			}
			b.WriteString(dimStyle.Render(wrap(line, inner)) + "\n")
		}
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(inner).
		Render(b.String())
	back := "d close"
	if m.cardID != "" {
		back = "esc back"
	}
	hint := dimStyle.Render("j/k file · enter read · " + back)
	return lipgloss.Place(m.width, max(4, m.height-5), lipgloss.Center, lipgloss.Center, box) + "\n" + hint
}

// readerDisplay is what the reader draws: rendered markdown when the file was
// rendered at this width, else the source lines. Nothing here renders.
func (m viewModel) readerDisplay() []string {
	if len(m.readerRendered) > 0 {
		return m.readerRendered
	}
	return m.readerLines
}

// renderReader turns the source into display lines once, when the file is
// opened and again only when the pane's width changes. Rendering in the frame
// path made every keystroke pay for a full markdown parse.
func (m *viewModel) renderReader() {
	m.readerRendered = nil
	if !isMarkdown(m.reader) || len(m.readerLines) == 0 || len(m.readerLines) > 2000 {
		m.readerWidth = 0
		return
	}
	width := max(40, m.width-8)
	if width == m.readerWidth && len(m.readerRendered) > 0 {
		return
	}
	m.readerRendered = renderMarkdown(strings.Join(m.readerLines, "\n"), width)
	m.readerWidth = width
}

func (m viewModel) readerView() string {
	inner := max(20, m.width-6)
	rows := max(3, m.height-8)
	lines := m.readerDisplay()
	body := ""
	if len(lines) == 0 {
		body = dimStyle.Render("empty file")
	} else {
		end := min(len(lines), m.readerTop+rows)
		body = strings.Join(m.paintedLines(lines, end), "\n")
	}
	title := fmt.Sprintf("%s  %d lines", filepath.Base(m.reader), len(lines))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(inner).
		Height(rows).
		Render(titleStyle.Render(title) + "\n" + body)
	return lipgloss.Place(m.width, max(4, m.height-5), lipgloss.Center, lipgloss.Center, box) + "\n" +
		dimStyle.Render(m.readerHint())
}

// readerHint is the reader footer. The search prompt takes it over while the
// operator types, and the match count stays after enter so n and N make sense.
func (m viewModel) readerHint() string {
	count := "no match"
	if len(m.matches) > 0 {
		count = fmt.Sprintf("%d/%d", m.matchIdx+1, len(m.matches))
	}
	switch {
	case m.searching:
		return "/" + m.query + "▏  " + count + " · enter keep · esc cancel"
	case m.query != "":
		return "/" + m.query + "  " + count + " · n next · N prev · esc back"
	}
	return "j/k scroll · esc back · d close"
}

// paintedLines is the visible slice of the reader, with the search painted in.
// A hit is painted on the display line the matches point at, so what the
// operator scrolls to and what lights up are the same line.
func (m viewModel) paintedLines(lines []string, end int) []string {
	out := append([]string(nil), lines[m.readerTop:end]...)
	if m.query == "" || len(m.matches) == 0 {
		return out
	}
	current := -1
	if m.matchIdx >= 0 && m.matchIdx < len(m.matches) {
		current = m.matches[m.matchIdx]
	}
	for i := range out {
		line := m.readerTop + i
		if !holdsLine(m.matches, line) {
			continue
		}
		style := matchStyle
		if line == current {
			style = currentMatchStyle
		}
		out[i] = highlightANSI(out[i], m.query, style)
	}
	return out
}

// holdsLine reports whether line is one of the sorted match lines.
func holdsLine(matches []int, line int) bool {
	i := sort.SearchInts(matches, line)
	return i < len(matches) && matches[i] == line
}

// highlightANSI paints every case-insensitive occurrence of query in style. It
// matches the visible text only: an escape sequence is never a hit, so a query
// such as "m" cannot land inside a colour code.
func highlightANSI(line, query string, style lipgloss.Style) string {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return line
	}
	// plain is the visible text and at maps each of its bytes to the byte it
	// starts at in line, escapes included.
	var plain strings.Builder
	var at []int
	for i := 0; i < len(line); {
		if n := escapeLen(line[i:]); n > 0 {
			i += n
			continue
		}
		at = append(at, i)
		plain.WriteByte(line[i])
		i++
	}
	vis := plain.String()
	lower := strings.ToLower(vis)
	if len(lower) != len(vis) {
		// Case folding changed the byte count, so offsets no longer map. Leave
		// the line alone rather than paint the wrong span.
		return line
	}
	var b strings.Builder
	from, prev := 0, 0
	for {
		j := strings.Index(lower[from:], q)
		if j < 0 {
			break
		}
		start, finish := from+j, from+j+len(q)
		rawStart := at[start]
		rawEnd := len(line)
		if finish < len(at) {
			rawEnd = at[finish]
		}
		b.WriteString(line[prev:rawStart])
		b.WriteString(style.Render(vis[start:finish]))
		prev = rawEnd
		from = finish
	}
	if prev == 0 {
		return line
	}
	b.WriteString(line[prev:])
	return b.String()
}

// escapeLen is the byte length of the escape sequence line starts with, or 0
// when it starts with none. Glamour emits SGR, and the reader must not read a
// parameter as text.
func escapeLen(line string) int {
	if len(line) == 0 || line[0] != 0x1b {
		return 0
	}
	if len(line) == 1 {
		return 1
	}
	switch line[1] {
	case '[': // CSI: parameters, then one final byte in @ to ~
		for i := 2; i < len(line); i++ {
			if line[i] >= 0x40 && line[i] <= 0x7e {
				return i + 1
			}
		}
		return len(line)
	case ']': // OSC ends at BEL or ST
		for i := 2; i < len(line); i++ {
			if line[i] == 0x07 {
				return i + 1
			}
			if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '\\' {
				return i + 2
			}
		}
		return len(line)
	}
	return 2
}

// readerMatches lists the display lines holding the query, case folded. The
// display is what the operator sees, so a hit is a line on screen, not a source
// line: the rendered line numbers are the ones the reader scrolls by.
func readerMatches(lines []string, query string) []int {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var out []int
	for i, line := range lines {
		if strings.Contains(strings.ToLower(ansi.Strip(line)), q) {
			out = append(out, i)
		}
	}
	return out
}

// searchReader recomputes the matches and jumps to the first one at or after
// the line the search started on, so refining a query keeps its place in the
// file instead of snapping back to the top.
func (m *viewModel) searchReader() {
	m.matches = readerMatches(m.readerDisplay(), m.query)
	if len(m.matches) == 0 {
		m.matchIdx = 0
		return
	}
	m.matchIdx = 0
	for i, line := range m.matches {
		if line >= m.searchTop {
			m.matchIdx = i
			break
		}
	}
	m.readerTop = m.matches[m.matchIdx]
}

// jumpMatch moves to a match and wraps at either end.
func (m *viewModel) jumpMatch(i int) {
	if len(m.matches) == 0 {
		return
	}
	n := len(m.matches)
	m.matchIdx = ((i % n) + n) % n
	m.readerTop = m.matches[m.matchIdx]
}

// reflowSearch re-finds the query after a re-render, which changes the display
// lines the matches point into. The match index survives.
func (m *viewModel) reflowSearch() {
	if m.query == "" {
		return
	}
	m.matches = readerMatches(m.readerDisplay(), m.query)
	m.jumpMatch(m.matchIdx)
}

// searchKey drives the reader's search prompt. Typing refines the query and
// previews the first hit; enter keeps it, esc puts the reader back where the
// search started.
func (m *viewModel) searchKey(key string) {
	switch key {
	case "esc":
		m.searching = false
		m.query, m.matches, m.matchIdx = "", nil, 0
		m.readerTop = m.searchTop
	case "enter":
		m.searching = false
		if len(m.matches) == 0 && m.query != "" {
			m.notice = "no match for " + m.query
		}
	case "ctrl+u":
		m.query, m.matches, m.matchIdx = "", nil, 0
		m.readerTop = m.searchTop
	case "backspace":
		if r := []rune(m.query); len(r) > 0 {
			m.query = string(r[:len(r)-1])
			m.searchReader()
		}
	default:
		if len([]rune(key)) == 1 {
			m.query += key
			m.searchReader()
		}
	}
}

func (m *viewModel) openArtifact() {
	u, ok := m.cardRow()
	if !ok {
		return
	}
	files := m.artifacts(u)
	if len(files) == 0 || m.artIdx >= len(files) {
		return
	}
	a := files[m.artIdx]
	if a.Err != "" {
		return
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		m.reader, m.readerLines = a.Path, []string{"error: " + err.Error()}
	} else {
		m.reader = a.Path
		m.readerLines = strings.Split(strings.ReplaceAll(string(data), "\t", "    "), "\n")
		if len(m.readerLines) > 5000 {
			m.readerLines = m.readerLines[:5000]
		}
	}
	m.readerTop = 0
	m.searching, m.query, m.matches, m.matchIdx = false, "", nil, 0
	m.overlay = "reader"
	m.renderReader()
}

func (m *viewModel) handleOverlayKey(key string) bool {
	switch m.overlay {
	case "detail":
		switch key {
		case "d":
			m.closeCard()
		case "esc":
			m.closeCard()
		case "j", "down", "tab", "l", "right":
			if n := len(m.artifacts(m.mustSelected())); n > 0 {
				m.artIdx = (m.artIdx + 1) % n
			}
		case "k", "up", "shift+tab", "h", "left":
			if n := len(m.artifacts(m.mustSelected())); n > 0 {
				m.artIdx = (m.artIdx + n - 1) % n
			}
		case "enter":
			m.openArtifact()
		default:
			return false
		}
		return true
	case "loop":
		rows := m.selectedLoopRounds()
		switch key {
		case "esc", "d":
			m.overlay, m.cardID = "", ""
		case "j", "down":
			if len(rows) > 0 {
				m.roundIdx = (m.roundIdx + 1) % len(rows)
			}
		case "k", "up":
			if len(rows) > 0 {
				m.roundIdx = (m.roundIdx + len(rows) - 1) % len(rows)
			}
		case "g", "home":
			m.roundIdx = 0
		case "G", "end":
			if len(rows) > 0 {
				m.roundIdx = len(rows) - 1
			}
		case "enter":
			m.openRoundCard()
		default:
			return false
		}
		return true
	case "reader":
		if m.searching {
			m.searchKey(key)
			return true
		}
		switch key {
		case "/":
			m.searching = true
			m.searchTop = m.readerTop
		case "n":
			m.jumpMatch(m.matchIdx + 1)
		case "N":
			m.jumpMatch(m.matchIdx - 1)
		case "j", "down":
			if m.readerTop+1 < len(m.readerDisplay()) {
				m.readerTop++
			}
		case "k", "up":
			if m.readerTop > 0 {
				m.readerTop--
			}
		case "g", "home":
			m.readerTop = 0
		case "G", "end":
			m.readerTop = max(0, len(m.readerDisplay())-1)
		case "esc", "d", "enter":
			m.query, m.matches, m.matchIdx = "", nil, 0
			m.overlay = "detail"
		default:
			return false
		}
		return true
	}
	return false
}

func (m viewModel) mustSelected() UnitRow {
	u, _ := m.cardRow()
	return u
}

// closeCard closes a card. A card opened from the loop timeline goes back to
// the timeline; every other card returns to the list.
func (m *viewModel) closeCard() {
	if m.cardID != "" {
		m.cardID = ""
		m.overlay = "loop"
		return
	}
	m.overlay = ""
}

// A loop round pairs a review with the fix it judges. The final review has no
// fix of its own: it judges the last round's fix.
type loopRoundRow struct {
	Round     int
	Final     bool
	Review    UnitRow
	Fix       UnitRow
	hasReview bool
	hasFix    bool
}

// loopChildKind reads the kind and round back out of a generated unit id. The
// loop row names the loop, so the suffix carries the rest.
func loopChildKind(loopID, unitID string) (string, int, bool) {
	rest := strings.TrimPrefix(unitID, loopID+"-")
	if rest == unitID {
		return "", 0, false
	}
	i := strings.LastIndex(rest, "-")
	if i < 0 {
		return "", 0, false
	}
	kind := rest[:i]
	if kind != "review" && kind != "fix" {
		return "", 0, false
	}
	round, err := strconv.Atoi(rest[i+1:])
	if err != nil {
		return "", 0, false
	}
	return kind, round, true
}

// loopRounds is one row per round, plus the final review when the loop has
// generated units. A loop with no generated units has no timeline rows.
func (m viewModel) loopRounds(l LoopRow) []loopRoundRow {
	if len(l.Units) == 0 || l.Rounds < 1 {
		return nil
	}
	rows := make([]loopRoundRow, l.Rounds)
	for i := range rows {
		rows[i].Round = i + 1
	}
	var final *loopRoundRow
	for _, id := range l.Units {
		kind, round, ok := loopChildKind(l.ID, id)
		if !ok {
			continue
		}
		u := m.unitByID(id)
		if u.ID == "" {
			continue
		}
		if kind == "review" && round == l.Rounds+1 {
			final = &loopRoundRow{Round: round, Final: true, Review: u, hasReview: true}
			continue
		}
		if round < 1 || round > l.Rounds {
			continue
		}
		switch kind {
		case "review":
			rows[round-1].Review, rows[round-1].hasReview = u, true
		case "fix":
			rows[round-1].Fix, rows[round-1].hasFix = u, true
		}
	}
	if final != nil {
		rows = append(rows, *final)
	}
	return rows
}

func (m viewModel) selectedLoop() *LoopRow {
	return m.snapLoop(m.selectedID())
}

func (m viewModel) selectedLoopRounds() []loopRoundRow {
	l := m.selectedLoop()
	if l == nil {
		return nil
	}
	return m.loopRounds(*l)
}

// currentRoundIndex opens the timeline on the loop's current round.
func (m viewModel) currentRoundIndex(l LoopRow) int {
	rows := m.loopRounds(l)
	if len(rows) == 0 {
		return 0
	}
	for i, r := range rows {
		if !r.Final && r.Round == l.Round {
			return i
		}
	}
	return min(max(l.Round-1, 0), len(rows)-1)
}

// roundPick is the unit a round's card and lane mean: the live one when there
// is one, else the review that judges the round.
func (m viewModel) roundPick(r loopRoundRow) (UnitRow, bool) {
	candidates := make([]UnitRow, 0, 2)
	if r.hasReview {
		candidates = append(candidates, r.Review)
	}
	if r.hasFix {
		candidates = append(candidates, r.Fix)
	}
	if len(candidates) == 0 {
		return UnitRow{}, false
	}
	for _, working := range []bool{true, false} {
		for _, u := range candidates {
			if u.Status == "working" && (!working || u.Pane != "") {
				return u, true
			}
		}
	}
	for _, u := range candidates {
		if u.Pane != "" {
			return u, true
		}
	}
	return candidates[0], true
}

func (m viewModel) roundUnit() (UnitRow, bool) {
	rows := m.selectedLoopRounds()
	if m.roundIdx < 0 || m.roundIdx >= len(rows) {
		return UnitRow{}, false
	}
	return m.roundPick(rows[m.roundIdx])
}

// liveLoopUnit is the loop's live lane, used when space f is pressed on the
// summary row itself.
func (m viewModel) liveLoopUnit(l LoopRow) (UnitRow, bool) {
	var any UnitRow
	for _, id := range l.Units {
		u := m.unitByID(id)
		if u.ID == "" {
			continue
		}
		if any.ID == "" {
			any = u
		}
		if u.Status == "working" && u.Pane != "" {
			return u, true
		}
	}
	for _, id := range l.Units {
		u := m.unitByID(id)
		if u.Status == "working" {
			return u, true
		}
	}
	for _, id := range l.Units {
		u := m.unitByID(id)
		if u.Pane != "" {
			return u, true
		}
	}
	return any, any.ID != ""
}

func (m *viewModel) openRoundCard() {
	u, ok := m.roundUnit()
	if !ok {
		return
	}
	m.cardID = u.ID
	m.artIdx = 0
	m.overlay = "detail"
}

// loopOverlay is the round timeline: what the loop is, where it is, and one
// line per round with the review and the fix that round produced.
func (m viewModel) loopOverlay() string {
	l := m.selectedLoop()
	if l == nil {
		return dimStyle.Render("no loop")
	}
	inner := max(20, min(m.width-6, 110))
	var b strings.Builder

	head := titleStyle.Render(l.ID) + "  " + statusStyles[l.State].Render(l.State) + dimStyle.Render("  loop")
	b.WriteString(head + "\n\n")
	b.WriteString("focus: " + wrap(collapseSpace(l.Focus), inner-7) + "\n")
	b.WriteString(fmt.Sprintf("cap: %d · current round: %d of %d\n", l.Rounds, l.Round, l.Rounds))
	b.WriteString("pass: " + wrap(collapseSpace(l.Pass), inner-6) + "\n")

	rows := m.loopRounds(*l)
	b.WriteString("\n" + sectionStyle.Render(fmt.Sprintf("ROUNDS %d", len(rows))) + "\n")
	if len(rows) == 0 {
		b.WriteString(dimStyle.Render("no rounds yet. the loop expands when it is dispatched.") + "\n")
	}
	for i, r := range rows {
		mark := "  "
		if i == m.roundIdx {
			mark = "▸ "
		}
		label := fmt.Sprintf("round %d", r.Round)
		if r.Final {
			label = "final"
		}
		review, fix := "-", "-"
		if r.hasReview {
			review = r.Review.Status
		}
		if r.hasFix {
			fix = r.Fix.Status
		}
		line := truncate(fmt.Sprintf("%s%s  review %-9s  fix %-9s", mark, pad(label, 7), review, fix), inner)
		if i == m.roundIdx {
			b.WriteString(selectedStyle.Render(line) + "\n")
		} else {
			b.WriteString(line + "\n")
		}
		// A failed verdict carries the reason in the unit's note. Name it under
		// the round, so the operator sees what the reviewer actually saw.
		note := ""
		if r.hasReview && r.Review.Status == "failed" {
			note = r.Review.Note
		}
		if note == "" && r.hasFix && r.Fix.Status == "failed" {
			note = r.Fix.Note
		}
		if note != "" {
			for _, line := range strings.Split(wrap("note: "+collapseSpace(note), max(10, inner-4)), "\n") {
				b.WriteString(dimStyle.Render("    "+line) + "\n")
			}
		}
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(inner).
		Render(b.String())
	hint := dimStyle.Render("j/k round · enter card · space f lane · esc close")
	return lipgloss.Place(m.width, max(4, m.height-5), lipgloss.Center, lipgloss.Center, box) + "\n" + hint
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
}
