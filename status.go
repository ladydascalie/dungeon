package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Section order: the top of the screen is the work that needs a human.
var sections = []struct{ status, label string }{
	{"blocked", "BLOCKED"},
	{"working", "WORKING"},
	{"ready", "READY"},
	{"waiting", "WAITING"},
	{"unproven", "UNPROVEN"},
	{"rejected", "REJECTED"},
	{"done", "DONE"},
	{"canceled", "CANCELED"},
}

type UnitRow struct {
	ID        string   `json:"id"`
	Status    string   `json:"status"`
	Minion    string   `json:"minion"`
	Role      string   `json:"role"`
	Pane      string   `json:"pane,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	DepsDone  int      `json:"deps_done"`
	DepsTotal int      `json:"deps_total"`
	Inputs    []string `json:"inputs,omitempty"`
	WaitsOn   []string `json:"waits_on,omitempty"`
	Goal      string   `json:"goal"`
	Note      string   `json:"note,omitempty"`
	Link      string   `json:"link,omitempty"`
}

type MinionRow struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	Status string `json:"status"`
	Agent  string `json:"agent,omitempty"`
	Pane   string `json:"pane,omitempty"`
	Unit   string `json:"unit,omitempty"`
}

type Snapshot struct {
	Encounter string         `json:"encounter"`
	PlanPath  string         `json:"plan_path"`
	StateAge  string         `json:"state_age"`
	Counts    map[string]int `json:"counts"`
	Units     []UnitRow      `json:"units"`
	Minions   []MinionRow    `json:"minions"`
}

// buildSnapshot derives readiness from the plan and the state. "ready" and
// "waiting" are display statuses: a unit that is planned is one or the other,
// and the difference is whether its deps have landed.
func buildSnapshot(plan Plan, planPath string, st State, agents map[string]Agent) Snapshot {
	roleOf := map[string]string{}
	for _, m := range plan.Minions {
		roleOf[m.ID] = m.Role
	}

	s := Snapshot{
		Encounter: plan.Encounter,
		PlanPath:  planPath,
		StateAge:  age(st.UpdatedAt),
		Counts:    map[string]int{},
	}

	for _, u := range plan.Units {
		status := st.UnitStatus(u.ID)
		row := UnitRow{
			ID:     u.ID,
			Status: status,
			Minion: u.Minion,
			Role:   roleOf[u.Minion],
			Goal:   u.Goal,
		}
		if us, ok := st.Units[u.ID]; ok {
			row.Note, row.Link = us.Note, us.Link
		}
		if a, ok := agents[u.Minion]; ok {
			row.Pane, row.Agent = a.Pane, a.Status
		} else if hint := st.Minions[u.Minion].Pane; hint != "" {
			row.Pane = hint
		}
		row.Inputs = u.Deps
		for _, d := range u.Deps {
			row.DepsTotal++
			if st.UnitStatus(d) == "done" {
				row.DepsDone++
			} else {
				row.WaitsOn = append(row.WaitsOn, d)
			}
		}
		if status == "planned" {
			if len(row.WaitsOn) == 0 {
				row.Status = "ready"
			} else {
				row.Status = "waiting"
			}
		}
		s.Counts[row.Status]++
		s.Units = append(s.Units, row)
	}

	// Units with work in flight come first inside a section.
	order := map[string]int{}
	for i, sec := range sections {
		order[sec.status] = i
	}
	sort.SliceStable(s.Units, func(i, j int) bool {
		return order[s.Units[i].Status] < order[s.Units[j].Status]
	})

	unitByMinion := map[string]string{}
	for _, u := range s.Units {
		if u.Status == "working" || u.Status == "blocked" {
			if _, busy := unitByMinion[u.Minion]; !busy {
				unitByMinion[u.Minion] = u.ID
			}
		}
	}
	for _, m := range plan.Minions {
		row := MinionRow{ID: m.ID, Role: m.Role, Status: st.MinionStatus(m.ID)}
		if a, ok := agents[m.ID]; ok {
			row.Pane, row.Agent = a.Pane, a.Status
			if row.Status == "absent" {
				row.Status = "spawned"
			}
		} else if hint := st.Minions[m.ID].Pane; hint != "" {
			row.Pane = hint
		}
		row.Unit = unitByMinion[m.ID]
		s.Minions = append(s.Minions, row)
	}
	return s
}

func renderText(w io.Writer, s Snapshot) {
	fmt.Fprintf(w, "%s  plan %s  state %s\n", s.Encounter, s.PlanPath, s.StateAge)
	fmt.Fprintf(w, "%d minions  %d units", len(s.Minions), len(s.Units))
	for _, sec := range sections {
		if n := s.Counts[sec.status]; n > 0 {
			fmt.Fprintf(w, "  %d %s", n, strings.ToLower(sec.label))
		}
	}
	fmt.Fprintln(w)

	idW, minW, stW, paneW := 4, 4, 6, 4
	for _, u := range s.Units {
		idW = max(idW, len(u.ID))
		minW = max(minW, len(u.Minion))
		stW = max(stW, len(u.Status))
		paneW = max(paneW, len(u.Pane))
	}

	for _, sec := range sections {
		var rows []UnitRow
		for _, u := range s.Units {
			if u.Status == sec.status {
				rows = append(rows, u)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%d)\n", sec.label, len(rows))
		for _, u := range rows {
			where := pad(u.Minion, minW)
			if u.Agent != "" {
				where = pad(u.Minion, minW) + "  " + pad(u.Agent, 8)
			} else {
				where = pad(u.Minion, minW) + "  " + pad("-", 8)
			}
			detail := fmt.Sprintf("deps %d/%d", u.DepsDone, u.DepsTotal)
			if len(u.WaitsOn) > 0 {
				detail += ", waits on " + strings.Join(u.WaitsOn, " ")
			}
			if u.Note != "" {
				detail = u.Note
			}
			fmt.Fprintf(w, "  %s  %s  %s  %s  %s\n",
				pad(u.ID, idW), where, pad(u.Pane, paneW), pad(u.Status, stW),
				truncate(detail+": "+u.Goal, 80))
		}
	}

	if len(s.Minions) > 0 {
		mID, mRole, mStat, mPane := 4, 4, 6, 4
		for _, m := range s.Minions {
			mID = max(mID, len(m.ID))
			mRole = max(mRole, len(m.Role))
			mStat = max(mStat, len(m.Status))
			mPane = max(mPane, len(m.Pane))
		}
		fmt.Fprintf(w, "\nMINIONS (%d)\n", len(s.Minions))
		for _, m := range s.Minions {
			line := fmt.Sprintf("  %s  %s  %s  %s",
				pad(m.ID, mID), pad(m.Role, mRole), pad(m.Status, mStat), pad(m.Pane, mPane))
			if m.Unit != "" {
				line += "  " + m.Unit
			}
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	}

	if s.StateAge == "none yet" {
		fmt.Fprintf(w, "\nnothing recorded yet. start with:\n  dungeon set unit %s working --note \"brief written\"\n",
			s.Units[0].ID)
	}
}

func age(at string) string {
	if at == "" {
		return "none yet"
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// runeLen counts characters, not bytes: the tree connectors and the ellipsis
// are multi-byte, and a width measured in bytes truncates them mid-glyph.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, n int) string {
	if w := runeLen(s); w >= n {
		return s
	} else {
		return s + strings.Repeat(" ", n-w)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
