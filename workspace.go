package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// One encounter, one workspace. Inside it, one tab per role, and one pane per
// minion of that role. A workspace is the unit of cleanup and of attention: the
// operator closes one thing, not sixteen.

type workspaceInfo struct {
	ID    string `json:"workspace_id"`
	Label string `json:"label"`
}

func (h herdrClient) workspaces(ctx context.Context) []workspaceInfo {
	out, err := h.run(ctx, "workspace", "list")
	if err != nil {
		return nil
	}
	var payload struct {
		Result struct {
			Workspaces []workspaceInfo `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil
	}
	return payload.Result.Workspaces
}

func (h herdrClient) findWorkspace(ctx context.Context, label string) string {
	for _, ws := range h.workspaces(ctx) {
		if ws.Label == label {
			return ws.ID
		}
	}
	return ""
}

// openWorkspace creates the encounter workspace and returns its first tab and
// root pane. The tab is renamed to the role it holds.
func (h herdrClient) openWorkspace(ctx context.Context, label, tabLabel, cwd string) (ws, tab, root string, err error) {
	out, err := h.run(ctx, "workspace", "create", "--label", label, "--cwd", cwd, "--no-focus")
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
	ws, tab, root = payload.Result.Workspace.ID, payload.Result.Tab.ID, payload.Result.Root.PaneID
	if ws == "" || tab == "" || root == "" {
		return "", "", "", fmt.Errorf("herdr workspace create returned an incomplete layout")
	}
	if _, err := h.run(ctx, "tab", "rename", tab, tabLabel); err != nil {
		return "", "", "", err
	}
	return ws, tab, root, nil
}

// openTab adds a tab for a role to an existing workspace.
func (h herdrClient) openTab(ctx context.Context, ws, label, cwd string) (tab, root string, err error) {
	out, err := h.run(ctx, "tab", "create", "--workspace", ws, "--label", label, "--cwd", cwd, "--no-focus")
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

func (h herdrClient) closeWorkspace(ctx context.Context, ws string) error {
	_, err := h.run(ctx, "workspace", "close", ws)
	return err
}

// workspaceOfPane answers the one question down needs before closing a
// workspace: is a pane the operator pinned inside it?
func (h herdrClient) workspaceOfPane(ctx context.Context, pane string) string {
	out, err := h.run(ctx, "pane", "get", pane)
	if err != nil {
		return ""
	}
	var payload struct {
		Result struct {
			Pane struct {
				Workspace string `json:"workspace_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return ""
	}
	return payload.Result.Pane.Workspace
}

// closeEncounter closes the workspace up created, unless a pane the operator
// pinned lives inside it. A pinned pane belongs to the operator, so the
// workspace stays and they close it when they are done.
func closeEncounter(ctx context.Context, h herdrClient, p Plan) {
	ws := h.findWorkspace(ctx, p.Encounter)
	if ws == "" {
		return
	}
	for _, m := range p.Minions {
		if m.Pane != "" && h.workspaceOfPane(ctx, m.Pane) == ws {
			return
		}
	}
	h.closeWorkspace(ctx, ws)
}

// tabFor is one role's tab: its root pane, and how many of its minions are
// already placed. Splits always come off the root, so the panes stay equal.
type tabFor struct {
	tab   string
	root  string
	total int
	used  int
}

func (t *tabFor) next(ctx context.Context, h herdrClient, cwd string) (string, error) {
	if t.used == 0 {
		t.used++
		return t.root, nil
	}
	ratio := 1.0 / float64(t.total-t.used)
	pane, err := h.splitPane(ctx, t.root, "down", cwd, ratio)
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
