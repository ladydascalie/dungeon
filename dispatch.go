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
	out           io.Writer
}

type digest struct {
	Dispatched []string
	Done       []string
	Rejected   []string
	Unproven   []string
	Blocked    []string
	NoLane     []string
}

func (d digest) empty() bool {
	return len(d.Dispatched)+len(d.Done)+len(d.Rejected)+len(d.Unproven)+len(d.Blocked)+len(d.NoLane) == 0
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
	return filepath.Join(filepath.Dir(planPath), "briefs", unit+".md")
}

func reportPath(planPath, unit string) string {
	return filepath.Join(filepath.Dir(planPath), "reports", unit+".md")
}

func minionDir(p Plan, m Minion) string {
	if writes(m.Role) {
		return worktreePath(p.Repo, p.Encounter, m.ID)
	}
	return p.Repo
}

// briefText is the whole contract the lane gets. It names what to do, what it
// owns, what it may not touch, and where to leave the report.
func briefText(p Plan, m Minion, u Unit, dir, report string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", u.ID, u.Goal)
	fmt.Fprintf(&b, "minion: %s (%s)\nworktree: %s\n", m.ID, m.Role, dir)
	if len(u.Paths) > 0 {
		b.WriteString("paths you own:\n")
		for _, path := range u.Paths {
			fmt.Fprintf(&b, "  - %s\n", path)
		}
	}
	if len(u.Deps) > 0 {
		fmt.Fprintf(&b, "deps that landed: %s\n", strings.Join(u.Deps, " "))
	}
	if u.Accept != "" {
		fmt.Fprintf(&b, "accept: %s\n", u.Accept)
	} else {
		b.WriteString("accept: none. This unit cannot be marked done, so say plainly what you did.\n")
	}
	fmt.Fprintf(&b, "\nreport: %s\n", report)
	b.WriteString(`
## What you may not do

- Touch a file outside the paths above.
- Edit the plan or the state file. They are not yours.
- Claim work you did not do.

Write the report to the path above. Keep it short: what changed, what you ran,
what you did not do.
`)
	return b.String()
}

func writeBrief(planPath string, p Plan, m Minion, u Unit) (string, error) {
	path := briefPath(planPath, u.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// Every path in the brief is absolute: the lane runs in a worktree, where a
	// relative .dungeon/ does not exist.
	absReport, err := filepath.Abs(reportPath(planPath, u.ID))
	if err != nil {
		return "", err
	}
	text := briefText(p, m, u, minionDir(p, m), absReport)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func dispatchUnit(ctx context.Context, h herdrClient, o dispatchOpts, p Plan, st *State, u Unit, m Minion) error {
	brief, err := writeBrief(o.planPath, p, m, u)
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
	if err := h.prompt(ctx, m.ID, text); err != nil {
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

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if line == "" {
		line = "no output"
	}
	return truncate(line, 120)
}

// reconcile looks at every working unit and settles the ones whose lane has
// stopped. A lane that is still working, or that herdr cannot classify, is left
// alone: guessing would settle work that is still running.
func reconcile(ctx context.Context, h herdrClient, o dispatchOpts, p Plan, st *State, d *digest) bool {
	agents := h.agents(ctx)
	progressed := false
	for _, u := range p.Units {
		if st.UnitStatus(u.ID) != "working" {
			continue
		}
		m, ok := findMinion(p, u.Minion)
		if !ok {
			continue
		}
		a, live := agents[m.ID]
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
				d.Done = append(d.Done, u.ID)
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

// readyUnits returns the planned units whose deps have all landed.
func readyUnits(p Plan, st *State) []Unit {
	var out []Unit
	for _, u := range p.Units {
		if st.UnitStatus(u.ID) != "planned" {
			continue
		}
		ready := true
		for _, d := range u.Deps {
			if st.UnitStatus(d) != "done" {
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
func next(ctx context.Context, o dispatchOpts, p Plan, st *State) (digest, error) {
	var d digest
	h := newHerdrClient()
	deadline := time.Now().Add(o.timeout)

	for {
		reconcile(ctx, h, o, p, st, &d)

		slots := len(p.Minions)
		if p.Concurrency > 0 {
			slots = p.Concurrency - workingCount(p, st)
		}
		agents := h.agents(ctx)
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
			if _, live := agents[m.ID]; !live {
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
