package main

import (
	"context"
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

	agents, err := h.railPanes(ctx)
	if err != nil {
		t.Fatalf("railPanes: %v", err)
	}
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
	agents, err = h.railPanes(ctx)
	if err != nil {
		t.Fatalf("railPanes after toggle: %v", err)
	}
	for _, a := range agents {
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
	for _, want := range []string{"PANES", "w1-qg", "fleet:0.0", "r1-qg", "fleet:1.0", "\ueefa working", "fleet-view", "─", "j/k  enter  r  q"} {
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
		interval: time.Second,
		agents: []railPaneInfo{
			{Label: "a", Pane: "%1"},
			{Label: "b", Pane: "%2"},
		},
		cursor: 1,
	}
	next, _ := m.Update(railSnapshotMsg{agents: []railPaneInfo{
		{Label: "c", Pane: "%3"},
		{Label: "b", Pane: "%2"},
	}})
	got := next.(railModel)
	if got.selectedPane() != "%2" {
		t.Fatalf("cursor moved off the selected pane: %+v", got.agents)
	}
}

func TestRailAgentKindFindsPiUnderAShellAndNode(t *testing.T) {
	procs := map[int][]railProc{
		100: {{pid: 101, ppid: 100, comm: "zsh", args: "/usr/bin/zsh -l"}},
		101: {{pid: 102, ppid: 101, comm: "node", args: "node /usr/local/bin/pi --no-mcp"}},
	}
	if got := railAgentKind("zsh", "daemonking", "", 100, procs); got != "pi" {
		t.Fatalf("agent kind %q, want pi", got)
	}
	if got := railAgentKind("bash", "daemonking", "w1-qg", 999, nil); got != "pi" {
		t.Fatalf("the dungeon option must win, got %q", got)
	}
	if got := railAgentKind("zsh", "π - fleet-w1", "", 999, nil); got != "pi" {
		t.Fatalf("the pi title must be enough when ps is unavailable, got %q", got)
	}
	if got := railAgentKind("zsh", "π - stale", "", 999, map[int][]railProc{}); got != "" {
		t.Fatalf("a stale pi title must not keep a shell in the rail, got %q", got)
	}
	if got := railAgentKind("pi", "daemonking", "", 999, map[int][]railProc{}); got != "pi" {
		t.Fatalf("a terminal-only pi must be seen by its command, got %q", got)
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
	statuses := loadRailStatuses(dir)
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
	next, _ := m.Update(railSnapshotMsg{agents: []railPaneInfo{
		{Label: "a", Pane: "%1", Active: true},
		{Label: "b", Pane: "%2"},
	}})
	m = next.(railModel)
	next, _ = m.Update(railSnapshotMsg{agents: []railPaneInfo{
		{Label: "a", Pane: "%1"},
		{Label: "b", Pane: "%2"},
	}})
	m = next.(railModel)
	if !m.agents[0].Active || m.agents[1].Active {
		t.Fatalf("the last active mark moved: %+v", m.agents)
	}
}
