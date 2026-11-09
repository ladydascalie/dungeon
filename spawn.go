package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// laneCut is what up learned about a lane's branch when it cut or reused the
// worktree. The facts go into the state and the brief, so the agent driving
// the lane sees the branch it inherited before it writes.
type laneCut struct {
	Path      string
	Branch    string
	Head      string
	Subject   string
	Ahead     int
	Dirty     int
	Resumed   bool
	Recreated bool
}

func ensureWorktree(repo, base, encounter, minion, path string) (laneCut, error) {
	branch := encounter + "/" + minion
	lane := laneCut{Path: path, Branch: branch}
	if _, err := os.Stat(path); err == nil {
		fillLaneFacts(repo, base, &lane)
		return lane, nil
	}
	create := true
	if branchExists(repo, branch) {
		lane.Ahead = commitsAhead(repo, base, branch)
		if lane.Ahead == 0 {
			// No unique commits: the branch holds no work that base lacks, so
			// replace it and start from base.
			if _, err := gitRun(repo, "branch", "-D", branch); err != nil {
				return lane, err
			}
			lane.Recreated = true
		} else {
			lane.Resumed = true
			create = false
		}
	}
	if err := cutWorktree(repo, base, branch, path, create); err != nil {
		return lane, err
	}
	fillLaneFacts(repo, base, &lane)
	return lane, nil
}

func branchExists(repo, branch string) bool {
	_, err := gitRun(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func commitsAhead(repo, base, branch string) int {
	out, err := gitRun(repo, "rev-list", "--count", base+"..refs/heads/"+branch)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}

func fillLaneFacts(repo, base string, lane *laneCut) {
	lane.Head, _ = gitRun(repo, "rev-parse", "--short", "refs/heads/"+lane.Branch)
	lane.Subject, _ = gitRun(repo, "log", "-1", "--format=%s", "refs/heads/"+lane.Branch)
	lane.Ahead = commitsAhead(repo, base, lane.Branch)
	if files, err := changedFiles(lane.Path); err == nil {
		lane.Dirty = len(files)
	}
}

// cutWorktree cuts a worktree at path. Worktrunk does the work when it is
// installed, so its hooks and ignored-file copies run. The path override pins
// the lane where worktreePath put it. Plain git is the fallback, and only when
// wt is missing.
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

// worktreePath uses the project's worktree convention, ../<repo>.worktrees/<name>,
// so dungeon's lanes sit where worktrunk puts every other worktree. A worktree
// nested in its own repository confuses every tool that walks the tree.
// This is the creation path, not proof of where an existing lane is. lanePath
// asks worktrunk or git first, then falls back to this convention.
func worktreePath(repo, encounter, minion string) string {
	root := filepath.Base(repo) + ".worktrees"
	return filepath.Join(filepath.Dir(repo), root, encounter+"-"+minion)
}

// ensureToken gives the run its own agent names on the first up. The state is
// saved by the caller, so the token survives the spawn and every later command
// reuses it.
func ensureToken(ctx context.Context, h mux, st *State, minions []Minion, gen func() string) error {
	if st.Token != "" {
		return nil
	}
	agents, err := h.Agents(ctx)
	if err != nil {
		// Without the live fleet a token could collide with a running
		// encounter, so the run waits for a readable backend.
		return err
	}
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

// spawnInput is one up: which encounter, what it looks like and where to say
// what happened.
type spawnInput struct {
	planPath string
	plan     Plan
	state    *State
	split    string
	out      io.Writer
}

func spawnMinions(ctx context.Context, h mux, in spawnInput) (spawnResult, error) {
	var res spawnResult
	planPath, p, st := in.planPath, in.plan, in.state
	if err := ensureToken(ctx, h, st, p.Minions, newToken); err != nil {
		return res, err
	}
	agents, err := h.Agents(ctx)
	if err != nil {
		return res, err
	}

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
	if in.split != "" {
		direction = in.split
	}

	ws := findWorkspace(ctx, h, p.Encounter)
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
			id, tabID, root, err := h.CreateWorkspace(ctx, p.Encounter, label, cwd)
			if err != nil {
				return nil, err
			}
			if err := tagProject(ctx, h, id, p); err != nil {
				return nil, err
			}
			ws, t.tab, t.root = id, tabID, root
		} else {
			tabID, root, err := h.CreateTab(ctx, ws, label, cwd)
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
			lane, err := ensureWorktree(p.Repo, p.Base, p.Encounter, owner, cwd)
			if err != nil {
				res.Failed = append(res.Failed, m.ID)
				fmt.Fprintf(in.out, "%s: %v\n", m.ID, err)
				continue
			}
			st.SetLane(owner, lane)
			if lane.Resumed {
				fmt.Fprintf(in.out, "%s: resumed %s at %s, ahead of %s: %d, uncommitted files: %d, %s\n",
					owner, lane.Branch, lane.Head, p.Base, lane.Ahead, lane.Dirty, lane.Path)
			}
			if lane.Recreated {
				fmt.Fprintf(in.out, "%s: recreated %s from %s, old branch had no unique commits, %s\n",
					owner, lane.Branch, p.Base, lane.Path)
			}
		}

		var pane string
		if m.Pane != "" && paneExists(ctx, h, m.Pane) {
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
			fmt.Fprintf(in.out, "%s: pane %s is gone, using %s\n", m.ID, m.Pane, pane)
		}

		sessionDir := filepath.Join(dir, "sessions", m.ID)
		if err := os.MkdirAll(sessionDir, 0o755); err != nil {
			return res, err
		}
		if err := h.StartAgent(ctx, name, pane, laneArgs(m.Role, m.Tools, sessionDir)); err != nil {
			res.Failed = append(res.Failed, m.ID)
			fmt.Fprintf(in.out, "%s: %v\n", m.ID, err)
			continue
		}
		st.SetMinion(m.ID, "spawned", "")
		st.PinPane(m.ID, pane)
		res.Spawned = append(res.Spawned, m.ID)
	}
	return res, nil
}
