package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// land merges what is done, in declaration order, into one integration branch,
// then runs every merged unit's acceptance command against the result. It never
// touches the base branch: landing produces a branch and a verdict, and the
// operator decides what to do with them.

type landResult struct {
	Branch   string
	Worktree string
	Merged   []string
	Gate     []gateResult
	Conflict string
	Reason   string
	// Held is set when a gate was cut short by an interrupt rather than failing
	// on its own. Nothing is claimed about that unit, so no gate is recorded.
	Held bool
}

type gateResult struct {
	Unit    string
	Command string
	OK      bool
	Receipt string
}

func (r landResult) String() string {
	var b strings.Builder
	if r.Conflict != "" {
		fmt.Fprintf(&b, "conflict landing %s: %s\n", r.Conflict, r.Reason)
		fmt.Fprintf(&b, "merged before the conflict: %s\n", orNone(r.Merged))
		return b.String()
	}
	if len(r.Merged) == 0 {
		b.WriteString("nothing to land\n")
		return b.String()
	}
	fmt.Fprintf(&b, "merged %d units on %s in %s\n", len(r.Merged), r.Branch, r.Worktree)
	fmt.Fprintf(&b, "  %s\n", strings.Join(r.Merged, " "))
	for _, g := range r.Gate {
		mark := "ok  "
		if !g.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  %s %s: %s (%s)\n", mark, g.Unit, g.Command, g.Receipt)
	}
	return b.String()
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, " ")
}

func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func ensureIntegrationWorktree(repo, base, path, branch string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return cutWorktree(repo, base, branch, path, !branchExists(repo, branch))
}

// landable returns the done units that produced code: a staff unit has a
// verdict, not a branch, so there is nothing to merge for it.
func landable(p Plan, st *State) []Unit {
	var out []Unit
	for _, u := range p.Units {
		if st.UnitStatus(u.ID) != "done" {
			continue
		}
		m, ok := findMinion(p, u.Minion)
		if !ok || !writes(m.Role) {
			continue
		}
		out = append(out, u)
	}
	return out
}

// landGroup is every done unit a minion produced. One minion is one branch, so
// it merges once however many units it finished.
type landGroup struct {
	Minion string
	Units  []Unit
}

// groupByMinion keeps the declaration order of each minion's first done unit,
// so the merge order matches the plan rather than the map.
func groupByMinion(units []Unit) []landGroup {
	var groups []landGroup
	at := map[string]int{}
	for _, u := range units {
		i, ok := at[u.Minion]
		if !ok {
			i = len(groups)
			at[u.Minion] = i
			groups = append(groups, landGroup{Minion: u.Minion})
		}
		groups[i].Units = append(groups[i].Units, u)
	}
	return groups
}

func land(ctx context.Context, o dispatchOpts, p Plan, st *State) (landResult, error) {
	var res landResult
	units := landable(p, st)
	if len(units) == 0 {
		return res, nil
	}
	res.Branch = p.Encounter + "/integration"
	res.Worktree = lanePath(p, "integration")
	if err := ensureIntegrationWorktree(p.Repo, p.Base, res.Worktree, res.Branch); err != nil {
		return res, err
	}

	commit := map[string]string{} // the merge each unit landed at
	for _, g := range groupByMinion(units) {
		branch := p.Encounter + "/" + g.Minion
		ids := make([]string, 0, len(g.Units))
		for _, u := range g.Units {
			ids = append(ids, u.ID)
		}
		if _, err := gitRun(res.Worktree, "merge", "--no-ff", branch, "-m",
			fmt.Sprintf("land %s: %s", strings.Join(ids, " "), firstLine(g.Units[0].Goal))); err != nil {
			res.Conflict, res.Reason = g.Units[0].ID, lastLine(err.Error())
			gitRun(res.Worktree, "merge", "--abort")
			return res, nil
		}
		head, _ := gitRun(res.Worktree, "rev-parse", "--short", "HEAD")
		for _, u := range g.Units {
			res.Merged = append(res.Merged, u.ID)
			commit[u.ID] = head
		}
	}

	// The gate is each unit's own acceptance command, run against the merged
	// result. A unit that passed alone can still fail here, which is the point.
	for _, u := range units {
		if o.skipGate || u.Accept == "" {
			continue
		}
		cmdCtx, cancel := acceptContext(ctx, o)
		out, err := acceptCommand(cmdCtx, res.Worktree, u.Accept)
		// The cause must be read before cancel, or every failed gate would look
		// like an interrupted one.
		held := err != nil && acceptCutShort(cmdCtx, o)
		cancel()
		if held {
			// The operator interrupted the sweep and the gate was killed with
			// it. That is not a failed gate: nothing is judged and the sweep
			// stops where it is.
			res.Held = true
			break
		}
		res.Gate = append(res.Gate, gateResult{
			Unit: u.ID, Command: u.Accept, OK: err == nil, Receipt: lastLine(string(out)),
		})
		if err != nil {
			break
		}
	}

	// A unit is landed when its gate passed, it has no gate, or the gate was
	// skipped. A failed gate leaves that unit and every unit after it unlanded,
	// so land exits 1 and claims nothing about them.
	passed := map[string]bool{}
	for _, g := range res.Gate {
		passed[g.Unit] = g.OK
	}
	for _, u := range units {
		switch {
		case o.skipGate || u.Accept == "":
		case passed[u.ID]:
		default:
			return res, nil
		}
		st.SetLanded(u.ID, res.Branch, commit[u.ID])
	}
	return res, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(strings.TrimSpace(s), 80)
}

