package main

import (
	"context"
	"fmt"
	"io"
)

// down stops the fleet and leaves everything the encounter wrote. Closing a pane
// kills the lane in it, so a working lane is refused unless the operator says
// otherwise: stopping work silently is the one thing this tool must not do.

// downOpts is one down: what to stop, how hard, and where to say it.
type downOpts struct {
	plan  Plan
	state *State
	force bool
	out   io.Writer
}

func down(ctx context.Context, h mux, o downOpts) (stopped, refused []string, err error) {
	agents := h.Agents(ctx)

	for _, m := range o.plan.Minions {
		// The multiplexer knows the lane by the run's derived name, never the
		// bare id.
		agent := o.state.AgentName(m.ID)
		a, live := agents[agent]
		if !live {
			o.state.SetMinion(m.ID, "absent", "")
			continue
		}
		if a.Status == "working" && !o.force {
			refused = append(refused, m.ID)
			fmt.Fprintf(o.out, "%s is working in %s; not closing it. pass --force to stop it anyway\n", m.ID, a.Pane)
			continue
		}
		if err := stopLane(ctx, h, agent, a.Pane, m.Pane != "" && m.Pane == a.Pane); err != nil {
			fmt.Fprintf(o.out, "%s: %v\n", m.ID, err)
			continue
		}
		o.state.SetMinion(m.ID, "absent", "")
		stopped = append(stopped, m.ID)
	}

	if len(refused) == 0 {
		closeEncounter(ctx, h, o.plan)
	}
	stopped, refused = verifyDown(ctx, h, o.state, stopped, refused)
	return stopped, refused, nil
}

func downCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("down", stderr)
	planPath, statePath := planFlags(fs)
	backend := muxFlag(fs)
	force := fs.Bool("force", false, "stop lanes that are still working")
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
	if *statePath == "" {
		*statePath = StatePath(*planPath)
	}
	st, err := LoadState(*statePath)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if st.Encounter != "" && st.Encounter != p.Encounter {
		fmt.Fprintf(stderr, "dungeon: %s holds encounter %q, the plan is %q\n",
			*statePath, st.Encounter, p.Encounter)
		return 2
	}
	h, err := openMux(ctx, *backend)
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}

	stopped, refused, err := down(ctx, h, downOpts{plan: p, state: &st, force: *force, out: stdout})
	if err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	if err := SaveState(*statePath, st); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "stopped %d minions, %d still running\n", len(stopped), len(refused))
	if len(refused) > 0 {
		return 1
	}
	fmt.Fprintf(stdout, "the plan, the state, the briefs and the reports are still in %s\n",
		statePathDir(*statePath))
	return 0
}

func statePathDir(statePath string) string {
	if i := len(statePath) - len("/state.json"); i > 0 && statePath[i:] == "/state.json" {
		return statePath[:i]
	}
	return statePath
}
