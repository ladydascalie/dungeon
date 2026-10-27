package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// Agent is one live herdr agent, keyed by the name its lane was given.
type Agent struct {
	Name   string `json:"name"`
	Pane   string `json:"pane_id"`
	Status string `json:"agent_status"`
	Cwd    string `json:"cwd"`
}

// probeAgents reads herdr's own view of the fleet. It is the only source of
// pane and liveness truth: dungeon never caches a pane id.
//
// A missing or failing herdr is not an error for the caller. A plan can be
// checked and a state edited on a machine with no session; the pane column
// then reads "-".
func probeAgents(ctx context.Context) map[string]Agent {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "herdr", "agent", "list").Output()
	if err != nil {
		return nil
	}
	var payload struct {
		Result struct {
			Agents []Agent `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil
	}
	byName := make(map[string]Agent, len(payload.Result.Agents))
	for _, a := range payload.Result.Agents {
		if a.Name != "" {
			byName[a.Name] = a
		}
	}
	return byName
}
