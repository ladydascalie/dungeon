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

// herdr is the herdr backend: the only file that speaks herdr's command line.
// Everything above it speaks mux.
type herdr struct {
	bin string
}

func newHerdr() herdr { return herdr{bin: "herdr"} }

var _ mux = herdr{}

// call executes one herdr command and returns its stdout. Errors carry stderr,
// because a silent herdr failure is the hardest kind to debug.
func (h herdr) call(ctx context.Context, args ...string) ([]byte, error) {
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

// Agents reads herdr's own view of the fleet. It is the only source of pane and
// liveness truth: dungeon never caches a pane id.
//
// A missing or failing herdr is not an error for the caller. A plan can be
// checked and a state edited on a machine with no session; the pane column
// then reads "-".
func (h herdr) Agents(ctx context.Context) map[string]Agent {
	out, err := h.call(ctx, "agent", "list")
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
		if a.Name == "" {
			continue
		}
		byName[a.Name] = a
	}
	return byName
}

// StartAgent launches pi in a prepared pane and waits for it to be ready.
// name is the derived agent name, not the plan's minion id.
func (h herdr) StartAgent(ctx context.Context, name, pane string, args []string) error {
	full := append([]string{"agent", "start", name, "--kind", "pi", "--pane", pane, "--"}, args...)
	_, err := h.call(ctx, full...)
	return err
}

// Prompt submits text to a lane by its derived name and returns as soon as it
// is submitted. The caller decides when to look again; a blocking wait would
// make the scheduler single file.
func (h herdr) Prompt(ctx context.Context, name, text string) error {
	_, err := h.call(ctx, "agent", "prompt", name, text)
	return err
}

func (h herdr) SendKeys(ctx context.Context, name string, keys ...string) error {
	_, err := h.call(ctx, append([]string{"agent", "send-keys", name}, keys...)...)
	return err
}

// Read returns the text herdr renders for a pane.
func (h herdr) Read(ctx context.Context, pane string) (string, error) {
	out, err := h.call(ctx, "pane", "read", pane)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Run types one command line into a pane and submits it.
func (h herdr) Run(ctx context.Context, pane, text string) error {
	_, err := h.call(ctx, "pane", "run", pane, text)
	return err
}

// PingCommand is the command a shell runs to deliver a message to pane. The
// caller quotes the message and appends it.
func (h herdr) PingCommand(pane string) string {
	return fmt.Sprintf("%s pane run %s", h.bin, pane)
}

// FocusAgent moves the operator to the pane hosting the agent.
func (h herdr) FocusAgent(ctx context.Context, name string) error {
	_, err := h.call(ctx, "agent", "focus", name)
	return err
}

// Pane reports one pane, and whether herdr still knows it.
func (h herdr) Pane(ctx context.Context, id string) (pane, error) {
	out, err := h.call(ctx, "pane", "get", id)
	if err != nil {
		return pane{}, err
	}
	var payload struct {
		Result struct {
			Pane struct {
				ID        string `json:"pane_id"`
				Tab       string `json:"tab_id"`
				Workspace string `json:"workspace_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return pane{}, fmt.Errorf("herdr pane get: %w", err)
	}
	p := pane{
		ID:        payload.Result.Pane.ID,
		Tab:       payload.Result.Pane.Tab,
		Workspace: payload.Result.Pane.Workspace,
	}
	if p.ID == "" {
		p.ID = id
	}
	return p, nil
}

// SplitPane carves one pane out of target. The ratio makes repeated splits of
// the same pane produce equal shares instead of halving the remainder.
func (h herdr) SplitPane(ctx context.Context, target, direction, cwd string, ratio float64) (string, error) {
	out, err := h.call(ctx, "pane", "split", target,
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

func (h herdr) ClosePane(ctx context.Context, pane string) error {
	_, err := h.call(ctx, "pane", "close", pane)
	return err
}

func (h herdr) CloseTab(ctx context.Context, tab string) error {
	_, err := h.call(ctx, "tab", "close", tab)
	return err
}

// laneArgs is the pi command line for a role: no skills, no MCP, a session
// directory inside the encounter, and a tool allowlist. A minion's own tools
// list replaces the role default; an empty one keeps the default.
func laneArgs(role, tools, sessionDir string) []string {
	if tools == "" {
		tools = "read,bash,edit,write"
		if !writes(role) {
			tools = "read,bash"
		}
	}
	return []string{
		"--no-skills", "--no-mcp",
		"--tools", tools,
		"--session-dir", sessionDir,
	}
}
