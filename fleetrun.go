package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// fleetTarget is one encounter in a sweep: its plan, its state, where both live,
// and the hash of the plan as it is now.
type fleetTarget struct {
	PlanPath  string
	StatePath string
	Plan      Plan
	State     State
	PlanHash  string
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

// fleetLoad is what loading a fleet found. duplicate stops the whole sweep,
// because two plan files that claim one encounter name would share one worktree,
// one branch and one box key.
type fleetLoad struct {
	targets   []*fleetTarget
	total     int
	skipped   int
	duplicate string
}

// loadFleet loads every encounter and applies the same guards the single-plan
// path applies: the plan must parse and validate, the state must hold this
// encounter, and the plan must not have changed since dispatch unless force
// accepts the edit. A plan that fails a guard is printed as skipped and left
// out, because a fleet is never dispatched from a half-loaded state.
//
// Loading writes nothing. Looking at a fleet must not arm the edit guard, create
// a state file or reorder discovery.
func loadFleet(force bool, out io.Writer) fleetLoad {
	var load fleetLoad
	seen := map[string]string{}
	for _, e := range discoverPlans() {
		load.total++
		t, reason := loadFleetTarget(e.Path, force)
		if reason != "" {
			fmt.Fprintf(out, "skipped %s: %s\n", shortPath(e.Path), reason)
			load.skipped++
			continue
		}
		if first, ok := seen[t.encounter()]; ok {
			load.duplicate = fmt.Sprintf(
				"two plan files declare encounter %q: %s and %s; an encounter is one worktree, one branch and one box key, so rename one",
				t.encounter(), shortPath(first), shortPath(e.Path))
			return load
		}
		seen[t.encounter()] = e.Path
		load.targets = append(load.targets, t)
	}
	return load
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
		return nil, fmt.Sprintf("plan changed since dispatch (%s, dispatched %s); re-run dungeon up, or pass --force",
			shortHash(h), shortHash(st.PlanHash))
	}
	return &fleetTarget{PlanPath: planPath, StatePath: statePath, Plan: p, State: st, PlanHash: h}, ""
}

// recordPlanHash writes the hash a --force accepted. Only the commands that
// dispatch, or that are told to force, call it: arming the edit guard is a
// decision, not a side effect of describing the fleet.
func recordPlanHash(targets []*fleetTarget) error {
	for _, t := range targets {
		if t.State.PlanHash == t.PlanHash {
			continue
		}
		t.State.PlanHash = t.PlanHash
		if err := SaveState(t.StatePath, t.State); err != nil {
			return err
		}
	}
	return nil
}

// noTargets refuses an empty sweep: a sweep of nothing could not run.
func noTargets(stderr io.Writer) int {
	fmt.Fprintln(stderr, "dungeon: no encounter to sweep: nothing under .dungeon parses and validates")
	return 2
}

// skippedSummary says how much of the fleet the sweep left out, so a scripted
// fleet dispatch does not silently lose an encounter.
func skippedSummary(out io.Writer, load fleetLoad) {
	if load.skipped > 0 {
		fmt.Fprintf(out, "%d of %d encounters skipped\n", load.skipped, load.total)
	}
}

// skippedCode is 1 when plans were left out of a sweep that otherwise ran.
func skippedCode(skipped int) int {
	if skipped > 0 {
		return 1
	}
	return 0
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
// per plan, and the deadline bounds the accept commands too, not just the sleep
// between passes.
func nextAll(ctx context.Context, h mux, o dispatchOpts, targets []*fleetTarget) (fleetDigest, error) {
	var fd fleetDigest
	if o.parentPane == "" {
		o.parentPane = h.SelfPane(ctx)
	}
	o.deadline = time.Now().Add(o.timeout)
	digests := make([]digest, len(targets))

	for {
		// The budget and the signal are read before every plan, so no plan
		// starts work the sweep cannot finish.
		outOfTime := !o.dryRun && time.Now().After(o.deadline)
		interrupted := ctx.Err() != nil
		working := 0
		for i, t := range targets {
			if !outOfTime && !interrupted {
				if err := passTarget(ctx, h, o, t, &digests[i]); err != nil {
					if ctx.Err() == nil {
						return fd, err
					}
					interrupted = true
				}
			}
			if workingCount(t.Plan, &t.State) > 0 {
				working++
			}
		}
		if o.dryRun || working == 0 {
			break
		}
		if interrupted || outOfTime || time.Now().After(o.deadline) {
			fmt.Fprintln(o.out, stopLine(ctx, "dungeon next --all"))
			break
		}
		time.Sleep(o.poll)
	}

	for i, t := range targets {
		fd.Blocks = append(fd.Blocks, fleetBlock{Encounter: t.encounter(), Digest: digests[i]})
	}
	return fd, nil
}

// passTarget runs one sweep pass over one plan and writes its state the moment
// it changed, so a crash mid-sweep loses nothing.
func passTarget(ctx context.Context, h mux, o dispatchOpts, t *fleetTarget, d *digest) error {
	to := o
	to.planPath, to.statePath = t.PlanPath, t.StatePath
	before, err := json.Marshal(t.State)
	if err != nil {
		return err
	}
	if _, err := step(ctx, h, to, t.Plan, &t.State, d); err != nil {
		return err
	}
	after, err := json.Marshal(t.State)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		return nil
	}
	return SaveState(t.StatePath, t.State)
}

