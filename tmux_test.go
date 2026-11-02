package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"
)

// testSocket names this process's tmux server. It is derived once, so every
// test in the binary shares it, and two test binaries never share a server.
var testSocket = fmt.Sprintf("dungeon-test-%d-%08x", os.Getpid(), rand.Uint32())

// socketPath is the file tmux binds for testSocket.
func socketPath() string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), testSocket)
}

// tmuxTest gives a test its own tmux server. It never talks to the operator's
// default server: every command names this process's socket and the server
// starts with /dev/null as its config.
func tmuxTest(t *testing.T) tmux {
	t.Helper()
	return tmuxServer(t, safeServerConfig)
}

// safeServerConfig is the config every private test server starts with. A pane
// dungeon starts with a shell writes the command line it is given to HISTFILE.
// Point every shell this server starts at /dev/null before it creates any pane,
// so the tests never touch the operator's shell history. A plain sh is the pane
// shell because zsh resets HISTFILE to $HOME/.zsh_history however the
// environment set it. The config is how the settings exist before the first
// pane: a server with no sessions exits, so set-environment cannot run after
// start-server.
const safeServerConfig = "set-environment -g HISTFILE /dev/null\n" +
	"set-option -g default-shell /bin/sh\n"

// tmuxTestDestroyUnattached gives a test a private server configured like the
// operator's: a session with no attached client is destroyed at once. dungeon
// creates every workspace detached, so this is the setting its sessions must
// survive.
func tmuxTestDestroyUnattached(t *testing.T) tmux {
	t.Helper()
	return tmuxServer(t, safeServerConfig+
		"set-option -g destroy-unattached on\n"+
		"set-option -g exit-unattached on\n")
}

// tmuxServer starts a private tmux server with config and keeps it alive with a
// keeper session. The keeper pins destroy-unattached and exit-unattached off on
// itself in the same invocation that creates it, because a config can turn them
// on globally and a second invocation would be too late.
func tmuxServer(t *testing.T, config string) tmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	conf := filepath.Join(t.TempDir(), "tmux.conf")
	if err := os.WriteFile(conf, []byte(config), 0o600); err != nil {
		t.Fatalf("write private tmux config: %v", err)
	}
	run := func(args ...string) ([]byte, error) {
		full := append([]string{"-L", testSocket, "-f", conf}, args...)
		return exec.Command("tmux", full...).CombinedOutput()
	}
	_, _ = run("kill-server")
	// tmux leaves the socket file behind after kill-server, and a later start
	// on the same name can lose a race against that stale file. The name is
	// ours alone, so remove it before binding.
	_ = os.Remove(socketPath())
	if out, err := run("start-server"); err != nil {
		t.Fatalf("start private tmux server: %v: %s", err, out)
	}
	// A server with no sessions exits at once, and the next client would start
	// one without the config above. A keeper session holds it open for the test.
	if out, err := run("new-session", "-d", "-s", "dungeon-test-server",
		";", "set-option", "-t", "dungeon-test-server", "destroy-unattached", "off",
		";", "set-option", "-t", "dungeon-test-server", "exit-unattached", "off"); err != nil {
		t.Fatalf("start keeper session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_, _ = run("kill-server")
		_ = os.Remove(socketPath())
	})
	return tmux{socket: testSocket}
}

