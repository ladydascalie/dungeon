package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRailPanesFindsDungeonAgentsAndSkipsTheRail(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	_, _, root, err := h.CreateWorkspace(ctx, "rail", "workers", dir)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if _, err := h.call(ctx, "set-option", "-p", "-t", root, paneAgentOption, "w1-test"); err != nil {
		t.Fatalf("mark root agent: %v", err)
	}
	other, err := h.SplitPane(ctx, root, "right", dir, 0.5)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if _, err := h.call(ctx, "set-option", "-p", "-t", other, paneAgentOption, "r1-test"); err != nil {
		t.Fatalf("mark other agent: %v", err)
	}

	read, err := h.railPanes(ctx)
	if err != nil {
		t.Fatalf("railPanes: %v", err)
	}
	agents := read.agents
	labels := map[string]bool{}
	for _, a := range agents {
		labels[a.Label] = true
	}
	if !labels["w1-test"] || !labels["r1-test"] {
		t.Fatalf("rail missed an agent: %+v", agents)
	}

	rail, err := h.railToggle(ctx, other, 24, "sleep 60")
	if err != nil {
		t.Fatalf("railToggle: %v", err)
	}
	read, err = h.railPanes(ctx)
	if err != nil {
		t.Fatalf("railPanes after toggle: %v", err)
	}
	for _, a := range read.agents {
		if a.Pane == rail {
			t.Fatalf("the rail listed itself: %+v", agents)
		}
	}
}

func TestRailToggleIsLeftmostAndIdempotent(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	_, tab, root, err := h.CreateWorkspace(ctx, "railtoggle", "workers", dir)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	right, err := h.SplitPane(ctx, root, "right", dir, 0.5)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}

	rail, err := h.railToggle(ctx, right, 24, "sleep 60")
	if err != nil {
		t.Fatalf("railToggle: %v", err)
	}
	if rail == "" {
		t.Fatal("railToggle returned no pane")
	}
	out, err := h.call(ctx, "list-panes", "-t", tab, "-F", "#{pane_left}\t#{pane_id}\t#{@agent_rail}")
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	first := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
	if !strings.HasSuffix(first, rail+"\t1") {
		t.Fatalf("rail is not the leftmost pane:\n%s", out)
	}

	again, err := h.railToggle(ctx, right, 24, "sleep 60")
	if err != nil {
		t.Fatalf("second railToggle: %v", err)
	}
	if again != rail {
		t.Fatalf("second toggle made a new rail %s, want %s", again, rail)
	}

	if _, err := h.railToggle(ctx, rail, 24, "sleep 60"); err != nil {
		t.Fatalf("close railToggle: %v", err)
	}
	if _, err := h.Pane(ctx, rail); err == nil {
		t.Fatal("the rail pane survived its close")
	}
}

