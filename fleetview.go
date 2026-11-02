package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The fleet view is a renderer over FleetModel. It reads no plan and no state
// of its own: every fact on screen is in the model, and the only live read is
// the selected lane's preview on the refresh tick.

var fleetSectionDefs = []struct {
	label string
	style string
}{
	{"BLOCKED", "blocked"},
	{"WORKING", "working"},
	{"READY", "ready"},
	{"SETTLED", "done"},
}

type fleetItem struct {
	row       FleetRow
	encounter string
	label     string
	status    string
	summary   bool
	broken    bool
	live      bool
	count     int
	err       string
	lastTouch time.Time
}

func fleetRowKey(row FleetRow) string {
	return "row\x00" + row.Encounter + "\x00" + row.ID
}

func fleetSummaryKey(encounter string) string {
	return "summary\x00" + encounter
}

func fleetItemKey(it fleetItem) string {
	if it.summary {
		return fleetSummaryKey(it.encounter)
	}
	if it.broken {
		return "broken\x00" + it.encounter
	}
	return fleetRowKey(it.row)
}

type fleetMsg struct {
	model         FleetModel
	keep          string
	preview       string
	previewFor    string
	previewHeader string
	previewErr    string
}

type fleetNoticeMsg struct {
	text string
	err  error
}

type fleetViewModel struct {
	interval time.Duration
	backend  string
	cache    *FleetCache
	run      CommandRunner

	model  FleetModel
	err    error
	notice string

	overlay string // "", "detail" or "actions"
	action  int

	expanded map[string]bool

	cursor int

	preview       string
	previewFor    string
	previewHeader string
	previewErr    string

	width  int
	height int
}

func newFleetViewModel(interval time.Duration, backend string) fleetViewModel {
	return fleetViewModel{
		interval: interval,
		backend:  backend,
		cache:    &FleetCache{},
		expanded: map[string]bool{},
		width:    120,
		height:   30,
	}
}

func (m fleetViewModel) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(m.interval))
}

func (m fleetViewModel) refresh() tea.Cmd {
	backend, keep := m.backend, m.selectedKey()
	expanded := m.expanded
	cache, run := m.cache, m.run
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		model := BuildFleetModel(ctx, FleetOptions{Run: run, Cache: cache})
		if keep == "" {
			keep = fleetFirstKey(model, expanded)
		}
		preview, forKey, header, previewErr := fleetPreview(ctx, backend, model, keep)
		return fleetMsg{
			model:         model,
			keep:          keep,
			preview:       preview,
			previewFor:    forKey,
			previewHeader: header,
			previewErr:    previewErr,
		}
	}
}

func (m fleetViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.refresh(), tick(m.interval))

	case fleetMsg:
		m.model = msg.model
		if msg.keep == "" {
			msg.keep = fleetFirstKey(msg.model, m.expanded)
		}
		m.rebuildOrder(msg.keep)
		m.preview = msg.preview
		m.previewFor = msg.previewFor
		m.previewHeader = msg.previewHeader
		m.previewErr = msg.previewErr
		m.err = nil
		return m, nil

	case fleetNoticeMsg:
		if msg.err != nil {
			m.notice = msg.text + ": " + msg.err.Error()
		} else {
			m.notice = msg.text
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.key(msg.String())
	}
	return m, nil
}

func (m *fleetViewModel) key(key string) tea.Cmd {
	if key == "ctrl+c" || key == "q" {
		return tea.Quit
	}
	if m.overlay == "actions" {
		return m.actionKey(key)
	}
	if m.overlay == "detail" {
		if key == "esc" {
			m.overlay = ""
		}
		return nil
	}
	m.notice = ""
	switch key {
	case "esc":
		// esc backs out of nothing here and never quits: the fleet view is the
		// only window an operator has on detached lanes, so q is the quit key.
		return nil
	case "j", "down":
		m.cursor++
	case "k", "up":
		m.cursor--
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = len(m.items()) - 1
	case "enter", "d":
		if _, ok := m.selectedItem(); ok {
			m.overlay = "detail"
		}
	case " ":
		if _, ok := m.selectedItem(); ok {
			m.overlay = "actions"
			m.action = 0
		}
	}
	m.clampCursor()
	return nil
}

type fleetAction struct {
	key   string
	label string
}

