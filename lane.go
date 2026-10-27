package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// A pin is a request. The plan may name the pane a minion should run in, because
// a pane the operator chose is where their attention already is. The pin is
// checked against herdr at spawn time and abandoned if the pane is gone, so a
// stale plan degrades to a fresh split instead of a failure.

var errNoPane = errors.New("no live agent and no known pane for this minion")

func (h herdrClient) paneExists(ctx context.Context, pane string) bool {
	_, err := h.run(ctx, "pane", "get", pane)
	return err == nil
}

// lanePane returns the pane a minion should run in. It creates the encounter tab
// once, on the first minion that needs one, and records it through root.
func (h herdrClient) lanePane(ctx context.Context, encounter, pinned string, root *string, cwd string, ratio float64) (string, error) {
	if pinned != "" && h.paneExists(ctx, pinned) {
		return pinned, nil
	}
	if *root == "" {
		created, err := h.createTab(ctx, encounter, cwd)
		if err != nil {
			return "", err
		}
		*root = created
	}
	return h.splitPane(ctx, *root, "down", cwd, ratio)
}

// stopPinned exits the agent and leaves the shell. The two interrupts must be
// separate writes with a gap: sent together, the agent coalesces them and keeps
// running, which down would then have to report as a lie.
func (h herdrClient) stopPinned(ctx context.Context, minion string) error {
	if err := h.sendKeys(ctx, minion, "ctrl+c"); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	return h.sendKeys(ctx, minion, "ctrl+c")
}

// verifyDown re-reads the fleet before down reports. A lane that ignores the
// interrupt is still running, and counting it as stopped would be the kind of
// claim this tool refuses everywhere else.
func verifyDown(ctx context.Context, h herdrClient, st *State, stopped, refused []string) ([]string, []string) {
	time.Sleep(500 * time.Millisecond)
	after := h.agents(ctx)
	var done []string
	for _, id := range stopped {
		if _, live := after[id]; live {
			st.SetMinion(id, "busy", "ignored the interrupt, still live")
			refused = append(refused, id)
			continue
		}
		done = append(done, id)
	}
	return done, refused
}

// focus jumps the operator to a unit's lane. A live agent is the exact answer:
// herdr focuses the pane hosting it. With no live agent, the pane hint recorded
// at spawn is all there is, and the best move is to focus its tab.
func (h herdrClient) focus(ctx context.Context, minion, pinned string) error {
	if _, err := h.run(ctx, "agent", "focus", minion); err == nil {
		return nil
	}
	if pinned == "" {
		return errNoPane
	}
	tab, err := h.paneTab(ctx, pinned)
	if err != nil {
		return err
	}
	_, err = h.run(ctx, "tab", "focus", tab)
	return err
}

func (h herdrClient) sendKeys(ctx context.Context, target string, keys ...string) error {
	_, err := h.run(ctx, append([]string{"agent", "send-keys", target}, keys...)...)
	return err
}

// stopLane ends a lane. A pane the operator pinned is theirs, so the agent is
// interrupted twice and its shell survives: one ctrl+c interrupts a turn, two
func (h herdrClient) stopLane(ctx context.Context, minion, pane string, pinned bool) error {
	if pinned {
		return h.stopPinned(ctx, minion)
	}
	return h.closePane(ctx, pane)
}

func (h herdrClient) paneTab(ctx context.Context, pane string) (string, error) {
	out, err := h.run(ctx, "pane", "get", pane)
	if err != nil {
		return "", err
	}
	var payload struct {
		Result struct {
			Pane struct {
				Tab string `json:"tab_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", err
	}
	if payload.Result.Pane.Tab == "" {
		return "", errNoPane
	}
	return payload.Result.Pane.Tab, nil
}
