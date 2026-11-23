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

// The fleet view is a renderer over FleetModel. The list reads no plan and no
// state of its own: every fact on screen is in the model, and the only live
// read is the selected lane's preview on the refresh tick. An opened encounter
// is the exception: it keeps a whole viewModel, refreshed on the same tick, so
// the split shows the encounter view's own facts and not a copy of them.

const (
	fleetSectionWorking = iota
	fleetSectionBlocked
	fleetSectionReady
	fleetSectionFailed
	fleetSectionSettled
)

// Sections in screen order. WORKING leads because the live lane is what an
// operator is watching, and a failed verdict is history: the fix that answered
// it never rewrites the failed unit, so failures would otherwise own the screen.
// FAILED and SETTLED fold to one row per encounter.
var fleetSectionDefs = []struct {
	label    string
	style    string
	folded   bool
	foldStat string // the status a folded summary carries, empty when it never folds
}{
	{"WORKING", "working", false, ""},
	{"BLOCKED", "blocked", false, ""},
	{"READY", "ready", false, ""},
	{"FAILED", "failed", true, "failed"},
	{"SETTLED", "done", true, "done"},
}

type fleetItem struct {
	row       FleetRow
	encounter string
	label     string
	status    string
	summary   bool
	fold      string // the fold key a summary toggles, empty on a unit row
	broken    bool
	live      bool
	count     int
	err       string
	lastTouch time.Time
}

func fleetRowKey(row FleetRow) string {
	return "row\x00" + row.Encounter + "\x00" + row.ID
}

func fleetItemKey(it fleetItem) string {
	if it.summary {
		return "summary\x00" + it.fold
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
	at            time.Time
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
	// modelAt is when the shown snapshot was built. It is the only clock the
	// view holds, and the honest age of every fact the model carries.
	modelAt time.Time

	overlay string // "", "detail" or "actions"
	action  int

	expanded map[string]bool

	// plan is the encounter opened under the fleet, nil when closed. focus is
	// "fleet" or "plan": the plan keeps the whole viewModel, so the innards of
	// a running encounter are one enter away and need no second process.
	plan        *viewModel
	planName    string
	planLoading bool
	want        string
	focus       string

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
		at := time.Now()
		if keep == "" {
			keep = fleetFirstKey(model, expanded)
		}
		var preview, forKey, header, previewErr string
		// The plan pane owns the rows the preview would use, so skip the pane
		// capture while it is open and pay for it again once the split closes.
		if m.plan == nil {
			preview, forKey, header, previewErr = fleetPreview(ctx, backend, model, keep)
		}
		return fleetMsg{
			model:         model,
			keep:          keep,
			preview:       preview,
			previewFor:    forKey,
			previewHeader: header,
			previewErr:    previewErr,
			at:            at,
		}
	}
}

func (m fleetViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sizePlan()
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.planCmd(), m.refresh(), tick(m.interval))

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
		m.modelAt = msg.at
		m.err = nil
		return m, nil

	case refreshedMsg:
		// The plan pane's own loads arrive here. The fleet model sends fleetMsg,
		// so the two never mix.
		if m.plan == nil {
			return m, nil
		}
		*m.plan = m.plan.applyRefresh(msg)
		m.planLoading = false
		if m.want != "" {
			m.plan.rebuildOrder(m.want)
			m.want = ""
		}
		return m, nil

	case fleetNoticeMsg:
		if msg.err != nil {
			m.notice = msg.text + ": " + msg.err.Error()
		} else {
			m.notice = msg.text
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.keyMsg(msg)
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
	case "tab":
		if m.plan != nil {
			m.focus = "plan"
		}
		return nil
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
	case "enter":
		if it, ok := m.selectedItem(); ok {
			return m.openPlan(it)
		}
	case "d":
		if _, ok := m.selectedItem(); ok {
			m.overlay = "detail"
		}
	case "r":
		return m.refresh()
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
	{"f", "jump to the lane"},
	{"h", "open the hunk session"},
	{"c", "copy the pane id"},
	{"e", "expand or fold failed and settled units"},
}

