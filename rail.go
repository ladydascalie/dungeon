package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The rail is a compact tmux pane that lists every pane running an agent. It
// exists because herdr cannot see lanes that dungeon put in tmux sessions. The
// renderer is bubbletea, so a refresh diffs cells instead of clearing the pane.

// railPaneInfo is one agent pane the rail can switch to.
type railPaneInfo struct {
	Pane            string `json:"pane"`
	Session         string `json:"session"`
	WindowID        string `json:"window_id"`
	WindowIndex     string `json:"window_index"`
	PaneIndex       string `json:"pane_index"`
	Command         string `json:"command"`
	Title           string `json:"title"`
	Agent           string `json:"agent,omitempty"`
	Kind            string `json:"kind"`
	Status          string `json:"status,omitempty"`
	Unit            string `json:"unit,omitempty"`
	Encounter       string `json:"encounter,omitempty"`
	Harness         string `json:"harness,omitempty"`
	HarnessApp      string `json:"harness_app,omitempty"`
	HarnessKind     string `json:"harness_kind,omitempty"`
	HarnessMsg      string `json:"harness_msg,omitempty"`
	HarnessTitle    string `json:"harness_title,omitempty"`
	HarnessProgress *int   `json:"harness_progress,omitempty"`
	Active          bool   `json:"active"`
	Label           string `json:"label"`
	OSCPiped        bool   `json:"-"`
}

// railStatus is the dungeon fact for one pane: which unit the lane is on and
// what the state file says about it.
type railStatus struct {
	Status    string
	Unit      string
	Encounter string
}

// railSourceIssue is a source the rail could not read. It carries the file's
// age, because a failure without a time reads like a value of zero.
type railSourceIssue struct {
	kind string
	path string
	err  error
	at   time.Time
}

// railRead is one whole answer from tmux: the rows, what could not be read,
// and when the read happened.
type railRead struct {
	agents []railPaneInfo
	issues []railSourceIssue
	at     time.Time
}