var fleetActions = []fleetAction{
	{"j", "jump to the lane"},
	{"h", "open the hunk session"},
	{"c", "copy the pane id"},
	{"e", "expand or fold settled units"},
}

func (m *fleetViewModel) actionKey(key string) tea.Cmd {
	switch key {
	case "esc", " ":
		m.overlay = ""
		return nil
	case "j", "down":
		m.action++
		if m.action >= len(fleetActions) {
			m.action = 0
		}
		return nil
	case "k", "up":
		m.action--
		if m.action < 0 {
			m.action = len(fleetActions) - 1
		}
		return nil
	case "enter":
		it, ok := m.selectedItem()
		m.overlay = ""
		if !ok {
			return nil
		}
		switch fleetActions[m.action].key {
		case "j":
			return m.jumpCmd(it)
		case "h":
			return m.hunkCmd(it)
		case "c":
			return m.copyPaneCmd(it)
		case "e":
			m.expanded[it.encounter] = !m.expanded[it.encounter]
			m.rebuildOrder("")
		}
	}
	m.notice = "no action on " + key
	return nil
}

func (m fleetViewModel) jumpCmd(it fleetItem) tea.Cmd {
	row, backend := it.row, m.backend
	if row.Pane == "" {
		return func() tea.Msg { return fleetNoticeMsg{text: "no pane on this row"} }
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h, err := openMux(ctx, backend)
		if err != nil {
			return fleetNoticeMsg{text: "cannot jump", err: err}
		}
		agents, err := h.Agents(ctx)
		if err != nil {
			return fleetNoticeMsg{text: "cannot read the lanes", err: err}
		}
		for name, agent := range agents {
			if agent.Pane == row.Pane {
				if err := h.FocusAgent(ctx, name); err != nil {
					return fleetNoticeMsg{text: "cannot jump", err: err}
				}
				return fleetNoticeMsg{text: "focused " + name}
			}
		}
		at, err := h.Pane(ctx, row.Pane)
		if err != nil {
			return fleetNoticeMsg{text: "pane is gone", err: err}
		}
		if err := h.FocusTab(ctx, at.Tab); err != nil {
			return fleetNoticeMsg{text: "cannot jump", err: err}
		}
		return fleetNoticeMsg{text: "focused pane " + row.Pane}
	}
}

func (m fleetViewModel) hunkCmd(it fleetItem) tea.Cmd {
	row := it.row
	if row.Review == nil || row.Review.Session == "" {
		return func() tea.Msg { return fleetNoticeMsg{text: "no hunk session on this worktree"} }
	}
	dir := row.Worktree
	if dir == "" {
		return func() tea.Msg { return fleetNoticeMsg{text: "no worktree for this row"} }
	}
	cmd := exec.Command("hunk", "diff")
	cmd.Dir = dir
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return fleetNoticeMsg{text: "hunk", err: err}
		}
		return fleetNoticeMsg{text: "closed the hunk session"}
	})
}

func (m fleetViewModel) copyPaneCmd(it fleetItem) tea.Cmd {
	if it.row.Pane == "" {
		return func() tea.Msg { return fleetNoticeMsg{text: "no pane id on this row"} }
	}
	pane := it.row.Pane
	return func() tea.Msg {
		// OSC 52 asks the terminal to copy, so no clipboard tool is needed and
		// the pane never leaves the fleet view.
		_, _ = fmt.Fprintf(os.Stdout, "\x1b]52;c;%s\a", base64.StdEncoding.EncodeToString([]byte(pane)))
		return fleetNoticeMsg{text: "copied " + pane}
	}
}

func (m *fleetViewModel) clampCursor() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if n := len(m.items()); n > 0 && m.cursor >= n {
		m.cursor = n - 1
	}
}

func (m *fleetViewModel) rebuildOrder(keep string) {
	m.cursor = 0
	for i, it := range m.items() {
		if keep != "" && fleetItemKey(it) == keep {
			m.cursor = i
			break
		}
	}
	m.clampCursor()
}

func (m fleetViewModel) selectedItem() (fleetItem, bool) {
	items := m.items()
	if m.cursor < 0 || m.cursor >= len(items) {
		return fleetItem{}, false
	}
	return items[m.cursor], true
}

func (m fleetViewModel) selectedKey() string {
	it, ok := m.selectedItem()
	if !ok {
		return ""
	}
	return fleetItemKey(it)
}

