package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The plan says what the work is. The state says where it has got to. They are
// separate files on purpose: editing the plan must not lose the run, and the
// run must not rewrite the plan.
type State struct {
	Encounter string                 `json:"encounter"`
	PlanHash  string                 `json:"plan_hash,omitempty"`
	Token     string                 `json:"token,omitempty"` // per-run suffix, so two runs never share a herdr name
	UpdatedAt string                 `json:"updated_at"`
	Units     map[string]UnitState   `json:"units"`
	Minions   map[string]MinionState `json:"minions"`
}

type UnitState struct {
	Status  string   `json:"status"`
	Note    string   `json:"note,omitempty"`
	At      string   `json:"at"`
	Files   []string `json:"files,omitempty"`
	Link    string   `json:"link,omitempty"` // a pull request or any URL: forge agnostic, http(s) only
	History []Change `json:"history,omitempty"`
}

type MinionState struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
	At     string `json:"at"`
	Pane   string `json:"pane,omitempty"` // a hint, refreshed from herdr whenever the lane is live
}

type Change struct {
	At   string `json:"at"`
	From string `json:"from"`
	To   string `json:"to"`
	Note string `json:"note,omitempty"`
}

// Unit statuses, most urgent first. `dungeon status` prints sections in this
// order, so the top of the screen is the work that needs a human.
var unitStatuses = []string{
	"blocked", "failed", "working", "planned", "unproven", "rejected", "done", "canceled",
}

var unitStatusSet = toSet(unitStatuses)

var minionStatuses = []string{"spawned", "busy", "waiting", "lost", "absent"}

var minionStatusSet = toSet(minionStatuses)

func toSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, v := range in {
		m[v] = true
	}
	return m
}

// A herdr agent name is global. Two encounters that both declare w1 would
// otherwise share one agent, so a run carries a two-character token and every
// agent it starts is named <minion>-<token>.
const (
	tokenLen      = 2
	agentNameMax  = 32 // herdr's limit
	agentIDMax    = agentNameMax - 1 - tokenLen
	tokenTries    = 64
	tokenAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// newToken is injectable so a test can make the re-roll deterministic.
var newToken = randomToken

func randomToken() string {
	var b [tokenLen]byte
	if _, err := rand.Read(b[:]); err == nil {
		for i := range b {
			b[i] = tokenAlphabet[int(b[i])%len(tokenAlphabet)]
		}
		return string(b[:])
	}
	// A clock is a poor source but the token is a label, not a secret.
	n := time.Now().UnixNano()
	for i := range b {
		b[i] = tokenAlphabet[n%int64(len(tokenAlphabet))]
		n /= int64(len(tokenAlphabet))
	}
	return string(b[:])
}

// AgentName is the herdr agent name for one minion: the minion id truncated to
// fit, a hyphen, then the token. Without a token it is the bare id, so a run
// that started before tokens keeps the agent it already has.
func AgentName(token, minion string) string {
	if token == "" {
		return minion
	}
	if len(minion) > agentIDMax {
		minion = minion[:agentIDMax]
	}
	return minion + "-" + token
}

func (s State) AgentName(minion string) string { return AgentName(s.Token, minion) }

// pickToken draws whole tokens until none of the derived names is live. A
// token that collides anywhere is abandoned, so a live collision is impossible
// rather than merely unlikely.
func pickToken(agents map[string]Agent, minions []Minion, gen func() string) (string, error) {
	last := ""
	for i := 0; i < tokenTries; i++ {
		token := gen()
		clash := ""
		for _, m := range minions {
			name := AgentName(token, m.ID)
			if _, live := agents[name]; live {
				clash = name
				break
			}
		}
		if clash == "" {
			return token, nil
		}
		last = clash
	}
	return "", fmt.Errorf("no free herdr agent name after %d tokens: %s is already live; "+
		"stop the other run with dungeon down, or close that agent, then run dungeon up again",
		tokenTries, last)
}

// StatePath sits next to the plan, so one encounter is one directory.
func StatePath(planPath string) string {
	return filepath.Join(filepath.Dir(planPath), "state.json")
}

func LoadState(path string) (State, error) {
	s := State{
		Units:   map[string]UnitState{},
		Minions: map[string]MinionState{},
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if s.Units == nil {
		s.Units = map[string]UnitState{}
	}
	if s.Minions == nil {
		s.Minions = map[string]MinionState{}
	}
	s.migrate()
	return s, nil
}

// Save writes through a temporary file so a reader never sees half a state.
func SaveState(path string, s State) error {
	s.UpdatedAt = now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *State) SetUnit(id, status, note string) error {
	if !unitStatusSet[status] {
		return fmt.Errorf("unit status %q is not one of %s", status, strings.Join(unitStatuses, ", "))
	}
	u := s.Units[id]
	u.History = append(u.History, Change{At: now(), From: u.Status, To: status, Note: note})
	u.Status = status
	u.Note = note
	u.At = now()
	s.Units[id] = u
	return nil
}

// Attach records a file the operator wants one click away from the unit: a
// brief, a report, a screenshot, a handoff note.
func (s *State) Attach(unit, path string) error {
	if path == "" {
		return fmt.Errorf("attach needs a path")
	}
	u := s.Units[unit]
	for _, f := range u.Files {
		if f == path {
			return nil
		}
	}
	u.Files = append(u.Files, path)
	if u.At == "" {
		u.At = now()
	}
	s.Units[unit] = u
	return nil
}

func (s *State) SetMinion(id, status, note string) error {
	if !minionStatusSet[status] {
		return fmt.Errorf("minion status %q is not one of %s", status, strings.Join(minionStatuses, ", "))
	}
	s.Minions[id] = MinionState{Status: status, Note: note, At: now(), Pane: s.Minions[id].Pane}
	return nil
}

func (s *State) PinPane(id, pane string) { m := s.Minions[id]; m.Pane = pane; s.Minions[id] = m }

// renamed maps the first vocabulary onto the current one, so a state file
// written before the rename keeps reading correctly.
var renamed = map[string]string{
	"dispatched": "working",
	"returned":   "finished",
	"accepted":   "done",
	"unverified": "unproven",
}

func (s *State) migrate() {
	fix := func(v string) string {
		if to, ok := renamed[v]; ok {
			return to
		}
		return v
	}
	for id, u := range s.Units {
		u.Status = fix(u.Status)
		for i := range u.History {
			u.History[i].From = fix(u.History[i].From)
			u.History[i].To = fix(u.History[i].To)
		}
		s.Units[id] = u
	}
	for id, m := range s.Minions {
		m.Status = fix(m.Status)
		s.Minions[id] = m
	}
}

func (s State) UnitStatus(id string) string {
	if u, ok := s.Units[id]; ok && u.Status != "" {
		return u.Status
	}
	return "planned"
}

func (s State) MinionStatus(id string) string {
	if m, ok := s.Minions[id]; ok && m.Status != "" {
		return m.Status
	}
	return "absent"
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
