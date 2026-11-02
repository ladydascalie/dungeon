package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// tmux is the tmux backend: the only file that speaks tmux's command line.
// Everything above it speaks mux.
//
// tmux has no agent protocol. Every live agent is AgentUnknown, and a lane is
// never classified from its foreground command. On tmux every unit is
// accept-driven: reconcile holds an unknown lane until its report lands, then
// settles it with the unit's accept command.
//
// Identity lives in tmux, not in this process. StartAgent records the agent
// name in a pane option and Agents reads it back from list-panes, so a
// restarted dungeon still finds its lanes. Ownership lives in tmux too: dungeon
// sets a session option when it creates a workspace, and CloseWorkspace refuses
// a session without it.
type tmux struct {
	// socket is the tmux server's socket name (-L). An empty socket means the
	// operator's default server. Tests set a private name so they never touch
	// it.
	socket string
}

func newTmux() tmux { return tmux{} }

var _ mux = tmux{}

// The option names are the durable contract between a running dungeon and a
// restarted one.
const (
	paneAgentOption    = "@dungeon_agent"
	sessionOwnerOption = "@dungeon_encounter"
)

// pasteSeq names buffers so two prompt calls in flight at once cannot paste
// each other's text.
var pasteSeq atomic.Uint64

func (t tmux) argv(args ...string) []string {
	out := make([]string, 0, len(args)+2)
	if t.socket != "" {
		out = append(out, "-L", t.socket)
	}
	return append(out, args...)
}

// call executes one tmux command and returns its stdout. Errors carry stderr,
// because a silent tmux failure is the hardest kind to debug.
func (t tmux) call(ctx context.Context, args ...string) ([]byte, error) {
	return t.run(ctx, nil, args...)
}

// run is call with stdin, for load-buffer.
func (t tmux) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", t.argv(args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("tmux %s: %s", strings.Join(args, " "), msg)
	}
	return out.Bytes(), nil
}

// Agents enumerates every pane carrying a recorded agent name. The option is
// the fleet: this process keeps no map, so a restart sees the same lanes. tmux
// has no agent protocol, so every status is AgentUnknown. An error means the
// server could not be read, which is not the same as an empty fleet.
func (t tmux) Agents(ctx context.Context) (map[string]Agent, error) {
	out, err := t.call(ctx, "list-panes", "-a", "-F", "#{pane_id}\t#{"+paneAgentOption+"}")
	if err != nil {
		// A tmux server with no sessions is gone. That is an empty fleet, not
		// an unreadable one: no server cannot own a pane.
		if strings.Contains(err.Error(), "no server running") {
			return map[string]Agent{}, nil
		}
		return nil, err
	}
	agents := map[string]Agent{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		id, name, ok := strings.Cut(line, "\t")
		if !ok || id == "" || name == "" {
			continue
		}
		agents[name] = Agent{Name: name, Pane: id, Status: AgentUnknown}
	}
	return agents, nil
}

// SelfPane is the pane dungeon runs in, which is where a lane pings its parent.
func (t tmux) SelfPane(ctx context.Context) string {
	return os.Getenv("TMUX_PANE")
}

// Workspaces lists every session. A session dungeon created carries its
// encounter label in a session option; any other session is shown by its name.
func (t tmux) Workspaces(ctx context.Context) []workspace {
	out, err := t.call(ctx, "list-sessions", "-F",
		"#{session_id}\t#{"+sessionOwnerOption+"}\t#{session_name}")
	if err != nil {
		return nil
	}
	all := []workspace{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || fields[0] == "" {
			continue
		}
		label := fields[2]
		if fields[1] != "" {
			label = fields[1]
		}
		all = append(all, workspace{ID: fields[0], Label: label})
	}
	return all
}

// requireTarget refuses an empty workspace, tab or pane id before tmux sees
// it. tmux reads an empty -t as the current session, window or pane, which is
// the operator's own, so an empty id is never a no-op.
func requireTarget(kind, id string) error {
	if id == "" {
		return fmt.Errorf("tmux: refusing an empty %s id", kind)
	}
	return nil
}

