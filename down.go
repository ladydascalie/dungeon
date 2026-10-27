package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// down stops the fleet and leaves everything the encounter wrote. Closing a pane
// kills the lane in it, so a working lane is refused unless the operator says
// otherwise: stopping work silently is the one thing this tool must not do.

type tabInfo struct {
	ID    string `json:"tab_id"`
	Label string `json:"label"`
}

func (h herdrClient) listTabs(ctx context.Context, workspace string) []tabInfo {
	if workspace == "" {
		return nil
	}
	out, err := h.run(ctx, "tab", "list", "--workspace", workspace)
	if err != nil {
		return nil
	}
	var payload struct {
		Result struct {
			Tabs []tabInfo `json:"tabs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil
	}
	return payload.Result.Tabs
}

func (h herdrClient) closePane(ctx context.Context, pane string) error {
	_, err := h.run(ctx, "pane", "close", pane)
	return err
}

func (h herdrClient) closeTab(ctx context.Context, tab string) error {
	_, err := h.run(ctx, "tab", "close", tab)
	return err
}

func down(ctx context.Context, p Plan, st *State, force bool, out io.Writer) (stopped, refused []string, err error) {
	h := newHerdrClient()
	agents := h.agents(ctx)

	for _, m := range p.Minions {
		a, live := agents[m.ID]
		if !live {
			st.SetMinion(m.ID, "absent", "")
			continue
		}
		if a.Status == "working" && !force {
			refused = append(refused, m.ID)
			fmt.Fprintf(out, "%s is working in %s; not closing it. pass --force to stop it anyway\n", m.ID, a.Pane)
			continue
		}
		if err := h.stopLane(ctx, m.ID, a.Pane, m.Pane != "" && m.Pane == a.Pane); err != nil {
			fmt.Fprintf(out, "%s: %v\n", m.ID, err)
			continue
		}
		st.SetMinion(m.ID, "absent", "")
		stopped = append(stopped, m.ID)
	}

	if len(refused) == 0 {
		for _, tab := range h.listTabs(ctx, os.Getenv("HERDR_WORKSPACE_ID")) {
			if tab.Label == p.Encounter {
				if err := h.closeTab(ctx, tab.ID); err == nil {
					break
				}
			}
		}
	}
	stopped, refused = verifyDown(ctx, h, st, stopped, refused)
	return stopped, refused, nil
}

func downCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("down", stderr)
	planPath, statePath := planFlags(fs)
	force := fs.Bool("force", false, "stop lanes that are still working")
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

	stopped, refused, err := down(ctx, p, &st, *force, stdout)
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