// railEncounterRoot finds the .dungeon a pane belongs to. A lane worktree sits
// beside the repo in <repo>.worktrees/<lane>, so the repo's .dungeon is checked
// before the worktree's own copy.
func railEncounterRoot(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	// Prefer the repo's .dungeon over a worktree-local copy. Dungeon cuts lane
	// worktrees at <repo>.worktrees/<lane>, so the repo is the sibling of the
	// worktree directory.
	for d := dir; ; d = filepath.Dir(d) {
		if strings.HasSuffix(d, ".worktrees") {
			repo := strings.TrimSuffix(d, ".worktrees")
			if root := filepath.Join(repo, ".dungeon"); dirExists(root) {
				return root, true
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
	}
	for {
		if root := filepath.Join(dir, ".dungeon"); dirExists(root) {
			return root, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// loadRailStatuses joins one .dungeon to the panes its state records. A broken
// plan or state contributes no status and is named as an issue, so an unknown
// never renders as a value. The rail still lists the pane.
func loadRailStatuses(root string) (map[string]railStatus, []railSourceIssue) {
	out := map[string]railStatus{}
	var issues []railSourceIssue
	now := time.Now()
	for _, e := range discoverPlansFrom(root) {
		at := now
		if info, err := os.Stat(e.Path); err == nil {
			at = info.ModTime()
		}
		if e.Err != nil {
			issues = append(issues, railSourceIssue{kind: "plan", path: e.Path, err: e.Err, at: at})
			continue
		}
		plan, findings, err := DecodeFile(e.Path)
		if err != nil {
			issues = append(issues, railSourceIssue{kind: "plan", path: e.Path, err: err, at: at})
			continue
		}
		if len(findings) > 0 {
			issues = append(issues, railSourceIssue{kind: "plan", path: e.Path, err: errors.New(findings[0].Message), at: at})
			continue
		}
		statePath := StatePath(e.Path)
		st, err := LoadState(statePath)
		if err != nil {
			stateAt := at
			if info, statErr := os.Stat(statePath); statErr == nil {
				stateAt = info.ModTime()
			}
			issues = append(issues, railSourceIssue{kind: "state", path: statePath, err: err, at: stateAt})
			continue
		}
		name := plan.Encounter
		if name == "" {
			name = e.Name
		}
		for _, u := range plan.Units {
			pane := st.Minions[u.Minion].Pane
			if pane == "" {
				continue
			}
			out[pane] = railStatus{
				Status:    st.UnitStatus(u.ID),
				Unit:      u.ID,
				Encounter: name,
			}
		}
	}
	return out, issues
}

// railAgentKinds maps a command name to the agent kind it belongs to. The rail
// uses it for the icon and colour, so a pane that only shows node still gets
// its real agent when the process tree names it.
var railAgentKinds = map[string]string{
	"pi": "pi", "claude": "claude", "codex": "codex", "gemini": "gemini",
	"opencode": "opencode", "cursor-agent": "cursor", "cursor": "cursor",
	"qwen": "qwen", "aider": "aider", "amp": "amp", "droid": "droid",
	"goose": "goose", "cline": "cline", "kilocode": "kilocode", "kimi": "kimi",
	"copilot": "copilot", "crush": "crush", "charm": "charm", "omp": "omp",
}

func railKindFromCommand(command string) string {
	return railAgentKinds[filepath.Base(command)]
}

// railAgentKind finds the identity behind a pane. A status report wins: the
// report is the identity, and the process tree is not consulted. Otherwise the
// dungeon pane option and the foreground command are the evidence.
func railAgentKind(cmd, agent string, osc oscStatus, reported bool) string {
	if reported && osc.State != "clear" {
		if osc.App != "" {
			return osc.App
		}
		return "agent"
	}
	if agent != "" {
		// dungeon only starts pi lanes today. Keep the option authoritative
		// even when the foreground command is still a shell.
		return "pi"
	}
	return railKindFromCommand(cmd)
}

func railAgentLabel(cmd, title, agent string) string {
	if agent != "" {
		return agent
	}
	if strings.HasPrefix(title, "π - ") {
		return strings.TrimPrefix(title, "π - ")
	}
	if title != "" {
		return title
	}
	return filepath.Base(cmd)
}

type railSnapshotMsg struct {
	read railRead
	err  error
	at   time.Time
}

type railControlMsg struct {
	event controlEvent
	ok    bool
}

type railFileMsg struct {
	event watchEvent
	ok    bool
}

type railWakeMsg struct{}

// railWakeDelay coalesces a burst: a SaveState rename and a report write can
// arrive together, and one re-read answers both.
const railWakeDelay = 50 * time.Millisecond

type railSources struct {
	events  <-chan controlEvent
	files   <-chan watchEvent
	control *tmuxControl
}

type railModel struct {
	tmux     tmux
	backstop time.Duration
	sources  railSources

	exe    string
	stamp  string
	reload bool

	agents     []railPaneInfo
	cursor     int
	scroll     int
	onGroup    string
	folded     map[string]bool
	lastActive string
	notice     string
	err        error

	wakeScheduled bool

	readAt       time.Time
	readErr      error
	readErrAt    time.Time
	watchErr     error
	watchErrAt   time.Time
	controlErr   error
	controlErrAt time.Time
	issues       []railSourceIssue

	width  int
	height int
}

func newRailModel(t tmux, backstop time.Duration, src railSources) railModel {
	return railModel{tmux: t, backstop: backstop, sources: src, width: 32, height: 30}
}

func (m railModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.refresh(), m.waitControl(), m.waitFiles()}
	if m.backstop > 0 {
		cmds = append(cmds, tick(m.backstop))
	}
	return tea.Batch(cmds...)
}

func (m railModel) waitControl() tea.Cmd {
	ch := m.sources.events
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		e, ok := <-ch
		return railControlMsg{event: e, ok: ok}
	}
}

func (m railModel) waitFiles() tea.Cmd {
	ch := m.sources.files
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		e, ok := <-ch
		return railFileMsg{event: e, ok: ok}
	}
}

// wake schedules one re-read shortly ahead. Events that arrive while it waits
// join it instead of starting their own.
func (m *railModel) wake() tea.Cmd {
	if m.wakeScheduled {
		return nil
	}
	m.wakeScheduled = true
	return tea.Tick(railWakeDelay, func(time.Time) tea.Msg { return railWakeMsg{} })
}

func (m railModel) refresh() tea.Cmd {
	t := m.tmux
	c := m.sources.control
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		read, err := t.railPanes(ctx)
		if err == nil {
			read.issues = append(read.issues, railPipeOSC(ctx, t, c, read.agents)...)
		}
		return railSnapshotMsg{read: read, err: err, at: time.Now()}
	}
}

// railPipeOSC attaches the status sink to a pane once. The @dungeon_osc option
// records that the pipe exists, so a refresh does not re-issue it.
func railPipeOSC(ctx context.Context, t tmux, c *tmuxControl, panes []railPaneInfo) []railSourceIssue {
	exe, err := os.Executable()
	if err != nil {
		return []railSourceIssue{{kind: "osc", err: err, at: time.Now()}}
	}
	server := oscServerKey()
	now := time.Now()
	var issues []railSourceIssue
	for _, p := range panes {
		if p.Pane == "" || p.Kind == "shell" || p.OSCPiped {
			continue
		}
		command := shellQuote(exe) + " osc-sink " + shellQuote(server) + " " + shellQuote(p.Pane)
		var pipeErr error
		if c != nil {
			_, pipeErr = c.do(ctx, "pipe-pane", "-o", "-t", p.Pane, command)
			if pipeErr == nil {
				_, pipeErr = c.do(ctx, "set-option", "-p", "-t", p.Pane, "@dungeon_osc", "1")
			}
		} else {
			_, pipeErr = t.call(ctx, "pipe-pane", "-o", "-t", p.Pane, command)
			if pipeErr == nil {
				_, pipeErr = t.call(ctx, "set-option", "-p", "-t", p.Pane, "@dungeon_osc", "1")
			}
		}
		if pipeErr != nil {
			issues = append(issues, railSourceIssue{kind: "osc", path: p.Pane, err: pipeErr, at: now})
		}
	}
	return issues
}

func (m railModel) focusCmd(pane string) tea.Cmd {
	c := m.sources.control
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if c == nil {
			return railNoticeMsg{text: "cannot focus", err: fmt.Errorf("tmux control is down")}
		}
		if err := c.railFocus(ctx, pane); err != nil {
			return railNoticeMsg{text: "cannot focus", err: err}
		}
		return nil
	}
}

