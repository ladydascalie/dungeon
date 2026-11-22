package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	// deadline bounds a whole fleet sweep. An accept command takes what is left
	// of it rather than its own --accept-timeout, so one slow gate cannot run
	// the sweep past the timeout it was given.
	deadline time.Time
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
	Dispatched  []string
	Would       []string // a dry run: what a real pass would have dispatched
	WouldSettle []string // a dry run: what a real pass would have settled
	WouldBlock  []string // a dry run: what a real pass would have blocked
	Done        []string
	Failed      []string
	Rejected    []string
	Unproven    []string
	Blocked     []string
	NoLane      []string
	PlanFix     []string
}

// planFixes names the plan edit a blocked or unproven unit is waiting on, so
// the parent asks for a plan change instead of narrating a stalled watch. A
// loop fix settles unproven on purpose: the review after it is the judge, so it
// never gets a hint.
func planFixes(p Plan, st *State, d digest) []string {
	var out []string
	for _, id := range d.Unproven {
		u, ok := findUnit(p, id)
		if !ok || u.Provisional || u.Loop != "" {
			continue
		}
		if strings.Contains(st.Units[id].Note, "no accept command") {
			out = append(out, id+": add an accept command to the plan, or accept that this unit stays unproven")
		}
	}
	for _, id := range d.Blocked {
		if strings.Contains(st.Units[id].Note, "changed files outside your paths") {
			out = append(out, id+": widen the unit's paths in the plan to cover the files it touched")
		}
	}
	return out
}

func (d digest) empty() bool {
	return len(d.Dispatched)+len(d.Would)+len(d.WouldSettle)+len(d.WouldBlock)+len(d.Done)+len(d.Failed)+len(d.Rejected)+len(d.Unproven)+len(d.Blocked)+len(d.NoLane) == 0
}

func (d digest) String() string {
	var b strings.Builder
	line := func(label string, ids []string) {
		if len(ids) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(ids, " "))
		}
	}
	line("dispatched", d.Dispatched)
	line("would dispatch", d.Would)
	line("would settle", d.WouldSettle)
	line("would block", d.WouldBlock)
	line("done", d.Done)
	line("failed", d.Failed)
	line("rejected", d.Rejected)
	line("unproven", d.Unproven)
	line("blocked", d.Blocked)
	line("no lane", d.NoLane)
	line("plan fix", d.PlanFix)
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