// CreateWorkspace creates one detached session, names its first window and
// records dungeon's ownership on the session.
func (t tmux) CreateWorkspace(ctx context.Context, label, tabLabel, cwd string) (wsID, tabID, rootPane string, err error) {
	out, err := t.call(ctx, "new-session", "-d", "-s", label, "-c", cwd, "-n", tabLabel,
		"-P", "-F", "#{session_id}\t#{window_id}\t#{pane_id}")
	if err != nil {
		return "", "", "", err
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
		return "", "", "", fmt.Errorf("tmux new-session returned an incomplete layout")
	}
	wsID, tabID, rootPane = fields[0], fields[1], fields[2]
	if _, err := t.call(ctx, "set-option", "-t", wsID, sessionOwnerOption, label); err != nil {
		return "", "", "", err
	}
	return wsID, tabID, rootPane, nil
}

// CreateTab adds one detached window to a session and returns its root pane.
func (t tmux) CreateTab(ctx context.Context, ws, label, cwd string) (tabID, rootPane string, err error) {
	if err := requireTarget("workspace", ws); err != nil {
		return "", "", err
	}
	args := []string{"new-window", "-d", "-c", cwd, "-t", ws}
	if label != "" {
		args = append(args, "-n", label)
	}
	args = append(args, "-P", "-F", "#{window_id}\t#{pane_id}")
	out, err := t.call(ctx, args...)
	if err != nil {
		return "", "", err
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		return "", "", fmt.Errorf("tmux new-window returned an incomplete layout")
	}
	return fields[0], fields[1], nil
}

// SplitPane carves one pane out of target. tmux -p is the share of the window
// the new pane takes, while ratio is the share target keeps, so the percentages
// are inverted. Repeated splits of the same target use the same ratio and stay
// equal. The new pane does not take focus.
func (t tmux) SplitPane(ctx context.Context, target, direction, cwd string, ratio float64) (string, error) {
	if err := requireTarget("pane", target); err != nil {
		return "", err
	}
	percent := int(math.Round((1 - ratio) * 100))
	if percent < 1 {
		percent = 1
	}
	if percent > 99 {
		percent = 99
	}
	args := []string{"split-window", "-d", "-t", target, "-c", cwd,
		"-p", strconv.Itoa(percent)}
	switch direction {
	case "right":
		args = append(args, "-h")
	case "left":
		args = append(args, "-h", "-b")
	case "down":
		args = append(args, "-v")
	case "up":
		args = append(args, "-v", "-b")
	default:
		return "", fmt.Errorf("tmux: unknown split direction %q", direction)
	}
	args = append(args, "-P", "-F", "#{pane_id}")
	out, err := t.call(ctx, args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("tmux split-window returned no pane")
	}
	return id, nil
}

