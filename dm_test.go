package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeDM installs a dm script on PATH that logs every call and answers the
// actions the backend sends. It is the same seam the tmux tests use.
func fakeDM(t *testing.T) (bin string, logFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "dm")
	logFile = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$2" in
  *'"action":"list-panes"'*)
    printf '{"panes":[{"id":"p9abc","name":"w1","status":"working"},{"id":"pX","status":"idle"}]}' ;;
  *'"action":"list-workspaces"'*)
    printf '[{"id":"s1","name":"enc"}]' ;;
  *'"action":"split"'*)
    printf '{"pane":"pNEW"}' ;;
  *'"action":"new-workspace"'*)
    printf '{"workspace":"sNEW","window":"wNEW","pane":"pROOT"}' ;;
  *'"action":"new-window"'*)
    printf '{"window":"w2","pane":"p3"}' ;;
  *'"action":"pane"'*)
    printf '{"id":"p9abc","workspace":"s1","window":"w1"}' ;;
  *'"action":"read"'*)
    printf '{"lines":["one","two"]}' ;;
  *'"action":"status"'*)
    printf '{"state":"idle"}' ;;
  *'"action":"close-pane"'*)
    case "$2" in *gone*) echo 'dm: no pane "gone"' >&2; exit 1 ;; esac ;;
  *'"action":"close-workspace"'*)
    case "$2" in *gone*) echo 'dm: no workspace "gone"' >&2; exit 1 ;; esac ;;
  *)
    : ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DM_BIN", bin)
	t.Setenv("FAKE_LOG", logFile)
	return bin, logFile
}

func callLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestDMAgentsReadsNamedPanesOnly(t *testing.T) {
	fakeDM(t)
	d := newDM()
	agents, err := d.Agents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %v, want only the named pane", agents)
	}
	a, ok := agents["w1"]
	if !ok || a.Pane != "p9abc" || a.Status != AgentWorking {
		t.Fatalf("agent = %+v", a)
	}
}

func TestDMSplitSendsRatioAndKeepsFocus(t *testing.T) {
	_, log := fakeDM(t)
	d := newDM()
	id, err := d.SplitPane(t.Context(), "p9abc", "right", "/src", 0.25)
	if err != nil {
		t.Fatal(err)
	}
	if id != "pNEW" {
		t.Fatalf("id = %q", id)
	}
	line := callLog(t, log)
	for _, want := range []string{`"action":"split"`, `"target":"p9abc"`, `"dir":"right"`, `"cwd":"/src"`, `"ratio":0.25`, `"nofocus":true`} {
		if !strings.Contains(line, want) {
			t.Fatalf("split call missing %s: %s", want, line)
		}
	}
}

func TestDMCreateWorkspaceAndTab(t *testing.T) {
	_, log := fakeDM(t)
	d := newDM()
	ws, tab, root, err := d.CreateWorkspace(t.Context(), "enc", "worker", "/src")
	if err != nil {
		t.Fatal(err)
	}
	if ws != "sNEW" || tab != "wNEW" || root != "pROOT" {
		t.Fatalf("ids = %q %q %q", ws, tab, root)
	}
	id, pane, err := d.CreateTab(t.Context(), "sNEW", "reviewer", "/src")
	if err != nil {
		t.Fatal(err)
	}
	if id != "w2" || pane != "p3" {
		t.Fatalf("tab = %q %q", id, pane)
	}
	line := callLog(t, log)
	if !strings.Contains(line, `"action":"new-workspace"`) || !strings.Contains(line, `"action":"new-window"`) {
		t.Fatalf("calls: %s", line)
	}
	if !strings.Contains(line, `"target":"sNEW"`) {
		t.Fatalf("new-window did not name its workspace: %s", line)
	}
}

func TestDMPaneAndRead(t *testing.T) {
	fakeDM(t)
	d := newDM()
	p, err := d.Pane(t.Context(), "p9abc")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "p9abc" || p.Tab != "w1" || p.Workspace != "s1" {
		t.Fatalf("pane = %+v", p)
	}
	text, err := d.Read(t.Context(), "p9abc")
	if err != nil || text != "one\ntwo" {
		t.Fatalf("read = %q, %v", text, err)
	}
}

func TestDMStartAgentRecordsNameThenRuns(t *testing.T) {
	_, log := fakeDM(t)
	d := newDM()
	err := d.StartAgent(t.Context(), "w1", "p9abc", []string{"--no-mcp", "--session-dir", "/tmp/s"})
	if err != nil {
		t.Fatal(err)
	}
	line := callLog(t, log)
	nameAt := strings.Index(line, `"action":"set-pane-name"`)
	runAt := strings.Index(line, `"action":"write-to"`)
	if nameAt < 0 || runAt < 0 || nameAt > runAt {
		t.Fatalf("name must be recorded before the run: %s", line)
	}
	if !strings.Contains(line, `exec 'pi' '--no-mcp' '--session-dir' '/tmp/s'`) {
		t.Fatalf("command not shell-joined: %s", line)
	}
}

func TestDMPingCommandQuotes(t *testing.T) {
	t.Setenv("DM_SESSION", "main")
	d := newDM()
	got := d.PingCommand("p9abc", "it's $HOME `x`")
	want := `dm write-to p9abc --msg 'it'\''s $HOME ` + "`x`" + `' --enter -s main`
	if got != want {
		t.Fatalf("ping = %q, want %q", got, want)
	}
}

