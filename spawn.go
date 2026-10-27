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

	var root string
	for i, m := range p.Minions {
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

		ratio := 1.0 / float64(len(p.Minions)-i)
		pane, err := h.lanePane(ctx, p.Encounter, m.Pane, &root, cwd, ratio)
		if err != nil {
			return res, err
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
