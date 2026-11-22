package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A fleet sweep is every encounter discoverPlans finds, driven one after the
// other through the same step a single next runs. The point of this file is
// that there is no second copy of the dispatch rules: a fleet that dispatched
// by its own rules would be a second scheduler, and the two would drift.

// defaultFleetBranch is the branch land --all combines the plans on.
const defaultFleetBranch = "fleet/integration"

// fleetTarget is one encounter in a sweep: its plan, its state, and where both
// live. A sweep is a list of targets, never a merged plan.
type fleetTarget struct {
	PlanPath  string
	StatePath string
	Plan      Plan
	State     State
}

func (t *fleetTarget) encounter() string { return t.Plan.Encounter }

// fleetOpts is what a sweep refuses: --plan names one encounter, --state names
// one state file, and a sweep is every encounter with its own state.
type fleetOpts struct {
	planPath  string
	statePath string
	stderr    io.Writer
}

// refusal is the --all combination that cannot be honoured, or "".
func (f fleetOpts) refusal() string {
	switch {
	case f.planPath != "":
		return "--all sweeps every encounter, so --plan does not apply"
	case f.statePath != "":
		return "--all keeps one state per encounter, so --state does not apply"
	}
	return ""
}

// fleetTargets loads every encounter and applies the same guards the
// single-plan path applies: the plan must parse and validate, the state must
// hold this encounter, and the plan must not have changed since dispatch unless
// force accepts the edit. A plan that fails a guard is printed as skipped and
// left out, because a fleet is never dispatched from a half-loaded state.
func fleetTargets(force bool, out io.Writer) []*fleetTarget {
	var targets []*fleetTarget
	for _, e := range discoverPlans() {
		t, reason := loadFleetTarget(e.Path, force)
		if reason != "" {
			fmt.Fprintf(out, "skipped %s: %s\n", shortPath(e.Path), reason)
			continue
		}
		targets = append(targets, t)
	}
	return targets
}

// loadFleetTarget loads one encounter. It returns the reason it was left out
// instead of a target, so every guard fails the same way.
func loadFleetTarget(planPath string, force bool) (*fleetTarget, string) {
	p, findings, err := loadPlan(planPath)
	if err != nil {
		return nil, err.Error()
	}
	if len(findings) > 0 {
		return nil, findings[0].String()
	}
	statePath := StatePath(planPath)
	st, err := LoadState(statePath)
	if err != nil {
		return nil, err.Error()
	}
	if st.Encounter == "" {
		st.Encounter = p.Encounter
	}
	if st.Encounter != p.Encounter {
		return nil, fmt.Sprintf("state holds encounter %q, the plan is %q", st.Encounter, p.Encounter)
	}
	h, err := PlanHash(planPath)
	if err != nil {
		return nil, err.Error()
	}
	if st.PlanHash != "" && st.PlanHash != h && !force {
		return nil, fmt.Sprintf("plan changed since dispatch (%s, dispatched %s); re-run dungeon up --all, or pass --force",
			shortHash(h), shortHash(st.PlanHash))
	}
	if st.PlanHash != h {
		st.PlanHash = h
		if err := SaveState(statePath, st); err != nil {
			return nil, err.Error()
		}
	}
	return &fleetTarget{PlanPath: planPath, StatePath: statePath, Plan: p, State: st}, ""
}

// noTargets refuses an empty sweep: a sweep of nothing could not run.
func noTargets(stderr io.Writer) int {
	fmt.Fprintln(stderr, "dungeon: no encounter to sweep: nothing under .dungeon parses and validates")
	return 2
}

// fleetDigest is one block per plan, in discovery order. Each block carries the
// same lines a single next prints, so a sweep reads like N encounters rather
// than a new kind of report.
type fleetDigest struct {
	Blocks []fleetBlock
}

type fleetBlock struct {
	Encounter string
	Digest    digest
}