type railNoticeMsg struct {
	text string
	err  error
}

func (m railModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampCursor()
		return m, nil

	case tickMsg:
		if m.backstop <= 0 {
			return m, nil
		}
		// A rail is one process per window, so replacing the binary would
		// leave every open rail on the old build. Check the file it runs and
		// re-exec, which keeps the pane and picks up the new build.
		if m.exe != "" && railStamp(m.exe) != m.stamp {
			m.reload = true
			return m, tea.Quit
		}
		return m, tea.Batch(m.refresh(), tick(m.backstop))

	case railControlMsg:
		if !msg.ok {
			return m, nil
		}
		cmd := m.waitControl()
		switch msg.event.Kind {
		case controlExit:
			m.controlErr = msg.event.Err
			m.controlErrAt = msg.event.At
			return m, cmd
		default:
			// connected and topology both mean the facts may have moved.
			if msg.event.Kind == controlConnected {
				m.controlErr = nil
			}
			return m, tea.Batch(cmd, m.wake())
		}

	case railFileMsg:
		if !msg.ok {
			return m, nil
		}
		cmd := m.waitFiles()
		if msg.event.Err != nil {
			m.watchErr = msg.event.Err
			m.watchErrAt = msg.event.At
			return m, cmd
		}
		m.watchErr = nil
		return m, tea.Batch(cmd, m.wake())

	case railWakeMsg:
		m.wakeScheduled = false
		return m, m.refresh()

	case railSnapshotMsg:
		keep := m.selectedPane()
		keepGroup := m.onGroup
		m.err = msg.err
		if msg.err == nil {
			m.readAt = msg.at
			m.readErr = nil
			m.issues = msg.read.issues
			m.agents = msg.read.agents
			for i := range m.agents {
				if m.agents[i].Active {
					m.lastActive = m.agents[i].Pane
				}
			}
			// When the rail has focus, no listed pane is active. Keep the last
			// active mark on the content pane so the rail still says where Enter
			// came from.
			if m.lastActive != "" {
				for i := range m.agents {
					if m.agents[i].Pane == m.lastActive {
						m.agents[i].Active = true
					}
				}
			}
			m.cursor = 0
			for i, a := range m.agents {
				if a.Pane == keep {
					m.cursor = i
					break
				}
			}
			m.onGroup = keepGroup
			m.clampCursor()
		} else {
			m.readErr = msg.err
			m.readErrAt = msg.at
		}
		return m, nil

	case railNoticeMsg:
		m.notice = msg.text
		if msg.err != nil {
			m.notice += ": " + msg.err.Error()
		}
		return m, nil

	case tea.MouseMsg:
		return m, m.mouse(msg)

	case tea.KeyMsg:
		return m, m.key(msg.String())
	}
	return m, nil
}

func (m *railModel) key(key string) tea.Cmd {
	m.notice = ""
	switch key {
	case "q", "ctrl+c":
		return tea.Quit
	case "esc":
		return nil
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "g", "home":
		m.move(-len(m.layout().rows()))
	case "G", "end":
		m.move(len(m.layout().rows()))
	case "z":
		m.toggleFold(m.foldTarget())
	case "Z":
		m.toggleFold(m.sessionTarget())
	case "r":
		return m.refresh()
	case "enter":
		if m.onGroup != "" {
			m.toggleFold(m.onGroup)
			m.clampCursor()
			return nil
		}
		if p := m.selectedPane(); p != "" {
			return m.focusCmd(p)
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		idx := int(key[0] - '1')
		if idx < len(m.agents) {
			m.onGroup = ""
			m.cursor = idx
			m.unfoldTo(idx)
			m.clampCursor()
			m.notice = "focused " + m.agents[idx].Label
			return m.focusCmd(m.agents[idx].Pane)
		}
	}
	m.clampCursor()
	return nil
}

func (m *railModel) mouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.move(-1)
	case tea.MouseButtonWheelDown:
		m.move(1)
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		row, ok := m.rowAt(msg.Y)
		if !ok {
			m.clampCursor()
			return nil
		}
		m.selectRow(row)
		m.clampCursor()
		if row.group != "" {
			return nil
		}
		return m.focusCmd(m.agents[row.pane].Pane)
	}
	m.clampCursor()
	return nil
}