type fleetGroup struct {
	name  string
	touch time.Time
	err   string
}

func fleetGroups(model FleetModel) []fleetGroup {
	groups := map[string]fleetGroup{}
	for _, enc := range model.Encounters {
		g := groups[enc.Name]
		g.name = enc.Name
		if enc.LastTouch.After(g.touch) {
			g.touch = enc.LastTouch
		}
		if enc.Err != "" {
			g.err = enc.Err
		}
		groups[enc.Name] = g
	}
	for _, row := range model.Rows {
		g := groups[row.Encounter]
		g.name = row.Encounter
		if row.LastTouch.After(g.touch) {
			g.touch = row.LastTouch
		}
		groups[row.Encounter] = g
	}
	out := make([]fleetGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].touch.Equal(out[j].touch) {
			return out[i].touch.After(out[j].touch)
		}
		return out[i].name < out[j].name
	})
	return out
}

func fleetSectionOf(status string) int {
	switch status {
	case "blocked", "failed", "unreadable":
		return 0
	case "working":
		return 1
	case "done", "canceled":
		return 3
	default:
		return 2
	}
}

func fleetItems(model FleetModel, expanded map[string]bool) []fleetItem {
	groups := fleetGroups(model)
	rowsByEncounter := map[string][]FleetRow{}
	for _, row := range model.Rows {
		rowsByEncounter[row.Encounter] = append(rowsByEncounter[row.Encounter], row)
	}

	var items []fleetItem
	for section := range fleetSectionDefs {
		for _, g := range groups {
			rows := rowsByEncounter[g.name]
			if section == 3 {
				var settled []FleetRow
				for _, row := range rows {
					if fleetSectionOf(row.Status) == 3 {
						settled = append(settled, row)
					}
				}
				if len(settled) == 0 {
					continue
				}
				live := len(settled) < len(rows)
				if expanded[g.name] {
					for _, row := range settled {
						items = append(items, fleetItem{row: row, encounter: g.name, label: row.ID, status: row.Status, lastTouch: row.LastTouch})
					}
					continue
				}
				items = append(items, fleetItem{
					encounter: g.name,
					label:     g.name,
					status:    "done",
					summary:   true,
					live:      live,
					count:     len(settled),
					lastTouch: g.touch,
				})
				continue
			}
			for _, row := range rows {
				if fleetSectionOf(row.Status) != section {
					continue
				}
				items = append(items, fleetItem{row: row, encounter: g.name, label: row.ID, status: row.Status, lastTouch: row.LastTouch})
			}
			if section == 0 && g.err != "" {
				items = append(items, fleetItem{
					encounter: g.name,
					label:     g.name,
					status:    "unreadable",
					broken:    true,
					err:       g.err,
					lastTouch: g.touch,
				})
			}
		}
	}
	return items
}

func (m fleetViewModel) items() []fleetItem {
	return fleetItems(m.model, m.expanded)
}

func fleetFirstKey(model FleetModel, expanded map[string]bool) string {
	for _, it := range fleetItems(model, expanded) {
		if !it.summary {
			return fleetItemKey(it)
		}
	}
	if items := fleetItems(model, expanded); len(items) > 0 {
		return fleetItemKey(items[0])
	}
	return ""
}

// fleetPreview reads the selected lane's pane, or its report when the lane is
// gone. It is called once per refresh tick, never from a key handler.
func fleetPreview(ctx context.Context, backend string, model FleetModel, key string) (text, forKey, header, errText string) {
	row, ok := fleetRowByKey(model, key)
	if !ok {
		return "", "", "", ""
	}
	forKey = key
	report := fleetReportText(model, row)
	if row.Pane == "" {
		if report != "" {
			return report, forKey, "report " + row.ID, ""
		}
		return "", forKey, "no live lane", "no live lane and no report"
	}
	h, err := openMux(ctx, backend)
	if err == nil {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		out, readErr := h.Read(readCtx, row.Pane)
		cancel()
		if readErr == nil {
			return out, forKey, "lane " + row.Pane, ""
		}
		err = readErr
	}
	if report != "" {
		return report, forKey, "report " + row.ID, "lane unreadable: " + err.Error()
	}
	return "", forKey, "lane " + row.Pane, err.Error()
}

