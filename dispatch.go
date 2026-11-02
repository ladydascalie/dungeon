package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A dispatch is five steps: write the brief, submit it to the lane, wait for
// the lane to settle, read the report, run the acceptance command. Only the
// last step can make a unit done.

type dispatchOpts struct {
	planPath      string
	statePath     string
	timeout       time.Duration
	acceptTimeout time.Duration
	poll          time.Duration
	dryRun        bool
	skipGate      bool
	force         bool
	out           io.Writer
	parentPane    string
	backend       string
}

// guardPlan refuses a plan edited after dispatch. The hash records what the
// lanes were told; --force accepts the edit and records the new hash.
func (o dispatchOpts) guardPlan(st *State, stderr io.Writer) int {
	if st.PlanHash == "" {
		return 0
	}
	h, err := PlanHash(o.planPath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if h == st.PlanHash {
		return 0
	}
	if !o.force {
		fmt.Fprintf(stderr, "dungeon: plan changed since dispatch (%s, dispatched %s)\n",
			shortHash(h), shortHash(st.PlanHash))
		fmt.Fprintln(stderr, "dungeon: re-run dungeon up, or pass --force to accept the edit")
		return 1
	}
	st.PlanHash = h
	if err := SaveState(o.statePath, *st); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	return 0
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

type digest struct {
	Dispatched []string
	Done       []string
	Failed     []string
	Rejected   []string
	Unproven   []string
	Blocked    []string
	NoLane     []string
}

func (d digest) empty() bool {
	return len(d.Dispatched)+len(d.Done)+len(d.Failed)+len(d.Rejected)+len(d.Unproven)+len(d.Blocked)+len(d.NoLane) == 0
}

func (d digest) String() string {
	var b strings.Builder
	line := func(label string, ids []string) {
		if len(ids) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(ids, " "))
		}
	}
	line("dispatched", d.Dispatched)
	line("done", d.Done)
	line("failed", d.Failed)
	line("rejected", d.Rejected)
	line("unproven", d.Unproven)
	line("blocked", d.Blocked)
	line("no lane", d.NoLane)
	if b.Len() == 0 {
		return "nothing to do\n"
	}
	return b.String()
}

func briefPath(planPath, unit string) string {
	return filepath.Join(encounterDir(planPath), "briefs", unit+".md")
}

func reportPath(planPath, unit string) string {
	return filepath.Join(encounterDir(planPath), "reports", unit+".md")
}

// minionDir is where a minion reads and runs its accept command. A writer
// works in its own worktree. A staff minion paired with a writer reads the
// writer's worktree, so a reviewer sees the branch it reviews. Staff with no
// pair reads the base checkout.
func minionDir(p Plan, m Minion) string {
	if writes(m.Role) {
		return lanePath(p, m.ID)
	}
	if m.Of != "" {
		return lanePath(p, m.Of)
	}
	return p.Repo
}

// lanePath is where a lane actually is. A worktree that moved after it was cut
// is still the lane, so ask worktrunk or git before falling back to the path
// dungeon would create.
func lanePath(p Plan, minion string) string {
	branch := p.Encounter + "/" + minion
	if path, ok := resolveWorktreePath(p.Repo, branch); ok {
		return path
	}
	return worktreePath(p.Repo, p.Encounter, minion)
}

// depReport is one dep's report, named by unit so the lane knows which report
// answers which dependency.
type depReport struct {
	ID   string
	Path string
}

// briefData is everything a brief says. It is grouped so the callers pass one
// value instead of a row of positions.
type briefData struct {
	minion     Minion
	unit       Unit
	dir        string
	report     string
	depReports []depReport
	parentPane string
	ping       string
	base       string
	lane       MinionState
}