func (m *fleetViewModel) actionKey(key string) tea.Cmd {
	// The key printed in the box runs its action. Only j, k and enter move the
	// cursor, so a named shortcut needs no second keypress.
	for i, action := range fleetActions {
		if key == action.key {
			m.action = i
			key = "enter"
			break
		}
	}
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
		case "f":
			return m.jumpCmd(it)
		case "h":
			return m.hunkCmd(it)
		case "c":
			return m.copyPaneCmd(it)
		case "e":
			if it.fold == "" {
				m.notice = "not a folded row: failed and settled units fold"
				return nil
			}
			m.expanded[it.fold] = !m.expanded[it.fold]
			// The summary becomes its units, or the reverse. Follow the encounter
			// in the same section, so a fold never throws the cursor to the top.
			m.rebuildOrder(fleetFoldCursorKey(m.items(), it))
		}
	}
	if key != "enter" {
		m.notice = "no action on " + key
	}
	return nil
}

// keyMsg routes a key to the pane that holds the focus. The plan pane is a
// whole dungeon view, so it takes every key it can use except the two that
// move between the panes.
func (m *fleetViewModel) keyMsg(msg tea.KeyMsg) tea.Cmd {
	if m.plan != nil && m.focus == "plan" {
		return m.planKey(msg)
	}
	return m.key(msg.String())
}

func (m *fleetViewModel) planKey(msg tea.KeyMsg) tea.Cmd {
	p := m.plan
	switch msg.String() {
	case "tab":
		m.focus = "fleet"
		return nil
	case "esc":
		if m.planAtRest() {
			m.closePlan()
			// The preview comes back the moment the split closes, not on the tick.
			return m.refresh()
		}
	}
	next, cmd := p.Update(msg)
	*p = next.(viewModel)
	return cmd
}

// planAtRest is true when the plan pane holds nothing of its own, so esc hands
// the keyboard back to the fleet instead of closing something inside the plan.
func (m *fleetViewModel) planAtRest() bool {
	p := m.plan
	return p == nil || (p.overlay == "" && !p.help && !p.prefix && !p.choiceMode())
}

// openPlan shows one encounter under the fleet. A unit row lands the plan
// cursor on that unit, so the innards open where the operator was looking.
func (m *fleetViewModel) openPlan(it fleetItem) tea.Cmd {
	var enc *FleetEncounter
	for i := range m.model.Encounters {
		if m.model.Encounters[i].Name == it.encounter {
			enc = &m.model.Encounters[i]
			break
		}
	}
	switch {
	case enc == nil:
		m.notice = "no plan for " + it.encounter
		return nil
	case enc.Err != "":
		m.notice = "the plan will not read: " + enc.Err
		return nil
	}
	if m.plan != nil && m.planName == it.encounter {
		if !it.summary && !it.broken && it.row.ID != "" {
			m.plan.rebuildOrder(it.row.ID)
		}
		m.focus = "plan"
		return nil
	}
	p := newViewModel(enc.Path, StatePath(enc.Path), m.interval)
	p.backend = m.backend
	m.plan, m.planName, m.planLoading = &p, it.encounter, true
	m.focus = "plan"
	m.want = ""
	if !it.summary && !it.broken {
		m.want = it.row.ID
	}
	m.sizePlan()
	return p.refresh()
}

func (m *fleetViewModel) closePlan() {
	m.plan = nil
	m.planName = ""
	m.planLoading = false
	m.want = ""
	m.focus = "fleet"
}

// planCmd re-reads the open plan on the fleet's own tick. One ticker drives
// both panes, so the split never drifts against itself.
func (m *fleetViewModel) planCmd() tea.Cmd {
	if m.plan == nil {
		return nil
	}
	return m.plan.refresh()
}

// splitHeights gives the fleet the top third of the frame and the open plan
// the rest. The plan keeps at least four rows: it is why the split exists.
func (m fleetViewModel) splitHeights() (fleetH, planH int) {
	fleetH = max(6, min(14, m.height/3))
	fleetH = min(fleetH, max(3, m.height-9))
	planH = max(4, m.height-fleetH-2)
	return fleetH, planH
}

