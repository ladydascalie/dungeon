package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The rail learns that tmux changed from one long-lived control-mode client.
// Control mode is a single connection that carries notifications when a
// window, pane or session moves, and runs commands on the same connection, so
// an action costs no new client and no subprocess.

const (
	controlConnected = "connected"
	controlExit      = "exit"
	controlTopology  = "topology"

	controlRetryFirst = 100 * time.Millisecond
	controlRetryMax   = 5 * time.Second
)

// controlEvent is one thing the rail must react to. A topology event carries
// the tmux notification name, so a test can name what woke it.
type controlEvent struct {
	Kind string
	Name string
	Args []string
	Err  error
	At   time.Time
}

type controlCall struct {
	args  []string
	seq   string
	reply chan controlResult
}

type controlResult struct {
	out string
	err error
}

type controlBlock struct {
	active bool
	seq    string
	buf    strings.Builder
}

// tmuxControl is one control-mode client. It reconnects on its own, so a tmux
// restart costs the rail a pause, not its life.
type tmuxControl struct {
	t   tmux
	out chan<- controlEvent
	ctx context.Context
	tmuxControlState
}

type tmuxControlState struct {
	cmdMu sync.Mutex

	mu        sync.Mutex
	stdin     io.WriteCloser
	connDone  chan struct{}
	pending   *controlCall
	connected bool
	connAt    time.Time
	lastErr   error
	lastErrAt time.Time
}

// startTmuxControl starts the reconnecting client. Events land on out without
// blocking the reader: a dropped notification is covered by the backstop.
func startTmuxControl(ctx context.Context, t tmux, out chan<- controlEvent) *tmuxControl {
	c := &tmuxControl{t: t, out: out, ctx: ctx}
	go c.run()
	return c
}

// run connects, reads until the connection dies, then reconnects with a
// bounded backoff. A connection that lasted a while resets the delay, so a
// flapping server cannot spin.
func (c *tmuxControl) run() {
	backoff := controlRetryFirst
	for {
		if c.ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := c.session()
		if c.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("tmux control exited")
		}
		c.setError(err)
		c.emit(controlEvent{Kind: controlExit, Err: err})
		if time.Since(started) > 10*time.Second {
			backoff = controlRetryFirst
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-c.ctx.Done():
			timer.Stop()
			return
		}
		backoff *= 2
		if backoff > controlRetryMax {
			backoff = controlRetryMax
		}
	}
}

// session is one connection's whole life: start the client, disable the pane
// output stream, then read until the client exits.
func (c *tmuxControl) session() error {
	args := c.t.argv("-C", "attach")
	if target := c.attachTarget(); target != "" {
		args = append(args, "-t", target)
	}
	cmd := exec.CommandContext(c.ctx, "tmux", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan struct{})
	closeDone := sync.OnceFunc(func() { close(done) })
	c.mu.Lock()
	c.stdin = stdin
	c.connDone = done
	c.connected = false
	c.connAt = time.Now()
	c.lastErr = nil
	c.mu.Unlock()

	// The reader must run before the first command goes out: it is what
	// delivers %begin/%end, and it closes done so a dead server unblocks a
	// waiting command at once.
	readDone := make(chan error, 1)
	go func() {
		err := c.read(stdout)
		closeDone()
		readDone <- err
	}()

	// Without this the client relays every pane byte as %output. The rail
	// reads pane text through pipe-pane, never from this stream.
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	_, _ = c.do(ctx, "refresh-client", "-f", "no-output")
	cancel()
	select {
	case <-done:
	default:
		c.mu.Lock()
		c.connected = true
		c.connAt = time.Now()
		c.lastErr = nil
		c.mu.Unlock()
		c.emit(controlEvent{Kind: controlConnected})
	}

	readErr := <-readDone

	c.mu.Lock()
	c.stdin = nil
	c.connected = false
	closeDone()
	c.connDone = nil
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	if pending != nil {
		pending.reply <- controlResult{err: errors.New("tmux control exited")}
	}
	_ = stdin.Close()
	waitErr := cmd.Wait()

	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return errors.New(collapseSpace(msg))
	}
	if waitErr != nil {
		return waitErr
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	return nil
}