// laneBoxKey is the session box key for one lane. It carries the encounter, so
// two encounters that both declare w1 post to two places and one parent
// watching the lane/ prefix sees both.
func laneBoxKey(encounter, minion string) string {
	return "lane/" + encounter + "/" + minion
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
// answers which dependency. A cross-plan dep also says the ref and commit it
// landed on: that report is the contract the lane reads.
type depReport struct {
	ID   string
	Path string
	Note string
}

// depLabels is every dep as one token, for a brief line or a status row.
func depLabels(deps []Dep) []string {
	out := make([]string, len(deps))
	for i, d := range deps {
		out[i] = d.String()
	}
	return out
}

// briefData is everything a brief says. It is grouped so the callers pass one
// value instead of a row of positions.
type briefData struct {
	minion     Minion
	unit       Unit
	dir        string
	report     string
	depReports []depReport
	ping       string
	pingNote   string
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
		fmt.Fprintf(&b, "deps that landed: %s\n", strings.Join(depLabels(u.Deps), " "))
	}
	if len(d.depReports) > 0 {
		b.WriteString("\n## reports from deps\n\n")
		for _, r := range d.depReports {
			fmt.Fprintf(&b, "- %s: %s", r.ID, r.Path)
			if r.Note != "" {
				fmt.Fprintf(&b, " (%s)", r.Note)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n## If this dispatch arrives again\n\n")
	b.WriteString("It is the same unit and the same worktree: continue from what is already there, do not restart, and write the report once.\n")
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
	if d.ping != "" {
		fmt.Fprintf(&b, "\n## Notify the parent when you finish\n\n")
		b.WriteString("Write the report first, then notify the parent:\n\n")
		fmt.Fprintf(&b, "    %s\n\n", d.ping)
		b.WriteString(d.pingNote + "\n")
	}
	b.WriteString(`
## What you may not do

- Touch a file outside the paths above.
- Edit the plan or the state file. They are not yours.
- Claim work you did not do.
- Commit. Leave the work in the worktree. Dungeon commits it on the lane
  branch when the lane stops.

Write the report to the path above. Keep it short: what changed, what you ran,
what you did not do.

## Before you settle

Verify the work before you settle. Run the accept command when the brief gives
one. Otherwise run the narrowest command that shows the change does what it
claims. If you cannot verify it, write "unverified" in the report and name
what is missing. A command you ran is evidence; a summary is not.
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
		planPath, note := o.planPath, ""
		if !dep.Local() {
			planPath = resolveDepPlan(o.planPath, dep)
			note = landedNote(planPath, dep.Unit)
		}
		abs, err := filepath.Abs(reportPath(planPath, dep.Unit))
		if err != nil {
			return "", err
		}
		depReports = append(depReports, depReport{ID: dep.String(), Path: abs, Note: note})
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
	ping := ""
	pingNote := ""
	if o.parentPane != "" {
		msg := fmt.Sprintf("%s: %s finished. Report: %s", u.ID, m.ID, absReport)
		if boxer, ok := h.(interface {
			BoxPing(key, message string) string
		}); ok {
			ping = boxer.BoxPing(laneBoxKey(p.Encounter, m.ID), msg)
			pingNote = "The parent reads the session box. Do not wait for a reply."
		} else {
			ping = h.PingCommand(o.parentPane, msg)
			pingNote = "That pane runs the agent that dispatched you. Do not wait for a reply."
		}
	}
	text := briefText(briefData{
		minion:     m,
		unit:       u,
		dir:        minionDir(p, m),
		report:     absReport,
		depReports: depReports,
		ping:       ping,
		pingNote:   pingNote,
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
		// The caller records it in the digest: a line printed here would sit
		// outside the plan's block, naming no encounter.
		return nil
	}
	// The unit is marked working with its prompt unconfirmed before the prompt
	// is sent. A killed process or a crash between the paste and the confirm
	// then leaves the evidence reconcile needs to deliver the brief again,
	// instead of a lane that looks busy and never was. A retry keeps the
	// existing status and its history: only the first dispatch changes it.
	if st.UnitStatus(u.ID) != "working" {
		if err := st.SetUnit(u.ID, "working", "brief "+brief); err != nil {
			return err
		}
	}
	st.SetPromptPending(u.ID, true)
	if err := SaveState(o.statePath, *st); err != nil {
		return err
	}
	if err := h.Prompt(ctx, st.AgentName(m.ID), text); err != nil {
		return err
	}
	st.SetPromptPending(u.ID, false)
	return SaveState(o.statePath, *st)
}

// settle runs the acceptance command. It is the only path to done.
func settle(ctx context.Context, o dispatchOpts, p Plan, st *State, u Unit, m Minion) string {
	report := reportPath(o.planPath, u.ID)
	if _, err := os.Stat(report); err != nil {
		st.SetUnit(u.ID, "blocked", "settled with no report at "+report)
		return "blocked"
	}
	// Dungeon commits the lane's work on the lane branch before the accept runs.
	// A change outside the unit's paths still blocks the unit and names the files.
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
	cmdCtx, cancel := acceptContext(ctx, o)
	defer cancel()
	out, err := acceptCommand(cmdCtx, minionDir(p, m), u.Accept)
	receipt := lastLine(string(out))
	if err != nil {
		if acceptCutShort(cmdCtx, o) {
			// The sweep was interrupted or ran out of time, not the gate. The
			// unit was not judged, so it stays working and the next sweep
			// settles it.
			return "held"
		}
		st.SetUnit(u.ID, "rejected", fmt.Sprintf("accept failed: %s (%s)", u.Accept, receipt))
		return "rejected"
	}
	st.SetUnit(u.ID, "done", fmt.Sprintf("accept: %s ok (%s)", u.Accept, receipt))
	return "done"
}

// acceptContext bounds one accept command by its own limit and by the sweep's
// deadline, whichever comes first, so --timeout covers an accept and not just
// the sleep between passes.
func acceptContext(ctx context.Context, o dispatchOpts) (context.Context, context.CancelFunc) {
	limit := time.Now().Add(o.acceptTimeout)
	if !o.deadline.IsZero() && o.deadline.Before(limit) {
		limit = o.deadline
	}
	return context.WithDeadline(ctx, limit)
}

// acceptCommand runs one accept command, bounded by cmdCtx. The command gets its
// own process group, so a gate that forks (go test, make, npm test) is killed
// with everything it started. Killing only sh leaves a grandchild holding the
// output pipe, and the caller then waits for that grandchild instead of for the
// deadline.
func acceptCommand(cmdCtx context.Context, dir, command string) ([]byte, error) {
	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	// A backstop for anything that left the group: waiting on its pipes must not
	// outlive the deadline by more than this.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		// The gate passed and left a child holding its output. The exit status
		// is the proof; a process it started is not the gate's verdict.
		err = nil
	}
	return out, err
}

// acceptCutShort reports whether an accept command was stopped by the caller
// rather than by failing on its own. An interrupted sweep cancels the context,
// and a sweep deadline expires it; an accept that runs into its own
// --accept-timeout is a rejection, not an unfinished sweep.
func acceptCutShort(cmdCtx context.Context, o dispatchOpts) bool {
	switch {
	case errors.Is(cmdCtx.Err(), context.Canceled):
		return true
	case errors.Is(cmdCtx.Err(), context.DeadlineExceeded):
		return !o.deadline.IsZero() && !time.Now().Before(o.deadline)
	}
	return false
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
// stopped. A lane that is still working is left alone: guessing would settle
// work that is still running. A lane the multiplexer cannot classify is held
// until its report exists, then judged by its accept command, because with no
// status protocol the report is the only finish signal there is.
//
// A working unit whose prompt was never confirmed is delivered again instead of
// being settled or left alone: the paste may never have landed, and a lane that
// was never told to work cannot finish. A lane that already wrote its report
// did get the brief, so the marker is cleared and the unit settles as usual.
func reconcile(ctx context.Context, h mux, o dispatchOpts, p Plan, st *State, d *digest) bool {
	// An unreadable fleet settles nothing: calling a working lane blocked
	// because the backend blinked would be a false claim.
	agents, err := h.Agents(ctx)
	if err != nil {
		return false
	}
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
			if o.dryRun {
				d.WouldBlock = append(d.WouldBlock, u.ID)
				continue
			}
			st.SetUnit(u.ID, "blocked", "minion "+m.ID+" has no live agent")
			d.Blocked = append(d.Blocked, u.ID)
			progressed = true
			continue
		}
		if st.PromptIsPending(u.ID) {
			if _, err := os.Stat(reportPath(o.planPath, u.ID)); err != nil {
				if err := dispatchUnit(ctx, h, o, p, st, u, m); err != nil {
					// The state still says unconfirmed, so the next pass tries
					// again. Reporting it keeps the failure visible.
					fmt.Fprintf(o.out, "%s: %v\n", u.ID, err)
				} else if o.dryRun {
					d.Would = append(d.Would, u.ID+" to "+m.ID)
				} else {
					d.Dispatched = append(d.Dispatched, u.ID)
				}
				progressed = !o.dryRun
				continue
			}
			// A report proves the lane was told its brief, whatever the marker
			// says, so the unit settles below rather than prompt a second time.
			if !o.dryRun {
				st.SetPromptPending(u.ID, false)
			}
		}
		switch a.Status {
		case AgentBlocked:
			if o.dryRun {
				d.WouldBlock = append(d.WouldBlock, u.ID)
				continue
			}
			st.SetUnit(u.ID, "blocked", "lane is waiting on you in "+a.Pane)
			d.Blocked = append(d.Blocked, u.ID)
			progressed = true
			continue
		case AgentIdle, AgentDone:
		case AgentUnknown:
			// No status protocol: the report is the only finish signal, so the
			// lane is held until it lands. Settling early would call a working
			// unit blocked on the first poll and never look at it again.
			if _, err := os.Stat(reportPath(o.planPath, u.ID)); err != nil {
				continue
			}
		default:
			continue
		}
		if o.dryRun {
			// Settling runs the accept command, commits the lane and writes the
			// unit. A preview says what it would settle and stops there.
			d.WouldSettle = append(d.WouldSettle, u.ID)
			continue
		}
		switch settle(ctx, o, p, st, u, m) {
		case "done":
			StopLoopAfter(p, st, u.ID)
			PromoteLoopFix(p, st, u.ID)
			d.Done = append(d.Done, u.ID)
			if reaper, ok := h.(interface {
				MarkReadyToReap(ctx context.Context, key string) error
			}); ok {
				key := laneBoxKey(p.Encounter, m.ID)
				if err := reaper.MarkReadyToReap(ctx, key); err != nil {
					fmt.Fprintf(o.out, "could not mark %s ready to be reaped: %v\n", key, err)
				}
			}
		case "failed":
			d.Failed = append(d.Failed, u.ID)
		case "rejected":
			d.Rejected = append(d.Rejected, u.ID)
		case "unproven":
			d.Unproven = append(d.Unproven, u.ID)
		case "held":
			// The sweep ran out of time inside the accept command. Nothing is
			// claimed about the unit: it stays working.
		default:
			d.Blocked = append(d.Blocked, u.ID)
		}
		progressed = true
	}
	if progressed && !o.dryRun {
		SaveState(o.statePath, *st)
	}
	d.PlanFix = planFixes(p, st, *d)
	return progressed && !o.dryRun
}

// readyUnits returns the planned units whose deps have all landed. A failed
// dep counts: it may be the review that tells the fixer what to do. So does an
// unproven dep when the dep is provisional, because the next review judges it.
func readyUnits(planPath string, p Plan, st *State) []Unit {
	var out []Unit
	for _, u := range p.Units {
		if st.UnitStatus(u.ID) != "planned" {
			continue
		}
		ready := true
		for _, d := range u.Deps {
			if !depSatisfied(planPath, p, st, d) {
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
//
// A cross-plan dep needs the other plan to be done with the unit. A unit that
// produced a branch must also be landed: done alone means proven, not merged. A
// staff unit has no branch to merge, so done is the whole proof, which is what
// lets one repo wait on another's review.
func depSatisfied(planPath string, p Plan, st *State, dep Dep) bool {
	if !dep.Local() {
		otherPath := resolveDepPlan(planPath, dep)
		other, err := LoadState(StatePath(otherPath))
		if err != nil {
			return false // an unreadable state satisfies nothing and is no error here
		}
		u, ok := other.Units[dep.Unit]
		if !ok || u.Status != "done" {
			return false
		}
		staff, known := depStaff(otherPath, dep.Unit)
		return (known && staff) || u.Landed != nil
	}
	switch st.UnitStatus(dep.Unit) {
	case "done", "failed":
		return true
	case "unproven":
		for _, u := range p.Units {
			if u.ID == dep.Unit {
				return u.Provisional
			}
		}
	}
	return false
}

// depStaff reports whether the named unit of another plan has no branch to
// merge. known is false when the plan or the unit cannot be read, and the dep
// then needs Landed: a claim nothing backs is not a claim.
func depStaff(planPath, unit string) (staff, known bool) {
	p, findings, err := DecodeFile(planPath)
	if err != nil || len(findings) > 0 {
		return false, false
	}
	p, _ = ExpandLoops(p)
	u, ok := findUnit(p, unit)
	if !ok {
		return false, false
	}
	m, ok := findMinion(p, u.Minion)
	if !ok {
		return false, false
	}
	return !writes(m.Role), true
}

// landedNote names the ref and commit a cross-plan dep landed on, so the brief
// points at a report that is really on a branch. A missing state says nothing.
func landedNote(planPath, unit string) string {
	other, err := LoadState(StatePath(planPath))
	if err != nil {
		return ""
	}
	if l := other.Units[unit].Landed; l != nil {
		return "landed on " + l.String()
	}
	return ""
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

// step is one pass over one encounter: settle the lanes that stopped, then
// dispatch every ready unit into the slots the plan has left. It is the whole
// of one next round, and next and next --all both loop over it, so the dispatch
// rules have exactly one home.
//
// It reports whether the pass changed something. An unreadable fleet is an
// error: a caller must not guess which lanes are alive.
func step(ctx context.Context, h mux, o dispatchOpts, p Plan, st *State, d *digest) (bool, error) {
	progressed := reconcile(ctx, h, o, p, st, d)

	// Every plan keeps its own cap: a fleet has no shared slot pool.
	slots := len(p.Minions)
	if p.Concurrency > 0 {
		slots = p.Concurrency - workingCount(p, st)
	}
	agents, err := h.Agents(ctx)
	if err != nil {
		return progressed, fmt.Errorf("could not read the fleet: %w", err)
	}
	busy := map[string]bool{}
	for _, u := range p.Units {
		if st.UnitStatus(u.ID) == "working" {
			busy[u.Minion] = true
		}
	}

	for _, u := range readyUnits(o.planPath, p, st) {
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
		if o.dryRun {
			// One pass dispatches one unit per minion and stops at the cap, so
			// the preview takes the same slots a real pass would.
			d.Would = append(d.Would, u.ID+" to "+m.ID)
			busy[m.ID] = true
			slots--
			continue
		}
		busy[m.ID] = true
		slots--
		d.Dispatched = append(d.Dispatched, u.ID)
		progressed = true
	}
	d.PlanFix = planFixes(p, st, *d)
	return progressed, nil
}

// next drives the encounter until nothing can move: settle what landed,
// dispatch what is ready, wait, repeat.
func next(ctx context.Context, h mux, o dispatchOpts, p Plan, st *State) (digest, error) {
	var d digest
	if o.parentPane == "" {
		o.parentPane = h.SelfPane(ctx)
	}
	deadline := time.Now().Add(o.timeout)

	for {
		if _, err := step(ctx, h, o, p, st, &d); err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(o.out, stopLine(ctx, "dungeon next"))
				break
			}
			return d, err
		}
		if o.dryRun {
			return d, nil
		}
		if workingCount(p, st) == 0 {
			break
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			fmt.Fprintln(o.out, stopLine(ctx, "dungeon next"))
			break
		}
		time.Sleep(o.poll)
	}
	return d, nil
}

// stopLine says why a loop stopped while lanes are still working: the operator
// interrupted it, or the sweep ran out of time.
func stopLine(ctx context.Context, again string) string {
	if ctx.Err() != nil {
		return "interrupted: lanes still working, run " + again + " again"
	}
	return "timeout: lanes still working, run " + again + " again"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