// briefText is the whole contract the lane gets. It names what to do, what it
// owns, what it may not touch, where to leave the report, where the deps left
// theirs, and how to ping the parent.
func briefText(d briefData) string {
	m, u := d.minion, d.unit
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", u.ID)
	if d.lane.Resumed {
		b.WriteString("## Lane branch reused\n\n")
		fmt.Fprintf(&b, "branch: %s\nhead: %s %s\nahead of %s: %d\n",
			d.lane.Branch, d.lane.Head, d.lane.Subject, d.base, d.lane.Ahead)
		if d.lane.Dirty > 0 {
			fmt.Fprintf(&b, "uncommitted files: %d\n", d.lane.Dirty)
		} else {
			b.WriteString("uncommitted: none\n")
		}
		b.WriteString("\nRead the existing commits before writing. Do not reset or delete them.\n\n")
	}
	if d.lane.Recreated {
		b.WriteString("## Lane branch recreated\n\n")
		fmt.Fprintf(&b, "branch: %s\ncreated from: %s\n", d.lane.Branch, d.base)
		b.WriteString("The old branch had no unique commits, so dungeon replaced it.\n\n")
	}
	fmt.Fprintf(&b, "%s\n\n", u.Goal)
	fmt.Fprintf(&b, "minion: %s (%s)\nworktree: %s\n", m.ID, m.Role, d.dir)
	if len(u.Paths) > 0 {
		b.WriteString("paths you own:\n")
		for _, path := range u.Paths {
			fmt.Fprintf(&b, "  - %s\n", path)
		}
	}
	if len(u.Deps) > 0 {
		fmt.Fprintf(&b, "deps that landed: %s\n", strings.Join(u.Deps, " "))
	}
	if len(d.depReports) > 0 {
		b.WriteString("\n## reports from deps\n\n")
		for _, r := range d.depReports {
			fmt.Fprintf(&b, "- %s: %s\n", r.ID, r.Path)
		}
	}
	switch {
	case u.Verdict != "":
		fmt.Fprintf(&b, "verdict: %s\n", u.Verdict)
		b.WriteString("Write that line in your report, exactly, when the unit passes.\n")
	case u.Accept != "":
		fmt.Fprintf(&b, "accept: %s\n", u.Accept)
	default:
		b.WriteString("accept: none. This unit cannot be marked done, so say plainly what you did.\n")
	}
	fmt.Fprintf(&b, "\nreport: %s\n", d.report)
	if d.parentPane != "" {
		fmt.Fprintf(&b, "\n## Ping the parent when you finish\n\n")
		b.WriteString("Write the report first, then wake the parent agent:\n\n")
		fmt.Fprintf(&b, "    %s %q\n\n", d.ping,
			fmt.Sprintf("%s: %s finished. Report: %s", u.ID, m.ID, d.report))
		b.WriteString("That pane runs the agent that dispatched you. Do not wait for a reply.\n")
	}
	b.WriteString(`
## What you may not do

- Touch a file outside the paths above.
- Edit the plan or the state file. They are not yours.
- Claim work you did not do.
- Leave work uncommitted. Commit on your branch, or there is nothing to land
  and the unit stays blocked.

Write the report to the path above. Keep it short: what changed, what you ran,
what you did not do.
`)
	return b.String()
}

// briefInput is one brief to write: which encounter, which lane, which run.
type briefInput struct {
	opts   dispatchOpts
	plan   Plan
	minion Minion
	unit   Unit
	state  *State
}

