package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// herdrAgent is herdr's wire shape for one agent. It stays private so no other
// file has to know herdr's JSON.
type herdrAgent struct {
	Name   string `json:"name"`
	Pane   string `json:"pane_id"`
	Status string `json:"agent_status"`
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
// liveness truth: dungeon never caches a pane id. An error means herdr could
// not be read, which is not the same as an empty fleet.
func (h herdr) Agents(ctx context.Context) (map[string]Agent, error) {
	out, err := h.call(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Result struct {
			Agents []herdrAgent `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	byName := make(map[string]Agent, len(payload.Result.Agents))
	for _, a := range payload.Result.Agents {
		if a.Name == "" {
			continue
		}
		byName[a.Name] = Agent{Name: a.Name, Pane: a.Pane, Status: parseAgentState(a.Status)}
	}
	return byName, nil
}

// parseAgentState maps herdr's status words onto the boundary's vocabulary. A
// word dungeon does not know is not guessed at.
func parseAgentState(s string) AgentState {
	switch AgentState(s) {
	case AgentWorking:
		return AgentWorking
	case AgentIdle:
		return AgentIdle
	case AgentBlocked:
		return AgentBlocked
	case AgentDone:
		return AgentDone
	}
	return AgentUnknown
}

// SelfPane is the pane dungeon runs in, which is where a lane pings its parent.
func (h herdr) SelfPane(ctx context.Context) string {
	return os.Getenv("HERDR_PANE_ID")
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

// PingCommand is the command a shell runs to deliver a message to pane. Quoting
// is herdr's, so the caller passes the message and gets a finished line.
func (h herdr) PingCommand(pane, message string) string {
	return fmt.Sprintf("%s pane run %s %s", h.bin, pane, shellQuote(message))
}

// shellQuote wraps s in single quotes, where a POSIX shell expands nothing. An
// embedded quote is closed, escaped and reopened, so $, a backtick or a newline
// arrives at the pane as written.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