// railGroup is one block of the rail: an encounter inside a session, or the
// panes a session has that no encounter owns. Its id is the fold key. An
// encounter that shares its session's name folds as the session, because its
// header would be the session header twice.
type railGroup struct {
	id        string
	session   string
	encounter string
	unscoped  bool
	panes     []int
}

func railSessionGroup(session string) string {
	return "session:" + session
}

func railEncounterGroup(session, encounter string) string {
	return "encounter:" + session + "/" + encounter
}

func railLooseGroup(session string) string {
	return "loose:" + session
}

// railGroups orders the panes for display: sessions in the order tmux lists
// them, encounters inside a session in the order they appear, then the panes no
// encounter owns. A session with no encounters is one group, so it needs no
// "no encounter" header.
func railGroups(agents []railPaneInfo) []railGroup {
	var sessions []string
	bySession := map[string][]int{}
	for i, a := range agents {
		if _, seen := bySession[a.Session]; !seen {
			sessions = append(sessions, a.Session)
		}
		bySession[a.Session] = append(bySession[a.Session], i)
	}
	var out []railGroup
	for _, session := range sessions {
		var encounters []string
		byEncounter := map[string][]int{}
		var loose []int
		for _, i := range bySession[session] {
			encounter := agents[i].Encounter
			if encounter == "" {
				loose = append(loose, i)
				continue
			}
			if _, seen := byEncounter[encounter]; !seen {
				encounters = append(encounters, encounter)
			}
			byEncounter[encounter] = append(byEncounter[encounter], i)
		}
		for _, encounter := range encounters {
			id := railEncounterGroup(session, encounter)
			if encounter == session {
				id = railSessionGroup(session)
			}
			out = append(out, railGroup{id: id, session: session, encounter: encounter, panes: byEncounter[encounter]})
		}
		if len(loose) > 0 {
			id := railSessionGroup(session)
			if len(encounters) > 0 {
				id = railLooseGroup(session)
			}
			out = append(out, railGroup{id: id, session: session, unscoped: len(encounters) > 0, panes: loose})
		}
	}
	return out
}

// railRow is one selectable line group: a header, or a pane tile.
type railRow struct {
	group string
	pane  int
}

// railLayout is the whole body of the rail, plus the pane or fold group each
// line belongs to. Scrolling, clicks and folds all read it, so a header can
// never shift a click onto the wrong pane.
type railLayout struct {
	lines     []string
	linePane  []int
	lineGroup []string
	groups    map[string]railGroup
	parent    map[string]string
}

func (l *railLayout) add(line string, pane int, group string) {
	l.lines = append(l.lines, line)
	l.linePane = append(l.linePane, pane)
	l.lineGroup = append(l.lineGroup, group)
}

// rows lists the selectable lines in order: a header once, and the top line of
// each pane tile.
func (l railLayout) rows() []railRow {
	var out []railRow
	for i := range l.lines {
		switch {
		case l.lineGroup[i] != "":
			out = append(out, railRow{group: l.lineGroup[i], pane: -1})
		case l.linePane[i] >= 0 && (i == 0 || l.linePane[i-1] != l.linePane[i]):
			out = append(out, railRow{pane: l.linePane[i]})
		}
	}
	return out
}

// groupHeader names a group. A folded group keeps its header and says how many
// panes it hides, so the fold is always reversible from the line it collapsed.
func (m railModel) groupHeader(id, name string, count, width int, indent string) string {
	mark := "\u25be "
	text := indent + mark + name
	if m.folded[id] {
		mark = "\u25b8 "
		text = fmt.Sprintf("%s%s%s (%d)", indent, mark, name, count)
	}
	if m.onGroup == id {
		return railSelectedStyle.Render(pad(truncate(text, width), width))
	}
	if indent != "" {
		return dimStyle.Render(truncate(text, width))
	}
	return boldStyle.Render(truncate(text, width))
}