// attachTarget keeps the control client on the session that hosts the rail,
// so it does not pin the most recently used session against destroy-unattached.
func (c *tmuxControl) attachTarget() string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	out, err := c.t.call(ctx, "display-message", "-p", "-t", pane, "#{session_id}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// read consumes one connection. Notifications are outside output blocks, so a
// notification is never mistaken for command output.
func (c *tmuxControl) read(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var block controlBlock
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if exitErr, done := c.handleLine(line, &block); done {
				return exitErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (c *tmuxControl) handleLine(line string, block *controlBlock) (error, bool) {
	// Inside a block every line is command output, including one that begins
	// with % because a pane id does. Only the block terminator is protocol.
	if block.active {
		name, args, ok := parseControlLine(line)
		if ok && (name == "%end" || name == "%error") {
			seq := ""
			if len(args) > 1 {
				seq = args[1]
			}
			if seq == block.seq {
				c.endBlock(name, args, block)
				return nil, false
			}
		}
		block.buf.WriteString(line)
		block.buf.WriteByte('\n')
		return nil, false
	}

	name, args, ok := parseControlLine(line)
	if !ok {
		return nil, false
	}
	switch name {
	case "%output":
		// Deliberate: the pane stream is disabled and never wanted.
		return nil, false
	case "%begin":
		c.beginBlock(args, block)
	case "%end", "%error":
		c.endBlock(name, args, block)
	case "%exit":
		reason := strings.TrimSpace(strings.Join(args, " "))
		if reason == "" {
			reason = "tmux control exited"
		}
		return errors.New(reason), true
	default:
		if controlTopologyName(name) {
			c.emit(controlEvent{Kind: controlTopology, Name: name, Args: args})
		}
	}
	return nil, false
}

func (c *tmuxControl) beginBlock(args []string, block *controlBlock) {
	seq, flags := "", ""
	if len(args) > 1 {
		seq = args[1]
	}
	if len(args) > 2 {
		flags = args[2]
	}
	c.mu.Lock()
	pending := c.pending
	mine := pending != nil && pending.seq == "" && flags == "1"
	if mine {
		pending.seq = seq
	}
	c.mu.Unlock()
	if !mine {
		// The initial handshake block belongs to no command.
		block.active = false
		return
	}
	block.active = true
	block.seq = seq
	block.buf.Reset()
}

func (c *tmuxControl) endBlock(name string, args []string, block *controlBlock) {
	seq := ""
	if len(args) > 1 {
		seq = args[1]
	}
	c.mu.Lock()
	pending := c.pending
	match := pending != nil && pending.seq != "" && pending.seq == seq
	if match {
		c.pending = nil
	}
	c.mu.Unlock()
	block.active = false
	if !match {
		return
	}
	out := strings.TrimSuffix(block.buf.String(), "\n")
	var err error
	if name == "%error" {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = "command failed"
		}
		err = fmt.Errorf("tmux %s: %s", strings.Join(pending.args, " "), collapseSpace(msg))
	}
	pending.reply <- controlResult{out: out, err: err}
}

func parseControlLine(line string) (string, []string, bool) {
	if !strings.HasPrefix(line, "%") {
		return "", nil, false
	}
	body := line[1:]
	name, rest, _ := strings.Cut(body, " ")
	if name == "" {
		return "", nil, false
	}
	var args []string
	if rest != "" {
		args = strings.Fields(rest)
	}
	return "%" + name, args, true
}

// controlTopologyName is the set of notifications that can change what the
// rail lists. %output is intentionally absent: it is a firehose, not a fact.
func controlTopologyName(name string) bool {
	switch name {
	case "%window-add", "%window-close", "%window-renamed",
		"%window-pane-changed", "%layout-change", "%pane-mode-changed",
		"%session-changed", "%session-renamed", "%session-window-changed":
		return true
	}
	return strings.HasPrefix(name, "%client-")
}

// do runs one command over the live connection and waits for its block. One
// command is in flight at a time; actions are rare and must not interleave.
func (c *tmuxControl) do(ctx context.Context, args ...string) (string, error) {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	c.mu.Lock()
	stdin := c.stdin
	done := c.connDone
	if stdin == nil {
		err := c.downErrorLocked()
		c.mu.Unlock()
		return "", err
	}
	call := &controlCall{args: args, reply: make(chan controlResult, 1)}
	c.pending = call
	c.mu.Unlock()

	line := controlCommandLine(args)
	if _, err := io.WriteString(stdin, line+"\n"); err != nil {
		c.clearPending(call)
		return "", err
	}
	select {
	case res := <-call.reply:
		return res.out, res.err
	case <-done:
		c.clearPending(call)
		return "", c.downError()
	case <-ctx.Done():
		c.clearPending(call)
		return "", ctx.Err()
	case <-c.ctx.Done():
		c.clearPending(call)
		return "", c.ctx.Err()
	}
}

func (c *tmuxControl) clearPending(call *controlCall) {
	c.mu.Lock()
	if c.pending == call {
		c.pending = nil
	}
	c.mu.Unlock()
}

func (c *tmuxControl) connectedNow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *tmuxControl) lastError() (error, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr, c.lastErrAt
}

func (c *tmuxControl) setError(err error) {
	c.mu.Lock()
	c.lastErr = err
	c.lastErrAt = time.Now()
	c.mu.Unlock()
}

// downError is honest about the source: the connection failed, and this is
// how long ago it failed.
func (c *tmuxControl) downError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.downErrorLocked()
}

