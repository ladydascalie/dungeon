package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultPlanPath = ".dungeon/plan.yaml"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// Exit codes are part of the contract: 0 clean, 1 findings, 2 could not run.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "check":
		return check(args[1:], stdout, stderr)
	case "status":
		return statusCmd(ctx, args[1:], stdout, stderr)
	case "set":
		return setCmd(ctx, args[1:], stdout, stderr)
	case "view":
		return viewCmd(ctx, args[1:], stdout, stderr)
	case "link":
		return linkCmd(args[1:], stdout, stderr)
	case "attach":
		return attachCmd(args[1:], stdout, stderr)
	case "init":
		return initCmd(args[1:], stdout, stderr)
	case "up":
		return upCmd(ctx, args[1:], stdout, stderr)
	case "next":
		return nextCmd(ctx, args[1:], stdout, stderr)
	case "land":
		return landCmd(ctx, args[1:], stdout, stderr)
	case "down":
		return downCmd(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "dungeon: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `dungeon — a plan that dispatches

  dungeon init   [--dir path] [--encounter name] [--base branch] [--force]
      Write a starter .dungeon/plan.yaml, and a .dungeon/.gitignore that keeps
      the run out of git while the plan stays in it.

  dungeon check  [--json] [--plan path] [path]
      Validate a plan. Exit 0 clean, 1 findings, 2 unreadable.

  dungeon status [--json] [--plan path] [--state path]
      Who is working, who is not, and which pane they are in.

  dungeon set unit   <id> <status> [--note text]
  dungeon set minion <id> <status> [--note text]
      Record a change. Unit statuses: blocked, working, planned,
      unproven, rejected, done, canceled.
      Minion statuses: spawned, busy, waiting, lost, absent.

  dungeon link <unit> <url> | --clear
      Pin a pull request, review or any http(s) url to a unit. The view shows
      it and opens it with o, whatever the forge.

  dungeon attach <unit> <path>
      Keep a file with a unit: a brief, a report, a handoff note. It shows up
      in the detail pane of dungeon view.

  dungeon up     [--no-dispatch] [--split right|down|left|up]
      Spawn a workspace, a tab per role and a pane per minion. Panes grow to the
      right by default, because tall and narrow beats short and wide; the plan's
      split: field or --split changes it. A writer gets its own git worktree on
      <encounter>/<minion>. Idempotent: a live minion is left alone.

  dungeon next   [--timeout 30m] [--accept-timeout 10m] [--poll 2s] [--dry-run]
      Settle the lanes that stopped, dispatch the units whose deps landed, and
      wait. The only path to done is the unit's accept command exiting 0.

  dungeon land   [--no-gate] [--plan path] [--state path]
      Merge the done units into <encounter>/integration in declaration order,
      then run each unit's accept command against the merged result. It never
      touches the base branch and never pushes.

  dungeon down   [--force] [--plan path] [--state path]
      Close the lanes and the encounter tab. A working lane is refused unless
      --force. The plan, the state, the briefs and the reports stay.

  dungeon view   [--once] [--interval 2s] [--plan path] [--state path]
      The live table. j/k move, g/G ends, r refresh, q quit.
      Piped output and --once print one frame instead.

The plan defaults to `+defaultPlanPath+`. State lives beside it in state.json.

`)
}

// parseMixed lets flags follow positionals. The stdlib flag package stops at
// the first positional, and `dungeon set unit ATOM1 working --note x` is the
// shape people type.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue // let Parse report the unknown flag
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return pos, nil
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func planFlags(fs *flag.FlagSet) (*string, *string) {
	plan := fs.String("plan", defaultPlan(), "path to the plan")
	state := fs.String("state", "", "path to state.json (default: beside the plan)")
	return plan, state
}

func check(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("check", stderr)
	asJSON := fs.Bool("json", false, "print findings as a JSON array")
	plan, _ := planFlags(fs)
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) > 0 {
		*plan = pos[0]
	}

	p, findings, err := DecodeFile(*plan)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if findings == nil {
		findings = Validate(p)
	}
	if findings == nil {
		findings = []Finding{}
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(findings); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
	} else if len(findings) == 0 {
		fmt.Fprintf(stdout, "ok: %s, %d minions, %d units\n",
			p.Encounter, len(p.Minions), len(p.Units))
	} else {
		for _, f := range findings {
			fmt.Fprintln(stdout, f)
		}
	}

	if len(findings) > 0 {
		return 1
	}
	return 0
}

func statusCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	asJSON := fs.Bool("json", false, "print the snapshot as JSON")
	planPath, statePath := planFlags(fs)
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) > 0 {
		*planPath = pos[0]
	}

	p, code := loadValidPlan(*planPath, stderr)
	if code != 0 {
		return code
	}
	if *statePath == "" {
		*statePath = StatePath(*planPath)
	}
	st, err := LoadState(*statePath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}

	snap := buildSnapshot(p, *planPath, st, probeAgents(ctx))
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(snap); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		return 0
	}
	renderText(stdout, snap)
	return 0
}

func setCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("set", stderr)
	note := fs.String("note", "", "why the status changed")
	planPath, statePath := planFlags(fs)
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) != 3 {
		fmt.Fprintln(stderr, "usage: dungeon set unit|minion <id> <status> [--note text]")
		return 2
	}
	kind, id, status := rest[0], rest[1], rest[2]

	p, code := loadValidPlan(*planPath, stderr)
	if code != 0 {
		return code
	}
	if *statePath == "" {
		*statePath = StatePath(*planPath)
	}
	st, err := LoadState(*statePath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if st.Encounter == "" {
		st.Encounter = p.Encounter
	}
	if st.Encounter != p.Encounter {
		fmt.Fprintf(stderr, "dungeon: %s holds encounter %q, the plan is %q\n",
			*statePath, st.Encounter, p.Encounter)
		return 2
	}

	switch kind {
	case "unit":
		u, ok := findUnit(p, id)
		if !ok {
			fmt.Fprintf(stderr, "dungeon: unit %q is not in %s\n", id, *planPath)
			return 2
		}
		before := st.UnitStatus(id)
		if err := st.SetUnit(id, status, *note); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		if err := SaveState(*statePath, st); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "%s %s (was %s)\n", id, status, before)
		if status == "working" && u.Minion != "" {
			if _, live := probeAgents(ctx)[u.Minion]; !live {
				fmt.Fprintf(stderr, "warning: minion %q has no live agent, so nothing is in that pane\n", u.Minion)
			}
		}
	case "minion":
		if _, ok := findMinion(p, id); !ok {
			fmt.Fprintf(stderr, "dungeon: minion %q is not in %s\n", id, *planPath)
			return 2
		}
		before := st.MinionStatus(id)
		if err := st.SetMinion(id, status, *note); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		if err := SaveState(*statePath, st); err != nil {
			fmt.Fprintf(stderr, "dungeon: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "%s %s (was %s)\n", id, status, before)
	default:
		fmt.Fprintf(stderr, "dungeon: set takes unit or minion, not %q\n", kind)
		return 2
	}
	return 0
}

// loadValidPlan refuses to work on a plan that does not validate: a status
// table over a broken plan is a lie with columns.
func loadValidPlan(path string, stderr io.Writer) (Plan, int) {
	p, findings, err := DecodeFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return Plan{}, 2
	}
	if findings == nil {
		findings = Validate(p)
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(stderr, f)
		}
		fmt.Fprintf(stderr, "dungeon: %d findings in %s\n", len(findings), path)
		return Plan{}, 1
	}
	return p, 0
}

func dispatchFlags(fs *flag.FlagSet, statePath *string) (*dispatchOpts, *string) {
	o := &dispatchOpts{}
	plan := fs.String("plan", defaultPlan(), "path to the plan")
	fs.StringVar(statePath, "state", "", "path to state.json (default: beside the plan)")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Minute, "how long next may wait for lanes")
	fs.DurationVar(&o.acceptTimeout, "accept-timeout", 10*time.Minute, "how long one accept command may run")
	fs.DurationVar(&o.poll, "poll", 2*time.Second, "how often to look at the lanes")
	return o, plan
}

func upCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("up", stderr)
	var statePath string
	o, planPath := dispatchFlags(fs, &statePath)
	noDispatch := fs.Bool("no-dispatch", false, "spawn the minions and stop")
	split := fs.String("split", "", "override the plan's split direction: right, down, left, up")
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		*planPath = rest[0]
	}
	p, code := loadValidPlan(*planPath, stderr)
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
	if st.Encounter == "" {
		st.Encounter = p.Encounter
	}
	if st.Encounter != p.Encounter {
		fmt.Fprintf(stderr, "dungeon: %s holds encounter %q, the plan is %q\n",
			statePath, st.Encounter, p.Encounter)
		return 2
	}

	res, err := spawnMinions(ctx, *planPath, p, &st, *split, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if err := SaveState(statePath, st); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprintln(stdout, res)
	if *noDispatch || len(res.Failed) > 0 {
		return 0
	}

	o.planPath, o.statePath, o.out = *planPath, statePath, stdout
	d, err := next(ctx, *o, p, &st)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(stdout, d)
	return 0
}

func nextCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("next", stderr)
	var statePath string
	o, planPath := dispatchFlags(fs, &statePath)
	dryRun := fs.Bool("dry-run", false, "say what would be dispatched")
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		*planPath = rest[0]
	}
	p, code := loadValidPlan(*planPath, stderr)
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
	if st.Encounter != "" && st.Encounter != p.Encounter {
		fmt.Fprintf(stderr, "dungeon: %s holds encounter %q, the plan is %q\n",
			statePath, st.Encounter, p.Encounter)
		return 2
	}
	o.planPath, o.statePath, o.out, o.dryRun = *planPath, statePath, stdout, *dryRun

	d, err := next(ctx, *o, p, &st)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprint(stdout, d)
	return 0
}

func linkCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("link", stderr)
	planPath, statePath := planFlags(fs)
	clear := fs.Bool("clear", false, "forget the link")
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) < 1 || (len(rest) < 2 && !*clear) {
		fmt.Fprintln(stderr, "usage: dungeon link <unit> <url> | --clear")
		return 2
	}
	unit := rest[0]
	raw := ""
	if len(rest) > 1 {
		raw = rest[1]
	}
	if *clear {
		raw = ""
	}

	p, code := loadValidPlan(*planPath, stderr)
	if code != 0 {
		return code
	}
	if _, ok := findUnit(p, unit); !ok {
		fmt.Fprintf(stderr, "dungeon: unit %q is not in %s\n", unit, *planPath)
		return 2
	}
	if *statePath == "" {
		*statePath = StatePath(*planPath)
	}
	st, err := LoadState(*statePath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if st.Encounter == "" {
		st.Encounter = p.Encounter
	}
	if st.Encounter != p.Encounter {
		fmt.Fprintf(stderr, "dungeon: %s holds encounter %q, the plan is %q\n", *statePath, st.Encounter, p.Encounter)
		return 2
	}
	if err := st.SetLink(unit, raw); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if err := SaveState(*statePath, st); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if raw == "" {
		fmt.Fprintf(stdout, "%s link cleared\n", unit)
		return 0
	}
	fmt.Fprintf(stdout, "%s %s\n", unit, st.Units[unit].Link)
	return 0
}

func attachCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("attach", stderr)
	planPath, statePath := planFlags(fs)
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) != 2 {
		fmt.Fprintln(stderr, "usage: dungeon attach <unit> <path>")
		return 2
	}
	unit, path := rest[0], rest[1]

	p, code := loadValidPlan(*planPath, stderr)
	if code != 0 {
		return code
	}
	if _, ok := findUnit(p, unit); !ok {
		fmt.Fprintf(stderr, "dungeon: unit %q is not in %s\n", unit, *planPath)
		return 2
	}
	if *statePath == "" {
		*statePath = StatePath(*planPath)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		fmt.Fprintf(stderr, "dungeon: %s is not a readable file\n", path)
		return 2
	}
	st, err := LoadState(*statePath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if st.Encounter == "" {
		st.Encounter = p.Encounter
	}
	if st.Encounter != p.Encounter {
		fmt.Fprintf(stderr, "dungeon: %s holds encounter %q, the plan is %q\n",
			*statePath, st.Encounter, p.Encounter)
		return 2
	}
	if err := st.Attach(unit, abs); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if err := SaveState(*statePath, st); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "%s attached %s\n", unit, abs)
	return 0
}

func findUnit(p Plan, id string) (Unit, bool) {
	for _, u := range p.Units {
		if u.ID == id {
			return u, true
		}
	}
	return Unit{}, false
}

func findMinion(p Plan, id string) (Minion, bool) {
	for _, m := range p.Minions {
		if m.ID == id {
			return m, true
		}
	}
	return Minion{}, false
}
