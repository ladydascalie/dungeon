package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// sets a session option when it creates a workspace, and a pane option on every
// pane it creates. CloseWorkspace refuses a session without its option, and
// StartAgent only respawns a pane with its own.
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
	paneOwnerOption    = "@dungeon_pane"
	paneRailOption     = "@agent_rail"
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
	return t.execTimeout(ctx, stdin, t.argv(args...)...)
}

// execTimeout runs one tmux invocation under the backend's timeout.
func (t tmux) execTimeout(ctx context.Context, stdin []byte, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", argv...)
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
		return nil, fmt.Errorf("tmux %s: %s", strings.Join(argv, " "), msg)
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
// $TMUX_PANE outlives the pane it names: a session that went away leaves the
// variable set to a dead id. A brief that carries such an id tells its lane to
// ping a pane that is not there, which fails in the lane's face. So the id is
// only answered when its server still has that pane. A stale id answers "",
// exactly as no pane does, because the brief omits the ping on an empty answer.
//
// The variable was set by the server named in $TMUX, so that server is asked
// first; a tmux backend with a private socket is still asking about the pane
// this process runs in. Without $TMUX the backend's own server is asked.
func (t tmux) SelfPane(ctx context.Context) string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	query := []string{"display-message", "-p", "-t", pane, "#{pane_id}"}
	argv := t.argv(query...)
	if socket := tmuxEnvSocket(os.Getenv("TMUX")); socket != "" {
		argv = append([]string{"-S", socket}, query...)
	}
	out, err := t.execTimeout(ctx, nil, argv...)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(out))
	if id != pane {
		return ""
	}
	return id
}

