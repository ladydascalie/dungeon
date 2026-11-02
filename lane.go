package main

import (
	"context"
	"errors"
	"time"
)

// A pin is a request. The plan may name the pane a minion should run in, because
// a pane the operator chose is where their attention already is. The pin is
// checked against the multiplexer at spawn time and abandoned if the pane is
// gone, so a stale plan degrades to a fresh split instead of a failure.

var errNoPane = errors.New("no live agent and no known pane for this minion")

func paneExists(ctx context.Context, h mux, pane string) bool {
	_, err := h.Pane(ctx, pane)
	return err == nil
}

// lanePane returns the pane a minion should run in. It creates the encounter tab
// once, on the first minion that needs one, and records it through root.
func lanePane(ctx context.Context, h mux, encounter, pinned string, root *string, cwd string, ratio float64) (string, error) {
	if pinned != "" && paneExists(ctx, h, pinned) {
		return pinned, nil
	}
	if *root == "" {
		_, rootPane, err := h.CreateTab(ctx, "", encounter, cwd)
		if err != nil {
			return "", err
		}
		*root = rootPane
	}
	return h.SplitPane(ctx, *root, "down", cwd, ratio)
}

// stopPinned exits the agent and leaves the shell. The two interrupts must be
// separate writes with a gap: sent together, the agent coalesces them and keeps
// running, which down would then have to report as a lie.
func stopPinned(ctx context.Context, h mux, agent string) error {
	if err := h.SendKeys(ctx, agent, "ctrl+c"); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	return h.SendKeys(ctx, agent, "ctrl+c")
}

// verifyDown re-reads the fleet before down reports. A lane that ignores the
// interrupt is still running, and counting it as stopped would be the kind of
// claim this tool refuses everywhere else.
func verifyDown(ctx context.Context, h mux, st *State, stopped, refused []string) ([]string, []string) {
	time.Sleep(500 * time.Millisecond)
	after, err := h.Agents(ctx)
	if err != nil {
		// Without the fleet there is no proof anything stopped, so nothing is
		// called stopped.
		for _, id := range stopped {
			st.SetMinion(id, "lost", "could not read the fleet, so it cannot be called stopped")
			refused = append(refused, id)
		}
		return nil, refused
	}
	var done []string
	for _, id := range stopped {
		if _, live := after[st.AgentName(id)]; live {
			st.SetMinion(id, "busy", "ignored the interrupt, still live")
			refused = append(refused, id)
			continue
		}
		done = append(done, id)
	}
	return done, refused
}

// focusLane jumps the operator to a lane. A live agent is the exact answer: the
// multiplexer focuses the pane hosting it. With no live agent, the pane hint
// recorded at spawn is all there is, and the best move is to focus its tab.
func focusLane(ctx context.Context, h mux, agent, hint string) error {
	if err := h.FocusAgent(ctx, agent); err == nil {
		return nil
	}
	if hint == "" {
		return errNoPane
	}
	info, err := h.Pane(ctx, hint)
	if err != nil {
		return err
	}
	if info.Tab == "" {
		return errNoPane
	}
	return h.FocusTab(ctx, info.Tab)
}

// stopLane ends a lane. A pane the operator pinned is theirs, so the agent is
// interrupted twice and its shell survives: one ctrl+c interrupts a turn, two
// exits the agent. A pane dungeon created is dungeon's to close.
func stopLane(ctx context.Context, h mux, agent, pane string, pinned bool) error {
	if pinned {
		return stopPinned(ctx, h, agent)
	}
	return h.ClosePane(ctx, pane)
}
