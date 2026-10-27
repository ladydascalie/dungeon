package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// up spawns the encounter's minions. It is idempotent: a minion whose agent is
// already live is left alone, so a second up resumes rather than doubling the
// fleet.
//
// The layout follows the plan's shape: one workspace per encounter, one tab per
// role, one pane per minion. A pinned minion keeps its own pane and is not moved
// into the workspace.

type spawnResult struct {
	Spawned []string
	Already []string
	Failed  []string
}

func (r spawnResult) String() string {
	return fmt.Sprintf("%d spawned, %d already live, %d failed",
		len(r.Spawned), len(r.Already), len(r.Failed))
}

func ensureWorktree(repo, base, encounter, minion, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // resume: the worktree is already cut
	}
	branch := encounter + "/" + minion
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, path, base)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git worktree add %s: %s", path, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// worktreePath keeps lanes beside the repo, never inside it: a worktree nested
// in its own repository confuses every tool that walks the tree.
func worktreePath(repo, encounter, minion string) string {
	return filepath.Join(filepath.Dir(repo), encounter+"-"+minion)
}

func spawnMinions(ctx context.Context, planPath string, p Plan, st *State, out io.Writer) (spawnResult, error) {
	var res spawnResult
	h := newHerdrClient()
	agents := h.agents(ctx)

	if _, err := os.Stat(p.Repo); err != nil {
		return res, fmt.Errorf("repo %s is not readable", p.Repo)
	}
	dir := encounterDir(planPath)
	_, counts := countRoles(p.Minions)

	ws := h.findWorkspace(ctx, p.Encounter)
	tabs := map[string]*tabFor{}

	// tab returns the tab a role runs in, creating the workspace and the tab on
	// first use. A resumed encounter reuses the workspace it finds.
	tab := func(m Minion) (*tabFor, error) {
		if t, ok := tabs[m.Role]; ok {
			return t, nil
		}
		label := roleLabel(m.Role, counts[m.Role])
		t := &tabFor{total: counts[m.Role]}
		if ws == "" {
			id, tabID, root, err := h.openWorkspace(ctx, p.Encounter, label, p.Repo)
			if err != nil {
				return nil, err
			}
			ws, t.tab, t.root = id, tabID, root
		} else {
			tabID, root, err := h.openTab(ctx, ws, label, p.Repo)
			if err != nil {
				return nil, err
			}
			t.tab, t.root = tabID, root
		}
		tabs[m.Role] = t
		return t, nil
	}

	for _, m := range p.Minions {
		if a, live := agents[m.ID]; live {
			st.SetMinion(m.ID, "spawned", "already live")
			st.PinPane(m.ID, a.Pane)
			res.Already = append(res.Already, m.ID)
			continue
		}

		cwd := p.Repo
		if writes(m.Role) {
			cwd = worktreePath(p.Repo, p.Encounter, m.ID)
			if err := ensureWorktree(p.Repo, p.Base, p.Encounter, m.ID, cwd); err != nil {
				res.Failed = append(res.Failed, m.ID)
				fmt.Fprintf(out, "%s: %v\n", m.ID, err)
				continue
			}
		}

		var pane string
		if m.Pane != "" && h.paneExists(ctx, m.Pane) {
			pane = m.Pane // the operator's pane wins over the layout
		} else {
			t, err := tab(m)
			if err != nil {
				return res, err
			}
			pane, err = t.next(ctx, h, cwd)
			if err != nil {
				return res, err
			}
		}
		if m.Pane != "" && pane != m.Pane {
			fmt.Fprintf(out, "%s: pane %s is gone, using %s\n", m.ID, m.Pane, pane)
		}

		sessionDir := filepath.Join(dir, "sessions", m.ID)
		if err := os.MkdirAll(sessionDir, 0o755); err != nil {
			return res, err
		}
		if err := h.startAgent(ctx, m.ID, pane, laneArgs(m.Role, sessionDir)); err != nil {
			res.Failed = append(res.Failed, m.ID)
			fmt.Fprintf(out, "%s: %v\n", m.ID, err)
			continue
		}
		st.SetMinion(m.ID, "spawned", "")
		st.PinPane(m.ID, pane)
		res.Spawned = append(res.Spawned, m.ID)
	}
	return res, nil
}
