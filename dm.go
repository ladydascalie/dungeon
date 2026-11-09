package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// dmAgent is the shape dm reports for one pane in its list-panes action.
type dmAgent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// dm is the dm backend: the only file that speaks dm's command line. dm is a
// durable multiplexer, so its panes carry the agent name in state.json and its
// OSC 7501 status is real, not inferred.
type dm struct {
	bin     string
	session string
}

func newDM() dm {
	bin := os.Getenv("DM_BIN")
	if bin == "" {
		bin = "dm"
	}
	return dm{bin: bin, session: os.Getenv("DM_SESSION")}
}

var (
	_ mux           = dm{}
	_ projectTagger = dm{}
)

// call sends one action to the session and decodes its result. dm's exec prints
// the result as JSON, and nothing at all when there is none.
func (d dm) call(ctx context.Context, action map[string]any, out any) error {
	raw, err := json.Marshal(action)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := []string{"exec", string(raw)}
	if d.session != "" {
		args = append(args, "-s", d.session)
	}
	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("dm %s: %s", action["action"], msg)
	}
	if out == nil || strings.TrimSpace(stdout.String()) == "" {
		return nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("dm %s: %w", action["action"], err)
	}
	return nil
}

// Agents reads every pane that carries a recorded agent name. dm persists the
// name in state.json, so a restarted dungeon still finds a live fleet. It uses
// list-panes, not the visible snapshot: a lane in a background workspace is
// still live.
func (d dm) Agents(ctx context.Context) (map[string]Agent, error) {
	var payload struct {
		Panes []dmAgent `json:"panes"`
	}
	if err := d.call(ctx, map[string]any{"action": "list-panes"}, &payload); err != nil {
		return nil, err
	}
	agents := map[string]Agent{}
	for _, p := range payload.Panes {
		if p.Name == "" {
			continue
		}
		agents[p.Name] = Agent{Name: p.Name, Pane: p.ID, Status: parseDMState(p.Status)}
	}
	return agents, nil
}

