package main

import "context"

// mux is the terminal multiplexer dungeon needs: workspaces, panes, the agents
// living in them, and nothing else.
//
// A second backend (tmux, for example) implements this interface and nothing
// above it changes. The contracts below are what such a backend must honour:
//
//   - Identifiers are opaque. Never parse a pane, tab or workspace id, and never
//     derive one from a name. Hold what a call returned and pass it back.
//   - Identifiers are stable while the object lives. They may be reused after a
//     close.
//   - Close only what dungeon created. The operator's panes, tabs and
//     workspaces are theirs; a pinned pane is theirs even after dungeon put an
//     agent in it.
//   - Do not move the operator's focus unless the call says focus. Creation and
//     splitting must not steal focus.
//   - Listing calls report what exists now; dungeon caches nothing.
//
// Calls take a context. A backend must stop and return promptly when it is
// cancelled.
type mux interface {
	// Agents reports every live agent, keyed by the name its lane was given.
	// This is the only source of liveness and status truth: a backend must read
	// it from the multiplexer, not infer it from what it started, and must say
	// AgentUnknown when it has no protocol to tell.
	//
	// The name must survive the dungeon process. A backend records each agent's
	// name inside the multiplexer at start (a pane option, a label, whatever it
	// has) and enumerates here every live pane carrying a recorded name. An
	// in-memory map is not enough: a restarted dungeon would see an empty fleet
	// and mark every live lane absent.
	//
	// A reachable multiplexer with no agents returns an empty map and no error.
	// An error means the fleet could not be read at all, which is not the same
	// as an empty fleet: a caller that decides on liveness must not mistake one
	// for the other.
	Agents(ctx context.Context) (map[string]Agent, error)

	// SelfPane reports the pane dungeon itself is running in, so a brief can
	// tell a lane where to ping. An empty string means the backend cannot tell.
	SelfPane(ctx context.Context) string

	// Workspaces lists every workspace, in the backend's own order. Empty is
	// valid. A missing or failing multiplexer returns nil.
	Workspaces(ctx context.Context) []workspace

	// CreateWorkspace creates one workspace with its first tab, labels that tab
	// tabLabel, and returns the workspace, the tab and that tab's root pane. It
	// is created without focus and rooted at cwd.
	CreateWorkspace(ctx context.Context, label, tabLabel, cwd string) (wsID, tabID, rootPane string, err error)

	// CreateTab adds one tab to ws and returns the tab and its root pane. An
	// empty ws means the current one. It is created without focus and rooted at
	// cwd.
	CreateTab(ctx context.Context, ws, label, cwd string) (tabID, rootPane string, err error)

	// SplitPane splits target and returns the new pane, rooted at cwd. direction
	// is one of right, down, left or up. ratio is the share of target the
	// existing pane keeps, so repeated splits of one target stay equal. The new
	// pane does not take focus.
	SplitPane(ctx context.Context, target, direction, cwd string, ratio float64) (newPane string, err error)

	// Pane reports one pane's tab and workspace. A pane that is gone is an
	// error; that is how a stale pin is detected.
	Pane(ctx context.Context, id string) (pane, error)

	// StartAgent starts an agent named name in pane, running the given command
	// arguments, and returns when the multiplexer accepts it. name must be the
	// name later calls use to address that agent, and the backend must record it
	// inside the multiplexer so Agents still finds it after a restart.
	StartAgent(ctx context.Context, name, pane string, args []string) error

	// Prompt submits text to the named agent as one prompt and returns as soon
	// as it is submitted. It does not wait for the turn to finish: the caller
	// decides when to look again.
	Prompt(ctx context.Context, name, text string) error

	// SendKeys writes keys to the named agent. Multiple keys are separate
	// writes and are not coalesced, so the caller controls that decision.
	SendKeys(ctx context.Context, name string, keys ...string) error

	// PingCommand returns the shell command that delivers message to pane, for
	// briefs that tell another agent how to reach the dispatcher. The backend
	// owns the wording, quoting included; callers must not build either.
	PingCommand(pane, message string) string

	// FocusAgent moves the operator's focus to the pane hosting the named agent.
	// An agent that is not live is an error.
	FocusAgent(ctx context.Context, name string) error

	// FocusTab moves the operator's focus to tab.
	FocusTab(ctx context.Context, tab string) error

	// ClosePane closes one pane dungeon created. It kills whatever runs in it,
	// so the caller decides whether that is safe.
	ClosePane(ctx context.Context, pane string) error

	// CloseWorkspace closes one workspace dungeon created, and the tabs in it.
	CloseWorkspace(ctx context.Context, ws string) error
}

// Agent is one live agent: the name its lane was given, the pane hosting it,
// and what the backend can say about it.
type Agent struct {
	Name   string
	Pane   string
	Status AgentState
}

// AgentState is what a backend knows about an agent. A backend with no agent
// protocol cannot know more than that a pane is alive, so it reports
// AgentUnknown rather than guessing a state it does not have.
//
// Unknown still runs the encounter. A protocol-less backend drives up, and
// then settles each unit by its accept command instead of the lane state; a
// lane it cannot classify is stopped only with down --force, and the operator
// can always record a status by hand with set.
type AgentState string

const (
	AgentUnknown AgentState = "unknown" // the backend cannot tell
	AgentWorking AgentState = "working"
	AgentIdle    AgentState = "idle"
	AgentBlocked AgentState = "blocked"
	AgentDone    AgentState = "done"
)

// workspace is one workspace: an opaque id and the label dungeon gave it.
type workspace struct {
	ID    string
	Label string
}

// pane is where a pane lives: its opaque id, its tab and its workspace. It is
// the answer to both "does this pane still exist" and "whose pane is it".
type pane struct {
	ID        string
	Tab       string
	Workspace string
}