// landFlags is every flag land takes. One builder feeds the router, landCmd and
// landAllCmd: a router that keeps its own, narrower copy silently drops --all
// as soon as any flag it does not know is present, which is the drift this
// builder exists to remove.
type landFlags struct {
	opts      *dispatchOpts
	planPath  *string
	statePath string
	noGate    *bool
	force     *bool
	all       *bool
	branch    *string
}

func landFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *landFlags) {
	fs := newFlagSet(name, stderr)
	lf := &landFlags{}
	lf.opts, lf.planPath = dispatchFlags(fs, &lf.statePath)
	lf.noGate = fs.Bool("no-gate", false, "merge only, do not run the acceptance commands")
	lf.force = fs.Bool("force", false, "accept a plan edited after dispatch")
	lf.all = fs.Bool("all", false, "land every encounter")
	lf.branch = fs.String("branch", defaultFleetBranch, "the branch that combines the plans, with --all")
	return fs, lf
}

// branchSet reports whether --branch was passed. The flag set is shared with
// land --all, so the single-plan path must ask rather than compare a default.
func branchSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "branch" {
			set = true
		}
	})
	return set
}

func landCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, lf := landFlagSet("land", stderr)
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		*lf.planPath = rest[0]
	}
	if *lf.all {
		// The router sends a sweep to landAllCmd. Reaching here with --all means
		// something routed wrong: hand it back rather than ignore the flag and
		// land one plan as if that was what was asked.
		return landAllCmd(ctx, args, stdout, stderr)
	}
	if branchSet(fs) {
		// The flag set is shared with land --all, so this path parses --branch
		// whether or not it can use it. Say so instead of dropping it: an
		// operator who named a branch must not believe it was honoured.
		fmt.Fprintf(stderr, "dungeon: --branch %s belongs to land --all\n", *lf.branch)
		fmt.Fprintln(stderr, "dungeon: one land merges onto <encounter>/integration; pass --all to name the combined branch")
		return 2
	}
	p, code := loadPlanFor(fs, lf.planPath, stderr)
	if code != 0 {
		return code
	}
	statePath := lf.statePath
	if statePath == "" {
		statePath = StatePath(*lf.planPath)
	}
	st, err := LoadState(statePath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	o := *lf.opts
	o.planPath, o.statePath, o.out, o.skipGate, o.force = *lf.planPath, statePath, stdout, *lf.noGate, *lf.force
	if code := o.guardPlan(&st, stderr); code != 0 {
		return code
	}

	res, err := land(ctx, o, p, &st)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	// land records what landed, so the state must be written even when a
	// conflict or a failed gate stopped it early.
	if err := SaveState(statePath, st); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(stdout, res)
	if res.Held {
		// The operator interrupted a gate. Nothing was judged, so this is not a
		// pass and not a failure: say it was interrupted and claim no landing.
		fmt.Fprintln(stdout, stopLine(ctx, "dungeon land"))
		return 2
	}
	if res.Conflict != "" {
		return 1
	}
	for _, g := range res.Gate {
		if !g.OK {
			return 1
		}
	}
	if len(res.Merged) > 0 {
		abs, _ := filepath.Abs(res.Worktree)
		fmt.Fprintf(stdout, "\nthe branch is local. nothing was pushed. inspect it at %s\n", abs)
	}
	return 0
}