// parseDMState maps dm's OSC 7501 states onto the boundary. error and the
// transient "?" an old daemon reported are not a state dungeon may guess at.
func parseDMState(s string) AgentState {
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

func (d dm) SelfPane(ctx context.Context) string {
	return os.Getenv("DM_PANE")
}

func (d dm) Workspaces(ctx context.Context) []workspace {
	// The action returns a bare array.
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := d.call(ctx, map[string]any{"action": "list-workspaces"}, &list); err != nil {
		return nil
	}
	out := make([]workspace, 0, len(list))
	for _, ws := range list {
		out = append(out, workspace{ID: ws.ID, Label: ws.Name})
	}
	return out
}

func (d dm) CreateWorkspace(ctx context.Context, label, tabLabel, cwd string) (wsID, tabID, rootPane string, err error) {
	var res struct {
		Workspace string `json:"workspace"`
		Window    string `json:"window"`
		Pane      string `json:"pane"`
	}
	err = d.call(ctx, map[string]any{
		"action": "new-workspace", "name": label, "window": tabLabel,
		"cwd": cwd, "nofocus": true,
	}, &res)
	return res.Workspace, res.Window, res.Pane, err
}

// SetProject puts a workspace in a project group. dm keeps the workspace, its
// tabs and its panes, and moves the row into that group's block.
func (d dm) SetProject(ctx context.Context, ws, project string) error {
	return d.call(ctx, map[string]any{
		"action": "set-project", "target": ws, "name": project,
	}, nil)
}

func (d dm) CreateTab(ctx context.Context, ws, label, cwd string) (tabID, rootPane string, err error) {
	var res struct {
		Window string `json:"window"`
		Pane   string `json:"pane"`
	}
	action := map[string]any{"action": "new-window", "name": label, "cwd": cwd, "nofocus": true}
	if ws != "" {
		action["target"] = ws
	}
	err = d.call(ctx, action, &res)
	return res.Window, res.Pane, err
}

func (d dm) SplitPane(ctx context.Context, target, direction, cwd string, ratio float64) (string, error) {
	var res struct {
		Pane string `json:"pane"`
	}
	err := d.call(ctx, map[string]any{
		"action": "split", "target": target, "dir": direction,
		"cwd": cwd, "ratio": ratio, "nofocus": true,
	}, &res)
	if err != nil {
		return "", err
	}
	if res.Pane == "" {
		return "", fmt.Errorf("dm split returned no pane")
	}
	return res.Pane, nil
}

func (d dm) Pane(ctx context.Context, id string) (pane, error) {
	var res struct {
		ID        string `json:"id"`
		Workspace string `json:"workspace"`
		Window    string `json:"window"`
	}
	err := d.call(ctx, map[string]any{"action": "pane", "target": id}, &res)
	if err != nil {
		return pane{}, err
	}
	if res.ID == "" {
		res.ID = id
	}
	return pane{ID: res.ID, Tab: res.Window, Workspace: res.Workspace}, nil
}

func (d dm) Read(ctx context.Context, id string) (string, error) {
	var res struct {
		Lines []string `json:"lines"`
	}
	if err := d.call(ctx, map[string]any{"action": "read", "target": id}, &res); err != nil {
		return "", err
	}
	return strings.Join(res.Lines, "\n"), nil
}

// StartAgent records the agent name in dm, then runs the command line in the
// pane. It waits until the pane reports a status, which is the signal that the
// program is up and the feature query was answered.
func (d dm) StartAgent(ctx context.Context, name, pane string, args []string) error {
	if err := d.call(ctx, map[string]any{
		"action": "set-pane-name", "target": pane, "name": name,
	}, nil); err != nil {
		return err
	}
	started := false
	defer func() {
		if started {
			return
		}
		// A failed start must not leave a name on a pane with no agent, or a
		// later up sees a live minion that does not exist.
		unset, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = d.call(unset, map[string]any{
			"action": "set-pane-name", "target": pane, "name": "",
		}, nil)
	}()
	// args are pi flags; the binary is implied by the mux, like tmux's respawn.
	line := "exec " + shellJoin(append([]string{"pi"}, args...))
	if err := d.call(ctx, map[string]any{
		"action": "write-to", "target": pane, "msg": line, "enter": true,
	}, nil); err != nil {
		return err
	}
	// dm answers OSC 7501, so pi reports idle once it is ready. An unknown
	// status after the deadline means the program never came up.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var st struct {
			State string `json:"state"`
		}
		if err := d.call(ctx, map[string]any{"action": "status", "target": pane}, &st); err == nil {
			if st.State != "" && st.State != "unknown" {
				started = true
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dm: agent %s in pane %s did not report ready", name, pane)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (d dm) Prompt(ctx context.Context, name, text string) error {
	p, err := d.agentPane(ctx, name)
	if err != nil {
		return err
	}
	return d.call(ctx, map[string]any{
		"action": "write-to", "target": p, "msg": text, "enter": true,
	}, nil)
}

func (d dm) SendKeys(ctx context.Context, name string, keys ...string) error {
	p, err := d.agentPane(ctx, name)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := d.call(ctx, map[string]any{
			"action": "keys", "target": p, "keys": base64.StdEncoding.EncodeToString([]byte(key)),
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

// PingCommand is the line a lane runs in its own shell. dm's own quoting is not
// exposed, so this file owns it.
func (d dm) PingCommand(pane, message string) string {
	// tmux sends the text and then Enter; a typed but unsubmitted line does not
	// wake an agent reading its own input.
	args := []string{d.bin, "write-to", pane, "--msg", shellQuote(message), "--enter"}
	if d.session != "" {
		args = append(args, "-s", d.session)
	}
	return strings.Join(args, " ")
}

// BoxPing is the line a lane runs to post its finish to the session box. The
// parent reads the box; no pane is typed into. The key names the lane, so the
// parent can filter with --prefix lane/ and map the entry back to a minion.
func (d dm) BoxPing(key, message string) string {
	args := []string{d.bin, "msg", "post", "--key", key, "--state", "done", "--msg", shellQuote(message)}
	if d.session != "" {
		args = append(args, "-s", d.session)
	}
	return strings.Join(args, " ")
}

func (d dm) FocusAgent(ctx context.Context, name string) error {
	p, err := d.agentPane(ctx, name)
	if err != nil {
		return err
	}
	return d.call(ctx, map[string]any{"action": "focus", "target": p}, nil)
}

func (d dm) FocusTab(ctx context.Context, tab string) error {
	return d.call(ctx, map[string]any{"action": "focus-window", "target": tab}, nil)
}

func (d dm) ClosePane(ctx context.Context, pane string) error {
	return ignoreGone(d.call(ctx, map[string]any{"action": "close-pane", "target": pane}, nil), "no pane")
}

func (d dm) CloseWorkspace(ctx context.Context, ws string) error {
	return ignoreGone(d.call(ctx, map[string]any{"action": "close-workspace", "target": ws}, nil), "no workspace")
}

// MarkReadyToReap posts a done message to the lane's key, so the multiplexer
// can actively find and reap the pane once the lane is no longer useful.
func (d dm) MarkReadyToReap(ctx context.Context, name string) error {
	return d.call(ctx, map[string]any{
		"action": "msg-post",
		"target": name,
		"res": map[string]any{
			"key":   name,
			"state": "done",
			"msg":   "lane done, ready to be reaped",
		},
	}, nil)
}

// ignoreGone treats closing something already closed as success. `down` may
// close a workspace's panes and then each minion's pane; the second call is not
// an error.
func ignoreGone(err error, marker string) error {
	if err != nil && strings.Contains(err.Error(), marker) {
		return nil
	}
	return err
}

// agentPane resolves the agent name through dm, never through a local map, so
// a restarted dungeon addresses the pane that exists now.
func (d dm) agentPane(ctx context.Context, name string) (string, error) {
	agents, err := d.Agents(ctx)
	if err != nil {
		return "", err
	}
	a, ok := agents[name]
	if !ok {
		return "", fmt.Errorf("dm: no agent %q", name)
	}
	return a.Pane, nil
}

// shellJoin quotes argv for a POSIX shell.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}
