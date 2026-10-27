package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Agent is one live herdr agent, keyed by the name its lane was given.
type Agent struct {
	Name   string `json:"name"`
	Pane   string `json:"pane_id"`
	Status string `json:"agent_status"`
	Cwd    string `json:"cwd"`
}

type herdrClient struct {
	bin string
}

func newHerdrClient() herdrClient { return herdrClient{bin: "herdr"} }

// run executes one herdr command and returns its stdout. Errors carry stderr,
// because a silent herdr failure is the hardest kind to debug.
func (h herdrClient) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("herdr %s: %s", strings.Join(args, " "), msg)
	}
	return out.Bytes(), nil
}

// probeAgents reads herdr's own view of the fleet. It is the only source of
// pane and liveness truth: dungeon never caches a pane id.
//
// A missing or failing herdr is not an error for the caller. A plan can be
// checked and a state edited on a machine with no session; the pane column
// then reads "-".
func (h herdrClient) agents(ctx context.Context) map[string]Agent {
	out, err := h.run(ctx, "agent", "list")
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

func probeAgents(ctx context.Context) map[string]Agent {
	return newHerdrClient().agents(ctx)
}

// createTab opens the encounter's tab and returns its root pane.
func (h herdrClient) createTab(ctx context.Context, label, cwd string) (string, error) {
	out, err := h.run(ctx, "tab", "create", "--label", label, "--cwd", cwd, "--no-focus")
	if err != nil {
		return "", err
	}
	var payload struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("herdr tab create: %w", err)
	}
	if payload.Result.RootPane.PaneID == "" {
		return "", fmt.Errorf("herdr tab create returned no root pane")
	}
	return payload.Result.RootPane.PaneID, nil
}

// splitPane carves one lane pane out of target. The ratio makes repeated splits
// of the same pane produce equal shares instead of halving the remainder.
func (h herdrClient) splitPane(ctx context.Context, target, direction, cwd string, ratio float64) (string, error) {
	out, err := h.run(ctx, "pane", "split", target,
		"--direction", direction, "--cwd", cwd,
		"--ratio", strconv.FormatFloat(ratio, 'f', 4, 64), "--no-focus")
	if err != nil {
		return "", err
	}
	var payload struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("herdr pane split: %w", err)
	}
	if payload.Result.Pane.PaneID == "" {
		return "", fmt.Errorf("herdr pane split returned no pane")
	}
	return payload.Result.Pane.PaneID, nil
}

// startAgent launches pi in a prepared pane and waits for it to be ready.
func (h herdrClient) startAgent(ctx context.Context, name, pane string, args []string) error {
	full := append([]string{"agent", "start", name, "--kind", "pi", "--pane", pane, "--"}, args...)
	_, err := h.run(ctx, full...)
	return err
}

// prompt submits text to a lane and returns as soon as it is submitted. The
// caller decides when to look again; a blocking wait would make the scheduler
// single file.
func (h herdrClient) prompt(ctx context.Context, name, text string) error {
	_, err := h.run(ctx, "agent", "prompt", name, text)
	return err
}

// laneArgs is the pi command line for a role: no skills, no MCP, a session
// directory inside the encounter, and a tool allowlist that matches the role.
func laneArgs(role, sessionDir string) []string {
	tools := "read,bash,edit,write"
	if !writes(role) {
		tools = "read,bash"
	}
	return []string{
		"--no-skills", "--no-mcp",
		"--tools", tools,
		"--session-dir", sessionDir,
	}
}