func TestRailViewShowsTheAgents(t *testing.T) {
	m := railModel{
		agents: []railPaneInfo{
			{Label: "w1-qg", Kind: "pi", Session: "fleet", WindowIndex: "0", PaneIndex: "0", Active: true},
			{Label: "r1-qg", Kind: "pi", Status: "working", Unit: "fleet-view", Session: "fleet", WindowIndex: "1", PaneIndex: "0"},
		},
		width:  40,
		height: 10,
	}
	out := m.View()
	for _, want := range []string{"PANES", "w1-qg", "fleet:0.0", "r1-qg", "fleet:1.0", "\ueefa working", "fleet-view", "─", "j/k  enter  z fold  r  q"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRailWithoutAgentsSaysSo(t *testing.T) {
	m := railModel{width: 30, height: 8}
	out := m.View()
	if !strings.Contains(out, "no panes") {
		t.Fatalf("missing the empty line:\n%s", out)
	}
}

func TestRailRefreshKeepsTheSelectedPane(t *testing.T) {
	m := railModel{
		backstop: time.Second,
		agents: []railPaneInfo{
			{Label: "a", Pane: "%1"},
			{Label: "b", Pane: "%2"},
		},
		cursor: 1,
	}
	next, _ := m.Update(railSnapshotMsg{read: railRead{agents: []railPaneInfo{
		{Label: "c", Pane: "%3"},
		{Label: "b", Pane: "%2"},
	}}})
	got := next.(railModel)
	if got.selectedPane() != "%2" {
		t.Fatalf("cursor moved off the selected pane: %+v", got.agents)
	}
}

func TestRailAgentKindUsesTheReportAsIdentity(t *testing.T) {
	report := oscStatus{State: "working", App: "pi"}
	if got := railAgentKind("zsh", "", report, true); got != "pi" {
		t.Fatalf("the report must win over a shell command, got %q", got)
	}
	if got := railAgentKind("bash", "w1-qg", oscStatus{State: "blocked"}, true); got != "agent" {
		t.Fatalf("a report without an app is still an agent, got %q", got)
	}
	if got := railAgentKind("bash", "w1-qg", oscStatus{}, false); got != "pi" {
		t.Fatalf("the dungeon option must win without a report, got %q", got)
	}
	if got := railAgentKind("pi", "", oscStatus{}, false); got != "pi" {
		t.Fatalf("a terminal-only pi must be seen by its command, got %q", got)
	}
	if got := railAgentKind("node", "", oscStatus{}, false); got != "" {
		t.Fatalf("a node without a report must not be guessed at, got %q", got)
	}
	if got := railAgentKind("zsh", "", oscStatus{State: "clear"}, true); got != "" {
		t.Fatalf("a cleared report must not identify a shell, got %q", got)
	}
}

func TestRailMouseClickSelectsTheTileAndFocusesIt(t *testing.T) {
	m := railModel{
		agents: []railPaneInfo{
			{Label: "a", Pane: "%1"},
			{Label: "b", Pane: "%2"},
		},
		width: 40, height: 10,
	}
	next, cmd := m.Update(tea.MouseMsg{
		X: 2, Y: 5,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	got := next.(railModel)
	if got.cursor != 1 {
		t.Fatalf("cursor %d, want the second tile", got.cursor)
	}
	if cmd == nil {
		t.Fatal("a click must focus the clicked agent")
	}
}

func TestRailStatusComesFromThePaneEncounter(t *testing.T) {
	root := t.TempDir()
	_, worktree, _ := fleetFixture(t, root)
	dir, ok := railEncounterRoot(worktree)
	if !ok {
		t.Fatal("no encounter root for the worktree")
	}
	statuses, issues := loadRailStatuses(dir)
	if len(issues) > 0 {
		t.Fatalf("fixture state unread: %+v", issues)
	}
	got, ok := statuses["%1"]
	if !ok {
		t.Fatalf("no status for the fleet lane: %+v", statuses)
	}
	if got.Status != "working" || got.Unit != "model" || got.Encounter != "alpha" {
		t.Fatalf("status %+v, want model working in alpha", got)
	}
}

func TestRailViewShowsHarnessStatusAndMessage(t *testing.T) {
	m := railModel{
		agents: []railPaneInfo{{
			Label:       "w1-qg",
			Kind:        "pi",
			Harness:     "blocked",
			HarnessKind: "permission",
			HarnessMsg:  "Approve the command?",
		}},
		width: 42, height: 10,
	}
	out := m.View()
	for _, want := range []string{"harness blocked", "Approve the command?"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRailKeepsTheLastActivePaneWhileFocused(t *testing.T) {
	m := railModel{width: 40, height: 10}
	next, _ := m.Update(railSnapshotMsg{read: railRead{agents: []railPaneInfo{
		{Label: "a", Pane: "%1", Active: true},
		{Label: "b", Pane: "%2"},
	}}})
	m = next.(railModel)
	next, _ = m.Update(railSnapshotMsg{read: railRead{agents: []railPaneInfo{
		{Label: "a", Pane: "%1"},
		{Label: "b", Pane: "%2"},
	}}})
	m = next.(railModel)
	if !m.agents[0].Active || m.agents[1].Active {
		t.Fatalf("the last active mark moved: %+v", m.agents)
	}
}

// railProbe runs the rail's real event loop without a terminal, so a test can
// prove which wake-up produced a refresh.
type railProbe struct {
	rail      railModel
	snapshots chan railSnapshotMsg
}

func (p *railProbe) Init() tea.Cmd { return p.rail.Init() }

func (p *railProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if snap, ok := msg.(railSnapshotMsg); ok {
		// The model takes ownership of the slice and mutates the active mark.
		// Copy it for the test so the probe does not race the event loop.
		snap.read.agents = append([]railPaneInfo(nil), snap.read.agents...)
		select {
		case p.snapshots <- snap:
		default:
		}
	}
	next, cmd := p.rail.Update(msg)
	p.rail = next.(railModel)
	return p, cmd
}

func (p *railProbe) View() string { return p.rail.View() }

func startRailProbe(t *testing.T, ctx context.Context, m railModel) (*railProbe, func()) {
	t.Helper()
	probe := &railProbe{rail: m, snapshots: make(chan railSnapshotMsg, 256)}
	prog := tea.NewProgram(probe,
		tea.WithInput(nil), tea.WithOutput(io.Discard),
		tea.WithoutRenderer(), tea.WithContext(ctx))
	done := make(chan error, 1)
	go func() {
		_, err := prog.Run()
		done <- err
	}()
	stop := func() {
		prog.Quit()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("rail program did not stop")
		}
	}
	return probe, stop
}

func waitRailSnapshot(t *testing.T, ch <-chan railSnapshotMsg, within time.Duration, what string, ok func(railSnapshotMsg) bool) railSnapshotMsg {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		select {
		case snap := <-ch:
			if snap.err != nil {
				continue
			}
			if ok(snap) {
				return snap
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the rail did not refresh after %s within %s", what, within)
		}
	}
}

// waitPaneCommand waits for a freshly created pane to exec its command. tmux
// reports the client's own name for the first instant of a new window.
func waitPaneCommand(t *testing.T, h tmux, pane, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := runInTestServer(t, h.socket, "display-message", "-p", "-t", pane, "#{pane_current_command}")
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s command %q, want %q", pane, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func drainSnapshots(ch <-chan railSnapshotMsg) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func railHasPane(pane string) func(railSnapshotMsg) bool {
	return func(snap railSnapshotMsg) bool {
		for _, a := range snap.read.agents {
			if a.Pane == pane {
				return true
			}
		}
		return false
	}
}

// The backstop is set to half a minute. Only the control-mode topology event
// can explain a refresh inside two seconds.
func TestRailWakesOnTmuxTopologyNotTheBackstop(t *testing.T) {
	h := tmuxTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan controlEvent, 256)
	control := startTmuxControl(ctx, h, events)
	waitControlConnected(t, control)
	files := make(chan watchEvent, 16)

	m := newRailModel(h, 30*time.Second, railSources{events: events, files: files, control: control})
	probe, stop := startRailProbe(t, ctx, m)
	defer stop()

	waitRailSnapshot(t, probe.snapshots, 3*time.Second, "the first read", func(railSnapshotMsg) bool { return true })

	pane := runInTestServer(t, h.socket, "new-window", "-d", "-P", "-F", "#{pane_id}",
		"-t", "dungeon-test-server", "sh")
	waitPaneCommand(t, h, pane, "sh")
	drainSnapshots(probe.snapshots)

	started := time.Now()
	runInTestServer(t, h.socket, "rename-window", "-t", pane, "woken")
	waitRailSnapshot(t, probe.snapshots, 2*time.Second, "a window rename", railHasPane(pane))
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("the rail refreshed only after %s", elapsed)
	}
}

// The same proof for a file: a state write must wake the rail long before the
// backstop fires.
func TestRailWakesOnStateWriteNotTheBackstop(t *testing.T) {
	h := tmuxTest(t)
	root := t.TempDir()
	dungeon := filepath.Join(root, ".dungeon")
	if err := os.MkdirAll(dungeon, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := `encounter: alpha
repo: /repo/alpha
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
units:
  - id: model
    minion: w1
    paths: [model.go]
    goal: Build the model.
    accept: go test ./...
`
	if err := os.WriteFile(filepath.Join(dungeon, "plan.yaml"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	pane := runInTestServer(t, h.socket, "new-window", "-d", "-P", "-F", "#{pane_id}",
		"-t", "dungeon-test-server", "-c", root, "sh")
	waitPaneCommand(t, h, pane, "sh")
	if err := os.WriteFile(filepath.Join(dungeon, "state.json.tmp"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dungeon, "state.json")
	if err := SaveState(statePath, State{
		Encounter: "alpha",
		Units:     map[string]UnitState{"model": {Status: "planned"}},
		Minions:   map[string]MinionState{"w1": {Status: "spawned", Pane: pane}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	files := make(chan watchEvent, 256)
	if _, err := startWatch(ctx, files, watchTreeDirs(dungeon), nil); err != nil {
		t.Fatalf("startWatch: %v", err)
	}

	m := newRailModel(h, 30*time.Second, railSources{files: files})
	probe, stop := startRailProbe(t, ctx, m)
	defer stop()

	planned := func(snap railSnapshotMsg) bool {
		for _, a := range snap.read.agents {
			if a.Pane == pane && a.Status == "planned" {
				return true
			}
		}
		return false
	}
	waitRailSnapshot(t, probe.snapshots, 3*time.Second, "the first read", planned)

	if err := SaveState(statePath, State{
		Encounter: "alpha",
		Units:     map[string]UnitState{"model": {Status: "working"}},
		Minions:   map[string]MinionState{"w1": {Status: "spawned", Pane: pane}},
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	waitRailSnapshot(t, probe.snapshots, 2*time.Second, "a state write", func(snap railSnapshotMsg) bool {
		for _, a := range snap.read.agents {
			if a.Pane == pane && a.Status == "working" {
				return true
			}
		}
		return false
	})
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("the rail refreshed only after %s", elapsed)
	}
}

func TestRailPanesUsesTheStatusReportAsIdentity(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	_, _, root, err := h.CreateWorkspace(ctx, "railreport", "workers", dir)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	t.Setenv("TMUX", "rail-report-test")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	pane := root
	if err := writeOSCStatus(oscStatus{Server: oscServerKey(), Pane: pane, State: "working", App: "claude", Msg: "reading"}); err != nil {
		t.Fatalf("write OSC record: %v", err)
	}
	defer removeOSCStatus(oscServerKey(), pane)

	read, err := h.railPanes(ctx)
	if err != nil {
		t.Fatalf("railPanes: %v", err)
	}
	for _, a := range read.agents {
		if a.Pane != pane {
			continue
		}
		if a.Kind != "claude" || a.Harness != "working" || a.HarnessMsg != "reading" {
			t.Fatalf("the report did not identify the pane: %+v", a)
		}
		return
	}
	t.Fatalf("the reporting pane was not listed: %+v", read.agents)
}

func TestRailNamesAnUnreadableState(t *testing.T) {
	dungeon := filepath.Join(t.TempDir(), ".dungeon")
	encounter := filepath.Join(dungeon, "alpha")
	if err := os.MkdirAll(encounter, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := "encounter: alpha\nminions:\n  - {id: w1, role: worker, worktree: w1}\nunits:\n  - id: model\n    minion: w1\n    goal: Build it.\n"
	if err := os.WriteFile(filepath.Join(encounter, "plan.yaml"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(encounter, "state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	statuses, issues := loadRailStatuses(dungeon)
	if len(statuses) != 0 {
		t.Fatalf("a broken state produced statuses: %+v", statuses)
	}
	if len(issues) != 1 || issues[0].kind != "state" {
		t.Fatalf("issues %+v, want one unread state", issues)
	}
}

func TestRailSaysASourceCouldNotBeReadWithItsAge(t *testing.T) {
	m := railModel{
		agents: []railPaneInfo{{Label: "w1", Kind: "pi", Pane: "%1"}},
		width:  72, height: 10,
		readAt:    time.Now().Add(-2 * time.Minute),
		readErr:   errors.New("no server running"),
		readErrAt: time.Now(),
	}
	out := m.View()
	for _, want := range []string{"tmux read failed", "just now", "no server running", "rows 2m ago"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	m = railModel{
		agents: []railPaneInfo{{Label: "w1", Kind: "pi", Pane: "%1"}},
		width:  84, height: 10,
		issues: []railSourceIssue{{
			kind: "state",
			path: filepath.Join("alpha", "state.json"),
			err:  errors.New("permission denied"),
			at:   time.Now(),
		}},
	}
	out = m.View()
	for _, want := range []string{"state state.json", "just now", "permission denied"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// railToggleEnv points the in-process CLI at the test's private tmux server and
// at pane, the way tmux sets its environment for a binding.
func railToggleEnv(t *testing.T, h tmux, pane string) {
	t.Helper()
	pid, err := h.call(context.Background(), "display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatalf("server pid: %v", err)
	}
	t.Setenv("TMUX", socketPath()+","+strings.TrimSpace(string(pid))+",0")
	t.Setenv("TMUX_PANE", pane)
}

func TestRailTogglePrintsNothingOnSuccess(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	_, _, root, err := h.CreateWorkspace(ctx, "quiet", "workers", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	right, err := h.SplitPane(ctx, root, "right", t.TempDir(), 0.5)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	railToggleEnv(t, h, right)

	code, stdout, stderr := runCLI("rail", "toggle", "--mux", "tmux")
	if code != 0 {
		t.Fatalf("toggle exit %d, stderr %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("a successful toggle wrote %q to stdout; tmux would show it", stdout)
	}
}

func TestRailTogglePrintPaneStillReportsThePane(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	_, _, root, err := h.CreateWorkspace(ctx, "quiet", "workers", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	right, err := h.SplitPane(ctx, root, "right", t.TempDir(), 0.5)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	railToggleEnv(t, h, right)

	code, stdout, stderr := runCLI("rail", "toggle", "--print-pane", "--mux", "tmux")
	if code != 0 {
		t.Fatalf("toggle exit %d, stderr %q", code, stderr)
	}
	rail := strings.TrimSpace(stdout)
	if rail == "" {
		t.Fatal("--print-pane printed no pane id")
	}
	if _, err := h.Pane(ctx, rail); err != nil {
		t.Fatalf("--print-pane reported %s, which is not a pane: %v", rail, err)
	}
}

func TestRailGroupsSessionsThenEncounters(t *testing.T) {
	agents := []railPaneInfo{
		{Label: "plain", Session: "10", Pane: "%0"},
		{Label: "w1", Session: "fleet", Pane: "%1", Encounter: "fleet-view"},
		{Label: "r1", Session: "fleet", Pane: "%2", Encounter: "review-fleet"},
		{Label: "w2", Session: "fleet", Pane: "%3", Encounter: "fleet-view"},
		{Label: "idle", Session: "fleet", Pane: "%4"},
		{Label: "w3", Session: "own-stack", Pane: "%5", Encounter: "rail-events"},
	}
	got := railGroups(agents)
	want := []struct {
		session, encounter string
		unscoped           bool
		panes              []int
	}{
		{"10", "", false, []int{0}},
		{"fleet", "fleet-view", false, []int{1, 3}},
		{"fleet", "review-fleet", false, []int{2}},
		{"fleet", "", true, []int{4}},
		{"own-stack", "rail-events", false, []int{5}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.session != w.session || g.encounter != w.encounter || g.unscoped != w.unscoped {
			t.Fatalf("group %d is %+v, want %+v", i, g, w)
		}
		if len(g.panes) != len(w.panes) {
			t.Fatalf("group %d panes %v, want %v", i, g.panes, w.panes)
		}
		for j := range w.panes {
			if g.panes[j] != w.panes[j] {
				t.Fatalf("group %d panes %v, want %v", i, g.panes, w.panes)
			}
		}
	}
}

func TestRailViewShowsSessionAndEncounterHeaders(t *testing.T) {
	m := railModel{
		agents: []railPaneInfo{
			{Label: "w1", Kind: "pi", Session: "fleet", Encounter: "fleet-view", WindowIndex: "1", PaneIndex: "1"},
			{Label: "idle", Kind: "pi", Session: "fleet", WindowIndex: "2", PaneIndex: "0"},
			{Label: "w9", Kind: "pi", Session: "own-stack", Encounter: "rail-events", WindowIndex: "0", PaneIndex: "1"},
		},
		width: 40, height: 20,
	}
	out := m.View()
	order := []string{"fleet", "fleet-view", "w1", "no encounter", "idle", "own-stack", "rail-events", "w9"}
	at := -1
	for _, want := range order {
		i := strings.Index(out, want)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
		if i < at {
			t.Fatalf("%q is out of order in:\n%s", want, out)
		}
		at = i
	}
}

func TestRailClickMapsThroughGroupHeaders(t *testing.T) {
	m := railModel{
		agents: []railPaneInfo{
			{Label: "w1", Pane: "%1", Session: "fleet", Encounter: "fleet-view"},
			{Label: "r1", Pane: "%2", Session: "fleet", Encounter: "review-fleet"},
		},
		width: 40, height: 20,
	}
	lay := m.layout()
	want := 1
	top := -1
	for i, pane := range lay.linePane {
		if pane == want {
			top = i
			break
		}
	}
	if top < 0 {
		t.Fatalf("no tile for pane %d in:\n%s", want, strings.Join(lay.lines, "\n"))
	}
	next, cmd := m.Update(tea.MouseMsg{
		X: 2, Y: top + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	})
	if got := next.(railModel).cursor; got != want {
		t.Fatalf("click on line %d selected %d, want %d", top, got, want)
	}
	if cmd == nil {
		t.Fatal("a click must focus the clicked pane")
	}
}

func TestRailWindowFollowsTheCursor(t *testing.T) {
	var agents []railPaneInfo
	for i := 0; i < 20; i++ {
		agents = append(agents, railPaneInfo{Label: "pane", Pane: "%" + string(rune('a'+i)), Session: "fleet"})
	}
	m := railModel{agents: agents, width: 40, height: 12, cursor: 18}
	m.clampCursor()
	lay := m.layout()
	start := m.window(lay)
	top := -1
	for i, pane := range lay.linePane {
		if pane == m.cursor {
			top = i
			break
		}
	}
	body := m.railBody()
	if top < start || top+1 >= start+body {
		t.Fatalf("selected tile at %d is outside the window %d..%d", top, start, start+body)
	}
}

func TestRailDoesNotRepeatTheSessionAsItsEncounter(t *testing.T) {
	m := railModel{
		agents: []railPaneInfo{
			{Label: "w1", Kind: "pi", Pane: "%1", Session: "fleet", Encounter: "fleet", WindowIndex: "1", PaneIndex: "1"},
		},
		width: 40, height: 12,
	}
	lay := m.layout()
	if len(lay.lines) != 3 {
		t.Fatalf("layout has %d lines, want one session header and two tile lines:\n%s",
			len(lay.lines), strings.Join(lay.lines, "\n"))
	}
}

func TestRailScrollsByOneTileNotAScreen(t *testing.T) {
	var agents []railPaneInfo
	for i := 0; i < 20; i++ {
		agents = append(agents, railPaneInfo{Label: "pane", Pane: "%" + string(rune('a'+i)), Session: "fleet"})
	}
	m := railModel{agents: agents, width: 40, height: 16}
	m.clampCursor()
	if got := m.scroll; got != 0 {
		t.Fatalf("start scroll %d, want 0", got)
	}
	before := m.scroll
	for i := 1; i < 4; i++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(railModel)
	}
	if m.scroll != before {
		t.Fatalf("the window moved while the cursor was inside it: %d -> %d", before, m.scroll)
	}
	// Walk to the bottom edge of the window; one more step moves it by one tile.
	for i := 0; i < 20; i++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(railModel)
	}
	if m.cursor != len(agents)-1 {
		t.Fatalf("cursor %d, want the last pane", m.cursor)
	}
	lay := m.layout()
	start := m.window(lay)
	top := -1
	for i, pane := range lay.linePane {
		if pane == m.cursor {
			top = i
			break
		}
	}
	if top < start || top+1 >= start+m.railBody() {
		t.Fatalf("the last pane is outside the window %d..%d (tile at %d)", start, start+m.railBody(), top)
	}
	if m.scroll > len(lay.lines)-m.railBody() {
		t.Fatalf("scroll %d is past the end of %d lines", m.scroll, len(lay.lines))
	}
}

func railFoldFixture() railModel {
	return railModel{
		agents: []railPaneInfo{
			{Label: "w1", Kind: "pi", Pane: "%1", Session: "fleet", Encounter: "fleet-view"},
			{Label: "w2", Kind: "pi", Pane: "%2", Session: "fleet", Encounter: "fleet-view"},
			{Label: "r1", Kind: "pi", Pane: "%3", Session: "fleet", Encounter: "review-fleet"},
			{Label: "idle", Kind: "pi", Pane: "%4", Session: "fleet"},
			{Label: "w9", Kind: "pi", Pane: "%5", Session: "own-stack", Encounter: "rail-events"},
		},
		width: 40, height: 30,
	}
}

func TestRailFoldsTheEncounterUnderTheCursor(t *testing.T) {
	m := railFoldFixture()
	m.key("z")
	want := railEncounterGroup("fleet", "fleet-view")
	if !m.folded[want] {
		t.Fatalf("z did not fold the encounter: %+v", m.folded)
	}
	if m.onGroup != want {
		t.Fatalf("cursor %q, want the folded header", m.onGroup)
	}
	joined := strings.Join(m.layout().lines, "\n")
	if strings.Contains(joined, "w1") || strings.Contains(joined, "w2") {
		t.Fatalf("the folded panes are still drawn:\n%s", joined)
	}
	if !strings.Contains(joined, "fleet-view (2)") {
		t.Fatalf("the folded header lost its count:\n%s", joined)
	}
	for _, keep := range []string{"r1", "idle", "w9"} {
		if !strings.Contains(joined, keep) {
			t.Fatalf("folding hid %q:\n%s", keep, joined)
		}
	}
}

func TestRailFoldsTheWholeSession(t *testing.T) {
	m := railFoldFixture()
	m.key("Z")
	if !m.folded[railSessionGroup("fleet")] {
		t.Fatalf("Z did not fold the session: %+v", m.folded)
	}
	joined := strings.Join(m.layout().lines, "\n")
	if strings.Contains(joined, "r1") || strings.Contains(joined, "idle") {
		t.Fatalf("the session's panes are still drawn:\n%s", joined)
	}
	if !strings.Contains(joined, "fleet (4)") {
		t.Fatalf("the folded session header lost its count:\n%s", joined)
	}
	if !strings.Contains(joined, "w9") {
		t.Fatalf("folding one session hid another:\n%s", joined)
	}
}

func TestRailUnfoldsFromTheFoldedHeader(t *testing.T) {
	m := railFoldFixture()
	m.key("z")
	m.key("z")
	want := railEncounterGroup("fleet", "fleet-view")
	if m.folded[want] {
		t.Fatalf("a second z did not unfold: %+v", m.folded)
	}
	if m.onGroup != want {
		t.Fatalf("the cursor left the header: %q", m.onGroup)
	}
	if !strings.Contains(strings.Join(m.layout().lines, "\n"), "w1") {
		t.Fatal("the unfolded panes are still hidden")
	}
}

func TestRailWalksHeadersWithTheKeyboard(t *testing.T) {
	m := railFoldFixture()
	m.key("k")
	if m.onGroup != railEncounterGroup("fleet", "fleet-view") {
		t.Fatalf("k from the first tile landed on %q, want the encounter header", m.onGroup)
	}
	if p := m.selectedPane(); p != "" {
		t.Fatalf("a header selected pane %q", p)
	}
	m.key("k")
	if m.onGroup != railSessionGroup("fleet") {
		t.Fatalf("k from the encounter header landed on %q, want the session header", m.onGroup)
	}
	m.key("j")
	m.key("j")
	if m.onGroup != "" || m.cursor != 0 {
		t.Fatalf("j back down landed on group %q pane %d", m.onGroup, m.cursor)
	}
}

func TestRailClickOnAHeaderSelectsItAndFocusesNothing(t *testing.T) {
	m := railFoldFixture()
	line := -1
	for i, group := range m.layout().lineGroup {
		if group == railSessionGroup("own-stack") {
			line = i
			break
		}
	}
	if line < 0 {
		t.Fatal("no session header for own-stack")
	}
	next, cmd := m.Update(tea.MouseMsg{
		X: 2, Y: line + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	})
	got := next.(railModel)
	if got.onGroup != railSessionGroup("own-stack") {
		t.Fatalf("the click selected %q, want the own-stack header", got.onGroup)
	}
	if cmd != nil {
		t.Fatal("a header click must not focus a pane")
	}
}

func TestRailKeepsFoldsAcrossARefresh(t *testing.T) {
	m := railFoldFixture()
	m.key("z")
	want := railEncounterGroup("fleet", "fleet-view")
	next, _ := m.Update(railSnapshotMsg{read: railRead{agents: m.agents}, at: time.Now()})
	got := next.(railModel)
	if !got.folded[want] {
		t.Fatalf("a refresh unfolded the group: %+v", got.folded)
	}
	if got.onGroup != want {
		t.Fatalf("a refresh moved the cursor to %q", got.onGroup)
	}
}

func TestRailJumpUnfoldsTheTargetPane(t *testing.T) {
	m := railFoldFixture()
	m.key("Z")
	if !m.folded[railSessionGroup("fleet")] {
		t.Fatalf("Z did not fold the session: %+v", m.folded)
	}
	m.key("2")
	if m.folded[railSessionGroup("fleet")] {
		t.Fatal("the number jump left the session folded")
	}
	if m.onGroup != "" || m.cursor != 1 {
		t.Fatalf("the jump selected group %q pane %d, want pane 1", m.onGroup, m.cursor)
	}
}

func TestRailRestartReplacesTheRailInThisWindow(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	_, _, root, err := h.CreateWorkspace(ctx, "railrestart", "workers", dir)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	rail, err := h.railToggle(ctx, root, 24, "sleep 60")
	if err != nil {
		t.Fatalf("railToggle: %v", err)
	}
	restarted, err := h.railRestart(ctx, root, false, 24, "sleep 60")
	if err != nil {
		t.Fatalf("railRestart: %v", err)
	}
	if len(restarted) != 1 {
		t.Fatalf("restarted %d rails, want 1: %v", len(restarted), restarted)
	}
	if restarted[0] == rail {
		t.Fatal("restart kept the old rail pane")
	}
	if _, err := h.Pane(ctx, rail); err == nil {
		t.Fatal("the old rail pane survived")
	}
	out, err := h.call(ctx, "display-message", "-p", "-t", restarted[0], "#{@agent_rail}\t#{pane_width}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "1\t24" {
		t.Fatalf("the new rail is %q, want the rail mark and its old width", got)
	}
}

func TestRailRestartAllLeavesNothingBehind(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	_, _, first, err := h.CreateWorkspace(ctx, "railall-a", "workers", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	_, _, second, err := h.CreateWorkspace(ctx, "railall-b", "workers", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	a, err := h.railToggle(ctx, first, 24, "sleep 60")
	if err != nil {
		t.Fatalf("railToggle a: %v", err)
	}
	b, err := h.railToggle(ctx, second, 24, "sleep 60")
	if err != nil {
		t.Fatalf("railToggle b: %v", err)
	}
	restarted, err := h.railRestart(ctx, first, true, 24, "sleep 60")
	if err != nil {
		t.Fatalf("railRestart --all: %v", err)
	}
	if len(restarted) != 2 {
		t.Fatalf("restarted %d rails, want 2: %v", len(restarted), restarted)
	}
	for _, old := range []string{a, b} {
		if _, err := h.Pane(ctx, old); err == nil {
			t.Fatalf("the old rail %s survived --all", old)
		}
	}
	for _, pane := range restarted {
		if _, err := h.Pane(ctx, pane); err != nil {
			t.Fatalf("the new rail %s is gone: %v", pane, err)
		}
	}
}

func TestRailRestartWithoutARailSaysSo(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	_, _, root, err := h.CreateWorkspace(ctx, "railnone", "workers", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if _, err := h.railRestart(ctx, root, false, 24, "sleep 60"); err == nil {
		t.Fatal("railRestart invented a rail where there was none")
	}
}

func TestRailReexecsWhenItsBinaryChanges(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "dungeon")
	if err := os.WriteFile(exe, []byte("build one"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := railModel{backstop: time.Second, exe: exe, stamp: railStamp(exe)}
	if m.stamp == "" {
		t.Fatal("no stamp for the binary")
	}
	next, cmd := m.Update(tickMsg{})
	if got := next.(railModel); got.reload {
		t.Fatal("an unchanged binary asked for a re-exec")
	} else if cmd == nil {
		t.Fatal("the unchanged rail stopped its tick")
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(exe, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	next, cmd = m.Update(tickMsg{})
	if got := next.(railModel); !got.reload {
		t.Fatal("a replaced binary did not ask for a re-exec")
	}
	if cmd == nil {
		t.Fatal("a re-exec must stop the running program")
	}
}

func TestRailMarksThePaneItRunsOn(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	_, _, root, err := h.CreateWorkspace(ctx, "railmark", "workers", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := h.markRailPane(ctx, root); err != nil {
		t.Fatalf("markRailPane: %v", err)
	}
	out, err := h.call(ctx, "display-message", "-p", "-t", root, "#{@agent_rail}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("the pane is marked %q, want 1", got)
	}
	if err := h.markRailPane(ctx, ""); err == nil {
		t.Fatal("markRailPane accepted an empty pane")
	}
}

func TestRailDeclaredLayoutScalesThePanesAside(t *testing.T) {
	var lay windowLayout
	raw := `{"V":2,"L":{"t":"p","w":120,"h":40,"x":0,"y":0,"a":true,"i":0,"I":"%1"}}`
	if err := json.Unmarshal([]byte(raw), &lay); err != nil {
		t.Fatalf("parse: %v", err)
	}
	declared, err := railDeclaredLayout(lay, "%9", 24)
	if err != nil {
		t.Fatalf("railDeclaredLayout: %v", err)
	}
	var got windowLayout
	if err := json.Unmarshal([]byte(declared), &got); err != nil {
		t.Fatalf("declared layout is not JSON: %v\n%s", err, declared)
	}
	if got.L.T != "h" || len(got.L.C) != 2 {
		t.Fatalf("root %+v, want a horizontal split of two cells", got.L)
	}
	rail, content := got.L.C[0], got.L.C[1]
	if rail.I != "%9" || rail.W != 24 || rail.X != 0 || !rail.A {
		t.Fatalf("rail cell %+v, want %%9 24 wide at x 0 and active", rail)
	}
	if content.I != "%1" || content.W != 96 || content.X != 24 || content.A {
		t.Fatalf("content cell %+v, want %%1 96 wide at x 24 and inactive", content)
	}
	if rail.Index == nil || content.Index == nil || *rail.Index != 0 || *content.Index != 1 {
		t.Fatalf("pane indexes %v %v, want 0 and 1", rail.Index, content.Index)
	}
	if got.L.W != 120 || got.L.H != 40 {
		t.Fatalf("window %dx%d, want 120x40", got.L.W, got.L.H)
	}
}

func TestRailDeclaredLayoutKeepsAVerticalStack(t *testing.T) {
	var lay windowLayout
	raw := `{"V":2,"L":{"t":"h","w":120,"h":40,"x":0,"y":0,"c":[` +
		`{"t":"p","w":60,"h":40,"x":0,"y":0,"i":0,"I":"%1"},` +
		`{"t":"v","w":59,"h":40,"x":61,"y":0,"c":[` +
		`{"t":"p","w":59,"h":20,"x":61,"y":0,"i":1,"I":"%2"},` +
		`{"t":"p","w":59,"h":19,"x":61,"y":21,"a":true,"i":2,"I":"%3"}]}]}}`
	if err := json.Unmarshal([]byte(raw), &lay); err != nil {
		t.Fatalf("parse: %v", err)
	}
	declared, err := railDeclaredLayout(lay, "%9", 24)
	if err != nil {
		t.Fatalf("railDeclaredLayout: %v", err)
	}
	var got windowLayout
	if err := json.Unmarshal([]byte(declared), &got); err != nil {
		t.Fatalf("declared layout is not JSON: %v", err)
	}
	content := got.L.C[1]
	if content.T != "h" || len(content.C) != 2 {
		t.Fatalf("content root %+v, want the old horizontal split", content)
	}
	stack := content.C[1]
	if stack.T != "v" || len(stack.C) != 2 {
		t.Fatalf("stack %+v, want the vertical split kept", stack)
	}
	if stack.C[0].H != 20 || stack.C[1].H != 19 || stack.C[1].Y != 21 {
		t.Fatalf("stack heights changed: %+v", stack.C)
	}
	if stack.C[0].I != "%2" || stack.C[1].I != "%3" {
		t.Fatalf("stack panes changed: %+v", stack.C)
	}
	active := 0
	var walk func(c layoutCell)
	walk = func(c layoutCell) {
		if c.T == "p" && c.A {
			active++
		}
		for _, child := range c.C {
			walk(child)
		}
	}
	walk(got.L)
	if active != 1 {
		t.Fatalf("%d active panes, tmux accepts exactly one", active)
	}
}

func TestRailDeclaredLayoutRefusesAWindowWithNoRoom(t *testing.T) {
	lay := windowLayout{V: 2, L: layoutCell{T: "p", W: 20, H: 10, I: "%1"}}
	if _, err := railDeclaredLayout(lay, "%9", 24); err == nil {
		t.Fatal("a rail wider than the window was accepted")
	}
}

func TestRailRetargetedLayoutKeepsEveryWidth(t *testing.T) {
	var lay windowLayout
	raw := `{"V":2,"L":{"t":"h","w":120,"h":40,"x":0,"y":0,"c":[` +
		`{"t":"p","w":24,"h":40,"x":0,"y":0,"a":true,"i":0,"I":"%9"},` +
		`{"t":"p","w":60,"h":40,"x":25,"y":0,"i":1,"I":"%1"},` +
		`{"t":"p","w":34,"h":40,"x":86,"y":0,"i":2,"I":"%2"}]}}`
	if err := json.Unmarshal([]byte(raw), &lay); err != nil {
		t.Fatalf("parse: %v", err)
	}
	declared, err := railRetargetedLayout(lay, "%9", "%10")
	if err != nil {
		t.Fatalf("railRetargetedLayout: %v", err)
	}
	var got windowLayout
	if err := json.Unmarshal([]byte(declared), &got); err != nil {
		t.Fatalf("declared layout is not JSON: %v", err)
	}
	widths := map[string]int{}
	for _, cell := range got.L.C {
		widths[cell.I] = cell.W
	}
	if _, stale := widths["%9"]; stale {
		t.Fatalf("the old rail is still in the layout: %+v", widths)
	}
	want := map[string]int{"%10": 24, "%1": 60, "%2": 34}
	for id, w := range want {
		if widths[id] != w {
			t.Fatalf("pane %s is %d wide, want %d: %+v", id, widths[id], w, widths)
		}
	}
	if _, err := railRetargetedLayout(lay, "%404", "%10"); err == nil {
		t.Fatal("retargeting a pane that is not in the layout was accepted")
	}
}

// paneWidths reads every pane of a window as id to width, and the window width.
func paneWidths(t *testing.T, h tmux, ctx context.Context, window string) (map[string]int, int) {
	t.Helper()
	out, err := h.call(ctx, "list-panes", "-t", window, "-F", "#{pane_id}\x1f#{pane_width}")
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	widths := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) != 2 {
			continue
		}
		var w int
		fmt.Sscan(f[1], &w)
		widths[f[0]] = w
	}
	total, err := h.call(ctx, "display-message", "-p", "-t", window, "#{window_width}")
	if err != nil {
		t.Fatalf("window_width: %v", err)
	}
	var windowWidth int
	fmt.Sscan(strings.TrimSpace(string(total)), &windowWidth)
	return widths, windowWidth
}

func TestRailToggleScalesTheWindowInsteadOfStealing(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	_, tab, root, err := h.CreateWorkspace(ctx, "raillayout", "workers", dir)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	right, err := h.SplitPane(ctx, root, "right", dir, 0.5)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	before, windowWidth := paneWidths(t, h, ctx, tab)
	if _, err := h.railToggle(ctx, right, 20, "sleep 60"); err != nil {
		t.Fatalf("railToggle: %v", err)
	}
	after, _ := paneWidths(t, h, ctx, tab)
	contentWidth := windowWidth - 20
	for id, had := range before {
		want := int(math.Round(float64(had) * float64(contentWidth) / float64(windowWidth)))
		got, ok := after[id]
		if !ok {
			t.Fatalf("pane %s vanished: %+v", id, after)
		}
		if got < want-2 || got > want+2 {
			t.Fatalf("pane %s is %d wide, want about %d (had %d of %d, rail 20): %+v",
				id, got, want, had, windowWidth, after)
		}
	}
	for id, w := range after {
		if _, had := before[id]; had {
			continue
		}
		if w != 20 {
			t.Fatalf("the rail is %d wide, want 20: %+v", w, after)
		}
	}
}

func TestRailRestartKeepsEveryWidth(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	_, tab, root, err := h.CreateWorkspace(ctx, "railkeep", "workers", dir)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	right, err := h.SplitPane(ctx, root, "right", dir, 0.5)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	rail, err := h.railToggle(ctx, right, 20, "sleep 60")
	if err != nil {
		t.Fatalf("railToggle: %v", err)
	}
	before, _ := paneWidths(t, h, ctx, tab)
	restarted, err := h.railRestart(ctx, right, false, 20, "sleep 60")
	if err != nil {
		t.Fatalf("railRestart: %v", err)
	}
	if len(restarted) != 1 {
		t.Fatalf("restarted %d rails, want 1", len(restarted))
	}
	after, _ := paneWidths(t, h, ctx, tab)
	for id, had := range before {
		if id == rail {
			continue
		}
		if after[id] != had {
			t.Fatalf("pane %s changed width from %d to %d: before %+v after %+v",
				id, had, after[id], before, after)
		}
	}
	if got := after[restarted[0]]; got != before[rail] {
		t.Fatalf("the new rail is %d wide, the old one was %d", got, before[rail])
	}
}