func writeBrief(h mux, in briefInput) (string, error) {
	o, p, m, u, st := in.opts, in.plan, in.minion, in.unit, in.state
	path := briefPath(o.planPath, u.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// Every path in the brief is absolute: the lane runs in a worktree, where a
	// relative .dungeon/ does not exist.
	absReport, err := filepath.Abs(reportPath(o.planPath, u.ID))
	if err != nil {
		return "", err
	}
	depReports := make([]depReport, 0, len(u.Deps))
	for _, dep := range u.Deps {
		abs, err := filepath.Abs(reportPath(o.planPath, dep))
		if err != nil {
			return "", err
		}
		depReports = append(depReports, depReport{ID: dep, Path: abs})
	}
	lane := MinionState{}
	if st != nil {
		switch {
		case writes(m.Role):
			lane = st.Minions[m.ID]
		case m.Of != "":
			lane = st.Minions[m.Of]
		}
	}
	text := briefText(briefData{
		minion:     m,
		unit:       u,
		dir:        minionDir(p, m),
		report:     absReport,
		depReports: depReports,
		parentPane: o.parentPane,
		ping:       h.PingCommand(o.parentPane),
		base:       p.Base,
		lane:       lane,
	})
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func dispatchUnit(ctx context.Context, h mux, o dispatchOpts, p Plan, st *State, u Unit, m Minion) error {
	brief, err := writeBrief(h, briefInput{opts: o, plan: p, minion: m, unit: u, state: st})
	if err != nil {
		return err
	}
	absBrief, _ := filepath.Abs(brief)
	absReport, _ := filepath.Abs(reportPath(o.planPath, u.ID))
	if err := os.MkdirAll(filepath.Dir(absReport), 0o755); err != nil {
		return err
	}
	text := fmt.Sprintf("Read %s and do it. Write your report to %s.", absBrief, absReport)
	if o.dryRun {
		fmt.Fprintf(o.out, "would dispatch %s to %s\n", u.ID, m.ID)
		return nil
	}
	if err := h.Prompt(ctx, st.AgentName(m.ID), text); err != nil {
		return err
	}
	if err := st.SetUnit(u.ID, "working", "brief "+brief); err != nil {
		return err
	}
	return SaveState(o.statePath, *st)
}

// settle runs the acceptance command. It is the only path to done.
func settle(ctx context.Context, o dispatchOpts, p Plan, st *State, u Unit, m Minion) string {
	report := reportPath(o.planPath, u.ID)
	if _, err := os.Stat(report); err != nil {
		st.SetUnit(u.ID, "blocked", "settled with no report at "+report)
		return "blocked"
	}
	// A lane that leaves work uncommitted leaves nothing to land, so the unit is
	// not done no matter what its accept command says about the working tree.
	if blockedByWorktree(o, p, st, u, m) {
		return "blocked"
	}
	if u.Verdict != "" {
		return settleVerdict(st, u, report)
	}
	if u.Accept == "" {
		st.SetUnit(u.ID, "unproven", "no accept command, so nothing can prove it")
		return "unproven"
	}
	cmdCtx, cancel := context.WithTimeout(ctx, o.acceptTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "sh", "-c", u.Accept)
	cmd.Dir = minionDir(p, m)
	out, err := cmd.CombinedOutput()
	receipt := lastLine(string(out))
	if err != nil {
		st.SetUnit(u.ID, "rejected", fmt.Sprintf("accept failed: %s (%s)", u.Accept, receipt))
		return "rejected"
	}
	st.SetUnit(u.ID, "done", fmt.Sprintf("accept: %s ok (%s)", u.Accept, receipt))
	return "done"
}

// settleVerdict proves a reviewer unit by its report, not by a shell command.
// Any report line whose trimmed text equals the wanted verdict proves it. A
// report without that line rejects the unit and names what it saw instead.
func settleVerdict(st *State, u Unit, report string) string {
	raw, err := os.ReadFile(report)
	if err != nil {
		st.SetUnit(u.ID, "blocked", "settled with no readable report at "+report)
		return "blocked"
	}
	seen := "none"
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Merge verdict:") {
			seen = line
		}
		if line == u.Verdict {
			st.SetUnit(u.ID, "done", "verdict: "+line)
			return "done"
		}
	}
	st.SetUnit(u.ID, "failed",
		fmt.Sprintf("wanted verdict %q, last Merge verdict: %s", u.Verdict, seen))
	return "failed"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if line == "" {
		line = "no output"
	}
	return truncate(line, 120)
}

