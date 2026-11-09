package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// clear retires an encounter whose work is finished. The plan goes first and
// the run stays: a brief and a report are the record of what was done, so
// deleting them is a separate word, --remove, and deleting the lanes another,
// --remove-worktrees.

// runArtifacts is what a run writes beside its plan. The plan is not here:
// deleting the plan is the command, deleting these is --remove.
var runArtifacts = []string{"state.json", "state.json.tmp", "briefs", "reports", "sessions"}

// clearOpts is one sweep: how far to go, the fleet it judged against, and where
// to say what happened.
type clearOpts struct {
	remove    bool
	worktrees bool
	dryRun    bool
	agents    map[string]Agent // nil when the fleet could not be read
	why       string           // why agents is nil, empty for an empty fleet
	warned    bool             // the unreadable fleet has been named once
	out       io.Writer
	errOut    io.Writer
}

// clearOutcome is what one encounter did. lanes reports whether the plan owned
// a worktree at all, so the sweep only offers --remove-worktrees when a lane
// was left behind.
type clearOutcome struct {
	cleared bool
	kept    bool
	finding bool
	lanes   bool
}

func clearCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("clear", stderr)
	planPath := fs.String("plan", "", "clear one encounter")
	remove := fs.Bool("remove", false, "delete the state, the briefs, the reports and the sessions too")
	removeWorktrees := fs.Bool("remove-worktrees", false, "remove the lane worktrees and their branches too")
	dryRun := fs.Bool("dry-run", false, "say what would go, and touch nothing")
	backend := muxFlag(fs)
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		*planPath = rest[0]
	}
	explicit := *planPath != ""
	root, haveRoot := encounterRoot()

	// One sweep over every encounter the shell can see, so a finished run of
	// them takes one command. --plan picks one and reports like a single
	// command instead.
	var targets []string
	if explicit {
		targets = []string{*planPath}
	} else if haveRoot {
		for _, e := range discoverPlans() {
			targets = append(targets, e.Path)
		}
	}
	// Without --plan there has to be something to clear: a plan, or, when
	// --remove says so, the run a cleared plan left behind.
	if !explicit && len(targets) == 0 && !(*remove && haveRoot) {
		where := ".dungeon"
		if haveRoot {
			where = shortPath(root)
		}
		fmt.Fprintf(stderr, "dungeon: no encounters under %s\n", where)
		return 2
	}

	agents, why := probeFleet(ctx, *backend)
	o := &clearOpts{
		remove: *remove, worktrees: *removeWorktrees, dryRun: *dryRun,
		agents: agents, why: why, out: stdout, errOut: stderr,
	}

	var cleared, kept, findings, lanesKept int
	for _, path := range targets {
		res, code := o.clearPlan(path, explicit)
		if code != 0 {
			return code
		}
		if res.cleared {
			cleared++
		}
		if res.kept {
			kept++
		}
		if res.finding {
			findings++
		}
		if res.cleared && res.lanes && !o.worktrees {
			lanesKept++
		}
	}
	var removedRuns int
	if !explicit && *remove && haveRoot {
		removedRuns, kept, findings = o.sweepRuns(root, kept, findings)
	}

	if cleared == 0 && kept == 0 && findings == 0 && removedRuns == 0 {
		fmt.Fprintln(stdout, "nothing to clear")
	}
	if findings > 0 {
		fmt.Fprintf(stdout, "cleared %d, kept %d, %d to look at\n", cleared, kept, findings)
	}
	if removedRuns > 0 {
		word := "runs"
		if removedRuns == 1 {
			word = "run"
		}
		fmt.Fprintf(stdout, "removed %d %s left by cleared plans\n", removedRuns, word)
	}
	if o.dryRun && (cleared > 0 || removedRuns > 0) {
		fmt.Fprintln(stdout, "dry run: nothing was deleted")
	}
	if cleared > 0 && !o.remove && !o.dryRun {
		fmt.Fprintln(stdout, "the state, the briefs, the reports and the sessions stay; --remove deletes them with the plan")
	}
	if lanesKept > 0 && !o.dryRun {
		fmt.Fprintln(stdout, "the lane worktrees and their branches stay; --remove-worktrees deletes them")
	}
	if findings > 0 {
		return 1
	}
	return 0
}

