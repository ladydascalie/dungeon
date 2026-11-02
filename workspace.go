package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// One encounter, one workspace. Inside it, one tab per role, and one pane per
// minion of that role. A workspace is the unit of cleanup and of attention: the
// operator closes one thing, not sixteen.

func (h herdr) Workspaces(ctx context.Context) []workspace {
	out, err := h.call(ctx, "workspace", "list")
	if err != nil {
		return nil
	}
	var payload struct {
		Result struct {
			Workspaces []struct {
				ID    string `json:"workspace_id"`
				Label string `json:"label"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil
	}
	all := make([]workspace, 0, len(payload.Result.Workspaces))
	for _, ws := range payload.Result.Workspaces {
		all = append(all, workspace{ID: ws.ID, Label: ws.Label})
	}
	return all
}

// CreateWorkspace creates the encounter workspace and returns its first tab and
// root pane. The tab is renamed to the role it holds.
func (h herdr) CreateWorkspace(ctx context.Context, label, tabLabel, cwd string) (wsID, tabID, rootPane string, err error) {
	out, err := h.call(ctx, "workspace", "create", "--label", label, "--cwd", cwd, "--no-focus")
	if err != nil {
		return "", "", "", err
	}
	var payload struct {
		Result struct {
			Workspace struct {
				ID string `json:"workspace_id"`
			} `json:"workspace"`
			Tab struct {
				ID string `json:"tab_id"`
			} `json:"tab"`
			Root struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", "", "", fmt.Errorf("herdr workspace create: %w", err)
	}
	wsID, tabID, rootPane = payload.Result.Workspace.ID, payload.Result.Tab.ID, payload.Result.Root.PaneID
	if wsID == "" || tabID == "" || rootPane == "" {
		return "", "", "", fmt.Errorf("herdr workspace create returned an incomplete layout")
	}
	if _, err := h.call(ctx, "tab", "rename", tabID, tabLabel); err != nil {
		return "", "", "", err
	}
	return wsID, tabID, rootPane, nil
}

// CreateTab adds a tab for a role to an existing workspace.
func (h herdr) CreateTab(ctx context.Context, ws, label, cwd string) (tabID, rootPane string, err error) {
	args := []string{"tab", "create"}
	if ws != "" {
		args = append(args, "--workspace", ws)
	}
	args = append(args, "--label", label, "--cwd", cwd, "--no-focus")
	out, err := h.call(ctx, args...)
	if err != nil {
		return "", "", err
	}
	var payload struct {
		Result struct {
			Tab struct {
				ID string `json:"tab_id"`
			} `json:"tab"`
			Root struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", "", fmt.Errorf("herdr tab create: %w", err)
	}
	if payload.Result.Tab.ID == "" || payload.Result.Root.PaneID == "" {
		return "", "", fmt.Errorf("herdr tab create returned an incomplete layout")
	}
	return payload.Result.Tab.ID, payload.Result.Root.PaneID, nil
}

// FocusTab moves the operator to a tab.
func (h herdr) FocusTab(ctx context.Context, tab string) error {
	_, err := h.call(ctx, "tab", "focus", tab)
	return err
}

func (h herdr) CloseWorkspace(ctx context.Context, ws string) error {
	_, err := h.call(ctx, "workspace", "close", ws)
	return err
}

// findWorkspace is the encounter's workspace, if it is still open.
func findWorkspace(ctx context.Context, h mux, label string) string {
	for _, ws := range h.Workspaces(ctx) {
		if ws.Label == label {
			return ws.ID
		}
	}
	return ""
}

// closeEncounter closes the workspace up created, unless a pane the operator
// pinned lives inside it. A pinned pane belongs to the operator, so the
// workspace stays and they close it when they are done.
func closeEncounter(ctx context.Context, h mux, p Plan) {
	ws := findWorkspace(ctx, h, p.Encounter)
	if ws == "" {
		return
	}
	for _, m := range p.Minions {
		if m.Pane == "" {
			continue
		}
		if info, err := h.Pane(ctx, m.Pane); err == nil && info.Workspace == ws {
			return
		}
	}
	h.CloseWorkspace(ctx, ws)
}

// tabFor is one role's tab: its root pane, and how many of its minions are
// already placed. Splits always come off the root, so the panes stay equal.
type tabFor struct {
	tab   string
	root  string
	total int
	used  int
}

func (t *tabFor) next(ctx context.Context, h mux, cwd, direction string) (string, error) {
	if t.used == 0 {
		t.used++
		return t.root, nil
	}
	// The multiplexer's ratio is the share the pane being split keeps, so the
	// new pane gets 1/(remaining+1) of the tab. Passing the new pane's share
	// gave a 2 lane tab a 256/28 split, not two halves.
	remaining := t.total - t.used
	ratio := float64(remaining) / float64(remaining+1)
	pane, err := h.SplitPane(ctx, t.root, direction, cwd, ratio)
	if err != nil {
		return "", err
	}
	t.used++
	return pane, nil
}

// roleLabel is the tab name for a role: singular for one minion, plural for
// more, so a glance at the tabs says how many lanes are in each.
func roleLabel(role string, n int) string {
	if n <= 1 {
		return role
	}
	return role + "s"
}

// countRoles returns the number of minions per role, in first appearance order.
func countRoles(minions []Minion) (order []string, counts map[string]int) {
	counts = map[string]int{}
	for _, m := range minions {
		if counts[m.Role] == 0 {
			order = append(order, m.Role)
		}
		counts[m.Role]++
	}
	return order, counts
}