func (f fleetDigest) String() string {
	var b strings.Builder
	for _, blk := range f.Blocks {
		fmt.Fprintf(&b, "%s\n", blk.Encounter)
		for _, line := range resultLines(blk.Digest.String()) {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
}

// resultLines splits a rendered result so each line can carry a header.
func resultLines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// nextAll drives every target until no target has a working unit. One mux, one
// deadline and one poll cover the whole sweep: a fleet does not get one timeout
// per plan.
func nextAll(ctx context.Context, h mux, o dispatchOpts, targets []*fleetTarget) (fleetDigest, error) {
	var fd fleetDigest
	if o.parentPane == "" {
		o.parentPane = h.SelfPane(ctx)
	}
	deadline := time.Now().Add(o.timeout)
	digests := make([]digest, len(targets))

	for {
		working := 0
		for i, t := range targets {
			to := o
			to.planPath, to.statePath = t.PlanPath, t.StatePath
			before, err := json.Marshal(t.State)
			if err != nil {
				return fd, err
			}
			if _, err := step(ctx, h, to, t.Plan, &t.State, &digests[i]); err != nil {
				return fd, err
			}
			// A state that moved is written as soon as its plan's pass ends, so
			// a crash mid-sweep loses nothing.
			after, err := json.Marshal(t.State)
			if err != nil {
				return fd, err
			}
			if !bytes.Equal(before, after) {
				if err := SaveState(t.StatePath, t.State); err != nil {
					return fd, err
				}
			}
			if workingCount(t.Plan, &t.State) > 0 {
				working++
			}
		}
		if o.dryRun || working == 0 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(o.out, "timeout: lanes still working, run dungeon next --all again")
			break
		}
		time.Sleep(o.poll)
	}

	for i, t := range targets {
		fd.Blocks = append(fd.Blocks, fleetBlock{Encounter: t.encounter(), Digest: digests[i]})
	}
	return fd, nil
}

// nextAllCmd sweeps every encounter. The mux is opened once for the whole
// sweep, not once per plan.
func nextAllCmd(ctx context.Context, o dispatchOpts, flags fleetOpts) int {
	if msg := flags.refusal(); msg != "" {
		fmt.Fprintf(flags.stderr, "dungeon: %s\n", msg)
		return 2
	}
	targets := fleetTargets(o.force, o.out)
	if len(targets) == 0 {
		return noTargets(flags.stderr)
	}
	h, err := openMux(ctx, o.backend)
	if err != nil {
		fmt.Fprintf(flags.stderr, "dungeon: %v\n", err)
		return 2
	}
	d, err := nextAll(ctx, h, o, targets)
	if err != nil {
		fmt.Fprintf(flags.stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(o.out, d)
	return 0
}

// upAllInput is one up --all: the dispatch options, the flags it refuses, and
// what the operator asked for.
type upAllInput struct {
	opts       dispatchOpts
	flags      fleetOpts
	split      string
	noDispatch bool
}

// upAllCmd spawns every encounter's minions, one workspace per encounter, then
// runs one next --all round unless the operator said not to or a spawn failed.
func upAllCmd(ctx context.Context, in upAllInput) int {
	if msg := in.flags.refusal(); msg != "" {
		fmt.Fprintf(in.flags.stderr, "dungeon: %s\n", msg)
		return 2
	}
	targets := fleetTargets(in.opts.force, in.opts.out)
	if len(targets) == 0 {
		return noTargets(in.flags.stderr)
	}
	h, err := openMux(ctx, in.opts.backend)
	if err != nil {
		fmt.Fprintf(in.flags.stderr, "dungeon: %v\n", err)
		return 2
	}
	failed := false
	for _, t := range targets {
		res, err := spawnMinions(ctx, h, spawnInput{
			planPath: t.PlanPath, plan: t.Plan, state: &t.State, split: in.split, out: in.opts.out,
		})
		if err != nil {
			fmt.Fprintf(in.flags.stderr, "dungeon: %s: %v\n", t.encounter(), err)
			return 2
		}
		if err := SaveState(t.StatePath, t.State); err != nil {
			fmt.Fprintf(in.flags.stderr, "dungeon: %v\n", err)
			return 2
		}
		fmt.Fprintf(in.opts.out, "%s: %s\n", t.encounter(), res)
		if len(res.Failed) > 0 {
			failed = true
		}
	}
	if in.noDispatch || failed {
		return 0
	}
	d, err := nextAll(ctx, h, in.opts, targets)
	if err != nil {
		fmt.Fprintf(in.flags.stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(in.opts.out, d)
	return 0
}

// fleetLand is what one land --all did: one result per plan, and the combined
// branch when two or more plans put work on it.
type fleetLand struct {
	Plans    []fleetLandPlan
	Branch   string
	Worktree string
	Merged   []string
	Gate     []gateResult
	Conflict string
	Reason   string
}

type fleetLandPlan struct {
	Encounter string
	Result    landResult
}

func (r fleetLand) String() string {
	var b strings.Builder
	for _, p := range r.Plans {
		for _, line := range resultLines(p.Result.String()) {
			fmt.Fprintf(&b, "%s: %s\n", p.Encounter, line)
		}
	}
	if r.Conflict != "" {
		fmt.Fprintf(&b, "conflict landing %s on %s: %s\n", r.Conflict, r.Branch, r.Reason)
		fmt.Fprintf(&b, "%s is not good: merged before the conflict: %s\n", r.Branch, orNone(r.Merged))
		return b.String()
	}
	if r.Branch == "" {
		return b.String()
	}
	fmt.Fprintf(&b, "fleet branch %s in %s\n", r.Branch, r.Worktree)
	fmt.Fprintf(&b, "  merged: %s\n", orNone(r.Merged))
	for _, g := range r.Gate {
		mark := "ok  "
		if !g.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  %s %s: %s (%s)\n", mark, g.Unit, g.Command, g.Receipt)
	}
	return b.String()
}

// fleetSource is one plan's integration branch on its way into the combined
// branch: the branch, the units that went into it and the plan they came from.
type fleetSource struct {
	Encounter string
	PlanPath  string
	Branch    string
	Units     []string
	Plan      Plan
}

// landAll lands each plan exactly as land does, then combines the plans that
// merged something on one branch, in sorted plan-path order so the combined
// branch is reproducible, and runs every merged unit's accept against it.
//
// A plan that conflicted or whose gate failed stops the sweep before the
// combined branch: half a plan on the fleet branch would be a lie about what
// the fleet proved.
func landAll(ctx context.Context, o dispatchOpts, targets []*fleetTarget, branch string) (fleetLand, error) {
	var res fleetLand
	var sources []fleetSource
	for _, t := range targets {
		to := o
		to.planPath, to.statePath = t.PlanPath, t.StatePath
		r, err := land(ctx, to, t.Plan, &t.State)
		if err != nil {
			return res, err
		}
		if err := SaveState(t.StatePath, t.State); err != nil {
			return res, err
		}
		res.Plans = append(res.Plans, fleetLandPlan{Encounter: t.encounter(), Result: r})
		if r.Conflict != "" || !gatesPassed(r) {
			return res, nil
		}
		if len(r.Merged) > 0 {
			sources = append(sources, fleetSource{
				Encounter: t.encounter(),
				PlanPath:  t.PlanPath,
				Branch:    t.encounter() + "/integration",
				Units:     r.Merged,
				Plan:      t.Plan,
			})
		}
	}
	if len(sources) < 2 {
		// One plan's integration branch is the whole result, so there is
		// nothing to combine and nothing new to prove.
		return res, nil
	}

	sort.SliceStable(sources, func(i, j int) bool { return sources[i].PlanPath < sources[j].PlanPath })
	repo, base := sources[0].Plan.Repo, sources[0].Plan.Base
	res.Branch = branch
	res.Worktree = branchWorktreePath(repo, branch)
	if err := ensureIntegrationWorktree(repo, base, res.Worktree, res.Branch); err != nil {
		return res, err
	}
	for _, src := range sources {
		ids := make([]string, 0, len(src.Units))
		for _, id := range src.Units {
			ids = append(ids, src.Encounter+"/"+id)
		}
		if _, err := gitRun(res.Worktree, "merge", "--no-ff", src.Branch, "-m",
			"fleet: land "+strings.Join(ids, " ")); err != nil {
			res.Conflict, res.Reason = src.Encounter, lastLine(err.Error())
			gitRun(res.Worktree, "merge", "--abort")
			return res, nil
		}
		res.Merged = append(res.Merged, ids...)
	}

	// The gate is each unit's own accept command, run against the combined
	// tree. A unit that passed on its plan's integration branch can still fail
	// here, which is the point of combining before believing anything.
	if !o.skipGate {
		for _, src := range sources {
			for _, id := range src.Units {
				u, ok := findUnit(src.Plan, id)
				if !ok || u.Accept == "" {
					continue
				}
				cmdCtx, cancel := context.WithTimeout(ctx, o.acceptTimeout)
				cmd := exec.CommandContext(cmdCtx, "sh", "-c", u.Accept)
				cmd.Dir = res.Worktree
				out, err := cmd.CombinedOutput()
				cancel()
				res.Gate = append(res.Gate, gateResult{
					Unit: src.Encounter + "/" + id, Command: u.Accept, OK: err == nil, Receipt: lastLine(string(out)),
				})
			}
		}
	}
	return res, nil
}

// gatesPassed reports whether a plan's own land gates all passed. A plan with
// no gate at all passed.
func gatesPassed(r landResult) bool {
	for _, g := range r.Gate {
		if !g.OK {
			return false
		}
	}
	return true
}

// branchWorktreePath is where a branch's worktree sits under the project's
// convention, the same place a lane branch goes: the slash is sanitized, never
// nested, so no worktree lives inside another.
func branchWorktreePath(repo, branch string) string {
	root := filepath.Base(repo) + ".worktrees"
	return filepath.Join(filepath.Dir(repo), root, strings.ReplaceAll(branch, "/", "-"))
}

// fleetLandGuard refuses a sweep land cannot honour: land merges branches in one
// checkout, so every plan must share one repo root and one base, and the
// combined branch must not be a plan's own integration branch.
func fleetLandGuard(targets []*fleetTarget, branch string, stderr io.Writer) int {
	first := targets[0]
	for _, t := range targets[1:] {
		if absPath(first.Plan.Repo) != absPath(t.Plan.Repo) {
			fmt.Fprintf(stderr, "dungeon: land merges branches in one checkout, but %s is in %s and %s is in %s\n",
				first.encounter(), first.Plan.Repo, t.encounter(), t.Plan.Repo)
			fmt.Fprintln(stderr, "dungeon: --all needs one repo root and one base; land one encounter with --plan instead")
			return 2
		}
		if first.Plan.Base != t.Plan.Base {
			fmt.Fprintf(stderr, "dungeon: %s lands on %s and %s lands on %s\n",
				first.encounter(), first.Plan.Base, t.encounter(), t.Plan.Base)
			fmt.Fprintln(stderr, "dungeon: --all needs one repo root and one base; land one encounter with --plan instead")
			return 2
		}
	}
	for _, t := range targets {
		if branch == t.Plan.Encounter+"/integration" {
			fmt.Fprintf(stderr, "dungeon: --branch %s is %s's own integration branch\n", branch, t.encounter())
			fmt.Fprintln(stderr, "dungeon: pass --branch <name> to name the combined branch something else")
			return 2
		}
	}
	return 0
}

// landAllCmd lands every encounter and, when two or more merged something,
// combines them on one branch.
func landAllCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("land", stderr)
	var statePath string
	o, planPath := dispatchFlags(fs, &statePath)
	noGate := fs.Bool("no-gate", false, "merge only, do not run the acceptance commands")
	force := fs.Bool("force", false, "accept a plan edited after dispatch")
	fs.Bool("all", false, "land every encounter")
	branch := fs.String("branch", defaultFleetBranch, "the branch that combines the plans")
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		*planPath = rest[0]
	}
	o.skipGate, o.force, o.out = *noGate, *force, stdout
	flags := fleetOpts{planPath: *planPath, statePath: statePath, stderr: stderr}
	if msg := flags.refusal(); msg != "" {
		fmt.Fprintf(stderr, "dungeon: %s\n", msg)
		return 2
	}
	targets := fleetTargets(o.force, stdout)
	if len(targets) == 0 {
		return noTargets(stderr)
	}
	if code := fleetLandGuard(targets, *branch, stderr); code != 0 {
		return code
	}
	res, err := landAll(ctx, *o, targets, *branch)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(stdout, res)
	if res.Branch != "" && res.Conflict == "" {
		abs, _ := filepath.Abs(res.Worktree)
		fmt.Fprintf(stdout, "\nthe branch is local. nothing was pushed. inspect it at %s\n", abs)
	}
	if res.Conflict != "" {
		return 1
	}
	for _, p := range res.Plans {
		if p.Result.Conflict != "" || !gatesPassed(p.Result) {
			return 1
		}
	}
	for _, g := range res.Gate {
		if !g.OK {
			return 1
		}
	}
	return 0
}

// landOrFleet keeps land.go in charge of the single-plan path: land --all goes
// to the fleet engine, and a bare land over many encounters names --all before
// land.go refuses without knowing about it.
func landOrFleet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// The parse here only decides which path to take. Anything it cannot read
	// is left to land.go, which owns the real flags and their errors.
	fs := newFlagSet("land", io.Discard)
	planPath, _ := planFlags(fs)
	fs.Bool("no-gate", false, "")
	fs.Bool("force", false, "")
	fs.Bool("all", false, "")
	fs.String("branch", "", "")
	rest, err := parseMixed(fs, args)
	if err == nil && hasFlag(args, "all") {
		return landAllCmd(ctx, args, stdout, stderr)
	}
	if err == nil && !planFlagSet(fs) && *planPath == "" && len(rest) == 0 {
		if found := discoverPlans(); len(found) > 1 {
			return refuseAmbiguous(found, stderr)
		}
	}
	return landCmd(ctx, args, stdout, stderr)
}

// hasFlag reports whether the exact token --name is among args. A flag that
// carries a value is never mistaken for one that does not.
func hasFlag(args []string, name string) bool {
	want := "--" + name
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
