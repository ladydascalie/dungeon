package main

import (
	"context"
	"fmt"
	"os"
)

// The boundary picks a backend in one place. Which multiplexer dungeon talks to
// is a property of the session, not of any command: --mux forces one, and the
// environment names one otherwise.

// openMux builds the backend the operator asked for, or the one the environment
// speaks. An error means there is no backend to open, so a command that acts on
// panes cannot run.
func openMux(ctx context.Context, forced string) (mux, error) {
	if forced != "" {
		return backendByName(forced)
	}
	switch {
	case os.Getenv("HERDR_ENV") == "1":
		return newHerdr(), nil
	case os.Getenv("DM_SESSION") != "":
		return newDM(), nil
	case os.Getenv("TMUX") != "":
		return newTmux(), nil
	}
	return newDM(), nil
}

// backendByName is the backend table. tmux lands here when its backend exists.
func backendByName(name string) (mux, error) {
	switch name {
	case "herdr":
		return newHerdr(), nil
	case "dm":
		return newDM(), nil
	case "tmux":
		return newTmux(), nil
	}
	return nil, fmt.Errorf("dungeon: unknown mux %q, want herdr, dm or tmux", name)
}

// probeFleet is the fleet read with the reason it failed, so a command that acts
// on the answer can say why it judged on something else. A nil map with no
// reason is an empty fleet; a non-empty reason is an unreadable one.
func probeFleet(ctx context.Context, forced string) (map[string]Agent, string) {
	h, err := openMux(ctx, forced)
	if err != nil {
		return nil, err.Error()
	}
	agents, err := h.Agents(ctx)
	if err != nil {
		return nil, err.Error()
	}
	return agents, ""
}

// probeAgents reads the fleet for a command that only displays it. No
// multiplexer is not an error there: the pane column reads "-".
func probeAgents(ctx context.Context, forced string) map[string]Agent {
	agents, _ := probeFleet(ctx, forced)
	return agents
}
