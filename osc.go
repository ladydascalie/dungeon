package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// OSC 7501 is the Program Status Protocol. A harness writes one escape
// sequence and the terminal keeps a current record for it. tmux does not keep
// that record and capture-pane drops it, so dungeon reads the raw pane stream
// through pipe-pane and stores the last report per pane.

const oscStatusPrefix = "\x1b]7501;"

type oscStatus struct {
	Server   string `json:"server,omitempty"`
	Pane     string `json:"pane"`
	State    string `json:"state"`
	App      string `json:"app,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Msg      string `json:"msg,omitempty"`
	Title    string `json:"title,omitempty"`
	Progress *int   `json:"progress,omitempty"`
	At       string `json:"at"`
}

// oscServerKey names the tmux server the rail is looking at. Pane ids repeat
// across servers, so status files are namespaced by it.
func oscServerKey() string {
	if v := os.Getenv("TMUX"); v != "" {
		return v
	}
	return "local"
}

func oscServerID(key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("%016x", h.Sum64())
}

func oscStateDir(server string) string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("dungeon-%d", os.Getuid()))
	}
	return filepath.Join(base, "dungeon", "osc", oscServerID(server))
}

func oscStatePath(server, pane string) string {
	safe := strings.ReplaceAll(pane, "%", "_")
	return filepath.Join(oscStateDir(server), safe+".json")
}

func readOSCStatus(pane string) (oscStatus, bool) {
	data, err := os.ReadFile(oscStatePath(oscServerKey(), pane))
	if err != nil {
		return oscStatus{}, false
	}
	var st oscStatus
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" {
		return oscStatus{}, false
	}
	return st, true
}

func writeOSCStatus(st oscStatus) error {
	dir := oscStateDir(st.Server)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st.At = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	path := oscStatePath(st.Server, st.Pane)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeOSCStatus(server, pane string) {
	_ = os.Remove(oscStatePath(server, pane))
}

func oscSinkCmd(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: dungeon osc-sink <server> <pane>")
		return 2
	}
	if err := runOSCSink(os.Stdin, args[0], args[1]); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 1
	}
	return 0
}

// runOSCSink consumes a pane's raw output and keeps the last root record.
func runOSCSink(r io.Reader, server, pane string) error {
	buf := make([]byte, 4096)
	var pending []byte
	last := ""
	for {
		n, err := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			pending, last = processOSC7501(pending, server, pane, last)
			if len(pending) > 8192 {
				pending = pending[len(pending)-4096:]
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	// A working or blocked record dies with the process that reported it.
	if last == "working" || last == "blocked" || last == "idle" {
		removeOSCStatus(server, pane)
	}
	return nil
}

func processOSC7501(buf []byte, server, pane, last string) ([]byte, string) {
	prefix := []byte(oscStatusPrefix)
	for {
		i := bytes.Index(buf, prefix)
		if i < 0 {
			// Keep a possible partial prefix for the next read.
			keep := len(prefix) - 1
			if len(buf) > keep {
				return buf[len(buf)-keep:], last
			}
			return buf, last
		}
		buf = buf[i:]
		rest := buf[len(prefix):]
		end, term := oscTerminator(rest)
		if end < 0 {
			if len(buf) > 4096 {
				return nil, last
			}
			return buf, last
		}
		body := string(rest[:end])
		if st, ok := parseOSC7501(body, pane); ok {
			st.Server = server
			last = st.State
			if st.State == "clear" {
				removeOSCStatus(server, pane)
			} else if err := writeOSCStatus(st); err != nil {
				// The pane stream must not block on a state write.
				_ = err
			}
		}
		buf = rest[end+term:]
	}
}

func oscTerminator(b []byte) (int, int) {
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case 0x07:
			return i, 1
		case 0x1b:
			if i+1 < len(b) && b[i+1] == '\\' {
				return i, 2
			}
		}
	}
	return -1, 0
}

func parseOSC7501(body, pane string) (oscStatus, bool) {
	if len(body) == 0 || len(body) > 4000 {
		return oscStatus{}, false
	}
	pairs := map[string]string{}
	for _, part := range strings.Split(body, ":") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || key == "" {
			continue
		}
		pairs[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	// Child records carry an id. The rail reports the harness root only.
	if id := pairs["id"]; id != "" {
		return oscStatus{}, false
	}
	state := pairs["state"]
	switch state {
	case "idle", "working", "done", "blocked", "error", "clear":
	default:
		return oscStatus{}, false
	}
	st := oscStatus{Pane: pane, State: state}
	if state == "clear" {
		return st, true
	}
	if app := pairs["app"]; validOSCApp(app) {
		st.App = app
	}
	if kind := pairs["kind"]; kind == "permission" || kind == "question" || kind == "auth" {
		st.Kind = kind
	}
	if n, err := strconv.Atoi(pairs["progress"]); err == nil && n >= 0 && n <= 100 {
		st.Progress = &n
	}
	if msg, err := decodeOSCText(pairs["msg"]); err == nil {
		st.Msg = msg
	} else {
		return oscStatus{}, false
	}
	if title, err := decodeOSCText(pairs["title"]); err == nil {
		st.Title = title
	} else {
		return oscStatus{}, false
	}
	return st, true
}

func validOSCApp(app string) bool {
	if app == "" || len(app) > 32 {
		return false
	}
	for _, r := range app {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '.', r == '+', r == '-':
		default:
			return false
		}
	}
	return true
}

func decodeOSCText(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("not utf8")
	}
	for _, r := range string(data) {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return "", fmt.Errorf("control character")
		}
	}
	return string(data), nil
}