func fleetRowByKey(model FleetModel, key string) (FleetRow, bool) {
	for _, row := range model.Rows {
		if fleetRowKey(row) == key {
			return row, true
		}
	}
	return FleetRow{}, false
}

func fleetReportText(model FleetModel, row FleetRow) string {
	path := fleetReportPath(model, row)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func fleetReportPath(model FleetModel, row FleetRow) string {
	for _, enc := range model.Encounters {
		if enc.Name == row.Encounter || filepath.Base(filepath.Dir(enc.Path)) == row.Encounter {
			return filepath.Join(filepath.Dir(enc.Path), "reports", row.ID+".md")
		}
	}
	return ""
}

func fleetLane(row FleetRow) string {
	lane := row.Minion
	if row.Role != "" {
		if lane != "" {
			lane += " "
		}
		lane += row.Role
	}
	if lane == "" {
		return "-"
	}
	return lane
}

func fleetChanged(row FleetRow) string {
	if row.WT == nil || (row.WT.Added == nil && row.WT.Deleted == nil) {
		return "-"
	}
	added, deleted := "?", "?"
	if row.WT.Added != nil {
		added = fmt.Sprint(*row.WT.Added)
	}
	if row.WT.Deleted != nil {
		deleted = fmt.Sprint(*row.WT.Deleted)
	}
	return "+" + added + "/-" + deleted
}

func fleetAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return age(t.UTC().Format(time.RFC3339))
}

func fleetDid(row FleetRow) string {
	if row.LastReport != nil {
		return "report " + fleetAge(*row.LastReport)
	}
	if row.Status != "" {
		return "status " + row.Status
	}
	return "-"
}

func fleetItemDid(it fleetItem) string {
	if it.broken {
		return truncate(collapseSpace(it.err), 60)
	}
	if it.summary {
		if it.live {
			return fmt.Sprintf("%d settled", it.count)
		}
		return fmt.Sprintf("%d settled, nothing live", it.count)
	}
	return fleetDid(it.row)
}

type fleetMetrics struct {
	status  int
	id      int
	lane    int
	pane    int
	branch  int
	changed int
	age     int
}

func (m fleetViewModel) metrics(items []fleetItem) fleetMetrics {
	mt := fleetMetrics{status: 6, id: 4, lane: 4, pane: 4, branch: 4, changed: 6, age: 4}
	for _, it := range items {
		mt.status = max(mt.status, runeLen(it.status))
		mt.id = max(mt.id, runeLen(it.label))
		mt.lane = max(mt.lane, runeLen(fleetLane(it.row)))
		mt.pane = max(mt.pane, runeLen(orDash(it.row.Pane)))
		mt.branch = max(mt.branch, runeLen(orDash(it.row.Branch)))
		mt.changed = max(mt.changed, runeLen(fleetChanged(it.row)))
		mt.age = max(mt.age, runeLen(fleetAge(it.lastTouch)))
	}
	mt.id = min(mt.id, 28)
	mt.lane = min(mt.lane, 20)
	mt.pane = min(mt.pane, 8)
	mt.branch = min(mt.branch, 26)
	return mt
}

func (m fleetViewModel) rowLine(it fleetItem, mt fleetMetrics, width int, selected bool) string {
	status := pad(truncate(it.status, mt.status), mt.status)
	id := pad(truncate(it.label, mt.id), mt.id)
	lane := pad(truncate(fleetLane(it.row), mt.lane), mt.lane)
	pane := pad(truncate(orDash(it.row.Pane), mt.pane), mt.pane)
	branch := pad(truncate(orDash(it.row.Branch), mt.branch), mt.branch)
	changed := pad(truncate(fleetChanged(it.row), mt.changed), mt.changed)
	when := pad(truncate(fleetAge(it.lastTouch), mt.age), mt.age)
	did := fleetItemDid(it)
	plain := fmt.Sprintf("%s  %s  %s  %s  %s  %s  %s  %s", status, id, lane, pane, branch, changed, when, did)
	plain = truncate(plain, max(1, width))
	if selected {
		return selectedStyle.Render(pad(plain, width))
	}
	style := statusStyles[it.status]
	if it.broken {
		style = statusStyles["blocked"]
	}
	return style.Render(status) + "  " + boldStyle.Render(id) + "  " +
		dimStyle.Render(lane+"  "+pane+"  "+branch+"  "+changed+"  "+when+"  "+did)
}