// clearPlan clears one encounter: the lanes, then the plan, then the run. An
// explicit --plan turns a plan that cannot be judged into an exit code rather
// than a skipped line.
func (o *clearOpts) clearPlan(path string, explicit bool) (clearOutcome, int) {
	p, planFindings, err := loadPlan(path)
	if err != nil {
		if explicit {
			fmt.Fprintf(o.errOut, "dungeon: %v\n", err)
			return clearOutcome{}, 2
		}
		fmt.Fprintf(o.out, "skipped %s: %v\n", shortPath(path), err)
		return clearOutcome{finding: true}, 0
	}
	if len(planFindings) > 0 {
		if explicit {
			for _, f := range planFindings {
				fmt.Fprintln(o.errOut, f)
			}
			fmt.Fprintf(o.errOut, "dungeon: %d findings in %s\n", len(planFindings), path)
			return clearOutcome{}, 1
		}
		fmt.Fprintf(o.out, "skipped %s: %s\n", shortPath(path), planFindings[0])
		return clearOutcome{finding: true}, 0
	}
	st, err := LoadState(StatePath(path))
	if err != nil {
		fmt.Fprintf(o.out, "skipped %s: %v\n", shortPath(path), err)
		return clearOutcome{finding: true}, 0
	}
	lanes := planLanes(p)
	if ok, live, reason := finishedPlan(p, st, o.agents); !ok {
		fmt.Fprintf(o.out, "kept %s: %s\n", shortPath(path), reason)
		return clearOutcome{kept: true, finding: live, lanes: len(lanes) > 0}, 0
	}
	// An unreadable fleet cannot prove a lane is gone, so say once that the
	// units alone decided, rather than let silence look like proof.
	if o.agents == nil && !o.warned {
		fmt.Fprintf(o.errOut, "dungeon: the fleet could not be read (%s), so clear judged on the units alone\n", o.why)
		o.warned = true
	}
	res := clearOutcome{lanes: len(lanes) > 0}
	// The lanes go first: once the plan is gone nothing names the worktrees, and
	// a lane that cannot be removed keeps its plan.
	if o.worktrees && !o.removeLanes(p, lanes) {
		fmt.Fprintf(o.out, "kept %s: a lane could not be removed\n", shortPath(path))
		res.kept, res.finding = true, true
		return res, 0
	}
	if o.dryRun {
		fmt.Fprintf(o.out, "would clear %s (%s)\n", shortPath(path), p.Encounter)
		if o.remove {
			fmt.Fprintf(o.out, "would remove the run in %s\n", shortPath(encounterDir(path)))
		}
		res.cleared = true
		return res, 0
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(o.out, "could not clear %s: %v\n", shortPath(path), err)
		res.finding = true
		return res, 0
	}
	fmt.Fprintf(o.out, "cleared %s (%s)\n", shortPath(path), p.Encounter)
	res.cleared = true
	if o.remove {
		if err := removeRun(encounterDir(path)); err != nil {
			fmt.Fprintf(o.out, "could not remove the run in %s: %v\n", shortPath(encounterDir(path)), err)
			res.finding = true
		}
	}
	return res, 0
}

// finishedPlan reports whether an encounter has nothing left to do: every unit
// the plan declares reached an end state, and no lane of it is still live. live
// means the operator has something to close, not that the work is unfinished.
func finishedPlan(p Plan, st State, agents map[string]Agent) (ok, live bool, why string) {
	if len(p.Units) == 0 {
		return false, false, "no units, so nothing was done"
	}
	var open []string
	for _, u := range p.Units {
		if s := st.UnitStatus(u.ID); s != "done" && s != "canceled" {
			open = append(open, u.ID+" "+s)
		}
	}
	if len(open) > 0 {
		word := "units"
		if len(open) == 1 {
			word = "unit"
		}
		return false, false, fmt.Sprintf("%d %s not settled: %s",
			len(open), word, truncate(strings.Join(open, ", "), 80))
	}
	for _, m := range p.Minions {
		if a, found := agents[st.AgentName(m.ID)]; found {
			return false, true, fmt.Sprintf("lane %s is still live in %s; run dungeon down first", m.ID, a.Pane)
		}
	}
	return true, false, ""
}