func (m railModel) layout() railLayout {
	width := max(10, m.width)
	rule := ruleStyle.Render(strings.Repeat("\u2500", width))
	groups := railGroups(m.agents)
	counts := map[string]int{}
	sessionCounts := map[string]int{}
	for _, g := range groups {
		counts[g.id] += len(g.panes)
		sessionCounts[g.session] += len(g.panes)
	}
	lay := railLayout{groups: map[string]railGroup{}, parent: map[string]string{}}
	session := ""
	sessionFolded := false
	for _, g := range groups {
		sessionID := railSessionGroup(g.session)
		lay.groups[g.id] = g
		lay.parent[g.id] = sessionID
		if g.session != session {
			session = g.session
			if len(lay.lines) > 0 {
				lay.add(rule, -1, "")
			}
			sessionFolded = m.folded[sessionID]
			lay.groups[sessionID] = railGroup{id: sessionID, session: g.session}
			lay.add(m.groupHeader(sessionID, g.session, sessionCounts[g.session], width, ""), -1, sessionID)
		}
		if sessionFolded {
			continue
		}
		switch {
		case g.encounter != "" && g.id != sessionID:
			lay.add(rule, -1, "")
			lay.add(m.groupHeader(g.id, g.encounter, counts[g.id], width, "  "), -1, g.id)
		case g.unscoped:
			lay.add(rule, -1, "")
			lay.add(m.groupHeader(g.id, "no encounter", counts[g.id], width, "  "), -1, g.id)
		}
		if m.folded[g.id] {
			continue
		}
		for i, idx := range g.panes {
			if i > 0 {
				lay.add(rule, -1, "")
			}
			for _, line := range m.rowLines(m.agents[idx], width, idx == m.cursor && m.onGroup == "") {
				lay.add(line, idx, "")
			}
		}
	}
	return lay
}

// railBody is the number of lines the rail has between its title and its
// footer.
func (m railModel) railBody() int {
	return max(1, m.height-3)
}

// selectionLine is the first line of whatever the cursor is on, or -1 when the
// selection is off screen.
func (m railModel) selectionLine(lay railLayout) int {
	if m.onGroup != "" {
		for i, group := range lay.lineGroup {
			if group == m.onGroup {
				return i
			}
		}
		return -1
	}
	for i, pane := range lay.linePane {
		if pane == m.cursor {
			return i
		}
	}
	return -1
}

// window picks the first body line to show. The rail holds its place while the
// cursor moves inside the window, and scrolls by the smallest step that brings
// the selection back into view, so one j moves one row, not a screen.
func (m railModel) window(lay railLayout) int {
	body := m.railBody()
	if len(lay.lines) <= body {
		return 0
	}
	start := m.scroll
	top := m.selectionLine(lay)
	switch {
	case top < 0:
	case top < start:
		start = top
	case top+1 >= start+body:
		start = top + 2 - body
	}
	if start > len(lay.lines)-body {
		start = len(lay.lines) - body
	}
	if start < 0 {
		start = 0
	}
	return start
}

// rowAt maps a screen line to the row under it, headers included.
func (m railModel) rowAt(y int) (railRow, bool) {
	rel := y - 2
	if rel < 0 {
		return railRow{}, false
	}
	lay := m.layout()
	line := m.window(lay) + rel
	if line < 0 || line >= len(lay.lines) {
		return railRow{}, false
	}
	if group := lay.lineGroup[line]; group != "" {
		return railRow{group: group, pane: -1}, true
	}
	if pane := lay.linePane[line]; pane >= 0 {
		return railRow{pane: pane}, true
	}
	return railRow{}, false
}

func (m railModel) rowIndex(rows []railRow) int {
	for i, row := range rows {
		if m.onGroup != "" {
			if row.group == m.onGroup {
				return i
			}
			continue
		}
		if row.group == "" && row.pane == m.cursor {
			return i
		}
	}
	return -1
}

func (m *railModel) selectRow(row railRow) {
	if row.group != "" {
		m.onGroup = row.group
		return
	}
	m.onGroup = ""
	m.cursor = row.pane
}

// move walks the selectable rows, headers included, so a folded header is one
// step away and never skipped.
func (m *railModel) move(delta int) {
	rows := m.layout().rows()
	if len(rows) == 0 {
		return
	}
	i := m.rowIndex(rows)
	switch {
	case i < 0:
		i = 0
	case i+delta < 0:
		i = 0
	case i+delta >= len(rows):
		i = len(rows) - 1
	default:
		i += delta
	}
	m.selectRow(rows[i])
}

// foldTarget is the group z folds: the encounter under the cursor, or its
// session when the pane has no encounter of its own.
func (m railModel) foldTarget() string {
	if m.onGroup != "" {
		return m.onGroup
	}
	if m.cursor < 0 || m.cursor >= len(m.agents) {
		return ""
	}
	a := m.agents[m.cursor]
	if a.Encounter != "" && a.Encounter != a.Session {
		return railEncounterGroup(a.Session, a.Encounter)
	}
	if a.Encounter == "" {
		for _, g := range railGroups(m.agents) {
			if g.id == railLooseGroup(a.Session) {
				return g.id
			}
		}
	}
	return railSessionGroup(a.Session)
}