// tmuxEnvSocket is the socket path a $TMUX value names. The value is
// "socket,pid,session"; an empty value names no server.
func tmuxEnvSocket(tmuxEnv string) string {
	socket, _, _ := strings.Cut(tmuxEnv, ",")
	return socket
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

// markPane records dungeon's ownership on a pane dungeon created. It is the
// same kind of fact as sessionOwnerOption: StartAgent reads it back to decide
// whether it may replace the pane's process, so the operator's own pane is
// never respawned.
func (t tmux) markPane(ctx context.Context, pane string) error {
	if err := requireTarget("pane", pane); err != nil {
		return err
	}
	_, err := t.call(ctx, "set-option", "-p", "-t", pane, paneOwnerOption, "1")
	return err
}

// paneOwned reports whether a pane carries dungeon's ownership marker.
func (t tmux) paneOwned(ctx context.Context, pane string) (bool, error) {
	out, err := t.call(ctx, "display-message", "-p", "-t", pane, "#{"+paneOwnerOption+"}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// errSessionGone marks a workspace session that tmux destroyed while dungeon was
// still creating it. CreateWorkspace retries only this error; any other failure
// is a real one and a retry would hide its cause.
var errSessionGone = errors.New("tmux: a new workspace session was destroyed while it was being created")

// createTries bounds how many times CreateWorkspace creates a session. The
// config can destroy a detached session faster than any follow-up call can
// claim it, so one retry covers a loss that the chained options did not.
const createTries = 2

// createSessionArgv is the one tmux invocation that creates a workspace and
// pins the session options that decide how long it lives. The operator's config
// can set destroy-unattached and exit-unattached globally, and dungeon creates
// every session detached, so a second invocation would leave a window in which
// tmux destroys the session before dungeon can claim it.
//
// The set-options are chained after new-session and target the label: it is
// unique among live sessions because tmux refuses a duplicate name, and tmux
// tries an exact session name before a prefix. tmux stops the chain at the
// first failing command, so a name collision leaves the foreign session alone.
func createSessionArgv(label, tabLabel, cwd string) []string {
	return []string{
		"new-session", "-d", "-s", label, "-c", cwd, "-n", tabLabel,
		"-P", "-F", "#{session_id}\t#{window_id}\t#{pane_id}",
		";",
		"set-option", "-t", label, "destroy-unattached", "off",
		";",
		"set-option", "-t", label, "exit-unattached", "off",
		";",
		"set-option", "-t", label, sessionOwnerOption, label,
	}
}

// CreateWorkspace creates one detached session, names its first window, and
// records dungeon's ownership on the session and its root pane. A session that
// tmux destroyed anyway is retried once, then refused with the cause in the
// error.
func (t tmux) CreateWorkspace(ctx context.Context, label, tabLabel, cwd string) (wsID, tabID, rootPane string, err error) {
	for try := 1; try <= createTries; try++ {
		wsID, tabID, rootPane, err = t.createWorkspaceOnce(ctx, label, tabLabel, cwd)
		if err == nil {
			return wsID, tabID, rootPane, nil
		}
		if !errors.Is(err, errSessionGone) {
			return "", "", "", err
		}
	}
	return "", "", "", fmt.Errorf("%w; workspace %s was destroyed on all %d tries because the tmux server still destroys detached sessions (destroy-unattached)", err, label, createTries)
}

// createWorkspaceOnce runs the chained creation and confirms the session lived
// through it. The confirmation is a separate call because tmux reports a
// destroyed session as a successful new-session.
func (t tmux) createWorkspaceOnce(ctx context.Context, label, tabLabel, cwd string) (string, string, string, error) {
	out, err := t.call(ctx, createSessionArgv(label, tabLabel, cwd)...)
	if err != nil {
		return "", "", "", err
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
		return "", "", "", fmt.Errorf("tmux new-session returned an incomplete layout")
	}
	wsID, tabID, rootPane := fields[0], fields[1], fields[2]
	alive, err := t.sessionAlive(ctx, wsID)
	if err != nil {
		return "", "", "", err
	}
	if !alive {
		return "", "", "", errSessionGone
	}
	if err := t.markPane(ctx, rootPane); err != nil {
		// A pane that went away with its session is the same loss, not a
		// failure to mark the pane.
		if alive, checkErr := t.sessionAlive(ctx, wsID); checkErr == nil && !alive {
			return "", "", "", errSessionGone
		}
		return "", "", "", err
	}
	return wsID, tabID, rootPane, nil
}

// sessionAlive reports whether a session still exists. A session that is gone
// is an answer, not an error; only a tmux that could not be asked is an error.
func (t tmux) sessionAlive(ctx context.Context, id string) (bool, error) {
	if err := requireTarget("workspace", id); err != nil {
		return false, err
	}
	if _, err := t.call(ctx, "has-session", "-t", id); err != nil {
		if isMissingSession(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// isMissingSession reports whether a tmux error says the target session does not
// exist. tmux words it differently per command: has-session says "can't find
// session" while set-option says "no such session".
func isMissingSession(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "can't find session") || strings.Contains(msg, "no such session")
}

// CreateTab adds one detached window to a session and returns its root pane,
// marked as dungeon's own.
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
	if err := t.markPane(ctx, fields[1]); err != nil {
		return "", "", err
	}
	return fields[0], fields[1], nil
}

// SplitPane carves one pane out of target and marks it as dungeon's own. tmux
// -p is the share of the window the new pane takes, while ratio is the share
// target keeps, so the percentages are inverted. Repeated splits of the same target use the same ratio and stay
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
	if err := t.markPane(ctx, id); err != nil {
		return "", err
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

// Read returns one pane's rendered text, joined so a wrapped line reads as the
// one line it was. It is the preview: a read, never a write, and a pane that is
// gone is an error.
func (t tmux) Read(ctx context.Context, id string) (string, error) {
	if err := requireTarget("pane", id); err != nil {
		return "", err
	}
	out, err := t.call(ctx, "capture-pane", "-p", "-J", "-t", id)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// StartAgent records the name on the pane, runs pi with args, and waits until
// the agent can take input: the pane's output has gone quiet, and on the
// operator's pane the foreground command has left the shell that was there. A
// changing foreground command is not enough on its own, because a TUI accepts a
// pasted prompt long after it has replaced the shell. The name is the identity a
// restarted dungeon reads back through Agents; nothing here keeps a map. A pane
// that never starts the agent is an error naming the pane.
//
// A name already live in a different pane is refused: every later lookup is by
// name, so two panes on one name would make it ambiguous. A name already live
// in this pane is the same agent, so the call is a no-op.
//
// A pane dungeon created carries the paneOwnerOption marker, and there StartAgent replaces
// the pane's process with respawn-pane: no interactive shell reads a command
// line, so nothing reaches the operator's HISTFILE, and the pane dies with the
// agent, so the name option cannot outlive it. A pane without the marker is the
// operator's own: its shell must survive, so the agent is pasted into it and the
// name is unset when the agent exits.
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
	owned, err := t.paneOwned(ctx, pane)
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
	if owned {
		if err := t.respawn(ctx, pane, command); err != nil {
			return err
		}
		// No shell is left behind, so there is no foreground command to leave:
		// an empty before disables that half of the readiness wait.
		if err := t.waitQuiet(ctx, pane, name, ""); err != nil {
			return err
		}
		started = true
		return nil
	}
	line := make([]string, len(command))
	for i, arg := range command {
		line[i] = shellQuote(arg)
	}
	// The option lives in tmux, not in the agent process, so it would outlive a
	// dead agent and make a ghost lane read live. The operator's shell stays, so
	// unset it there after the agent exits, however it exits.
	unset := append([]string{"tmux"}, t.argv("set-option", "-p", "-u", "-t", pane, paneAgentOption)...)
	for i, arg := range unset {
		unset[i] = shellQuote(arg)
	}
	if err := t.paste(ctx, pane, strings.Join(line, " ")+"; "+strings.Join(unset, " ")); err != nil {
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

// respawn replaces the pane's process with the agent, so the pane runs no
// interactive shell and dies with the agent. tmux execs the command and its
// arguments as an argv, without a shell, so they are passed as they are.
func (t tmux) respawn(ctx context.Context, pane string, command []string) error {
	argv := []string{"respawn-pane", "-k", "-t", pane, "--"}
	argv = append(argv, command...)
	_, err := t.call(ctx, argv...)
	return err
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
// there before the agent. An empty shell means the pane has no shell to return
// to, which is the respawned case. Bounded: an agent that never settles is an
// error that names the pane.
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

// needlePoll is the pause between two looks at a pane while a prompt lands or
// submits. It is short because a TUI renders a paste in a few hundred
// milliseconds, and every look is one tmux call.
const needlePoll = 50 * time.Millisecond

// pasteDeadline is how long the one paste may take to appear on the pane. A
// fresh pi renders a paste at 310-420ms; the budget is several seconds so a
// busy TUI is never mistaken for a lost paste. The only cost of waiting is a
// slow error, while the cost of giving up early is a second paste.
const pasteDeadline = 5 * time.Second

// submitSettle is the pause between an Enter and the next look that decides
// whether the prompt was submitted.
const submitSettle = 200 * time.Millisecond

// submitTries bounds how many Enters Prompt sends when a slow TUI ignores the
// first one.
const submitTries = 10

// submitDeadline bounds the whole submit phase, so a TUI that keeps the text on
// the input line for another reason cannot hold Prompt forever.
const submitDeadline = 5 * time.Second

// drawDeadline is how long Prompt waits for an agent to draw anything at all
// before it pastes into a blank pane anyway.
const drawDeadline = 5 * time.Second

// settleDeadline bounds the wait for a drawing agent to stop drawing. An agent
// that streams while idle is pasted into regardless; the confirmation decides
// what happened.
const settleDeadline = 15 * time.Second

// Prompt submits text to the named agent as one prompt. The text travels
// through a buffer and bracketed paste, so a long prompt keeps its bytes.
//
// Delivery is two-sided, because a paste sent while a TUI is still starting is
// lost without an error. Prompt waits for the pane's screen to stop changing, so
// a booting TUI is not pasted into. It then pastes exactly once and polls until
// the pane shows the pasted text. Only then does it press Enter, and it polls
// until the pane no longer holds the text as pending input. It returns when the
// input is submitted, not when the turn finishes.
//
// One call is one paste. A prompt pasted twice renders twice and the agent
// receives it twice, so a paste that never appears is an error, never a retry
// inside this call: the caller decides whether to call Prompt again.
func (t tmux) Prompt(ctx context.Context, name, text string) error {
	pane, err := t.agentPane(ctx, name)
	if err != nil {
		return err
	}
	if err := t.settlePane(ctx, pane); err != nil {
		return err
	}
	needle := inputNeedle(text)
	if needle == "" {
		return fmt.Errorf("tmux: pane %s: prompt has no visible text to confirm", pane)
	}
	if err := t.landPrompt(ctx, pane, needle, text); err != nil {
		return err
	}
	return t.submitPrompt(ctx, pane, needle)
}

// settlePane waits until the pane has drawn something and then stopped drawing,
// so a paste does not race a booting TUI. A fresh pi draws its startup banner
// for seconds and can clear the screen under the paste, which looks like a
// submitted prompt: the pane would show the text once and then lose it. A pane
// that never draws is pasted into at drawDeadline, and one that never settles at
// settleDeadline: the two-sided confirmation below still decides what happened.
func (t tmux) settlePane(ctx context.Context, pane string) error {
	drawnAt := time.Now().Add(drawDeadline)
	settleAt := time.Now().Add(settleDeadline)
	var last string
	drawn := false
	since := time.Now()
	for {
		screen, err := t.capture(ctx, pane)
		if err != nil {
			return err
		}
		if strings.TrimSpace(screen) != "" {
			drawn = true
		}
		now := time.Now()
		if !drawn {
			if now.After(drawnAt) {
				return nil
			}
		} else {
			if screen != last {
				last = screen
				since = now
			}
			if now.Sub(since) >= readyQuiet || now.After(settleAt) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(needlePoll):
		}
	}
}

// landPrompt pastes text once and waits until the pane shows it. A paste the
// pane never shows did not land, and is reported as an error that names the
// pane. It is not pasted again: a second paste would render twice and deliver
// the prompt twice, which is worse than a caller that has to retry the call.
func (t tmux) landPrompt(ctx context.Context, pane, needle, text string) error {
	if err := t.pasteText(ctx, pane, text); err != nil {
		return err
	}
	shown, err := t.waitShown(ctx, pane, needle, pasteDeadline)
	if err != nil {
		return err
	}
	if !shown {
		return fmt.Errorf("tmux: pane %s: pasted prompt did not land; it did not appear within %s and was not pasted again", pane, pasteDeadline)
	}
	return nil
}

// waitShown polls until needle is visible anywhere on the pane, or until within
// has passed.
func (t tmux) waitShown(ctx context.Context, pane, needle string, within time.Duration) (bool, error) {
	deadline := time.Now().Add(within)
	for {
		shown, _, err := t.promptShown(ctx, pane, needle)
		if err != nil {
			return false, err
		}
		if shown {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(needlePoll):
		}
	}
}

// submitPrompt presses Enter and waits until the pane no longer holds the text
// as pending input. Text still on the cursor's line after an Enter means the
// TUI ignored it, so Enter is sent again, bounded in both count and time. Text
// gone from the pane entirely also counts: that TUI has cleared the input.
func (t tmux) submitPrompt(ctx context.Context, pane, needle string) error {
	deadline := time.Now().Add(submitDeadline)
	enters := 0
	var lastEnter time.Time
	for {
		now := time.Now()
		if now.After(deadline) {
			return fmt.Errorf("tmux: pane %s: prompt was pasted but not submitted after %d Enters", pane, enters)
		}
		if enters >= submitTries {
			return fmt.Errorf("tmux: pane %s: prompt was pasted but still pending after %d Enters", pane, enters)
		}
		if enters == 0 || now.Sub(lastEnter) >= submitSettle {
			if _, err := t.call(ctx, "send-keys", "-t", pane, "Enter"); err != nil {
				return fmt.Errorf("tmux: pane %s: %w", pane, err)
			}
			enters++
			lastEnter = now
		}
		shown, pending, err := t.promptShown(ctx, pane, needle)
		if err != nil {
			return err
		}
		if !shown || !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(needlePoll):
		}
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

// promptShown reports whether needle is on the pane at all, which proves a paste
// landed, and whether it is on the cursor's line, which is where a TUI echoes
// input it has not submitted. The second answer is a hint, not a proof: a TUI
// that hides the cursor reports a row unrelated to the input, so the caller
// treats a needle gone from the whole pane as submitted too.
func (t tmux) promptShown(ctx context.Context, pane, needle string) (shown, pending bool, err error) {
	out, err := t.call(ctx, "display-message", "-p", "-t", pane, "#{cursor_y}")
	if err != nil {
		return false, false, err
	}
	row, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false, false, fmt.Errorf("tmux: pane %s reported cursor row %q", pane, out)
	}
	screen, err := t.capture(ctx, pane)
	if err != nil {
		return false, false, err
	}
	lines := strings.Split(screen, "\n")
	pending = row >= 0 && row < len(lines) && strings.Contains(lines[row], needle)
	return strings.Contains(screen, needle), pending, nil
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

// pasteText loads text into a private tmux buffer, pastes it bracketed and
// deletes the buffer, without pressing Enter.
func (t tmux) pasteText(ctx context.Context, pane, text string) error {
	buf := fmt.Sprintf("dungeon-%d-%d", os.Getpid(), pasteSeq.Add(1))
	if _, err := t.run(ctx, []byte(text), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if _, err := t.call(ctx, "paste-buffer", "-p", "-d", "-b", buf, "-t", pane); err != nil {
		_, _ = t.call(ctx, "delete-buffer", "-b", buf)
		return err
	}
	return nil
}

// paste pastes text and presses Enter, for a caller that does not need to
// confirm delivery itself.
func (t tmux) paste(ctx context.Context, pane, text string) error {
	if err := t.pasteText(ctx, pane, text); err != nil {
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
// Every command is separated by ";" so tmux does not read the next command as
// the previous one's arguments.
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
		argv = append(argv, "switch-client", "-t", paneSession, ";")
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

// railPanes reads the panes the agent rail shows. It is deliberately tmux-only:
// the rail exists for lanes that herdr cannot see, so it reads tmux directly
// instead of widening the mux interface for one view.
//
// The process tree is not walked. An agent that reports status identifies
// itself through its OSC record, and the report is the identity.
func (t tmux) railPanes(ctx context.Context) (railRead, error) {
	const delim = "\x1f"
	out, err := t.call(ctx, "list-panes", "-a", "-F",
		"#{pane_id}"+delim+"#{session_name}"+delim+"#{window_id}"+delim+
			"#{window_index}"+delim+"#{pane_index}"+delim+
			"#{pane_current_command}"+delim+"#{pane_title}"+delim+
			"#{@dungeon_agent}"+delim+"#{@agent_rail}"+delim+
			"#{pane_current_path}"+delim+"#{pane_active}"+delim+
			"#{@dungeon_osc}")
	if err != nil {
		return railRead{}, err
	}
	now := time.Now()
	server := oscServerKey()
	type cachedStatuses struct {
		statuses map[string]railStatus
		issues   []railSourceIssue
	}
	statusCache := map[string]cachedStatuses{}
	read := railRead{at: now}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, delim)
		if len(f) != 12 || f[0] == "" {
			continue
		}
		if f[8] == "1" {
			continue // the rail itself
		}
		osc, reported, oscErr := readRailOSC(server, f[0])
		if oscErr != nil {
			read.issues = append(read.issues, railSourceIssue{
				kind: "osc", path: oscStatePath(server, f[0]), err: oscErr, at: now,
			})
		}
		kind := railAgentKind(f[5], f[7], osc, reported)
		if kind == "" {
			if !isRailShell(f[5]) {
				continue
			}
			kind = "shell"
		}
		label := railAgentLabel(f[5], f[6], f[7])
		if kind == "shell" {
			label = f[5]
		}
		row := railPaneInfo{
			Pane:        f[0],
			Session:     f[1],
			WindowID:    f[2],
			WindowIndex: f[3],
			PaneIndex:   f[4],
			Command:     f[5],
			Title:       f[6],
			Agent:       f[7],
			Kind:        kind,
			Active:      f[10] == "1",
			OSCPiped:    f[11] == "1",
			Label:       label,
		}
		if root, ok := railEncounterRoot(f[9]); ok {
			cached, found := statusCache[root]
			if !found {
				cached.statuses, cached.issues = loadRailStatuses(root)
				statusCache[root] = cached
				read.issues = append(read.issues, cached.issues...)
			}
			if status, found := cached.statuses[row.Pane]; found {
				row.Status, row.Unit, row.Encounter = status.Status, status.Unit, status.Encounter
			}
		}
		if reported {
			row.Harness = osc.State
			row.HarnessApp = osc.App
			row.HarnessKind = osc.Kind
			row.HarnessMsg = osc.Msg
			row.HarnessTitle = osc.Title
			row.HarnessProgress = osc.Progress
		}
		read.agents = append(read.agents, row)
	}
	return read, nil
}

// readRailOSC separates an absent record from an unreadable one. A missing
// record means the pane never reported; a corrupt record is a source failure
// and must be said out loud.
func readRailOSC(server, pane string) (oscStatus, bool, error) {
	path := oscStatePath(server, pane)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return oscStatus{}, false, nil
	}
	if err != nil {
		return oscStatus{}, false, err
	}
	var st oscStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return oscStatus{}, false, err
	}
	if st.State == "" {
		return oscStatus{}, false, fmt.Errorf("empty state")
	}
	return st, true, nil
}

// railToggle opens, focuses, or closes the rail in the target's window. It
// splits the leftmost pane, not the focused one, so the rail stays on the edge
// after a split in the middle of a window.
func (t tmux) railToggle(ctx context.Context, target string, width int, command string) (string, error) {
	if err := requireTarget("pane", target); err != nil {
		return "", err
	}
	window, err := t.call(ctx, "display-message", "-p", "-t", target, "#{window_id}")
	if err != nil {
		return "", err
	}
	win := strings.TrimSpace(string(window))
	if win == "" {
		return "", fmt.Errorf("tmux: pane %s has no window", target)
	}
	const delim = "\x1f"
	out, err := t.call(ctx, "list-panes", "-t", win, "-F",
		"#{pane_id}"+delim+"#{@agent_rail}"+delim+"#{pane_left}")
	if err != nil {
		return "", err
	}
	rail, left, haveLeft := "", 0, false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, delim)
		if len(f) != 3 {
			continue
		}
		if f[1] == "1" {
			rail = f[0]
		}
		if n, err := strconv.Atoi(f[2]); err == nil && (!haveLeft || n < left) {
			left, haveLeft = n, true
		}
	}
	if rail != "" {
		if target == rail {
			return "", t.callNoOutput(ctx, "kill-pane", "-t", rail)
		}
		return rail, t.callNoOutput(ctx, "select-pane", "-t", rail)
	}
	if !haveLeft {
		return "", fmt.Errorf("tmux: window %s has no panes", win)
	}
	// Resolve the leftmost pane id from the same list.
	leftPane := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, delim)
		if len(f) != 3 {
			continue
		}
		if n, err := strconv.Atoi(f[2]); err == nil && n == left {
			leftPane = f[0]
			break
		}
	}
	if leftPane == "" {
		return "", fmt.Errorf("tmux: window %s has no left pane", win)
	}
	cwd, err := t.call(ctx, "display-message", "-p", "-t", target, "#{pane_current_path}")
	if err != nil {
		return "", err
	}
	if width < 12 {
		width = 12
	}
	w, err := t.call(ctx, "display-message", "-p", "-t", win, "#{window_width}")
	if err == nil {
		if total := strings.TrimSpace(string(w)); total != "" {
			var n int
			if _, scanErr := fmt.Sscan(total, &n); scanErr == nil && n < width*2 {
				width = n / 3
				if width < 12 {
					width = 12
				}
			}
		}
	}
	railOut, err := t.call(ctx, "split-window", "-hb", "-l", strconv.Itoa(width),
		"-t", leftPane, "-c", strings.TrimSpace(string(cwd)),
		"-P", "-F", "#{pane_id}", command)
	if err != nil {
		return "", err
	}
	rail = strings.TrimSpace(string(railOut))
	if rail == "" {
		return "", fmt.Errorf("tmux split-window returned no rail pane")
	}
	if err := t.callNoOutput(ctx, "set-option", "-p", "-t", rail, paneRailOption, "1"); err != nil {
		return "", err
	}
	if err := t.callNoOutput(ctx, "select-pane", "-t", rail); err != nil {
		return "", err
	}
	return rail, nil
}

// callNoOutput runs one tmux command and discards stdout.
func (t tmux) callNoOutput(ctx context.Context, args ...string) error {
	_, err := t.call(ctx, args...)
	return err
}
