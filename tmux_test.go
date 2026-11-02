package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tmuxTest gives a test its own tmux server. It never talks to the operator's
// default server: every command names the dungeon-test socket and the server
// starts with /dev/null as its config.
func tmuxTest(t *testing.T) tmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	const socket = "dungeon-test"
	run := func(args ...string) ([]byte, error) {
		full := append([]string{"-L", socket, "-f", "/dev/null"}, args...)
		return exec.Command("tmux", full...).CombinedOutput()
	}
	_, _ = run("kill-server")
	if out, err := run("start-server"); err != nil {
		t.Fatalf("start private tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = run("kill-server") })
	return tmux{socket: socket}
}

// runInTestServer executes a raw tmux command on the test's private server.
func runInTestServer(t *testing.T, socket string, args ...string) string {
	t.Helper()
	full := append([]string{"-L", socket}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTmuxAgentsOnACleanServerIsEmpty(t *testing.T) {
	h := tmuxTest(t)
	agents, err := h.Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("Agents on a clean server = %v, want empty", agents)
	}
}

func TestTmuxWorkspaceLifecycle(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	cwd := t.TempDir()

	ws, tab, root, err := h.CreateWorkspace(ctx, "enc", "worker", cwd)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if ws == "" || tab == "" || root == "" {
		t.Fatalf("CreateWorkspace returned incomplete ids: %q %q %q", ws, tab, root)
	}
	info, err := h.Pane(ctx, root)
	if err != nil {
		t.Fatalf("Pane root: %v", err)
	}
	if info.Tab != tab || info.Workspace != ws {
		t.Fatalf("Pane root = %+v, want tab %s workspace %s", info, tab, ws)
	}

	tab2, pane2, err := h.CreateTab(ctx, ws, "workers", cwd)
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	info, err = h.Pane(ctx, pane2)
	if err != nil {
		t.Fatalf("Pane tab root: %v", err)
	}
	if info.Tab != tab2 || info.Workspace != ws {
		t.Fatalf("Pane tab root = %+v, want tab %s workspace %s", info, tab2, ws)
	}

	pane3, err := h.SplitPane(ctx, pane2, "right", cwd, 0.5)
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	info, err = h.Pane(ctx, pane3)
	if err != nil {
		t.Fatalf("Pane split: %v", err)
	}
	if info.Tab != tab2 || info.Workspace != ws {
		t.Fatalf("Pane split = %+v, want tab %s workspace %s", info, tab2, ws)
	}
	if _, err := h.SplitPane(ctx, pane2, "sideways", cwd, 0.5); err == nil {
		t.Fatal("SplitPane accepted an unknown direction")
	}

	if err := h.ClosePane(ctx, pane3); err != nil {
		t.Fatalf("ClosePane: %v", err)
	}
	if _, err := h.Pane(ctx, pane3); err == nil {
		t.Fatal("Pane still reports a closed pane")
	}

	if err := h.CloseWorkspace(ctx, ws); err != nil {
		t.Fatalf("CloseWorkspace: %v", err)
	}
	if _, err := h.Pane(ctx, root); err == nil {
		t.Fatal("Pane still reports a closed workspace's root")
	}
	for _, w := range h.Workspaces(ctx) {
		if w.ID == ws {
			t.Fatalf("Workspaces still lists closed workspace %s", ws)
		}
	}
}

func TestTmuxStartAgentRecordsIdentity(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()

	bin := t.TempDir()
	script := filepath.Join(bin, "pi")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ws, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	name := "w1-aa"
	done := make(chan error, 1)
	go func() { done <- h.StartAgent(ctx, name, root, []string{"--no-skills"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartAgent: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StartAgent did not return after the foreground command changed")
	}

	agents, err := h.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	a, ok := agents[name]
	if !ok {
		t.Fatalf("Agents = %v, want %s", agents, name)
	}
	if a.Pane != root {
		t.Fatalf("agent pane = %s, want %s", a.Pane, root)
	}
	if a.Status != AgentUnknown {
		t.Fatalf("agent status = %s, want %s", a.Status, AgentUnknown)
	}

	// A new backend with the same socket is a restarted dungeon: identity has
	// to come back from the pane option, not from this process.
	restarted := tmux{socket: h.socket}
	agents, err = restarted.Agents(ctx)
	if err != nil {
		t.Fatalf("restarted Agents: %v", err)
	}
	if _, ok := agents[name]; !ok {
		t.Fatalf("restarted Agents = %v, want %s", agents, name)
	}

	if err := h.CloseWorkspace(ctx, ws); err != nil {
		t.Fatalf("CloseWorkspace: %v", err)
	}
}

func TestTmuxPingCommandQuoting(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	_, _, root, err := h.CreateWorkspace(ctx, "enc", "shell", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	file := filepath.Join(t.TempDir(), "ping.txt")
	runInTestServer(t, h.socket, "send-keys", "-t", root, "-l", "cat > "+shellQuote(file))
	runInTestServer(t, h.socket, "send-keys", "-t", root, "Enter")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(file); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cat never started in the pane")
		}
		time.Sleep(20 * time.Millisecond)
	}

	msg := "cost $HOME `echo nope` and 'quoted'\nsecond line"
	ping := h.PingCommand(root, msg)
	if out, err := exec.Command("sh", "-c", ping).CombinedOutput(); err != nil {
		t.Fatalf("run ping command: %v: %s\n%s", err, out, ping)
	}

	deadline = time.Now().Add(5 * time.Second)
	for {
		got, _ := os.ReadFile(file)
		if string(got) == msg+"\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ping delivered %q, want %q plus Enter", got, msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTmuxCloseWorkspaceRefusesAForeignSession(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()

	runInTestServer(t, h.socket, "new-session", "-d", "-s", "foreign", "sleep 60")
	foreign := runInTestServer(t, h.socket, "display-message", "-p", "-t", "foreign", "#{session_id}")
	if foreign == "" {
		t.Fatal("foreign session has no id")
	}
	if err := h.CloseWorkspace(ctx, foreign); err == nil {
		t.Fatal("CloseWorkspace closed a session without the ownership option")
	}
	if got := runInTestServer(t, h.socket, "list-sessions", "-F", "#{session_id}"); !strings.Contains(got, foreign) {
		t.Fatalf("foreign session %s is gone; sessions: %s", foreign, got)
	}
}

func TestTmuxPingCommandLeadingDash(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	_, _, root, err := h.CreateWorkspace(ctx, "enc", "shell", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	file := filepath.Join(t.TempDir(), "ping.txt")
	runInTestServer(t, h.socket, "send-keys", "-t", root, "-l", "cat > "+shellQuote(file))
	runInTestServer(t, h.socket, "send-keys", "-t", root, "Enter")
	waitForFile(t, file)

	msg := "-l is not a flag here, and neither is --"
	ping := h.PingCommand(root, msg)
	if out, err := exec.Command("sh", "-c", ping).CombinedOutput(); err != nil {
		t.Fatalf("run ping command: %v: %s\n%s", err, out, ping)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := os.ReadFile(file)
		if string(got) == msg+"\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ping delivered %q, want %q plus Enter", got, msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForFile blocks until a shell started by a test has opened its redirect.
func waitForFile(t *testing.T, file string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(file); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cat never started: %s never appeared", file)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTmuxEmptyTargetIsAnError(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	ws, _, root, err := h.CreateWorkspace(ctx, "enc", "shell", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	checks := []struct {
		member string
		err    error
	}{
		{"CreateTab", func() error { _, _, err := h.CreateTab(ctx, "", "tab", t.TempDir()); return err }()},
		{"SplitPane", func() error { _, err := h.SplitPane(ctx, "", "right", t.TempDir(), 0.5); return err }()},
		{"Pane", func() error { _, err := h.Pane(ctx, ""); return err }()},
		{"StartAgent", h.StartAgent(ctx, "w1-aa", "", nil)},
		{"FocusTab", h.FocusTab(ctx, "")},
		{"ClosePane", h.ClosePane(ctx, "")},
		{"CloseWorkspace", h.CloseWorkspace(ctx, "")},
	}
	for _, c := range checks {
		if c.err == nil {
			t.Errorf("%s accepted an empty target", c.member)
		}
	}
	if t.Failed() {
		return
	}

	// An empty target must not have resolved to the current session: both the
	// workspace and its root pane are still there.
	if got := runInTestServer(t, h.socket, "list-sessions", "-F", "#{session_id}"); !strings.Contains(got, ws) {
		t.Fatalf("session %s is gone after an empty target; sessions: %s", ws, got)
	}
	if _, err := h.Pane(ctx, root); err != nil {
		t.Fatalf("root pane %s is gone after an empty target: %v", root, err)
	}
}

func TestTmuxStartAgentRefusesANameLiveInAnotherPane(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()

	bin := t.TempDir()
	script := filepath.Join(bin, "pi")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ws, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	other, err := h.SplitPane(ctx, root, "right", t.TempDir(), 0.5)
	if err != nil {
		t.Fatalf("SplitPane other: %v", err)
	}

	name := "w1-aa"
	done := make(chan error, 1)
	go func() { done <- h.StartAgent(ctx, name, root, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartAgent: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StartAgent did not start the agent")
	}

	// Same name, same pane: the agent is already there, so the call is a no-op.
	if err := h.StartAgent(ctx, name, root, nil); err != nil {
		t.Fatalf("StartAgent on the same pane = %v, want nil", err)
	}
	agents, err := h.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if a := agents[name]; a.Pane != root {
		t.Fatalf("agent %s is at %s, want %s", name, a.Pane, root)
	}

	// Same name, different pane: refused, and the first pane keeps the name.
	if err := h.StartAgent(ctx, name, other, nil); err == nil {
		t.Fatal("StartAgent accepted a name already live in another pane")
	}
	agents, err = h.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents after refusal: %v", err)
	}
	if a := agents[name]; a.Pane != root {
		t.Fatalf("agent %s moved to %s after a refusal, want %s", name, a.Pane, root)
	}
	if _, err := h.Pane(ctx, other); err != nil {
		t.Fatalf("refusing a duplicate name closed the other pane: %v", err)
	}

	if err := h.CloseWorkspace(ctx, ws); err != nil {
		t.Fatalf("CloseWorkspace: %v", err)
	}
}