// sizePlan gives the plan pane the geometry it renders to. viewModel draws to
// its own width and height, so the split owns that pair, not the plan.
func (m *fleetViewModel) sizePlan() {
	if m.plan == nil {
		return
	}
	_, planH := m.splitHeights()
	m.plan.width, m.plan.height = m.width, planH
}

// splitView puts the fleet on top and the open plan underneath. The plan keeps
// its own header, its own footer and its own keys: the split is a geometry,
// not a second renderer.
func (m fleetViewModel) splitView() string {
	if m.height < 16 {
		// Too short for two panes: the plan takes the frame, and the fleet keeps
		// only the footer hint that names it.
		return m.planPane(max(4, m.height-2)) + "\n" + m.footerView()
	}
	fleetH, planH := m.splitHeights()
	if m.overlay == "actions" {
		// The box needs its rows, and the plan lends them while it is open.
		fleetH = min(max(fleetH, 10), max(3, m.height-9))
		planH = max(4, m.height-fleetH-2)
	}
	top := m.fleetPane(fleetH)
	if m.overlay == "actions" {
		top = overlayBottomRight(top, m.actionBox(), m.width)
	}
	return top + "\n" + m.planPane(planH) + "\n" + m.footerView()
}

// fleetPane draws the list without the preview, sized to the split.
func (m fleetViewModel) fleetPane(height int) string {
	rows := max(1, height-m.headerHeight())
	lines, cursorLine := m.listLines(m.width)
	lines = window(lines, cursorLine, rows)
	block := m.headerView() + "\n" + strings.Join(lines, "\n")
	if pad := height - m.headerHeight() - len(lines); pad > 0 {
		block += strings.Repeat("\n", pad)
	}
	return block
}