// runInTestServer executes a raw tmux command on the test's private server.
func runInTestServer(t *testing.T, socket string, args ...string) string {
	t.Helper()
	full := append([]string{"-L", socket, "-f", "/dev/null"}, args...)
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

// TestTmuxWorkspaceSurvivesDestroyUnattached covers the operator's config:
// destroy-unattached is on globally, so tmux destroys every session that has no
// attached client. dungeon creates every workspace detached, so the session and
// its pane must still be there a moment after creation.
func TestTmuxWorkspaceSurvivesDestroyUnattached(t *testing.T) {
	h := tmuxTestDestroyUnattached(t)
	ctx := context.Background()
	cwd := t.TempDir()

	if got := runInTestServer(t, h.socket, "show-options", "-gv", "destroy-unattached"); got != "on" {
		t.Fatalf("test server destroy-unattached = %q, want on", got)
	}
	if clients := runInTestServer(t, h.socket, "list-clients", "-F", "#{client_name}"); clients != "" {
		t.Fatalf("test server has clients: %s", clients)
	}

	ws, tab, root, err := h.CreateWorkspace(ctx, "enc", "worker", cwd)
	if err != nil {
		t.Fatalf("CreateWorkspace with destroy-unattached on: %v", err)
	}
	for _, option := range []string{"destroy-unattached", "exit-unattached"} {
		if got := runInTestServer(t, h.socket, "show-options", "-t", ws, "-v", option); got != "off" {
			t.Fatalf("workspace %s %s = %q, want off", ws, option, got)
		}
	}

	// No client is attached. A session the config can destroy is gone within
	// milliseconds; wait longer than that before deciding.
	time.Sleep(500 * time.Millisecond)

	if got := runInTestServer(t, h.socket, "list-sessions", "-F", "#{session_id}"); !strings.Contains(got, ws) {
		t.Fatalf("workspace %s is gone after a moment with no client attached; sessions: %s", ws, got)
	}
	info, err := h.Pane(ctx, root)
	if err != nil {
		t.Fatalf("workspace %s lost its pane %s: %v", ws, root, err)
	}
	if info.Tab != tab || info.Workspace != ws {
		t.Fatalf("Pane root = %+v, want tab %s workspace %s", info, tab, ws)
	}
	if panes := runInTestServer(t, h.socket, "list-panes", "-t", ws, "-F", "#{pane_id}"); !strings.Contains(panes, root) {
		t.Fatalf("workspace %s lists panes %q, want %s", ws, panes, root)
	}

	// The session is still dungeon's own, so it closes only on request.
	if err := h.CloseWorkspace(ctx, ws); err != nil {
		t.Fatalf("CloseWorkspace: %v", err)
	}
	if got := runInTestServer(t, h.socket, "list-sessions", "-F", "#{session_id}"); strings.Contains(got, ws) {
		t.Fatalf("CloseWorkspace left workspace %s; sessions: %s", ws, got)
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

	waitForShell(t, h, root)
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

// TestTmuxStartAgentRespawnsADungeonPane proves the pane dungeon created runs
// the agent as its own process: no shell holds it, so no shell history is
// written.
func TestTmuxStartAgentRespawnsADungeonPane(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	fakePi(t, "exec sleep 60")

	_, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	waitForShell(t, h, root)

	name := "w1-aa"
	if err := h.StartAgent(ctx, name, root, []string{"--no-skills"}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	agents, err := h.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if a, ok := agents[name]; !ok || a.Pane != root {
		t.Fatalf("Agents = %v, want %s on %s", agents, name, root)
	}

	command := runInTestServer(t, h.socket, "display-message", "-p", "-t", root, "#{pane_current_command}")
	if isShell(command) {
		t.Fatalf("dungeon pane still runs its shell %q; the agent was pasted, not respawned", command)
	}
	pid := runInTestServer(t, h.socket, "display-message", "-p", "-t", root, "#{pane_pid}")
	out, err := exec.Command("ps", "-o", "comm=", "-p", pid).Output()
	if err != nil {
		t.Fatalf("ps %s: %v", pid, err)
	}
	if proc := strings.TrimSpace(string(out)); isShell(proc) {
		t.Fatalf("pane process %s is the shell %q, want the agent", pid, proc)
	}
}

// TestTmuxDungeonPaneDiesWithItsAgent proves a respawned pane cannot outlive
// its agent: the pane goes away with the process, so the name option cannot
// leave a ghost lane behind.
func TestTmuxDungeonPaneDiesWithItsAgent(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "started")
	fakePi(t, ": > "+shellQuote(marker)+"; sleep 1")

	_, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	waitForShell(t, h, root)

	name := "w1-aa"
	if err := h.StartAgent(ctx, name, root, nil); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	pollAgents(t, h, name, true)
	waitForFile(t, marker)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := h.Pane(ctx, root); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent exited but its dungeon pane is still there")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pollAgents(t, h, name, false)
}

// TestTmuxStartAgentKeepsAForeignPanesShell proves the operator's own pane is
// not respawned: the agent is pasted into the shell, the shell survives it, and
// the name is unset when the agent exits.
func TestTmuxStartAgentKeepsAForeignPanesShell(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	// A long enough agent for the fleet to be observed while it runs. It is a
	// different foreground command from the shell, which StartAgent watches on
	// this path.
	fakePi(t, "exec sleep 3")

	// A session dungeon did not create, so its pane carries no marker.
	runInTestServer(t, h.socket, "new-session", "-d", "-s", "operator", "-c", t.TempDir())
	pane := runInTestServer(t, h.socket, "display-message", "-p", "-t", "operator", "#{pane_id}")
	waitForShell(t, h, pane)

	// The operator's shell must not write dungeon's command lines to a real
	// history file either.
	histFile := filepath.Join(t.TempDir(), "histfile")
	runInTestServer(t, h.socket, "send-keys", "-t", pane, "-l", "echo \"$HISTFILE\" > "+shellQuote(histFile))
	runInTestServer(t, h.socket, "send-keys", "-t", pane, "Enter")
	waitForFile(t, histFile)
	if got, err := os.ReadFile(histFile); err != nil || strings.TrimSpace(string(got)) != "/dev/null" {
		t.Fatalf("test pane HISTFILE = %q (%v), want /dev/null", got, err)
	}

	name := "w1-aa"
	if err := h.StartAgent(ctx, name, pane, nil); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	pollAgents(t, h, name, true)
	pollAgents(t, h, name, false)

	// The shell is the pane's foreground command again, and the pane is alive.
	deadline := time.Now().Add(5 * time.Second)
	for {
		command, err := h.paneCommand(ctx, pane)
		if err == nil && isShell(command) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the operator pane never returned to its shell: command %q, err %v", command, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := h.Pane(ctx, pane); err != nil {
		t.Fatalf("the operator pane is gone after the agent ran: %v", err)
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

	waitForShell(t, h, root)
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

// fakePi writes a fake pi into its own directory and puts it first on PATH.
func fakePi(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	script := filepath.Join(bin, "pi")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// pollAgents samples the fleet until want agrees with whether name is listed.
func pollAgents(t *testing.T, h tmux, name string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		agents, err := h.Agents(context.Background())
		if err != nil {
			t.Fatalf("Agents: %v", err)
		}
		_, listed := agents[name]
		if listed == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent %s listed = %v, want %v; fleet: %v", name, listed, want, agents)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTmuxStartAgentLeavesNoGhostWhenTheAgentExits(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "started")
	// The agent is seen to start, so the failed-start cleanup is not what
	// unsets the name. It exits on its own a moment later.
	fakePi(t, ": > "+shellQuote(marker)+"; sleep 1")

	_, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	waitForShell(t, h, root)
	name := "w1-aa"
	if err := h.StartAgent(ctx, name, root, nil); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	pollAgents(t, h, name, true)

	waitForFile(t, marker)
	pollAgents(t, h, name, false)
}

func TestTmuxLiveAgentStaysListedWhileItRuns(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	fakePi(t, "exec sleep 60")

	_, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	waitForShell(t, h, root)
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

	pollAgents(t, h, name, true)
}

func TestFocusArgv(t *testing.T) {
	cases := []struct {
		name                                           string
		paneSession, paneWindow, pane, clientSessionID string
		want                                           []string
	}{
		{
			// The client already watches the pane's session: no switch.
			name:        "same session",
			paneSession: "$1", paneWindow: "@2", pane: "%3", clientSessionID: "$1",
			want: []string{"select-window", "-t", "@2", ";", "select-pane", "-t", "%3"},
		},
		{
			// The pane lives in another session: switch the client first.
			name:        "different session",
			paneSession: "$4", paneWindow: "@5", pane: "%6", clientSessionID: "$1",
			want: []string{
				"switch-client", "-t", "$4",
				";",
				"select-window", "-t", "@5",
				";",
				"select-pane", "-t", "%6",
			},
		},
		{
			// Nobody is attached: no client to switch, and no error.
			name:        "no client",
			paneSession: "$4", paneWindow: "@5", pane: "%6", clientSessionID: "",
			want: []string{"select-window", "-t", "@5", ";", "select-pane", "-t", "%6"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := focusArgv(c.paneSession, c.paneWindow, c.pane, c.clientSessionID)
			if !slices.Equal(got, c.want) {
				t.Fatalf("focusArgv = %q, want %q", got, c.want)
			}
			assertFocusSeparators(t, got)
		})
	}
}

// assertFocusSeparators fails unless every focus command after the first has a
// ";" immediately before it. A missing separator makes tmux read the next
// command as the previous one's arguments.
func assertFocusSeparators(t *testing.T, argv []string) {
	t.Helper()
	seen := false
	for i, tok := range argv {
		switch tok {
		case "switch-client", "select-window", "select-pane":
			if !seen {
				seen = true
				continue
			}
			if argv[i-1] != ";" {
				t.Fatalf("focus argv has no separator before %s: %q", tok, argv)
			}
		}
	}
	if !seen {
		t.Fatalf("focus argv has no command: %q", argv)
	}
}

// focusEnv is the environment for a tmux command run against the test server.
// A non-empty clientTMUX points the command at an attached client; an empty one
// strips TMUX so the command runs with no current client.
func focusEnv(clientTMUX string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") {
			continue
		}
		env = append(env, e)
	}
	if clientTMUX != "" {
		env = append(env, "TMUX="+clientTMUX)
	}
	return env
}

// runFocusArgv runs the emitted sequence the way focus does, not as a string.
func runFocusArgv(t *testing.T, h tmux, argv, env []string) {
	t.Helper()
	cmd := exec.Command("tmux", h.argv(argv...)...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux %q: %v: %s", argv, err, out)
	}
}

// attachControlClient attaches a scriptable tmux client to session and waits
// until tmux lists it. The test cleanup kills it.
func attachControlClient(t *testing.T, h tmux, session string) {
	t.Helper()
	cmd := exec.Command("tmux", h.argv("-C", "attach-session", "-t", session)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("control client stdin: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start control client: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := h.call(ctx, "list-clients", "-F", "#{client_session}")
		if err == nil && strings.TrimSpace(string(out)) == session {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("control client did not attach to session %s", session)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sessionID must turn the name #{client_session} reports into the id a pane
// carries. Comparing the name to a pane's session id would always be false.
func TestTmuxSessionIDResolvesName(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	ws, _, _, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	got, err := h.sessionID(ctx, "enc")
	if err != nil {
		t.Fatalf("sessionID: %v", err)
	}
	if got != ws {
		t.Fatalf("sessionID(enc) = %q, want %q", got, ws)
	}
}

// The emitted argv must actually run. A string-only assertion missed a missing
// ";" after switch-client: tmux read select-window as switch-client's argument.
func TestFocusArgvRunsAgainstALiveServer(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()

	if _, _, _, err := h.CreateWorkspace(ctx, "one", "worker", t.TempDir()); err != nil {
		t.Fatalf("CreateWorkspace one: %v", err)
	}
	target, targetTab, targetPane, err := h.CreateWorkspace(ctx, "two", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace two: %v", err)
	}
	at, err := h.Pane(ctx, targetPane)
	if err != nil {
		t.Fatalf("Pane: %v", err)
	}

	// No client: no switch, and window and pane are still selected.
	argv := focusArgv(at.Workspace, at.Tab, at.ID, "")
	assertFocusSeparators(t, argv)
	runFocusArgv(t, h, argv, focusEnv(""))
	if got := runInTestServer(t, h.socket, "display-message", "-p", "-t", target, "#{window_id}"); got != targetTab {
		t.Fatalf("active window = %s, want %s", got, targetTab)
	}
	if got := runInTestServer(t, h.socket, "display-message", "-p", "-t", target, "#{pane_id}"); got != targetPane {
		t.Fatalf("active pane = %s, want %s", got, targetPane)
	}

	// A client on another session: the emitted sequence switches it, then
	// selects the window and pane.
	attachControlClient(t, h, "one")
	source, err := h.sessionID(ctx, "one")
	if err != nil {
		t.Fatalf("sessionID one: %v", err)
	}
	argv = focusArgv(at.Workspace, at.Tab, at.ID, source)
	assertFocusSeparators(t, argv)
	serverPID, err := h.call(ctx, "display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatalf("server pid: %v", err)
	}
	clientTMUX := socketPath() + "," + strings.TrimSpace(string(serverPID)) + ",0"
	runFocusArgv(t, h, argv, focusEnv(clientTMUX))

	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := h.call(ctx, "list-clients", "-F", "#{client_session}")
		if err == nil && strings.TrimSpace(string(out)) == "two" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client did not end on session two; list-clients: %s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTmuxFocusWithNoClientAttached(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()

	ws, _, _, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	second, other, err := h.CreateTab(ctx, ws, "reviewer", t.TempDir())
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	// No client is attached to this private server, which is the normal case.
	if clients := runInTestServer(t, h.socket, "list-clients", "-F", "#{client_name}"); clients != "" {
		t.Fatalf("test server has clients: %s", clients)
	}
	runInTestServer(t, h.socket, "set-option", "-p", "-t", other, paneAgentOption, "w1-aa")

	// FocusTab lands on the other window of the same session.
	if err := h.FocusTab(ctx, second); err != nil {
		t.Fatalf("FocusTab with no client: %v", err)
	}
	active := runInTestServer(t, h.socket, "display-message", "-p", "-t", ws, "#{window_id}")
	if active != second {
		t.Fatalf("active window = %s, want %s", active, second)
	}

	// FocusAgent moves to the window and the pane that hosts the agent.
	if err := h.FocusAgent(ctx, "w1-aa"); err != nil {
		t.Fatalf("FocusAgent with no client: %v", err)
	}
	active = runInTestServer(t, h.socket, "display-message", "-p", "-t", ws, "#{window_id}")
	if active != second {
		t.Fatalf("active window after FocusAgent = %s, want %s", active, second)
	}
	activePane := runInTestServer(t, h.socket, "display-message", "-p", "-t", ws, "#{pane_id}")
	if activePane != other {
		t.Fatalf("active pane after FocusAgent = %s, want %s", activePane, other)
	}
}

func TestTmuxFocusAcrossSessionsWithNoClientAttached(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()

	if _, _, _, err := h.CreateWorkspace(ctx, "one", "worker", t.TempDir()); err != nil {
		t.Fatalf("CreateWorkspace one: %v", err)
	}
	second, secondTab, secondPane, err := h.CreateWorkspace(ctx, "two", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace two: %v", err)
	}
	runInTestServer(t, h.socket, "set-option", "-p", "-t", secondPane, paneAgentOption, "w2-bb")

	// No client is attached, so there is nothing to switch; the window and pane
	// of the other session are still selected, ready for the next attach.
	if err := h.FocusAgent(ctx, "w2-bb"); err != nil {
		t.Fatalf("FocusAgent across sessions with no client: %v", err)
	}
	active := runInTestServer(t, h.socket, "display-message", "-p", "-t", second, "#{window_id}")
	if active != secondTab {
		t.Fatalf("active window of %s = %s, want %s", second, active, secondTab)
	}
	activePane := runInTestServer(t, h.socket, "display-message", "-p", "-t", second, "#{pane_id}")
	if activePane != secondPane {
		t.Fatalf("active pane of %s = %s, want %s", second, activePane, secondPane)
	}
}

// fakeSlowAgent writes a fake pi that boots slowly, echoes the prompt it is
// given, ignores the first Enter after boot, and submits on the next one. It is
// the process behind TestTmuxPromptSubmitsAfterASlowAgentIgnoresTheFirstEnter.
func fakeSlowAgent(t *testing.T, out string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary path: %v", err)
	}
	body := "DUNGEON_FAKE_AGENT_OUT=" + shellQuote(out) + "\n" +
		"export DUNGEON_FAKE_AGENT_OUT\n" +
		"DUNGEON_FAKE_AGENT_DELAY=1s\n" +
		"export DUNGEON_FAKE_AGENT_DELAY\n" +
		"exec " + shellQuote(bin) + " -test.run '^TestFakeAgentHelper$'\n"
	binDir := t.TempDir()
	script := filepath.Join(binDir, "pi")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestFakeAgentHelper is the fake agent body. It only does work when the test
// binary is re-executed by fakeSlowAgent.
func TestFakeAgentHelper(t *testing.T) {
	out := os.Getenv("DUNGEON_FAKE_AGENT_OUT")
	if out == "" {
		t.Skip("helper process for the tmux prompt tests")
	}
	delay, err := time.ParseDuration(os.Getenv("DUNGEON_FAKE_AGENT_DELAY"))
	if err != nil {
		t.Fatalf("fake agent delay: %v", err)
	}
	if err := runFakeAgent(out, delay); err != nil {
		t.Fatalf("fake agent: %v", err)
	}
}

// runFakeAgent reads the terminal in raw mode, echoes what it is given so the
// text is visibly on the input line, ignores every Enter during boot and the
// first Enter after it, then writes the submitted line to out.
func runFakeAgent(out string, boot time.Duration) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(fd, old)

	if _, err := os.Stdout.WriteString("fake agent booting\r\n"); err != nil {
		return err
	}
	ready := time.Now().Add(boot)
	bootKey := true
	line := make([]byte, 0, 256)
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 {
			if err != nil {
				return nil
			}
			continue
		}
		switch buf[0] {
		case '\r', '\n':
			if time.Now().Before(ready) {
				continue
			}
			if bootKey {
				// The first Enter after boot is still ignored, as a TUI that
				// has not finished starting does. The text stays on the line.
				bootKey = false
				continue
			}
			if len(line) == 0 {
				continue
			}
			if err := os.WriteFile(out, []byte(string(line)+"\nsubmitted\n"), 0o644); err != nil {
				return err
			}
			if _, err := os.Stdout.WriteString("\r\nsubmitted\r\n"); err != nil {
				return err
			}
			// Stay alive after submitting. A respawned pane dies with its agent,
			// so the test could not read the transcript back if this exited.
			time.Sleep(time.Minute)
			return nil
		case 0x7f, 0x08:
			if len(line) > 0 {
				line = line[:len(line)-1]
				_, _ = os.Stdout.WriteString("\b \b")
			}
		default:
			line = append(line, buf[0])
			if _, err := os.Stdout.Write(buf[:1]); err != nil {
				return err
			}
		}
	}
}

func TestTmuxPromptSubmitsAfterASlowAgentIgnoresTheFirstEnter(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "prompt.txt")
	fakeSlowAgent(t, out)

	_, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	name := "w1-aa"
	waitForShell(t, h, root)
	if err := h.StartAgent(ctx, name, root, nil); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	text := "prompt-submit: deliver this line"
	if err := h.Prompt(ctx, name, text); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitForFile(t, out)
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read fake agent output: %v", err)
	}
	want := text + "\nsubmitted\n"
	if string(got) != want {
		t.Fatalf("fake agent received %q, want %q", got, want)
	}
}

// TestTmuxPromptSubmitsToARealPi is the case that failed: a fresh pi renders a
// paste hundreds of milliseconds after it, and the Enter sent with the paste is
// swallowed during startup. Prompt must prove delivery itself, so pi takes the
// prompt and starts a turn with no key from this test.
func TestTmuxPromptSubmitsToARealPi(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not installed")
	}
	h := tmuxTest(t)
	ctx := context.Background()

	_, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	name := "w1-aa"
	if err := h.StartAgent(ctx, name, root, []string{"--no-skills"}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	text := "prompt-confirm: reply with the single word ok"
	if err := h.Prompt(ctx, name, text); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// No key came from this test. pi took the prompt when it shows its working
	// indicator, or when its status line counts tokens for the request. A fast
	// model can finish before the first look, so poll for either.
	deadline := time.Now().Add(20 * time.Second)
	var screen string
	for {
		screen = mustCapture(t, h, root)
		if strings.Contains(screen, "Working") || strings.Contains(screen, "↑") {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pi never took the prompt %q:\n%s", text, screen)
}

// mustCapture reads a pane's text or fails the test.
func mustCapture(t *testing.T, h tmux, pane string) string {
	t.Helper()
	screen, err := h.capture(context.Background(), pane)
	if err != nil {
		t.Fatalf("capture %s: %v", pane, err)
	}
	return screen
}

func TestTmuxStartAgentWaitsForOutputToSettle(t *testing.T) {
	h := tmuxTest(t)
	ctx := context.Background()
	fakePi(t, `i=0
while [ $i -lt 8 ]; do echo "booting $i"; sleep 0.1; i=$((i+1)); done
echo ready
exec sleep 60`)

	_, _, root, err := h.CreateWorkspace(ctx, "enc", "worker", t.TempDir())
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	name := "w1-aa"
	waitForShell(t, h, root)
	if err := h.StartAgent(ctx, name, root, nil); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	screen, err := h.capture(ctx, root)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !strings.Contains(screen, "ready") {
		t.Fatalf("StartAgent returned before the agent's output settled:\n%s", screen)
	}
}

// waitForShell blocks until a freshly created pane's shell is reading input.
// tmux maps a pane before its shell has started; a paste sent then is lost.
func waitForShell(t *testing.T, h tmux, pane string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		command, commandErr := h.paneCommand(ctx, pane)
		screen, screenErr := h.capture(ctx, pane)
		if commandErr == nil && screenErr == nil && isShell(command) && strings.TrimSpace(screen) != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s shell did not become ready: command %q, content:\n%s", pane, command, screen)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// isShell reports whether a foreground command is a shell reading a tty.
func isShell(name string) bool {
	switch filepath.Base(name) {
	case "sh", "bash", "dash", "zsh", "fish", "ksh", "mksh", "ash", "yash":
		return true
	}
	return false
}