// Pane reports one live pane's window and session. A pane that is gone is an
// error.
func (t tmux) Pane(ctx context.Context, id string) (pane, error) {
	if err := requireTarget("pane", id); err != nil {
		return pane{}, err
	}
	out, err := t.call(ctx, "display-message", "-p", "-t", id,
		"#{pane_id}\t#{window_id}\t#{session_id}")
	if err != nil {
		return pane{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 3 || fields[0] == "" {
		return pane{}, fmt.Errorf("tmux: pane %s is gone", id)
	}
	return pane{ID: fields[0], Tab: fields[1], Workspace: fields[2]}, nil
}

// StartAgent records the name on the pane, runs pi with args, and waits until
// the agent can take input: the foreground command has left the shell that was
// there, and the pane's output has gone quiet. A changing foreground command is
// not enough on its own, because a TUI accepts a pasted prompt long after it
// has replaced the shell. The name is the identity a restarted dungeon reads
// back through Agents; nothing here keeps a map. A pane that never starts the
// agent is an error naming the pane.
//
// A name already live in a different pane is refused: every later lookup is by
// name, so two panes on one name would make it ambiguous. A name already live
// in this pane is the same agent, so the call is a no-op.
func (t tmux) StartAgent(ctx context.Context, name, pane string, args []string) error {
	if err := requireTarget("pane", pane); err != nil {
		return err
	}
	agents, err := t.Agents(ctx)
	if err != nil {
		return err
	}
	if a, live := agents[name]; live {
		if a.Pane == pane {
			return nil
		}
		return fmt.Errorf("tmux: agent name %s is already live in pane %s", name, a.Pane)
	}
	before, err := t.paneCommand(ctx, pane)
	if err != nil {
		return err
	}
	if _, err := t.call(ctx, "set-option", "-p", "-t", pane, paneAgentOption, name); err != nil {
		return err
	}
	started := false
	defer func() {
		if started {
			return
		}
		// A failed start must not leave a name on a pane that has no agent, or
		// a restarted dungeon would see a lane that is not there.
		unset := context.WithoutCancel(ctx)
		unset, cancel := context.WithTimeout(unset, 5*time.Second)
		defer cancel()
		_, _ = t.call(unset, "set-option", "-p", "-u", "-t", pane, paneAgentOption)
	}()
	command := append([]string{"pi"}, args...)
	for i, arg := range command {
		command[i] = shellQuote(arg)
	}
	// The option lives in tmux, not in the agent process, so it would outlive a
	// dead agent and make a ghost lane read live. Unset it after the agent
	// exits, however it exits.
	unset := append([]string{"tmux"}, t.argv("set-option", "-p", "-u", "-t", pane, paneAgentOption)...)
	for i, arg := range unset {
		unset[i] = shellQuote(arg)
	}
	line := strings.Join(command, " ") + "; " + strings.Join(unset, " ")
	if err := t.paste(ctx, pane, line); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		now, err := t.paneCommand(ctx, pane)
		if err == nil && now != "" && now != before {
			if err := t.waitQuiet(ctx, pane, name, before); err != nil {
				return err
			}
			started = true
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tmux: pane %s did not start agent %s within 30s", pane, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// paneCommand is the foreground command of a pane.
func (t tmux) paneCommand(ctx context.Context, pane string) (string, error) {
	out, err := t.call(ctx, "display-message", "-p", "-t", pane, "#{pane_current_command}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// readyQuiet is how long a pane's output must stay unchanged before StartAgent
// calls the agent ready. Boot banners arrive in bursts; a quiet gap longer than
// the burst spacing means the agent has reached its prompt.
const readyQuiet = 300 * time.Millisecond

// waitQuiet blocks until the pane's captured output has not changed for
// readyQuiet, while the foreground command stays away from the shell that was
// there before the agent. Bounded: an agent that never settles is an error that
// names the pane.
func (t tmux) waitQuiet(ctx context.Context, pane, name, before string) error {
	deadline := time.Now().Add(10 * time.Second)
	last, err := t.capture(ctx, pane)
	if err != nil {
		return err
	}
	since := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		now, err := t.capture(ctx, pane)
		if err != nil {
			return err
		}
		command, err := t.paneCommand(ctx, pane)
		if err == nil && command == before {
			return fmt.Errorf("tmux: pane %s returned to %q before agent %s was ready", pane, command, name)
		}
		if now != last {
			last = now
			since = time.Now()
		}
		if time.Since(since) >= readyQuiet {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tmux: pane %s output did not settle for agent %s within 10s", pane, name)
		}
	}
}

// capture reads a pane's visible text, without escape sequences.
func (t tmux) capture(ctx context.Context, pane string) (string, error) {
	out, err := t.call(ctx, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// submitSettle is the pause between an Enter and the look that decides whether
// the prompt was submitted.
const submitSettle = 200 * time.Millisecond

// submitTries bounds the extra Enters Prompt sends when a slow TUI ignored the
// first one.
const submitTries = 10

// Prompt submits text to the named agent as one prompt. The text travels
// through a buffer and bracketed paste, so a long prompt keeps its bytes, then
// Enter submits it. A TUI may not be listening yet: when the pasted text is
// still on the cursor's input line, Prompt presses Enter again a bounded number
// of times. It returns when the input is submitted, not when the turn finishes.
// A prompt that will not submit is an error that names the pane, so text is
// never left silently unsent.
func (t tmux) Prompt(ctx context.Context, name, text string) error {
	pane, err := t.agentPane(ctx, name)
	if err != nil {
		return err
	}
	if err := t.paste(ctx, pane, text); err != nil {
		return err
	}
	return t.confirmSubmit(ctx, pane, text)
}

// confirmSubmit looks at the cursor's line after the first Enter. Paste echoes
// the text on that line and leaves the cursor after it; a TUI that accepted the
// prompt clears the line or moves the cursor away. Text still under the cursor
// means the Enter was ignored, so send another.
func (t tmux) confirmSubmit(ctx context.Context, pane, text string) error {
	needle := inputNeedle(text)
	if needle == "" {
		return nil
	}
	enters := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(submitSettle):
		}
		held, err := t.inputHolds(ctx, pane, needle)
		if err != nil {
			return err
		}
		if !held {
			return nil
		}
		if enters >= submitTries {
			return fmt.Errorf("tmux: pane %s still holds the prompt after %d Enters", pane, enters+1)
		}
		if _, err := t.call(ctx, "send-keys", "-t", pane, "Enter"); err != nil {
			return fmt.Errorf("tmux: pane %s: %w", pane, err)
		}
		enters++
	}
}

// inputNeedle is the tail of the prompt's last line: the visible part of the
// text that the terminal echoes under the cursor. A wrapped prompt still ends
// with it on the cursor's line.
func inputNeedle(text string) string {
	last := text
	if i := strings.LastIndexByte(last, '\n'); i >= 0 {
		last = last[i+1:]
	}
	last = strings.TrimRight(last, " \t\r")
	if last == "" {
		return ""
	}
	runes := []rune(last)
	const tail = 16
	if len(runes) > tail {
		runes = runes[len(runes)-tail:]
	}
	return string(runes)
}

// inputHolds reports whether needle is on the pane's cursor line. Only that
// line counts: submitted text in the transcript above is not a pending prompt.
func (t tmux) inputHolds(ctx context.Context, pane, needle string) (bool, error) {
	out, err := t.call(ctx, "display-message", "-p", "-t", pane, "#{cursor_y}")
	if err != nil {
		return false, err
	}
	row, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false, fmt.Errorf("tmux: pane %s reported cursor row %q", pane, out)
	}
	screen, err := t.capture(ctx, pane)
	if err != nil {
		return false, err
	}
	lines := strings.Split(screen, "\n")
	if row < 0 || row >= len(lines) {
		return false, nil
	}
	return strings.Contains(lines[row], needle), nil
}

func (t tmux) agentPane(ctx context.Context, name string) (string, error) {
	agents, err := t.Agents(ctx)
	if err != nil {
		return "", err
	}
	a, ok := agents[name]
	if !ok {
		return "", fmt.Errorf("tmux: no live agent named %s", name)
	}
	return a.Pane, nil
}

// paste loads text into a private tmux buffer, pastes it bracketed, deletes the
// buffer, then presses Enter.
func (t tmux) paste(ctx context.Context, pane, text string) error {
	buf := fmt.Sprintf("dungeon-%d-%d", os.Getpid(), pasteSeq.Add(1))
	if _, err := t.run(ctx, []byte(text), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if _, err := t.call(ctx, "paste-buffer", "-p", "-d", "-b", buf, "-t", pane); err != nil {
		_, _ = t.call(ctx, "delete-buffer", "-b", buf)
		return err
	}
	_, err := t.call(ctx, "send-keys", "-t", pane, "Enter")
	return err
}

// SendKeys writes keys to the named agent. Each key is a separate write, so the
// caller decides whether two keys are one gesture.
func (t tmux) SendKeys(ctx context.Context, name string, keys ...string) error {
	pane, err := t.agentPane(ctx, name)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := t.call(ctx, "send-keys", "-t", pane, key); err != nil {
			return err
		}
	}
	return nil
}

// PingCommand is a line a POSIX shell can run to deliver a message to pane.
// The message is shell-quoted, so $, a backtick and a newline arrive literally,
// and it is passed after `--` so a message that starts with a dash is not read
// as a tmux flag.
func (t tmux) PingCommand(pane, message string) string {
	bin := "tmux"
	if t.socket != "" {
		bin += " -L " + shellQuote(t.socket)
	}
	return fmt.Sprintf("%s send-keys -t %s -l -- %s && %s send-keys -t %s Enter",
		bin, shellQuote(pane), shellQuote(message), bin, shellQuote(pane))
}

// focusArgv is the tmux command sequence that lands the operator on a pane.
//
// select-window comes before select-pane because select-pane only changes the
// active pane of a window; it does not change which window is active, so the
// operator would land in the right pane of the wrong window.
//
// A client is switched only when it is attached to another session, which is
// where switch-client belongs. A pane in a detached session is the normal case,
// so an empty clientSession is not an error: the window and pane are still
// selected on that session, and the next attach lands on them. Nothing here
// steals focus from a caller that did not ask: this helper is reachable only
// through FocusAgent and FocusTab.
func focusArgv(paneSession, paneWindow, pane, clientSession string) []string {
	argv := []string{}
	if clientSession != "" && clientSession != paneSession {
		argv = append(argv, "switch-client", "-t", paneSession)
	}
	return append(argv,
		"select-window", "-t", paneWindow,
		";",
		"select-pane", "-t", pane)
}

// clientSessionID is the id of the session the client a focus move acts on
// watches, which is the client tmux resolves for this process. #{client_session}
// is a name, so it is resolved to an id before any comparison: a name never
// equals a pane's session id. An empty answer means no client is attached;
// display-message prints nothing then, which is an answer, not an error, so a
// focus call still runs.
func (t tmux) clientSessionID(ctx context.Context) string {
	out, err := t.call(ctx, "display-message", "-p", "#{client_session}")
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return ""
	}
	id, err := t.sessionID(ctx, name)
	if err != nil {
		return ""
	}
	return id
}

// sessionID resolves a session name to its id. tmux tries an exact session
// name before a prefix, so a name that is also another session's prefix still
// resolves to itself.
func (t tmux) sessionID(ctx context.Context, name string) (string, error) {
	out, err := t.call(ctx, "display-message", "-p", "-t", name, "#{session_id}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("tmux: session %s has no id", name)
	}
	return id, nil
}

// focus selects window and pane on the pane's session, moving the client to
// that session first when the client watches another one.
func (t tmux) focus(ctx context.Context, at pane) error {
	_, err := t.call(ctx, focusArgv(at.Workspace, at.Tab, at.ID, t.clientSessionID(ctx))...)
	return err
}

// FocusAgent moves the operator to the pane hosting the named agent, switching
// the client's session when the agent lives in another one. An agent that is
// not live is an error.
func (t tmux) FocusAgent(ctx context.Context, name string) error {
	id, err := t.agentPane(ctx, name)
	if err != nil {
		return err
	}
	at, err := t.Pane(ctx, id)
	if err != nil {
		return err
	}
	return t.focus(ctx, at)
}

// FocusTab moves the operator to a window, and to the pane active in it, on the
// window's own session.
func (t tmux) FocusTab(ctx context.Context, tab string) error {
	if err := requireTarget("tab", tab); err != nil {
		return err
	}
	out, err := t.call(ctx, "display-message", "-p", "-t", tab,
		"#{pane_id}\t#{window_id}\t#{session_id}")
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 3 || fields[0] == "" {
		return fmt.Errorf("tmux: window %s is gone", tab)
	}
	return t.focus(ctx, pane{ID: fields[0], Tab: fields[1], Workspace: fields[2]})
}

// ClosePane kills one pane dungeon created. It kills whatever runs in it.
func (t tmux) ClosePane(ctx context.Context, pane string) error {
	if err := requireTarget("pane", pane); err != nil {
		return err
	}
	_, err := t.call(ctx, "kill-pane", "-t", pane)
	return err
}

// CloseWorkspace kills a session only when it carries dungeon's ownership
// option. A session the operator or another project made is refused, never
// killed.
func (t tmux) CloseWorkspace(ctx context.Context, ws string) error {
	if err := requireTarget("workspace", ws); err != nil {
		return err
	}
	out, err := t.call(ctx, "display-message", "-p", "-t", ws, "#{"+sessionOwnerOption+"}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("tmux: session %s is not a dungeon workspace; refusing to close it", ws)
	}
	_, err = t.call(ctx, "kill-session", "-t", ws)
	return err
}
