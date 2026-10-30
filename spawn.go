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
// installed, so its own config decides the layout, hooks and ignored-file
// copies. The path argument is where worktreePath expects the lane to land:
// worktrunk's config must produce the same place. Plain git is the fallback,
// and only when wt is missing.
func cutWorktree(repo, base, branch, path string, create bool) error {
	if _, err := exec.LookPath("wt"); err == nil {
		args := []string{
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

// worktreePath uses the project's worktree convention, ../<repo>.worktrees/<name>,
// so dungeon's lanes sit where worktrunk puts every other worktree. A worktree
// nested in its own repository confuses every tool that walks the tree.
func worktreePath(repo, encounter, minion string) string {
	root := filepath.Base(repo) + ".worktrees"
	return filepath.Join(filepath.Dir(repo), root, encounter+"-"+minion)
}

// ensureToken gives the run its own herdr names on the first up. The state is
// saved by the caller, so the token survives the spawn and every later command
// reuses it.
func ensureToken(ctx context.Context, h herdrClient, st *State, minions []Minion, gen func() string) error {
	if st.Token != "" {
		return nil
	}
	agents := h.agents(ctx)
	// A tokenless state is an in-flight legacy run only when this state records
	// the pane its bare agent is in. A bare name live anywhere else belongs to
	// another run, so adopting it would put two encounters on one agent.
	if legacyRunLive(st, agents, minions) {
		return nil
	}
	token, err := pickToken(agents, minions, gen)
	if err != nil {
		return err
	}
	st.Token = token
	return nil
}

// legacyRunLive reports whether a state with no token still owns the bare
// agents it sees. The pane record is the proof: a live bare agent at the pane
// this state recorded is this run's lane. Any other live bare agent is another
// run's.
func legacyRunLive(st *State, agents map[string]Agent, minions []Minion) bool {
	for _, m := range minions {
		pane := st.Minions[m.ID].Pane
		if pane == "" {
			continue
		}
		if a, live := agents[m.ID]; live && a.Pane == pane {
			return true
		}
	}
	return false
}

func spawnMinions(ctx context.Context, planPath string, p Plan, st *State, splitOverride string, out io.Writer) (spawnResult, error) {
	var res spawnResult
	h := newHerdrClient()
	if err := ensureToken(ctx, h, st, p.Minions, newToken); err != nil {
		return res, err
	}
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
		name := st.AgentName(m.ID)
		if a, live := agents[name]; live {
			st.SetMinion(m.ID, "spawned", "already live")
			st.PinPane(m.ID, a.Pane)
			res.Already = append(res.Already, m.ID)
			continue
		}

		// A reviewer reads its writer's worktree, so it starts there, not in
		// the base checkout. The accept command already runs in the same place.
		// The worktree must exist before the pane opens, so a reviewer declared
		// before its writer still gets a real cwd.
		cwd := minionDir(p, m)
		owner := ""
		switch {
		case writes(m.Role):
			owner = m.ID
		case m.Of != "":
			owner = m.Of
		}
		if owner != "" {
			if err := ensureWorktree(p.Repo, p.Base, p.Encounter, owner, cwd); err != nil {
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
		if err := h.startAgent(ctx, name, pane, laneArgs(m.Role, m.Tools, sessionDir)); err != nil {
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