// reconcile looks at every working unit and settles the ones whose lane has
// stopped. A lane that is still working, or that the multiplexer cannot
// classify, is left alone: guessing would settle work that is still running.
func reconcile(ctx context.Context, h mux, o dispatchOpts, p Plan, st *State, d *digest) bool {
	agents := h.Agents(ctx)
	progressed := false
	for _, u := range p.Units {
		if st.UnitStatus(u.ID) != "working" {
			continue
		}
		m, ok := findMinion(p, u.Minion)
		if !ok {
			continue
		}
		a, live := agents[st.AgentName(m.ID)]
		if !live {
			st.SetUnit(u.ID, "blocked", "minion "+m.ID+" has no live agent")
			d.Blocked = append(d.Blocked, u.ID)
			progressed = true
			continue
		}
		switch a.Status {
		case "blocked":
			st.SetUnit(u.ID, "blocked", "lane is waiting on you in "+a.Pane)
			d.Blocked = append(d.Blocked, u.ID)
			progressed = true
		case "idle", "done":
			switch settle(ctx, o, p, st, u, m) {
			case "done":
				StopLoopAfter(p, st, u.ID)
				PromoteLoopFix(p, st, u.ID)
				d.Done = append(d.Done, u.ID)
			case "failed":
				d.Failed = append(d.Failed, u.ID)
			case "rejected":
				d.Rejected = append(d.Rejected, u.ID)
			case "unproven":
				d.Unproven = append(d.Unproven, u.ID)
			default:
				d.Blocked = append(d.Blocked, u.ID)
			}
			progressed = true
		}
	}
	if progressed {
		SaveState(o.statePath, *st)
	}
	return progressed
}

// readyUnits returns the planned units whose deps have all landed. A failed
// dep counts: it may be the review that tells the fixer what to do. So does an
// unproven dep when the dep is provisional, because the next review judges it.
func readyUnits(p Plan, st *State) []Unit {
	var out []Unit
	for _, u := range p.Units {
		if st.UnitStatus(u.ID) != "planned" {
			continue
		}
		ready := true
		for _, d := range u.Deps {
			if !depSatisfied(p, st, d) {
				ready = false
				break
			}
		}
		if ready {
			out = append(out, u)
		}
	}
	return out
}

// depSatisfied answers whether a dep releases its dependants. A provisional
// unit is unproven on purpose: the review after it is the judge.
func depSatisfied(p Plan, st *State, dep string) bool {
	switch st.UnitStatus(dep) {
	case "done", "failed":
		return true
	case "unproven":
		for _, u := range p.Units {
			if u.ID == dep {
				return u.Provisional
			}
		}
	}
	return false
}

func workingCount(p Plan, st *State) int {
	n := 0
	for _, u := range p.Units {
		if st.UnitStatus(u.ID) == "working" {
			n++
		}
	}
	return n
}

// next drives the encounter until nothing can move: settle what landed,
// dispatch what is ready, wait, repeat.
func next(ctx context.Context, h mux, o dispatchOpts, p Plan, st *State) (digest, error) {
	var d digest
	deadline := time.Now().Add(o.timeout)

	for {
		reconcile(ctx, h, o, p, st, &d)

		slots := len(p.Minions)
		if p.Concurrency > 0 {
			slots = p.Concurrency - workingCount(p, st)
		}
		agents := h.Agents(ctx)
		busy := map[string]bool{}
		for _, u := range p.Units {
			if st.UnitStatus(u.ID) == "working" {
				busy[u.Minion] = true
			}
		}

		for _, u := range readyUnits(p, st) {
			if slots <= 0 {
				break
			}
			m, ok := findMinion(p, u.Minion)
			if !ok || busy[m.ID] {
				continue
			}
			if _, live := agents[st.AgentName(m.ID)]; !live {
				if !contains(d.NoLane, u.ID) {
					d.NoLane = append(d.NoLane, u.ID)
				}
				continue
			}
			if err := dispatchUnit(ctx, h, o, p, st, u, m); err != nil {
				d.Blocked = append(d.Blocked, u.ID)
				fmt.Fprintf(o.out, "%s: %v\n", u.ID, err)
				continue
			}
			if !o.dryRun {
				busy[m.ID] = true
				slots--
				d.Dispatched = append(d.Dispatched, u.ID)
			}
		}

		if o.dryRun {
			return d, nil
		}
		if workingCount(p, st) == 0 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(o.out, "timeout: lanes still working, run dungeon next again")
			break
		}
		time.Sleep(o.poll)
	}
	return d, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