func TestDMBoxPingQuotes(t *testing.T) {
	t.Setenv("DM_SESSION", "main")
	d := newDM()
	got := d.BoxPing("lane/w1", "it's $HOME `x`")
	want := `dm msg post --key lane/w1 --state done --msg 'it'\''s $HOME ` + "`x`" + `' -s main`
	if got != want {
		t.Fatalf("box ping = %q, want %q", got, want)
	}
}

func TestDMSendKeysSendsEachKey(t *testing.T) {
	_, log := fakeDM(t)
	d := newDM()
	if err := d.SendKeys(t.Context(), "w1", "a", "\x1b"); err != nil {
		t.Fatal(err)
	}
	line := callLog(t, log)
	if strings.Count(line, `"action":"keys"`) != 2 {
		t.Fatalf("keys calls: %s", line)
	}
}

func TestParseDMState(t *testing.T) {
	for in, want := range map[string]AgentState{
		"working": AgentWorking, "idle": AgentIdle, "blocked": AgentBlocked,
		"done": AgentDone, "error": AgentUnknown, "": AgentUnknown, "?": AgentUnknown,
	} {
		if got := parseDMState(in); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestDMCloseIsIdempotent(t *testing.T) {
	fakeDM(t)
	d := newDM()
	if err := d.ClosePane(t.Context(), "gone1"); err != nil {
		t.Fatalf("closing a gone pane: %v", err)
	}
	if err := d.CloseWorkspace(t.Context(), "gone2"); err != nil {
		t.Fatalf("closing a gone workspace: %v", err)
	}
	if err := d.ClosePane(t.Context(), "p1"); err != nil {
		t.Fatalf("closing a live pane: %v", err)
	}
}

func TestDMWorkspaces(t *testing.T) {
	fakeDM(t)
	d := newDM()
	ws := d.Workspaces(context.Background())
	if len(ws) != 1 || ws[0].ID != "s1" || ws[0].Label != "enc" {
		t.Fatalf("workspaces = %+v", ws)
	}
}

// TestDMBackendAgainstRealDM drives the whole interface against a real dm
// session in a temp state directory. It skips when dm is not installed.
func TestDMBackendAgainstRealDM(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	bin := os.Getenv("DM_BIN")
	if bin == "" {
		bin = "dm"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("dm not on PATH: %v", err)
	}
	state := t.TempDir()
	t.Setenv("DM_STATE", state)
	t.Setenv("DM_SESSION", "it")
	t.Setenv("DM_BIN", bin)
	cwd := t.TempDir()

	cmd := exec.Command(bin, "new", "-s", "it")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dm new: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		filepath.Walk(state, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".pid") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			var pid int
			fmt.Sscanf(string(b), "%d", &pid)
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			return nil
		})
		os.RemoveAll(state)
	})

	ctx := context.Background()
	d := newDM()

	ws, tab, root, err := d.CreateWorkspace(ctx, "enc", "worker", cwd)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if ws == "" || tab == "" || root == "" {
		t.Fatalf("empty ids: %q %q %q", ws, tab, root)
	}

	split, err := d.SplitPane(ctx, root, "down", cwd, 0.25)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	got, err := d.Pane(ctx, split)
	if err != nil {
		t.Fatalf("pane: %v", err)
	}
	if got.Tab != tab || got.Workspace != ws {
		t.Fatalf("pane lives in %q/%q, want %q/%q", got.Workspace, got.Tab, ws, tab)
	}

	if _, err := d.Read(ctx, root); err != nil {
		t.Fatalf("read: %v", err)
	}

	// A command that reports idle is enough for the readiness wait.
	report := `printf '\033]7501;state=idle\033\\'; sleep 60`
	if err := d.StartAgent(ctx, "w1-7f", root, []string{"sh", "-c", report}); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	agents, err := d.Agents(ctx)
	if err != nil {
		t.Fatalf("agents: %v", err)
	}
	a, ok := agents["w1-7f"]
	if !ok || a.Pane != root || a.Status != AgentIdle {
		t.Fatalf("agents = %v", agents)
	}

	if err := d.Prompt(ctx, "w1-7f", "echo prompt-arrived"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := d.FocusAgent(ctx, "w1-7f"); err != nil {
		t.Fatalf("focus agent: %v", err)
	}
	if err := d.FocusTab(ctx, tab); err != nil {
		t.Fatalf("focus tab: %v", err)
	}
	if err := d.SendKeys(ctx, "w1-7f", "x"); err != nil {
		t.Fatalf("send keys: %v", err)
	}

	if err := d.ClosePane(ctx, split); err != nil {
		t.Fatalf("close pane: %v", err)
	}
	if _, err := d.Pane(ctx, split); err == nil {
		t.Fatal("closed pane still resolves")
	}
	if err := d.CloseWorkspace(ctx, ws); err != nil {
		t.Fatalf("close workspace: %v", err)
	}
	for _, w := range d.Workspaces(ctx) {
		if w.ID == ws {
			t.Fatalf("workspace %s still listed", ws)
		}
	}
}