func (m fleetViewModel) listLines(width int) ([]string, int) {
	items := m.items()
	if len(items) == 0 {
		return []string{dimStyle.Render("no encounters under .dungeon")}, 0
	}
	mt := m.metrics(items)
	var lines []string
	cursorLine := 0
	selected := m.selectedKey()
	section := -1
	sectionCount := map[int]int{}
	for _, it := range items {
		sectionCount[fleetSectionOf(it.status)]++
	}
	for _, it := range items {
		sec := fleetSectionOf(it.status)
		if sec != section {
			if section != -1 {
				lines = append(lines, "")
			}
			lines = append(lines, sectionStyle.Render(fmt.Sprintf("%s %d", fleetSectionDefs[sec].label, sectionCount[sec])))
			section = sec
		}
		if fleetItemKey(it) == selected {
			cursorLine = len(lines)
		}
		lines = append(lines, m.rowLine(it, mt, width, fleetItemKey(it) == selected))
	}
	return lines, cursorLine
}

func (m fleetViewModel) render() string {
	if m.err != nil {
		return titleStyle.Render("dungeon fleet") + "\n\n" + m.err.Error() + "\n\n" +
			dimStyle.Render("space r to retry, q to quit") + "\n"
	}
	var body string
	switch m.overlay {
	case "detail":
		body = m.detailOverlay()
	case "actions":
		body = m.listBody()
		body = overlayBottomRight(body, m.actionBox(), m.width)
	default:
		body = m.listBody()
	}
	return m.headerView() + "\n" + body + "\n" + m.footerView()
}

// View clamps the frame to the terminal exactly as the encounter view does.
func (m fleetViewModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return m.render()
	}
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(m.render())
}

func (m fleetViewModel) headerView() string {
	left := titleStyle.Render("dungeon fleet")
	counts := map[int]int{}
	for _, it := range m.items() {
		counts[fleetSectionOf(it.status)]++
	}
	right := dimStyle.Render(fmt.Sprintf("%d encounters · %d units", len(m.model.Encounters), len(m.model.Rows)))
	chips := ""
	for i, sec := range fleetSectionDefs {
		if n := counts[i]; n > 0 {
			chips += "  " + statusStyles[sec.style].Render(fmt.Sprintf("%d %s", n, strings.ToLower(sec.label)))
		}
	}
	return spread(left, right, m.width) + "\n" + chips + "\n" +
		ruleStyle.Render(strings.Repeat("─", max(0, m.width)))
}

func (m fleetViewModel) listBody() string {
	previewH := min(10, max(3, (m.height-5)/3))
	rows := max(3, m.height-5-previewH)
	lines, cursorLine := m.listLines(m.width)
	lines = window(lines, cursorLine, rows)
	body := strings.Join(lines, "\n")
	if pad := rows - len(lines); pad > 0 {
		body += strings.Repeat("\n", pad)
	}
	return body + "\n" + m.previewView(previewH)
}

func (m fleetViewModel) previewView(height int) string {
	head := sectionStyle.Render("PREVIEW")
	if m.previewFor != "" {
		head += " " + dimStyle.Render(m.previewHeader)
	}
	lines := []string{ruleStyle.Render(strings.Repeat("─", max(0, m.width))), head}
	var text []string
	if m.preview != "" {
		text = strings.Split(strings.ReplaceAll(m.preview, "\t", "    "), "\n")
		for i := range text {
			text[i] = truncate(text[i], max(1, m.width))
		}
		if len(text) > height-2 {
			text = text[len(text)-(height-2):]
		}
	} else if m.previewErr != "" {
		text = []string{dimStyle.Render(truncate(m.previewErr, max(1, m.width)))}
	} else {
		text = []string{dimStyle.Render("no lane selected")}
	}
	lines = append(lines, text...)
	return strings.Join(lines, "\n")
}