func (c *tmuxControl) downErrorLocked() error {
	when := c.lastErrAt
	msg := "tmux control is down"
	if c.lastErr != nil {
		msg += ": " + collapseSpace(c.lastErr.Error())
	}
	if !when.IsZero() {
		msg += " (" + age(when.UTC().Format(time.RFC3339)) + ")"
	}
	return errors.New(msg)
}

func (c *tmuxControl) emit(e controlEvent) {
	if c.out == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	select {
	case c.out <- e:
	default:
		// The backstop re-reads; a dropped wake-up is a delay, not a loss.
	}
}

// controlCommandLine quotes every argument, because a control-mode line is
// parsed by tmux, not by a shell. Single quotes keep pane ids, formats and the
// osc-sink command line intact.
func controlCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = controlQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func controlQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) ||
			strings.ContainsRune("_@%.:/-,=+#", r) {
			continue
		}
		safe = false
		break
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// railFocus moves the operator's client to a pane over the control connection.
// The control client is not the operator's client, so switch-client names the
// attached, non-control client explicitly.
func (c *tmuxControl) railFocus(ctx context.Context, paneID string) error {
	out, err := c.do(ctx, "display-message", "-p", "-t", paneID,
		"#{pane_id}\t#{window_id}\t#{session_id}")
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 3 || fields[0] == "" {
		return fmt.Errorf("tmux: pane %s is gone", paneID)
	}
	pane, window, session := fields[0], fields[1], fields[2]

	clients, err := c.do(ctx, "list-clients", "-F",
		"#{client_name}\t#{client_control_mode}\t#{session_id}")
	if err != nil {
		return err
	}
	client, clientSession := "", ""
	fallback, fallbackSession := "", ""
	for _, line := range strings.Split(strings.TrimSpace(clients), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		if f[1] == "1" {
			continue // our own control client
		}
		if fallback == "" {
			fallback, fallbackSession = f[0], f[2]
		}
		if f[2] == session {
			client, clientSession = f[0], f[2]
			break
		}
	}
	if client == "" {
		client, clientSession = fallback, fallbackSession
	}
	if client != "" && clientSession != session {
		if _, err := c.do(ctx, "switch-client", "-c", client, "-t", session); err != nil {
			return err
		}
	}
	if _, err := c.do(ctx, "select-window", "-t", window); err != nil {
		return err
	}
	_, err = c.do(ctx, "select-pane", "-t", pane)
	return err
}
