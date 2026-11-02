package main

import (
	"context"
	"errors"
	"io"
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
