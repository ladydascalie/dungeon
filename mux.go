package main

import "context"

// Mux is the terminal multiplexer dungeon needs: workspaces, tabs and panes, the
// agents living in them, and nothing else.
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
//   - A read returns rendered text, as the terminal shows it. Never markup,
//     escape sequences or raw cell data.
//   - Listing calls report what exists now; dungeon caches nothing.
//
// Calls take a context. A backend must stop and return promptly when it is
// cancelled; a missing multiplexer is reported as an error, except for the
// read-only probes named below.
type Mux interface {
	// Agents reports every live agent, keyed by the name its lane was given.
	// This is the only source of liveness and status truth: a backend must read
	// it from the multiplexer, not infer it from what it started. A missing or
	// failing multiplexer returns nil, not an error, so plan checking and state
	// editing work on a machine with no session.
	Agents(ctx context.Context) map[string]Agent

	// Workspaces lists every workspace, in the backend's own order. Empty is
	// valid. A missing or failing multiplexer returns nil.
	Workspaces(ctx context.Context) []Workspace

	// CreateWorkspace creates one workspace with its first tab, labels that tab
	// tabLabel, and returns the workspace, the tab and that tab's root pane. It
	// is created without focus and rooted at cwd.
	CreateWorkspace(ctx context.Context, label, tabLabel, cwd string) (workspace, tab, rootPane string, err error)

	// CreateTab adds one tab to workspace and returns the tab and its root pane.
	// An empty workspace means the current one. It is created without focus and
	// rooted at cwd.
	CreateTab(ctx context.Context, workspace, label, cwd string) (tab, rootPane string, err error)

	// Tabs lists the tabs of one workspace. An empty workspace returns nil. A
	// failing backend returns nil.
	Tabs(ctx context.Context, workspace string) []Tab

	// SplitPane splits target and returns the new pane, rooted at cwd. direction
	// is one of right, down, left or up. ratio is the share of target the
	// existing pane keeps, so repeated splits of one target stay equal. The new
	// pane does not take focus.
	SplitPane(ctx context.Context, target, direction, cwd string, ratio float64) (pane string, err error)

	// Pane reports one pane's tab and workspace. A pane that is gone is an
	// error; that is how a stale pin is detected.
	Pane(ctx context.Context, pane string) (Pane, error)

	// StartAgent starts an agent named name in pane, running the given command
	// arguments, and returns when the multiplexer accepts it. name must be the
	// name later calls use to address that agent.
	StartAgent(ctx context.Context, name, pane string, args []string) error

	// Prompt submits text to the named agent as one prompt and returns as soon
	// as it is submitted. It does not wait for the turn to finish: the caller
	// decides when to look again.
	Prompt(ctx context.Context, name, text string) error

	// SendKeys writes keys to the named agent. Multiple keys are separate
	// writes and are not coalesced, so the caller controls that decision.
	SendKeys(ctx context.Context, name string, keys ...string) error

	// Read returns the rendered text currently visible in pane. It returns text
	// only, never markup and never escape sequences.
	Read(ctx context.Context, pane string) (string, error)

	// Run types one command line into pane and submits it. The caller owns the
	// text; the backend must not quote or rewrite it.
	Run(ctx context.Context, pane, text string) error

	// PingCommand returns the command line a shell can run to deliver text into
	// pane, for briefs that tell another agent how to reach the dispatcher. The
	// backend decides the wording; callers must not build it themselves.
	PingCommand(pane string) string

	// FocusAgent moves the operator's focus to the pane hosting the named agent.
	// An agent that is not live is an error.
	FocusAgent(ctx context.Context, name string) error

	// FocusTab moves the operator's focus to tab.
	FocusTab(ctx context.Context, tab string) error

	// ClosePane closes one pane dungeon created. It kills whatever runs in it,
	// so the caller decides whether that is safe.
	ClosePane(ctx context.Context, pane string) error

	// CloseTab closes one tab dungeon created, and the panes in it.
	CloseTab(ctx context.Context, tab string) error

	// CloseWorkspace closes one workspace dungeon created, and the tabs in it.
	CloseWorkspace(ctx context.Context, workspace string) error
}

// Workspace is one workspace: an opaque id and the label dungeon gave it.
type Workspace struct {
	ID    string
	Label string
}

// Tab is one tab in a workspace: an opaque id and the label dungeon gave it.
type Tab struct {
	ID    string
	Label string
}

// Pane is where a pane lives: its opaque id, its tab and its workspace. It is
// the answer to both "does this pane still exist" and "whose pane is it".
type Pane struct {
	ID        string
	Tab       string
	Workspace string
}