// sessionTarget is the session Z folds, from a header or from a pane.
func (m railModel) sessionTarget() string {
	if m.onGroup != "" {
		if parent := m.layout().parent[m.onGroup]; parent != "" {
			return parent
		}
		if strings.HasPrefix(m.onGroup, "session:") {
			return m.onGroup
		}
		return ""
	}
	if m.cursor < 0 || m.cursor >= len(m.agents) {
		return ""
	}
	return railSessionGroup(m.agents[m.cursor].Session)
}

func (m *railModel) toggleFold(id string) {
	if id == "" {
		return
	}
	if m.folded == nil {
		m.folded = map[string]bool{}
	}
	if m.folded[id] {
		delete(m.folded, id)
	} else {
		m.folded[id] = true
	}
	m.onGroup = id
}

// unfoldTo opens the groups that hide a pane, before the rail jumps to it.
func (m *railModel) unfoldTo(idx int) {
	if m.folded == nil || idx < 0 || idx >= len(m.agents) {
		return
	}
	a := m.agents[idx]
	delete(m.folded, railSessionGroup(a.Session))
	if a.Encounter != "" && a.Encounter != a.Session {
		delete(m.folded, railEncounterGroup(a.Session, a.Encounter))
	}
	if a.Encounter == "" {
		delete(m.folded, railLooseGroup(a.Session))
	}
}

func (m *railModel) clampCursor() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if n := len(m.agents); n > 0 && m.cursor >= n {
		m.cursor = n - 1
	}
	lay := m.layout()
	if m.onGroup != "" {
		if _, ok := lay.groups[m.onGroup]; !ok {
			m.onGroup = ""
		}
	}
	m.scroll = m.window(lay)
}

func (m railModel) selectedPane() string {
	if m.onGroup != "" {
		return ""
	}
	it, ok := m.selectedAgent()
	if !ok {
		return ""
	}
	return it.Pane
}

func (m railModel) selectedAgent() (railPaneInfo, bool) {
	if m.cursor < 0 || m.cursor >= len(m.agents) {
		return railPaneInfo{}, false
	}
	return m.agents[m.cursor], true
}

var (
	railSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#313244")).
				Foreground(lipgloss.Color("#cdd6f4")).
				Bold(true)
	railActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#5fa887"))
)

var railKindColors = map[string]string{
	"pi": "#5fa887", "claude": "#e08d53", "codex": "#9fb0cf",
	"gemini": "#b39dcc", "cursor": "#7facab", "opencode": "#e0a94c",
	"qwen": "#b39dcc", "aider": "#7facab", "amp": "#e0705a",
	"droid": "#9fb0cf", "goose": "#5fa887", "cline": "#e08d53",
	"kilocode": "#e0a94c", "kimi": "#b39dcc", "copilot": "#9fb0cf",
	"crush": "#e0705a", "charm": "#7facab", "omp": "#5fa887",
	"shell": "#8b8fa6",
}

var railShellCommands = map[string]bool{
	"zsh": true, "bash": true, "sh": true, "dash": true, "fish": true,
	"ksh": true, "mksh": true, "ash": true, "yash": true, "nu": true,
}

func isRailShell(command string) bool {
	return railShellCommands[filepath.Base(command)]
}

func railKindMark(kind string) (string, lipgloss.Style) {
	glyph := "󰚩"
	switch kind {
	case "pi":
		glyph = "π"
	case "shell":
		glyph = "󰆍"
	}
	color, ok := railKindColors[kind]
	if !ok {
		color = "#8b8fa6"
	}
	return glyph, lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true)
}

func railStatusStyle(status string) lipgloss.Style {
	if style, ok := statusStyles[status]; ok {
		return style
	}
	switch status {
	case "error":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
	case "idle":
		return dimStyle
	}
	return dimStyle
}

func railBarStyle(a railPaneInfo) lipgloss.Style {
	status := a.Harness
	if status == "" {
		status = a.Status
	}
	switch status {
	case "blocked", "error":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#e0705a"))
	case "working", "done":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#5fa887"))
	case "idle":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#8b8fa6"))
	}
	if color, ok := railKindColors[a.Kind]; ok {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(color))
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#6aa88d"))
}

func railStatusBadge(a railPaneInfo) (plain, styled string) {
	if a.Harness != "" {
		plain = "harness " + a.Harness
		styled = dimStyle.Render("harness ") + railStatusStyle(a.Harness).Render(a.Harness)
		return plain, styled
	}
	if a.Status != "" {
		plain = "\ueefa " + a.Status
		styled = dimStyle.Render("\ueefa ") + railStatusStyle(a.Status).Render(a.Status)
		return plain, styled
	}
	return "", ""
}

