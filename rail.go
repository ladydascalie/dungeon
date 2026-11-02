package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
}

// railStatus is the dungeon fact for one pane: which unit the lane is on and
// what the state file says about it.
type railStatus struct {
	Status    string
	Unit      string
	Encounter string
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
// plan contributes no status; the rail still lists the pane.
func loadRailStatuses(root string) map[string]railStatus {
	out := map[string]railStatus{}
	for _, e := range discoverPlansFrom(root) {
		if e.Err != nil {
			continue
		}
		plan, findings, err := DecodeFile(e.Path)
		if err != nil || len(findings) > 0 {
			continue
		}
		st, err := LoadState(StatePath(e.Path))
		if err != nil {
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
	return out
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

type railProc struct {
	pid  int
	ppid int
	comm string
	args string
}

// loadRailProcs reads the process tree once per refresh. Agent detection needs
// it because a TUI can run under node or bun, or be a child of an interactive
// shell, while pane_current_command still says node or zsh.
func loadRailProcs(ctx context.Context) map[int][]railProc {
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid=,ppid=,comm=,args=").Output()
	if err != nil {
		return nil
	}
	byParent := map[int][]railProc{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		byParent[ppid] = append(byParent[ppid], railProc{
			pid: pid, ppid: ppid, comm: fields[2],
			args: strings.Join(fields[3:], " "),
		})
	}
	return byParent
}

func railKindFromCommand(command string) string {
	return railAgentKinds[filepath.Base(command)]
}

func railKindFromArgs(args string) string {
	for _, field := range strings.Fields(args) {
		field = strings.Trim(field, "\"'()[]{};,")
		if kind := railKindFromCommand(field); kind != "" {
			return kind
		}
		// A node or bun invocation often names the agent in its first
		// argument, not the runtime command itself.
		if strings.Contains(field, "/") {
			if kind := railKindFromCommand(field); kind != "" {
				return kind
			}
		}
	}
	return ""
}

func railKindFromTitle(title string) string {
	switch {
	case strings.HasPrefix(title, "π - "):
		return "pi"
	case strings.HasPrefix(title, "Claude"):
		return "claude"
	case strings.HasPrefix(title, "Codex"):
		return "codex"
	case strings.HasPrefix(title, "Gemini"):
		return "gemini"
	case strings.HasPrefix(title, "Cursor"):
		return "cursor"
	}
	return ""
}

// railAgentKind finds the agent behind a pane. The dungeon option wins, then
// the pane command and title, then a descendant process. The last step is what
// keeps pi visible when it was started from a shell.
func railAgentKind(cmd, title, agent string, pid int, procs map[int][]railProc) string {
	if agent != "" {
		// dungeon only starts pi lanes today. Keep the option authoritative
		// even when the foreground command is still a shell.
		return "pi"
	}
	if kind := railKindFromCommand(cmd); kind != "" {
		return kind
	}
	if kind := railKindBelow(pid, procs, 0); kind != "" {
		return kind
	}
	// A title outlives the process that set it. Only trust it when the process
	// table could not be read at all.
	if procs == nil {
		return railKindFromTitle(title)
	}
	return ""
}

func railKindBelow(pid int, procs map[int][]railProc, depth int) string {
	if depth > 6 || procs == nil {
		return ""
	}
	for _, p := range procs[pid] {
		if kind := railKindFromCommand(p.comm); kind != "" {
			return kind
		}
		if kind := railKindFromArgs(p.args); kind != "" {
			return kind
		}
		if kind := railKindBelow(p.pid, procs, depth+1); kind != "" {
			return kind
		}
	}
	return ""
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
	agents []railPaneInfo
	err    error
}

type railModel struct {
	tmux     tmux
	interval time.Duration

	agents     []railPaneInfo
	cursor     int
	lastActive string
	notice     string
	err        error

	width  int
	height int
}

func newRailModel(t tmux, interval time.Duration) railModel {
	return railModel{tmux: t, interval: interval, width: 32, height: 30}
}

func (m railModel) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(m.interval))
}

func (m railModel) refresh() tea.Cmd {
	t := m.tmux
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		agents, err := t.railPanes(ctx)
		if err == nil {
			t.ensureRailOSC(ctx, agents)
		}
		return railSnapshotMsg{agents: agents, err: err}
	}
}

func (m railModel) focusCmd(pane string) tea.Cmd {
	t := m.tmux
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := t.railFocus(ctx, pane); err != nil {
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
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.refresh(), tick(m.interval))

	case railSnapshotMsg:
		keep := m.selectedPane()
		m.err = msg.err
		if msg.err == nil {
			m.agents = msg.agents
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
			m.clampCursor()
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
		m.cursor++
	case "k", "up":
		m.cursor--
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = len(m.agents) - 1
	case "r":
		return m.refresh()
	case "enter":
		if p := m.selectedPane(); p != "" {
			return m.focusCmd(p)
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		idx := int(key[0] - '1')
		if idx < len(m.agents) {
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
		m.cursor--
	case tea.MouseButtonWheelDown:
		m.cursor++
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		if idx, ok := m.tileAt(msg.Y); ok {
			m.cursor = idx
			m.clampCursor()
			return m.focusCmd(m.agents[idx].Pane)
		}
	}
	m.clampCursor()
	return nil
}

func (m railModel) tileRange() (start, end int) {
	body := max(1, m.height-3)
	tiles := max(1, (body+1)/railTileStep)
	start = m.cursor - tiles/2
	if start < 0 {
		start = 0
	}
	if start+tiles > len(m.agents) {
		start = len(m.agents) - tiles
	}
	if start < 0 {
		start = 0
	}
	end = min(len(m.agents), start+tiles)
	return start, end
}

func (m railModel) tileAt(y int) (int, bool) {
	rel := y - 2
	if rel < 0 || len(m.agents) == 0 {
		return 0, false
	}
	start, end := m.tileRange()
	tile := rel / railTileStep
	if start+tile >= end {
		return 0, false
	}
	return start + tile, true
}

func (m *railModel) clampCursor() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if n := len(m.agents); n > 0 && m.cursor >= n {
		m.cursor = n - 1
	}
}

func (m railModel) selectedPane() string {
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

const railTileStep = 3 // two lines per agent, one separator between

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
	if m.err != nil {
		lines = append(lines, dimStyle.Render(truncate("tmux: "+m.err.Error(), width)))
	} else if len(m.agents) == 0 {
		lines = append(lines, dimStyle.Render("no panes"))
	} else {
		start, end := m.tileRange()
		for i := start; i < end; i++ {
			if i > start {
				lines = append(lines, ruleStyle.Render(strings.Repeat("─", width)))
			}
			lines = append(lines, m.rowLines(m.agents[i], width, i == m.cursor)...)
		}
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	keys := "j/k  enter  r  q"
	if m.notice != "" {
		keys = m.notice
	} else if it, ok := m.selectedAgent(); ok {
		if it.HarnessMsg != "" {
			keys = it.HarnessMsg
		} else if it.HarnessTitle != "" {
			keys = it.HarnessTitle
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
	if len(args) > 0 && args[0] == "toggle" {
		return railToggleCmd(ctx, args[1:], stdout, stderr)
	}
	fs := newFlagSet("rail", stderr)
	interval := fs.Duration("interval", time.Second, "how often to re-read the agent panes")
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
	t, err := railBackend(ctx, *backend)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	m := newRailModel(t, *interval)
	m.width, m.height = *width, *height
	if *asJSON {
		agents, err := t.railPanes(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
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
		agents, err := t.railPanes(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		m.agents = agents
		fmt.Fprint(stdout, m.View())
		return 0
	}
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	return 0
}

func railToggleCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("rail toggle", stderr)
	width := fs.Int("width", 32, "rail width in columns")
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
	if rail != "" {
		fmt.Fprintln(stdout, rail)
	}
	return 0
}
