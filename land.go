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
	create := true
	if _, err := gitRun(repo, "rev-parse", "--verify", branch); err == nil {
		create = false
	}
	return cutWorktree(repo, base, branch, path, create)
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
	res.Worktree = worktreePath(p.Repo, p.Encounter, "integration")
	if err := ensureIntegrationWorktree(p.Repo, p.Base, res.Worktree, res.Branch); err != nil {
		return res, err
	}

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
		for _, u := range g.Units {
			res.Merged = append(res.Merged, u.ID)
		}
	}

	// The gate is each unit's own acceptance command, run against the merged
	// result. A unit that passed alone can still fail here, which is the point.
	for _, u := range units {
		if o.skipGate || u.Accept == "" {
			continue
		}
		cmdCtx, cancel := context.WithTimeout(ctx, o.acceptTimeout)
		cmd := exec.CommandContext(cmdCtx, "sh", "-c", u.Accept)
		cmd.Dir = res.Worktree
		out, err := cmd.CombinedOutput()
		cancel()
		res.Gate = append(res.Gate, gateResult{
			Unit: u.ID, Command: u.Accept, OK: err == nil, Receipt: lastLine(string(out)),
		})
		if err != nil {
			break
		}
	}
	return res, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(strings.TrimSpace(s), 80)
}

func landCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("land", stderr)
	var statePath string
	o, planPath := dispatchFlags(fs, &statePath)
	noGate := fs.Bool("no-gate", false, "merge only, do not run the acceptance commands")
	force := fs.Bool("force", false, "accept a plan edited after dispatch")
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		*planPath = rest[0]
	}
	p, code := loadPlanFor(fs, planPath, stderr)
	if code != 0 {
		return code
	}
	if statePath == "" {
		statePath = StatePath(*planPath)
	}
	st, err := LoadState(statePath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	o.planPath, o.statePath, o.out, o.skipGate, o.force = *planPath, statePath, stdout, *noGate, *force
	if code := o.guardPlan(&st, stderr); code != 0 {
		return code
	}

	res, err := land(ctx, *o, p, &st)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(stdout, res)
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
