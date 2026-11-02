package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func waitControlConnected(t *testing.T, c *tmuxControl) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !c.connectedNow() {
		if time.Now().After(deadline) {
			err, at := c.lastError()
			t.Fatalf("tmux control did not connect: %v (%v)", err, at)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitControlEvent(t *testing.T, out <-chan controlEvent, kind string, within time.Duration) controlEvent {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		select {
		case ev := <-out:
			if ev.Kind == kind {
				return ev
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("no %s event within %s", kind, within)
		}
	}
}

func TestEventsControlRunsCommandsAndSeesTopology(t *testing.T) {
	h := tmuxTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan controlEvent, 256)
	c := startTmuxControl(ctx, h, out)
	waitControlConnected(t, c)

	got, err := c.do(context.Background(), "display-message", "-p", "hello world")
	if err != nil {
		t.Fatalf("control command: %v", err)
	}
	if strings.TrimSpace(got) != "hello world" {
		t.Fatalf("control output %q, want hello world", got)
	}

	if _, err := c.do(context.Background(), "definitely-not-a-tmux-command"); err == nil {
		t.Fatal("a failed command must return an error")
	} else if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error %q does not carry tmux's message", err)
	}

	pane := runInTestServer(t, h.socket, "new-window", "-d", "-P", "-F", "#{pane_id}",
		"-t", "dungeon-test-server", "sh")
	window := runInTestServer(t, h.socket, "display-message", "-p", "-t", pane, "#{window_id}")

	deadline := time.Now().Add(2 * time.Second)
	sawWindowAdd := false
	for !sawWindowAdd && time.Now().Before(deadline) {
		select {
		case ev := <-out:
			if ev.Kind != controlTopology {
				continue
			}
			if ev.Name == "%output" {
				t.Fatalf("the pane stream reached the rail as an event")
			}
			if ev.Name == "%window-add" && len(ev.Args) > 0 && ev.Args[0] == window {
				sawWindowAdd = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !sawWindowAdd {
		t.Fatalf("no %%window-add for %s within 2s", window)
	}
}

func TestEventsControlReconnectsAfterServerRestart(t *testing.T) {
	h := tmuxTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan controlEvent, 256)
	c := startTmuxControl(ctx, h, out)
	waitControlConnected(t, c)

	if err := exec.Command("tmux", "-L", h.socket, "kill-server").Run(); err != nil {
		t.Fatalf("kill private tmux server: %v", err)
	}
	waitControlEvent(t, out, controlExit, 3*time.Second)

	// The same socket comes back. A rail that gave up would never connect again.
	tmuxServer(t, safeServerConfig)
	deadline := time.Now().Add(8 * time.Second)
	for !c.connectedNow() {
		if time.Now().After(deadline) {
			err, at := c.lastError()
			t.Fatalf("control did not reconnect: %v (%v)", err, at)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := c.do(context.Background(), "display-message", "-p", "back"); err != nil {
		t.Fatalf("command after reconnect: %v", err)
	}
}

func TestEventsRailFocusRunsOverTheControlConnection(t *testing.T) {
	h := tmuxTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan controlEvent, 256)
	c := startTmuxControl(ctx, h, out)
	waitControlConnected(t, c)

	first := runInTestServer(t, h.socket, "new-window", "-d", "-P", "-F", "#{pane_id}",
		"-t", "dungeon-test-server", "sh")
	second := runInTestServer(t, h.socket, "new-window", "-d", "-P", "-F", "#{pane_id}",
		"-t", "dungeon-test-server", "sh")
	runInTestServer(t, h.socket, "select-window", "-t", first)
	if err := c.railFocus(context.Background(), second); err != nil {
		t.Fatalf("railFocus: %v", err)
	}
	active := runInTestServer(t, h.socket, "display-message", "-p", "-t", second, "#{pane_active}")
	if active != "1" {
		t.Fatalf("focused pane is not active: %s", active)
	}
}