// planLanes names the worktrees a plan owns: one per writer, and the writer's
// lane for a reviewer that reads it. A scout owns none.
func planLanes(p Plan) []string {
	seen := map[string]bool{}
	var ids []string
	for _, m := range p.Minions {
		id := m.ID
		if !writes(m.Role) {
			id = m.Of
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// removeLanes deletes the lane worktrees and their branches. Only an explicit
// --remove-worktrees does this: a lane branch can hold work that never landed,
// so deleting one is never implied by deleting the plan.
func (o *clearOpts) removeLanes(p Plan, lanes []string) bool {
	ok := true
	for _, id := range lanes {
		branch := p.Encounter + "/" + id
		path := lanePath(p, id)
		label := branch + " at " + shortPath(path)
		_, statErr := os.Stat(path)
		hasPath, hasBranch := statErr == nil, branchExists(p.Repo, branch)
		switch {
		case !hasPath && !hasBranch:
			continue // the lane was never cut, or it is already gone
		case o.dryRun:
			fmt.Fprintf(o.out, "would remove lane %s\n", label)
			continue
		}
		if hasPath {
			if _, err := gitRun(p.Repo, "worktree", "remove", "--force", path); err != nil {
				fmt.Fprintf(o.out, "could not remove lane %s: %v\n", label, err)
				ok = false
				continue
			}
		}
		if hasBranch {
			if _, err := gitRun(p.Repo, "branch", "-D", branch); err != nil {
				fmt.Fprintf(o.out, "could not delete branch %s: %v\n", branch, err)
				ok = false
				continue
			}
		}
		fmt.Fprintf(o.out, "removed lane %s\n", label)
	}
	if ok && !o.dryRun {
		// A lane whose directory is already gone still holds a worktree entry.
		_, _ = gitRun(p.Repo, "worktree", "prune")
	}
	return ok
}

// sweepRuns removes the runs that cleared plans left behind: a directory under
// the encounter root that holds a run and no plan.yaml.
func (o *clearOpts) sweepRuns(root string, kept, findings int) (removed, keptOut, findingsOut int) {
	for _, dir := range orphanRuns(root) {
		ok, live, why := settledRun(dir, o.agents)
		if !ok {
			fmt.Fprintf(o.out, "kept the run in %s: %s\n", shortPath(dir), why)
			kept++
			if live {
				findings++
			}
			continue
		}
		if o.dryRun {
			fmt.Fprintf(o.out, "would remove the run in %s\n", shortPath(dir))
			removed++
			continue
		}
		if err := removeRun(dir); err != nil {
			fmt.Fprintf(o.out, "could not remove the run in %s: %v\n", shortPath(dir), err)
			findings++
			continue
		}
		fmt.Fprintf(o.out, "removed the run in %s\n", shortPath(dir))
		removed++
	}
	return removed, kept, findings
}

// orphanRuns lists the run directories no plan claims: the encounter root
// itself when its plan is gone, and every subdirectory without a plan.yaml that
// still holds what a run wrote.
func orphanRuns(root string) []string {
	var out []string
	add := func(dir string) {
		if _, err := os.Stat(filepath.Join(dir, "plan.yaml")); err == nil {
			return // a plan claims this directory
		}
		if hasRun(dir) {
			out = append(out, dir)
		}
	}
	add(root)
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			add(filepath.Join(root, e.Name()))
		}
	}
	return out
}

// hasRun reports whether a directory holds anything a run wrote.
func hasRun(dir string) bool {
	for _, name := range runArtifacts {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// settledRun judges a run whose plan is gone, from its state alone. A run that
// cannot prove it settled stays: the briefs and the reports are the only
// account of what was done.
func settledRun(dir string, agents map[string]Agent) (ok, live bool, why string) {
	statePath := filepath.Join(dir, "state.json")
	if _, err := os.Stat(statePath); err != nil {
		return false, false, "no state.json, so clear cannot tell what happened"
	}
	st, err := LoadState(statePath)
	if err != nil {
		return false, false, "state.json does not read"
	}
	var open []string
	for id := range st.Units {
		if s := st.UnitStatus(id); s != "done" && s != "canceled" {
			open = append(open, id+" "+s)
		}
	}
	if len(open) > 0 {
		sort.Strings(open)
		word := "units"
		if len(open) == 1 {
			word = "unit"
		}
		return false, false, fmt.Sprintf("%d %s not settled: %s",
			len(open), word, truncate(strings.Join(open, ", "), 80))
	}
	for id := range st.Minions {
		if a, found := agents[st.AgentName(id)]; found {
			return false, true, fmt.Sprintf("lane %s is still live in %s", id, a.Pane)
		}
	}
	return true, false, ""
}

// removeRun deletes what a run wrote in dir. The directory goes only when it is
// empty: the default plan shares .dungeon with every named encounter, so a
// blind RemoveAll would take the neighbours with it.
func removeRun(dir string) error {
	for _, name := range runArtifacts {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		return os.Remove(dir)
	}
	return nil
}
