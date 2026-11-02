package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parityLeg is one backend row. A third backend is one more row: open gives a
// mux, clean says whether the test owns an empty fleet, and inside says whether
// this test process is running inside that multiplexer.
type parityLeg struct {
	name      string
	clean     bool
	available func() bool
	open      func(t *testing.T) mux
	inside    func() bool
	// ready blocks until pane's shell can accept input. A backend that cannot
	// tell leaves it nil; the caller then pastes without waiting.
	ready func(t *testing.T, m mux, pane string)
}

var parityLegs = []parityLeg{
	{
		name:      "herdr",
		clean:     false, // the test itself runs in the operator's live fleet
		available: func() bool { return os.Getenv("HERDR_ENV") == "1" },
		open:      func(t *testing.T) mux { return newHerdr() },
		inside:    func() bool { return os.Getenv("HERDR_PANE_ID") != "" },
	},
	{
		name:      "tmux",
		clean:     true,
		available: func() bool { _, err := exec.LookPath("tmux"); return err == nil },
		open:      parityTmux,
		inside:    func() bool { return os.Getenv("TMUX_PANE") != "" },
		ready:     parityTmuxReady,
	},
}

// parityTmux starts the private tmux server this leg owns. The keepalive
// session makes start-server persist, and carries no agent option, so the fleet
// is still empty. The server never sees the operator's config or socket.
func parityTmux(t *testing.T) mux {
	t.Helper()
	const socket = "dungeon-parity"
	raw := func(args ...string) {
		t.Helper()
		full := append([]string{"-L", socket, "-f", "/dev/null"}, args...)
		if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
	}
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	raw("new-session", "-d", "-s", "parity-keepalive")
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	return tmux{socket: socket}
}

// parityTmuxReady polls a fresh pane until its foreground command is a shell
// and that shell has drawn a prompt. A new pane can be mapped before its shell
// is reading input, so a paste sent too early disappears. On deadline it fails
// with the pane's content, because a lost paste is not visible anywhere else.
func parityTmuxReady(t *testing.T, m mux, pane string) {
	t.Helper()
	tp, ok := m.(tmux)
	if !ok {
		return
	}
	read := func(args ...string) (string, error) {
		full := append([]string{"-L", tp.socket, "-f", "/dev/null"}, args...)
		out, err := exec.Command("tmux", full...).CombinedOutput()
		return string(out), err
	}
	const deadline = 10 * time.Second
	start := time.Now()
	for {
		command, commandErr := read("display-message", "-p", "-t", pane, "#{pane_current_command}")
		content, contentErr := read("capture-pane", "-p", "-t", pane)
		if commandErr == nil && contentErr == nil &&
			parityIsShell(strings.TrimSpace(command)) && strings.TrimSpace(content) != "" {
			return
		}
		if time.Since(start) > deadline {
			content, _ = read("capture-pane", "-p", "-t", pane)
			t.Fatalf("pane %s shell did not become ready in %s: command %q, content:\n%s",
				pane, deadline, strings.TrimSpace(command), content)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// parityIsShell reports whether name is a shell that reads a prompt from its
// tty. tmux reports the foreground process, which is the shell only after it has
// replaced the placeholder and started reading.
func parityIsShell(name string) bool {
	switch filepath.Base(name) {
	case "sh", "bash", "dash", "zsh", "fish", "ksh", "mksh", "ash", "yash":
		return true
	}
	return false
}

// parityShell runs one PingCommand line exactly as a lane would.
func parityShell(t *testing.T, line string) {
	t.Helper()
	if out, err := exec.Command("sh", "-c", line).CombinedOutput(); err != nil {
		t.Fatalf("sh -c %q: %v: %s", line, err, out)
	}
}

func parityWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestParity runs one body through every backend. It checks the mux contract,
// not a backend's command line: no test here inspects an id or a status.
//
// Focus is not observable through mux, so the no-focus half of CreateWorkspace
// and SplitPane cannot be asserted here. Both backends pass their no-focus flag
// in their own tests.
func TestParity(t *testing.T) {
	for _, leg := range parityLegs {
		leg := leg
		t.Run(leg.name, func(t *testing.T) {
			if !leg.available() {
				t.Skipf("%s is not available here", leg.name)
			}
			h := leg.open(t)
			ctx := t.Context()

			before, err := h.Agents(ctx)
			if err != nil {
				t.Fatalf("Agents on a clean multiplexer: %v", err)
			}
			if before == nil {
				t.Fatal("Agents returned a nil map")
			}
			if leg.clean && len(before) != 0 {
				t.Fatalf("Agents on a clean multiplexer = %v, want empty", before)
			}

			cwd := t.TempDir()
			label := fmt.Sprintf("dungeon-parity-%d", time.Now().UnixNano())
			ws, tab, root, err := h.CreateWorkspace(ctx, label, "parity", cwd)
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			if ws == "" || tab == "" || root == "" {
				t.Fatalf("CreateWorkspace returned incomplete ids: %q %q %q", ws, tab, root)
			}
			t.Cleanup(func() { _ = h.CloseWorkspace(context.Background(), ws) })

			info, err := h.Pane(ctx, root)
			if err != nil {
				t.Fatalf("Pane root: %v", err)
			}
			if info.Tab != tab || info.Workspace != ws {
				t.Fatalf("Pane root = %+v, want tab %s workspace %s", info, tab, ws)
			}

			tab2, pane2, err := h.CreateTab(ctx, ws, "parity-tab", cwd)
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

			self := h.SelfPane(ctx)
			if leg.inside() && self == "" {
				t.Fatalf("SelfPane is empty, but the test runs inside %s", leg.name)
			}
			if !leg.inside() && self != "" {
				t.Fatalf("SelfPane = %q, but the test does not run inside %s", self, leg.name)
			}

			file := filepath.Join(t.TempDir(), "ping.txt")
			if leg.ready != nil {
				leg.ready(t, h, pane2)
			}
			parityShell(t, h.PingCommand(pane2, "cat > "+shellQuote(file)))
			parityWait(t, "the cat pane", func() bool {
				_, err := os.Stat(file)
				return err == nil
			})

			message := "cost $5 and `echo nope` and a 'quote'\nsecond line"
			parityShell(t, h.PingCommand(pane2, message))
			// Enter submits the line, so the pane's tty adds one newline after
			// the message. Everything before it must be byte for byte.
			want := message + "\n"
			parityWait(t, "the pinned message", func() bool {
				got, err := os.ReadFile(file)
				return err == nil && string(got) == want
			})
			if got, _ := os.ReadFile(file); string(got) != want {
				t.Fatalf("PingCommand delivered %q, want %q", got, want)
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
				t.Fatal("Pane still reports a closed workspace root")
			}
			if _, err := h.Pane(ctx, pane2); err == nil {
				t.Fatal("Pane still reports a closed workspace tab root")
			}

			closed := map[string]bool{root: true, pane2: true, pane3: true}
			after, err := h.Agents(ctx)
			if err != nil {
				t.Fatalf("Agents after close: %v", err)
			}
			for _, a := range after {
				if closed[a.Pane] {
					t.Fatalf("Agents still lists closed pane %s as %s", a.Pane, a.Name)
				}
			}
			for _, w := range h.Workspaces(ctx) {
				if w.ID == ws {
					t.Fatalf("Workspaces still lists closed workspace %s", ws)
				}
			}
		})
	}
}
