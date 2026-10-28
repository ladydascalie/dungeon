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
	return cutWorktree(repo, base, encounter+"/"+minion, path, true)
}

// cutWorktree cuts a worktree at path. Worktrunk does the work when it is
// installed, so the operator's layout, hooks and ignored-file copies apply.
// Plain git is the fallback, and only when wt is missing.
func cutWorktree(repo, base, branch, path string, create bool) error {
	if _, err := exec.LookPath("wt"); err == nil {
		args := []string{
			"--config-set", fmt.Sprintf("worktree-path=%q", path),
			"-C", repo, "-y", "switch", "--no-cd", "--format", "json",
		}
		if create {
			args = append(args, "--create", "--base", base)
		}
		args = append(args, branch)
		var stderr strings.Builder
		cmd := exec.Command("wt", args...)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("wt switch %s: %s", branch, strings.TrimSpace(stderr.String()))
		}
	} else {
		args := []string{"-C", repo, "worktree", "add"}
		if create {
			args = append(args, "-b", branch, path, base)
		} else {
			args = append(args, path, branch)
		}
		var stderr strings.Builder
		cmd := exec.Command("git", args...)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git worktree add %s: %s", path, strings.TrimSpace(stderr.String()))
		}
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("worktree %s was not created: %w", path, err)
	}
	return nil
}

// worktreePath keeps lanes beside the repo, never inside it: a worktree nested
// in its own repository confuses every tool that walks the tree.
func worktreePath(repo, encounter, minion string) string {
	return filepath.Join(filepath.Dir(repo), encounter+"-"+minion)
}

func spawnMinions(ctx context.Context, planPath string, p Plan, st *State, splitOverride string, out io.Writer) (spawnResult, error) {
	var res spawnResult
	h := newHerdrClient()
	agents := h.agents(ctx)

	if _, err := os.Stat(p.Repo); err != nil {
		return res, fmt.Errorf("repo %s is not readable", p.Repo)
	}
	dir := encounterDir(planPath)
	_, counts := countRoles(p.Minions)
	// Tall and narrow beats short and wide, so panes grow sideways unless the
	// plan says otherwise.
	direction := p.Split
	if direction == "" {
		direction = "right"
	}
	if splitOverride != "" {
		direction = splitOverride
	}

	ws := h.findWorkspace(ctx, p.Encounter)
	tabs := map[string]*tabFor{}

	// tab returns the tab a role runs in, creating the workspace and the tab on
	// first use. The root pane takes the first minion's cwd, so a reviewer's tab
	// opens where the reviewer must read. A resumed encounter reuses the
	// workspace it finds.
	tab := func(m Minion, cwd string) (*tabFor, error) {
		if t, ok := tabs[m.Role]; ok {
			return t, nil
		}
		label := roleLabel(m.Role, counts[m.Role])
		t := &tabFor{total: counts[m.Role]}
		if ws == "" {
			id, tabID, root, err := h.openWorkspace(ctx, p.Encounter, label, cwd)
			if err != nil {
				return nil, err
			}
			ws, t.tab, t.root = id, tabID, root
		} else {
			tabID, root, err := h.openTab(ctx, ws, label, cwd)
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

		// A reviewer reads its writer's worktree, so it starts there, not in
		// the base checkout. The accept command already runs in the same place.
		cwd := minionDir(p, m)
		if writes(m.Role) {
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
			t, err := tab(m, cwd)
			if err != nil {
				return res, err
			}
			pane, err = t.next(ctx, h, cwd, direction)
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
		if err := h.startAgent(ctx, m.ID, pane, laneArgs(m.Role, m.Tools, sessionDir)); err != nil {
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