func (m railModel) rowLines(a railPaneInfo, width int, selected bool) []string {
	glyph, glyphStyle := railKindMark(a.Kind)
	place := a.Session + ":" + a.WindowIndex + "." + a.PaneIndex
	badgePlain, badgeStyled := railStatusBadge(a)

	inner := max(1, width-2) // one column for the active rail, one for the gap
	maxLabel := max(1, inner-runeLen(glyph)-runeLen(badgePlain)-4)
	label := truncate(a.Label, maxLabel)
	leftPlain := glyph + " " + label
	gap := inner - runeLen(leftPlain) - runeLen(badgePlain)
	if gap < 1 {
		gap = 1
	}
	topPlain := truncate(leftPlain+strings.Repeat(" ", gap)+badgePlain, inner)

	detail := a.Kind
	if a.Kind == "shell" {
		detail = "terminal"
	} else if a.Unit != "" {
		detail = a.Unit
	} else if a.HarnessApp != "" {
		detail = a.HarnessApp
	}
	bottomPlain := truncate(place+" · "+detail, inner)

	barText := " "
	if a.Active {
		barText = railBarStyle(a).Render("▌")
	}

	if selected {
		return []string{
			barText + " " + railSelectedStyle.Render(pad(topPlain, inner)),
			barText + " " + railSelectedStyle.Render(pad(bottomPlain, inner)),
		}
	}

	top := barText + " " + glyphStyle.Render(glyph) + " " + boldStyle.Render(label)
	used := 2 + runeLen(glyph) + runeLen(label)
	if badgePlain != "" {
		gap = max(1, inner-used-runeLen(badgePlain))
		top += strings.Repeat(" ", gap) + badgeStyled
		top += strings.Repeat(" ", max(0, inner-used-gap-runeLen(badgePlain)))
	} else {
		top += strings.Repeat(" ", max(0, inner-used))
	}

	bottom := barText + " " + dimStyle.Render(place) + dimStyle.Render(" · "+detail)
	bottom += strings.Repeat(" ", max(0, inner-runeLen(bottomPlain)))
	return []string{top, bottom}
}

func (m railModel) counts() (agents, shells int) {
	for _, a := range m.agents {
		if a.Kind == "shell" {
			shells++
		} else {
			agents++
		}
	}
	return agents, shells
}

// railAge names how old a fact is. An empty time is unknown, never zero.
func railAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return age(t.UTC().Format(time.RFC3339))
}

// sourceWarning says which source failed, and how old it is. The rail renders
// this instead of a value it does not have.
func (m railModel) sourceWarning() string {
	if m.readErr != nil {
		msg := "tmux read failed " + railAge(m.readErrAt) + ": " + collapseSpace(m.readErr.Error())
		if !m.readAt.IsZero() {
			msg += " · rows " + railAge(m.readAt)
		}
		return msg
	}
	if m.watchErr != nil {
		return "watch " + railAge(m.watchErrAt) + ": " + collapseSpace(m.watchErr.Error())
	}
	if len(m.issues) > 0 {
		it := m.issues[0]
		msg := it.kind + " " + filepath.Base(it.path) + " " + railAge(it.at)
		if it.err != nil {
			msg += ": " + collapseSpace(it.err.Error())
		}
		if len(m.issues) > 1 {
			msg += fmt.Sprintf(" (+%d)", len(m.issues)-1)
		}
		return msg
	}
	if m.controlErr != nil {
		return "control " + railAge(m.controlErrAt) + ": " + collapseSpace(m.controlErr.Error())
	}
	return ""
}

func (m railModel) render() string {
	width := max(10, m.width)
	height := max(3, m.height)
	agents, shells := m.counts()
	right := fmt.Sprintf("%d agents", agents)
	if shells > 0 {
		if agents > 0 {
			right += " · "
		}
		right += fmt.Sprintf("%d shell", shells)
		if shells != 1 {
			right += "s"
		}
	}
	lines := []string{
		spread(titleStyle.Render("󰚩")+" "+boldStyle.Render("PANES"), dimStyle.Render(right), width),
		ruleStyle.Render(strings.Repeat("─", width)),
	}
	if len(m.agents) == 0 {
		if m.err != nil {
			lines = append(lines, dimStyle.Render(truncate("tmux: "+m.err.Error(), width)))
		} else {
			lines = append(lines, dimStyle.Render("no panes"))
		}
	} else {
		lay := m.layout()
		start := m.window(lay)
		end := min(len(lay.lines), start+m.railBody())
		lines = append(lines, lay.lines[start:end]...)
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	keys := "j/k  enter  z fold  r  q"
	switch {
	case m.notice != "":
		keys = m.notice
	case m.sourceWarning() != "":
		keys = m.sourceWarning()
	default:
		if it, ok := m.selectedAgent(); ok {
			if it.HarnessMsg != "" {
				keys = it.HarnessMsg
			} else if it.HarnessTitle != "" {
				keys = it.HarnessTitle
			}
		}
	}
	lines = append(lines, dimStyle.Render(truncate(keys, width)))
	return strings.Join(lines, "\n")
}

// View clamps the rail to the pane exactly as the encounter view does.
func (m railModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return m.render()
	}
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(m.render())
}