// planPane draws the open plan at the height the split gives it. The plan
// renders to its own size, so the split hands it one before every frame.
func (m fleetViewModel) planPane(height int) string {
	if m.plan == nil {
		return ""
	}
	if m.planLoading {
		return dimStyle.Render(truncate("loading "+m.planName+" …", max(1, m.width)))
	}
	m.plan.width, m.plan.height = m.width, height
	lines := strings.Split(m.plan.View(), "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
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
	if !m.model.Sources.Hunk.Available {
		why := m.model.Sources.Hunk.Err
		if why == "" {
			why = "no answer"
		}
		return func() tea.Msg { return fleetNoticeMsg{text: "hunk could not be asked: " + why} }
	}
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
		return fleetNoticeMsg{text: "asked the terminal to copy " + pane}
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

// fleetSectionCounts totals the units behind each section. A folded row stands
// for every unit it hides, so the count is the work, not the line count.
func fleetSectionCounts(items []fleetItem) map[int]int {
	counts := map[int]int{}
	for _, it := range items {
		n := 1
		if it.summary {
			n = it.count
		}
		counts[fleetSectionOf(it.status)] += n
	}
	return counts
}

func fleetSectionOf(status string) int {
	switch status {
	case "working":
		return fleetSectionWorking
	case "blocked", "unreadable":
		return fleetSectionBlocked
	case "failed":
		return fleetSectionFailed
	case "done", "canceled":
		return fleetSectionSettled
	default:
		return fleetSectionReady
	}
}

// fleetItems orders the screen: sections top to bottom, and inside a section the
// freshest unit first, so the newest work of every encounter leads together.
func fleetItems(model FleetModel, expanded map[string]bool) []fleetItem {
	var items []fleetItem
	for section, def := range fleetSectionDefs {
		var in []fleetItem
		if def.folded {
			in = fleetFoldedItems(model, section, expanded)
		} else {
			for _, row := range model.Rows {
				if fleetSectionOf(row.Status) == section {
					in = append(in, fleetRowItem(row))
				}
			}
			if section == fleetSectionBlocked {
				in = append(in, fleetBrokenItems(model)...)
			}
		}
		sort.SliceStable(in, func(i, j int) bool {
			if in[i].lastTouch.Equal(in[j].lastTouch) {
				return in[i].encounter < in[j].encounter
			}
			return in[i].lastTouch.After(in[j].lastTouch)
		})
		items = append(items, in...)
	}
	return items
}

func fleetRowItem(row FleetRow) fleetItem {
	return fleetItem{
		row:       row,
		encounter: row.Encounter,
		label:     row.ID,
		status:    row.Status,
		lastTouch: fleetRowTouch(row),
	}
}

// fleetRowTouch is when the unit itself moved. A state that recorded no
// timestamp falls back to the encounter's touch, which is the newest it can know.
func fleetRowTouch(row FleetRow) time.Time {
	if row.UnitTouch.IsZero() {
		return row.LastTouch
	}
	return row.UnitTouch
}

// fleetFoldKey names one encounter's fold in one section, so FAILED and SETTLED
// open and close independently.
func fleetFoldKey(encounter string, section int) string {
	return fmt.Sprintf("%s\x00%d", encounter, section)
}

// fleetFoldCursorKey names the row the cursor lands on after a fold toggle: the
// first item of the same encounter in the same section, which is the summary
// that replaced the units or the first unit it revealed.
func fleetFoldCursorKey(items []fleetItem, it fleetItem) string {
	section := fleetSectionOf(it.status)
	for _, other := range items {
		if other.encounter == it.encounter && fleetSectionOf(other.status) == section {
			return fleetItemKey(other)
		}
	}
	return ""
}

// fleetFoldedItems folds one encounter's units of a section into one row. A
// failed round keeps its verdict in the state forever: the fix that answers it
// is a later unit, so a listed failure would read as work still outstanding.
func fleetFoldedItems(model FleetModel, section int, expanded map[string]bool) []fleetItem {
	byEncounter := map[string][]FleetRow{}
	var order []string
	for _, row := range model.Rows {
		if fleetSectionOf(row.Status) != section {
			continue
		}
		if _, ok := byEncounter[row.Encounter]; !ok {
			order = append(order, row.Encounter)
		}
		byEncounter[row.Encounter] = append(byEncounter[row.Encounter], row)
	}
	var items []fleetItem
	for _, name := range order {
		rows := byEncounter[name]
		if expanded[fleetFoldKey(name, section)] {
			for _, row := range rows {
				items = append(items, fleetRowItem(row))
			}
			continue
		}
		items = append(items, fleetItem{
			encounter: name,
			label:     name,
			status:    fleetSectionDefs[section].foldStat,
			summary:   true,
			fold:      fleetFoldKey(name, section),
			live:      fleetEncounterLive(model, name),
			count:     len(rows),
			lastTouch: fleetNewestTouch(rows),
		})
	}
	return items
}

// fleetBrokenItems names every encounter the fleet could not read. There is no
// unit to hang the error on, so the row carries the encounter's name.
func fleetBrokenItems(model FleetModel) []fleetItem {
	var items []fleetItem
	for _, g := range fleetGroups(model) {
		if g.err == "" {
			continue
		}
		items = append(items, fleetItem{
			encounter: g.name,
			label:     g.name,
			status:    "unreadable",
			broken:    true,
			err:       g.err,
			lastTouch: g.touch,
		})
	}
	return items
}

// fleetEncounterLive reports whether the encounter still has a unit in play: a
// lane running, a blocked unit, or a unit ready to start. Settled and failed
// units are neither, so a fold over them can say nothing is live.
func fleetEncounterLive(model FleetModel, encounter string) bool {
	for _, row := range model.Rows {
		if row.Encounter != encounter {
			continue
		}
		switch fleetSectionOf(row.Status) {
		case fleetSectionWorking, fleetSectionBlocked, fleetSectionReady:
			return true
		}
	}
	return false
}

func fleetNewestTouch(rows []FleetRow) time.Time {
	var newest time.Time
	for _, row := range rows {
		if at := fleetRowTouch(row); at.After(newest) {
			newest = at
		}
	}
	return newest
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
			return report, forKey, "report " + row.ID + " · not live",
				"no live lane: the report stands in for the pane"
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
		return report, forKey, "report " + row.ID + " · not live",
			"lane unreadable, the report stands in for the pane: " + err.Error()
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

// The view draws two different nothings. fleetNone means the row never carried
// the field: a unit with no worktree has no diff to measure. fleetUnknown means
// the fact should exist and was not read, because a source failed or the state
// never recorded it. A zero is neither.
const (
	fleetUnknown = "?"
	fleetNone    = "-"
)

func orUnknown(s string) string {
	if s == "" {
		return fleetUnknown
	}
	return s
}

func fleetPane(row FleetRow) string { return orUnknown(row.Pane) }

func fleetBranch(row FleetRow) string { return orUnknown(row.Branch) }

// fleetChanged is the uncommitted diff worktrunk reports, never the branch's
// own commits. A row with a worktree and no worktrunk fact has an unknown
// diff, not a zero one.
func fleetChanged(row FleetRow) string {
	if row.WT == nil {
		if row.Worktree == "" {
			return fleetNone
		}
		return fleetUnknown
	}
	added, deleted := fleetUnknown, fleetUnknown
	if row.WT.Added != nil {
		added = fmt.Sprint(*row.WT.Added)
	}
	if row.WT.Deleted != nil {
		deleted = fmt.Sprint(*row.WT.Deleted)
	}
	return "+" + added + "/-" + deleted
}

// A folded summary names an encounter, not a unit. It never carried a pane, a
// branch, a lane or a diff, so it must not draw the unknown glyph for them.
func fleetItemLane(it fleetItem) string {
	if it.summary {
		return ""
	}
	return fleetLane(it.row)
}

func fleetItemPane(it fleetItem) string {
	if it.summary {
		return ""
	}
	return fleetPane(it.row)
}

func fleetItemBranch(it fleetItem) string {
	if it.summary {
		return ""
	}
	return fleetBranch(it.row)
}

func fleetItemChanged(it fleetItem) string {
	if it.summary {
		return ""
	}
	return fleetChanged(it.row)
}

// fleetAheadDirty is written once, at spawn, and never refreshed. A row with no
// worktree recorded neither, so it knows neither.
func fleetAheadDirty(row FleetRow) string {
	if row.Worktree == "" {
		return "unknown, never recorded"
	}
	return fmt.Sprintf("%d / %d, recorded at spawn, never refreshed, no timestamp", row.Ahead, row.Dirty)
}

func fleetChecks(checks *FleetChecks) string {
	if checks == nil {
		return "no result"
	}
	status := checks.Status
	if status == "" {
		status = "unknown"
	}
	if checks.Stale {
		status += " (stale, not the current head)"
	}
	return status
}

func fleetPR(pr *FleetPR) string {
	if pr == nil {
		return fleetNone
	}
	out := fmt.Sprintf("#%d %s", pr.Number, orUnknown(pr.Review))
	switch {
	case pr.Mergeable == nil:
		out += " · mergeable unknown"
	case *pr.Mergeable:
		out += " · mergeable"
	default:
		out += " · not mergeable"
	}
	return out
}

// fleetSourceLine names every source and whether it answered. A source that
// failed feeds no column, so the header must say which one and why. The age is
// the snapshot's: it is the only clock the view holds.
func fleetSourceLine(model FleetModel, at time.Time) string {
	parts := []string{
		fleetSourceState("wt", model.Sources.Worktrunk, fmt.Sprintf("cached up to %s", fleetWTCacheTTL)),
		fleetSourceState("hunk", model.Sources.Hunk, ""),
	}
	if !at.IsZero() {
		parts = append(parts, "snapshot "+fleetAge(at))
	}
	parts = append(parts, "changed is the uncommitted diff")
	return "sources  " + strings.Join(parts, " · ")
}

func fleetSourceState(name string, src FleetSource, note string) string {
	if src.Available {
		if note != "" {
			return name + " ok (" + note + ")"
		}
		return name + " ok"
	}
	why := src.Err
	if why == "" {
		why = "no answer"
	}
	return name + " unavailable: " + why
}

// fleetMissingState names encounters with no state.json. LoadState answers an
// empty state with no error and UnitStatus then answers "planned", so without
// this the view would claim a state that was never written.
func fleetMissingState(model FleetModel) []string {
	var missing []string
	for _, enc := range model.Encounters {
		if enc.Path == "" || enc.Err != "" {
			continue
		}
		if _, err := os.Stat(StatePath(enc.Path)); os.IsNotExist(err) {
			missing = append(missing, enc.Name)
		}
	}
	return missing
}

func fleetStateMissing(model FleetModel, encounter string) bool {
	for _, enc := range model.Encounters {
		if enc.Name != encounter || enc.Path == "" || enc.Err != "" {
			continue
		}
		_, err := os.Stat(StatePath(enc.Path))
		return os.IsNotExist(err)
	}
	return false
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
			return fmt.Sprintf("%d %s", it.count, fleetFoldedNoun(it))
		}
		return fmt.Sprintf("%d %s, nothing live", it.count, fleetFoldedNoun(it))
	}
	return fleetDid(it.row)
}

// fleetFoldedNoun names what a folded row counts. A failure is a recorded
// verdict, not a queue: the units it folded have already had their say.
func fleetFoldedNoun(it fleetItem) string {
	if fleetSectionOf(it.status) == fleetSectionFailed {
		return "failed"
	}
	return "settled"
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
		mt.lane = max(mt.lane, runeLen(fleetItemLane(it)))
		mt.pane = max(mt.pane, runeLen(fleetItemPane(it)))
		mt.branch = max(mt.branch, runeLen(fleetItemBranch(it)))
		mt.changed = max(mt.changed, runeLen(fleetItemChanged(it)))
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
	lane := pad(truncate(fleetItemLane(it), mt.lane), mt.lane)
	pane := pad(truncate(fleetItemPane(it), mt.pane), mt.pane)
	branch := pad(truncate(fleetItemBranch(it), mt.branch), mt.branch)
	changed := pad(truncate(fleetItemChanged(it), mt.changed), mt.changed)
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
	sectionCount := fleetSectionCounts(items)
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
			dimStyle.Render("r retry · q quit") + "\n"
	}
	if m.overlay == "detail" {
		// The card is a full frame: a card squeezed into the top third is
		// unreadable, and esc brings the split back.
		return m.headerView() + "\n" + m.detailOverlay() + "\n" + m.footerView()
	}
	if m.plan != nil {
		return m.splitView()
	}
	if m.overlay == "actions" {
		return m.headerView() + "\n" + overlayBottomRight(m.listBody(), m.actionBox(), m.width) + "\n" + m.footerView()
	}
	return m.headerView() + "\n" + m.listBody() + "\n" + m.footerView()
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
	counts := fleetSectionCounts(m.items())
	right := dimStyle.Render(fmt.Sprintf("%d encounters · %d units", len(m.model.Encounters), len(m.model.Rows)))
	chips := ""
	for i, sec := range fleetSectionDefs {
		if n := counts[i]; n > 0 {
			chips += "  " + statusStyles[sec.style].Render(fmt.Sprintf("%d %s", n, strings.ToLower(sec.label)))
		}
	}
	out := spread(left, right, m.width) + "\n" + chips + "\n" +
		dimStyle.Render(truncate(fleetSourceLine(m.model, m.modelAt), max(1, m.width))) + "\n"
	if missing := fleetMissingState(m.model); len(missing) > 0 {
		note := "state  no state.json for " + strings.Join(missing, ", ") + ": those units read as planned"
		out += statusStyles["blocked"].Render(truncate(note, max(1, m.width))) + "\n"
	}
	return out + ruleStyle.Render(strings.Repeat("─", max(0, m.width)))
}