func (m fleetViewModel) detailOverlay() string {
	it, ok := m.selectedItem()
	if !ok {
		return dimStyle.Render("no row selected")
	}
	inner := max(30, min(m.width-6, 110))
	var b strings.Builder
	if it.broken {
		b.WriteString(titleStyle.Render(it.label) + "  " + statusStyles["blocked"].Render("unreadable") + "\n\n")
		b.WriteString(wrap(it.err, inner) + "\n")
	} else if it.summary {
		b.WriteString(titleStyle.Render(it.label) + "  " + statusStyles["done"].Render("settled") + "\n\n")
		b.WriteString(fmt.Sprintf("%d settled units · last touch %s\n", it.count, fleetAge(it.lastTouch)))
		if !it.live {
			b.WriteString(dimStyle.Render("nothing live in this encounter") + "\n")
		}
	} else {
		row := it.row
		b.WriteString(titleStyle.Render(row.ID) + "  " + statusStyles[row.Status].Render(row.Status) +
			dimStyle.Render(fmt.Sprintf("  %s %s", row.Role, row.Minion)) + "\n\n")
		fields := [][2]string{
			{"encounter", row.Encounter},
			{"pane", orDash(row.Pane)},
			{"branch", orDash(row.Branch)},
			{"worktree", orDash(row.Worktree)},
			{"changed", fleetChanged(row)},
			{"ahead / dirty", fmt.Sprintf("%d / %d", row.Ahead, row.Dirty)},
			{"accept", orDash(row.Accept)},
			{"last report", "-"},
			{"last touch", fleetAge(row.LastTouch)},
		}
		if row.LastReport != nil {
			fields[7][1] = fleetAge(*row.LastReport)
		}
		if row.Review != nil {
			fields = append(fields, [2]string{"hunk session", row.Review.Session})
		}
		if row.WT != nil && row.WT.PR != nil {
			fields = append(fields, [2]string{"pull request", fmt.Sprintf("#%d %s", row.WT.PR.Number, row.WT.PR.Review)})
		}
		if row.WT != nil && row.WT.Checks != nil {
			fields = append(fields, [2]string{"checks", row.WT.Checks.Status})
		}
		for _, field := range fields {
			b.WriteString(dimStyle.Render(fmt.Sprintf("%-14s", field[0])) + truncate(field[1], max(1, inner-14)) + "\n")
		}
	}
	if m.preview != "" {
		b.WriteString("\n" + sectionStyle.Render("PREVIEW") + "\n")
		preview := strings.Split(strings.ReplaceAll(m.preview, "\t", "    "), "\n")
		if len(preview) > 12 {
			preview = preview[len(preview)-12:]
		}
		for _, line := range preview {
			b.WriteString(truncate(line, inner) + "\n")
		}
	} else if m.previewErr != "" {
		b.WriteString("\n" + dimStyle.Render(truncate(m.previewErr, inner)) + "\n")
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(inner).
		Render(b.String())
	return box
}

func (m fleetViewModel) actionBox() string {
	rows := []string{dimStyle.Render("space") + "   " + dimStyle.Render("esc closes")}
	for i, action := range fleetActions {
		line := fmt.Sprintf("%s  %s", action.key, action.label)
		if i == m.action {
			line = selectedStyle.Render(line)
		}
		rows = append(rows, line)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63")).
		Padding(0, 1).
		Render(strings.Join(rows, "\n"))
}

func (m fleetViewModel) footerView() string {
	keys := "j/k move · enter detail · space actions · q quit"
	if m.overlay == "detail" {
		keys = "esc close · q quit"
	}
	if m.overlay == "actions" {
		keys = "j/k choose · enter run · esc close · q quit"
	}
	if m.notice != "" {
		keys = m.notice
	}
	return ruleStyle.Render(strings.Repeat("─", max(0, m.width))) + "\n" + dimStyle.Render(keys)
}

func fleetCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("fleet", stderr)
	interval := fs.Duration("interval", 2*time.Second, "how often to re-read the fleet")
	backend := muxFlag(fs)
	once := fs.Bool("once", false, "print one frame and exit")
	asJSON := fs.Bool("json", false, "print the fleet model as JSON")
	width := fs.Int("width", 120, "frame width for --once")
	height := fs.Int("height", 30, "frame height for --once")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "dungeon: fleet reads every encounter; it takes no plan")
		return 2
	}

	m := newFleetViewModel(*interval, *backend)
	m.width, m.height = *width, *height
	model := BuildFleetModel(ctx, FleetOptions{Cache: m.cache})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(model); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		return 0
	}
	if *once || !isTerminal(stdout) {
		m.model = model
		keep := fleetFirstKey(model, m.expanded)
		m.rebuildOrder(keep)
		m.preview, m.previewFor, m.previewHeader, m.previewErr = fleetPreview(ctx, *backend, model, keep)
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