func railBackend(ctx context.Context, forced string) (tmux, error) {
	h, err := openMux(ctx, forced)
	if err != nil {
		return tmux{}, err
	}
	t, ok := h.(tmux)
	if !ok {
		return tmux{}, fmt.Errorf("dungeon rail needs the tmux backend")
	}
	return t, nil
}

func railCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "toggle":
			return railToggleCmd(ctx, args[1:], stdout, stderr)
		case "restart":
			return railRestartCmd(ctx, args[1:], stdout, stderr)
		}
	}
	fs := newFlagSet("rail", stderr)
	backstop := fs.Duration("backstop", 10*time.Second, "slow re-read so a missed event cannot freeze the rail")
	legacy := fs.Duration("interval", 0, "deprecated alias for --backstop")
	backend := muxFlag(fs)
	once := fs.Bool("once", false, "print one frame and exit")
	asJSON := fs.Bool("json", false, "print the agent rows as JSON")
	width := fs.Int("width", 32, "rail width for --once")
	height := fs.Int("height", 30, "rail height for --once")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dungeon: rail takes no plan, got %q\n", fs.Arg(0))
		return 2
	}
	if *legacy > 0 {
		*backstop = *legacy
	}
	t, err := railBackend(ctx, *backend)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	m := newRailModel(t, *backstop, railSources{})
	m.width, m.height = *width, *height
	if *asJSON {
		read, err := t.railPanes(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		agents := read.agents
		if agents == nil {
			agents = []railPaneInfo{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(agents); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		return 0
	}
	if *once || !isTerminal(stdout) {
		read, err := t.railPanes(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		m.agents = read.agents
		m.issues = read.issues
		m.readAt = read.at
		fmt.Fprint(stdout, m.View())
		return 0
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan controlEvent, 64)
	files := make(chan watchEvent, 256)
	control := startTmuxControl(runCtx, t, events)
	m.sources = railSources{events: events, files: files, control: control}
	if _, err := railWatch(runCtx, files); err != nil {
		m.watchErr = err
		m.watchErrAt = time.Now()
	}
	m.exe, _ = os.Executable()
	m.stamp = railStamp(m.exe)
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(runCtx))
	final, err := prog.Run()
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if fm, ok := final.(railModel); ok && fm.reload {
		return reexec(myExe(stderr), stderr)
	}
	return 0
}

// railStamp identifies a build. A rail re-execs when the file it runs changes,
// which is what makes go install reach every open rail.
func railStamp(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", info.ModTime().UnixNano(), info.Size())
}

func myExe(stderr io.Writer) string {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return ""
	}
	return exe
}

// reexec replaces this process with the same command, so the rail keeps its
// pane and starts the build that was installed over it.
func reexec(exe string, stderr io.Writer) int {
	if exe == "" {
		return 2
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		fmt.Fprintf(stderr, "dungeon: re-exec %s: %v\n", exe, err)
		return 2
	}
	return 0
}

func railToggleCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("rail toggle", stderr)
	width := fs.Int("width", 32, "rail width in columns")
	printPane := fs.Bool("print-pane", false, "print the rail pane id on success")
	backend := muxFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dungeon: rail toggle takes no arguments, got %q\n", fs.Arg(0))
		return 2
	}
	t, err := railBackend(ctx, *backend)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	target := os.Getenv("TMUX_PANE")
	if target == "" {
		// tmux run-shell does not set TMUX_PANE. The client that invoked the
		// binding is the pane target, so ask tmux for it before refusing.
		out, err := t.call(ctx, "display-message", "-p", "#{pane_id}")
		if err == nil {
			target = strings.TrimSpace(string(out))
		}
	}
	if target == "" {
		fmt.Fprintln(stderr, "dungeon: rail toggle needs a tmux pane")
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	rail, err := t.railToggle(ctx, target, *width, shellQuote(exe)+" rail")
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if *printPane && rail != "" {
		fmt.Fprintln(stdout, rail)
	}
	return 0
}

// railRestartCmd replaces open rails with one running this binary.
func railRestartCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("rail restart", stderr)
	all := fs.Bool("all", false, "restart every rail, in every session")
	width := fs.Int("width", 32, "rail width when the old rail does not report one")
	backend := muxFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dungeon: rail restart takes no arguments, got %q\n", fs.Arg(0))
		return 2
	}
	t, err := railBackend(ctx, *backend)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	target := os.Getenv("TMUX_PANE")
	if target == "" {
		out, err := t.call(ctx, "display-message", "-p", "#{pane_id}")
		if err == nil {
			target = strings.TrimSpace(string(out))
		}
	}
	if target == "" {
		fmt.Fprintln(stderr, "dungeon: rail restart needs a tmux pane")
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if _, err := t.railRestart(ctx, target, *all, *width, shellQuote(exe)+" rail"); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	return 0
}