// nextAllCmd sweeps every encounter. The mux is opened once for the whole
// sweep, not once per plan.
func nextAllCmd(ctx context.Context, o dispatchOpts, flags fleetOpts) int {
	if msg := flags.refusal(); msg != "" {
		fmt.Fprintf(flags.stderr, "dungeon: %s\n", msg)
		return 2
	}
	load, code := sweepTargets(o.force, o.out, flags.stderr)
	if code != 0 {
		return code
	}
	if o.force {
		if err := recordPlanHash(load.targets); err != nil {
			fmt.Fprintf(flags.stderr, "dungeon: %v\n", err)
			return 2
		}
	}
	h, err := openMux(ctx, o.backend)
	if err != nil {
		fmt.Fprintf(flags.stderr, "dungeon: %v\n", err)
		return 2
	}
	d, err := nextAll(ctx, h, o, load.targets)
	if err != nil {
		fmt.Fprintf(flags.stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(o.out, d)
	skippedSummary(o.out, load)
	return skippedCode(load.skipped)
}

// sweepTargets loads the fleet and turns a refusal into the exit code the
// operator gets. Every fleet command starts here.
func sweepTargets(force bool, out, stderr io.Writer) (fleetLoad, int) {
	load := loadFleet(force, out)
	if load.duplicate != "" {
		fmt.Fprintf(stderr, "dungeon: %s\n", load.duplicate)
		return load, 2
	}
	if len(load.targets) == 0 {
		return load, noTargets(stderr)
	}
	return load, 0
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
	load, code := sweepTargets(in.opts.force, in.opts.out, in.flags.stderr)
	if code != 0 {
		return code
	}
	h, err := openMux(ctx, in.opts.backend)
	if err != nil {
		fmt.Fprintf(in.flags.stderr, "dungeon: %v\n", err)
		return 2
	}
	failed := false
	for _, t := range load.targets {
		res, err := spawnMinions(ctx, h, spawnInput{
			planPath: t.PlanPath, plan: t.Plan, state: &t.State, split: in.split, out: in.opts.out,
		})
		if err != nil {
			fmt.Fprintf(in.flags.stderr, "dungeon: %s: %v\n", t.encounter(), err)
			return 2
		}
		// The hash is recorded at spawn, as up does: the lanes were told this
		// plan, so this is what an edit after dispatch is measured against.
		t.State.PlanHash = t.PlanHash
		if err := SaveState(t.StatePath, t.State); err != nil {
			fmt.Fprintf(in.flags.stderr, "dungeon: %v\n", err)
			return 2
		}
		fmt.Fprintf(in.opts.out, "%s: %s\n", t.encounter(), res)
		if len(res.Failed) > 0 {
			failed = true
		}
	}
	if !in.noDispatch && !failed {
		d, err := nextAll(ctx, h, in.opts, load.targets)
		if err != nil {
			fmt.Fprintf(in.flags.stderr, "dungeon: %v\n", err)
			return 2
		}
		fmt.Fprint(in.opts.out, d)
	}
	skippedSummary(in.opts.out, load)
	return skippedCode(load.skipped)
}

// fleetLand is what one land --all did: one result per plan, and the combined
// branch when two or more plans put work on it.
type fleetLand struct {
	Plans     []fleetLandPlan
	NotLanded []string
	Branch    string
	Worktree  string
	Merged    []string
	Gate      []gateResult
	Conflict  string
	Reason    string
	// Held is set when an interrupt cut a gate short. The sweep stopped there,
	// so nothing is a finding and the fleet branch is withheld.
	Held bool
}

type fleetLandPlan struct {
	Encounter string
	Result    landResult
}

// good reports whether the combined branch may be called proven: it exists, no
// conflict stopped it, and every gate passed on it.
func (r fleetLand) good() bool {
	if r.Branch == "" || r.Conflict != "" || len(r.NotLanded) > 0 {
		return false
	}
	for _, g := range r.Gate {
		if !g.OK {
			return false
		}
	}
	return true
}

// failedGates names the units whose accept failed on the combined tree.
func (r fleetLand) failedGates() []string {
	var out []string
	for _, g := range r.Gate {
		if !g.OK {
			out = append(out, g.Unit)
		}
	}
	return out
}

func (r fleetLand) String() string {
	var b strings.Builder
	for _, p := range r.Plans {
		for _, line := range resultLines(p.Result.String()) {
			fmt.Fprintf(&b, "%s: %s\n", p.Encounter, line)
		}
	}
	if len(r.NotLanded) > 0 {
		fmt.Fprintf(&b, "did not land: %s\n", strings.Join(r.NotLanded, " "))
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
	// A branch whose gate failed is on disk and holds the failing unit. It is
	// never reported the way a proven one is.
	if failed := r.failedGates(); len(failed) > 0 {
		fmt.Fprintf(&b, "%s is not good: %s failed on the combined tree\n", r.Branch, strings.Join(failed, " "))
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
// Every plan is landed even when an earlier one fails: each plan's land stands
// on its own. The combined branch is withheld when any plan conflicted or failed
// its own gate, because half a fleet on one branch would be a lie about what the
// fleet proved.
func landAll(ctx context.Context, o dispatchOpts, targets []*fleetTarget, branch string) (fleetLand, error) {
	var res fleetLand
	var sources []fleetSource
	withheld := false
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
		if r.Held {
			// The operator stopped the sweep mid-gate. Nothing after this point
			// was judged or landed, so the sweep stops and claims nothing.
			res.Held = true
			return res, nil
		}
		switch {
		case r.Conflict != "" || !gatesPassed(r):
			withheld = true
			if len(r.Merged) > 0 || r.Conflict != "" {
				res.NotLanded = append(res.NotLanded, t.encounter())
			}
		case len(r.Merged) > 0:
			sources = append(sources, fleetSource{
				Encounter: t.encounter(),
				PlanPath:  t.PlanPath,
				Branch:    t.encounter() + "/integration",
				Units:     r.Merged,
				Plan:      t.Plan,
			})
		}
	}
	if withheld || len(sources) < 2 {
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
				cmdCtx, cancel := acceptContext(ctx, o)
				out, err := acceptCommand(cmdCtx, res.Worktree, u.Accept)
				// The cause must be read before cancel, or every failed gate
				// would look like an interrupted one.
				held := err != nil && acceptCutShort(cmdCtx, o)
				cancel()
				if held {
					// The operator interrupted the sweep and this gate was killed
					// with it. It was not judged, so it is not a failed gate, and
					// no further gate runs under a cancelled context.
					res.Held = true
					return res, nil
				}
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
// combined branch must not be a branch a plan already owns, nor land on a
// worktree that holds another branch.
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
		switch {
		case branch == t.Plan.Base:
			fmt.Fprintf(stderr, "dungeon: --branch %s is %s's base branch\n", branch, t.encounter())
			fmt.Fprintln(stderr, "dungeon: land never moves a base branch; pass --branch <name> to name the combined branch something else")
			return 2
		case branch == t.encounter()+"/integration":
			fmt.Fprintf(stderr, "dungeon: --branch %s is %s's own integration branch\n", branch, t.encounter())
			fmt.Fprintln(stderr, "dungeon: pass --branch <name> to name the combined branch something else")
			return 2
		}
		for _, m := range t.Plan.Minions {
			if branch == t.encounter()+"/"+m.ID {
				fmt.Fprintf(stderr, "dungeon: --branch %s is %s's lane branch\n", branch, m.ID)
				fmt.Fprintln(stderr, "dungeon: pass --branch <name> to name the combined branch something else")
				return 2
			}
		}
	}
	// The combined worktree must be empty or already on this branch. Merging
	// into a lane's worktree would put another plan's work on a live branch.
	repo := first.Plan.Repo
	path := branchWorktreePath(repo, branch)
	if info, err := os.Stat(path); err == nil && info.IsDir() && !worktreeOnBranch(path, branch) {
		head, _ := gitRun(path, "rev-parse", "--abbrev-ref", "HEAD")
		fmt.Fprintf(stderr, "dungeon: %s is a worktree of branch %s, so %s cannot go there\n", path, head, branch)
		fmt.Fprintln(stderr, "dungeon: pass --branch <name> to name the combined branch something else")
		return 2
	}
	// A branch that already exists somewhere else is someone's work. Merging the
	// fleet onto it would move it. The only existing branch a land may use is one
	// it cut itself, which the fleet worktree is checked out on.
	if branchExists(repo, branch) && !worktreeOnBranch(path, branch) {
		fmt.Fprintf(stderr, "dungeon: branch %s already exists, so a land --all would move it\n", branch)
		fmt.Fprintln(stderr, "dungeon: pass --branch <name> to name a new combined branch, or delete that branch first")
		return 2
	}
	return 0
}

// worktreeOnBranch reports whether path is a worktree sitting on branch.
func worktreeOnBranch(path, branch string) bool {
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return false
	}
	head, err := gitRun(path, "rev-parse", "--abbrev-ref", "HEAD")
	return err == nil && head == branch
}

// landAllCmd lands every encounter and, when two or more merged something,
// combines them on one branch.
func landAllCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, lf := landFlagSet("land", stderr)
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		*lf.planPath = rest[0]
	}
	o := *lf.opts
	o.skipGate, o.force, o.out = *lf.noGate, *lf.force, stdout
	flags := fleetOpts{planPath: *lf.planPath, statePath: lf.statePath, stderr: stderr}
	if msg := flags.refusal(); msg != "" {
		fmt.Fprintf(stderr, "dungeon: %s\n", msg)
		return 2
	}
	load, code := sweepTargets(o.force, stdout, stderr)
	if code != 0 {
		return code
	}
	if o.force {
		if err := recordPlanHash(load.targets); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
	}
	if code := fleetLandGuard(load.targets, *lf.branch, stderr); code != 0 {
		return code
	}
	res, err := landAll(ctx, o, load.targets, *lf.branch)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(stdout, res)
	skippedSummary(stdout, load)
	if res.Held {
		// An interrupted land could not run. It is not a finding: no gate was
		// judged, and the fleet branch is withheld.
		fmt.Fprintln(stdout, stopLine(ctx, "dungeon land --all"))
		return 2
	}
	if res.good() {
		abs, _ := filepath.Abs(res.Worktree)
		fmt.Fprintf(stdout, "\nthe branch is local. nothing was pushed. inspect it at %s\n", abs)
	}
	if !res.good() && (res.Conflict != "" || len(res.NotLanded) > 0 || len(res.failedGates()) > 0) {
		return 1
	}
	if !res.good() {
		// Nothing combined: the plan gates before it decide.
		for _, p := range res.Plans {
			if p.Result.Conflict != "" {
				return 1
			}
			if !gatesPassed(p.Result) {
				return 1
			}
		}
	}
	return skippedCode(load.skipped)
}

// landOrFleet keeps land.go in charge of the single-plan path: land --all goes
// to the fleet engine, and a bare land over many encounters names --all before
// land.go refuses without knowing about it.
func landOrFleet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// The router parses with the same flag set the commands use, so a flag it
	// does not route on cannot make it miss --all.
	fs, lf := landFlagSet("land", io.Discard)
	rest, err := parseMixed(fs, args)
	if err != nil {
		// Let landCmd report the flag error the operator actually made.
		return landCmd(ctx, args, stdout, stderr)
	}
	if *lf.all {
		return landAllCmd(ctx, args, stdout, stderr)
	}
	if !planFlagSet(fs) && *lf.planPath == "" && len(rest) == 0 {
		if found := discoverPlans(); len(found) > 1 {
			return refuseAmbiguous(found, stderr)
		}
	}
	return landCmd(ctx, args, stdout, stderr)
}