// headerHeight counts the lines headerView draws, so the list keeps the frame
// inside the terminal.
func (m fleetViewModel) headerHeight() int {
	h := 4 // title, chips, sources, rule
	if len(fleetMissingState(m.model)) > 0 {
		h++
	}
	return h
}

func (m fleetViewModel) listBody() string {
	room := max(6, m.height-m.headerHeight()-2) // 2 is the footer rule and its key line
	previewH := min(10, max(3, room/3))
	rows := max(3, room-previewH)
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
	if m.previewErr != "" {
		// The error stands even when a report fills the pane: a dead lane
		// reading as a live one is the failure this view must not make.
		lines = append(lines, statusStyles["blocked"].Render(truncate("! "+m.previewErr, max(1, m.width))))
	}
	room := max(1, height-len(lines))
	var text []string
	if m.preview != "" {
		text = strings.Split(strings.ReplaceAll(m.preview, "\t", "    "), "\n")
		for i := range text {
			text[i] = truncate(text[i], max(1, m.width))
		}
		if len(text) > room {
			text = text[len(text)-room:]
		}
	} else if m.previewErr == "" {
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
		noun := fleetFoldedNoun(it)
		b.WriteString(titleStyle.Render(it.label) + "  " + statusStyles[it.status].Render(noun) + "\n\n")
		b.WriteString(fmt.Sprintf("%d %s units · newest %s\n", it.count, noun, fleetAge(it.lastTouch)))
		if it.live {
			b.WriteString(dimStyle.Render("the encounter still has units outside this fold") + "\n")
		} else {
			b.WriteString(dimStyle.Render("nothing live in this encounter") + "\n")
		}
	} else {
		row := it.row
		b.WriteString(titleStyle.Render(row.ID) + "  " + statusStyles[row.Status].Render(row.Status) +
			dimStyle.Render(fmt.Sprintf("  %s %s", row.Role, row.Minion)) + "\n\n")
		fields := [][2]string{
			{"encounter", row.Encounter},
			{"pane", fleetPane(row)},
			{"branch", fleetBranch(row)},
			{"worktree", orUnknown(row.Worktree)},
			{"changed", fleetChanged(row)},
			{"ahead / dirty", fleetAheadDirty(row)},
			{"accept", orDash(row.Accept)},
			{"last report", "-"},
			{"last touch", fleetAge(fleetRowTouch(row))},
		}
		if row.LastReport != nil {
			fields[7][1] = fleetAge(*row.LastReport)
		}
		if row.Worktree != "" {
			switch {
			case row.Review != nil && row.Review.Session != "":
				fields = append(fields, [2]string{"hunk session", row.Review.Session})
			case !m.model.Sources.Hunk.Available:
				fields = append(fields, [2]string{"hunk session", "unknown, hunk could not be asked"})
			default:
				fields = append(fields, [2]string{"hunk session", "none open"})
			}
		}
		if row.WT != nil {
			fields = append(fields, [2]string{"checks", fleetChecks(row.WT.Checks)})
			if row.WT.PR != nil {
				fields = append(fields, [2]string{"pull request", fleetPR(row.WT.PR)})
			}
		} else if row.Worktree != "" {
			fields = append(fields, [2]string{"wt", "no fact for this worktree, changed and checks unknown"})
		}
		if fleetStateMissing(m.model, row.Encounter) {
			fields = append(fields, [2]string{"state", "no state.json, this unit reads as planned"})
		}
		for _, field := range fields {
			b.WriteString(dimStyle.Render(fmt.Sprintf("%-14s", field[0])) + truncate(field[1], max(1, inner-14)) + "\n")
		}
		b.WriteString(dimStyle.Render(truncate(fleetSourceLine(m.model, m.modelAt), max(1, inner))) + "\n")
	}
	if m.preview != "" {
		b.WriteString("\n" + sectionStyle.Render("PREVIEW") + "\n")
		if m.previewErr != "" {
			b.WriteString(statusStyles["blocked"].Render(truncate("! "+m.previewErr, inner)) + "\n")
		}
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
	keys := "j/k move · enter plan · d detail · space actions · q quit"
	if m.plan != nil {
		keys = "FLEET · tab plan · j/k move · enter plan · d detail · space actions · q quit"
		if m.focus == "plan" {
			keys = "PLAN · tab fleet · esc back · ctrl+c quit"
		}
	}
	if m.overlay == "detail" {
		keys = "esc close · q quit"
	}
	if m.overlay == "actions" {
		keys = "f/h/c/e run · j/k choose · enter run · esc close · q quit"
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
		m.modelAt = time.Now()
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
