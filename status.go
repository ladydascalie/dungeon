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
	{"failed", "FAILED"},
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

// LoopRow is one loop as one thing: its declaration, its round progress and
// the action it is on. Units are the generated unit ids, in round order.
type LoopRow struct {
	ID       string   `json:"id"`
	Focus    string   `json:"focus"`
	Rounds   int      `json:"rounds"`
	Reviewer string   `json:"reviewer"`
	Fixer    string   `json:"fixer"`
	Pass     string   `json:"pass"`
	Started  bool     `json:"started"`
	Passed   bool     `json:"passed"`
	Round    int      `json:"round"`
	State    string   `json:"state"`
	Units    []string `json:"units"`
}

type Snapshot struct {
	Encounter string         `json:"encounter"`
	PlanPath  string         `json:"plan_path"`
	PlanHash  string         `json:"plan_hash,omitempty"`
	PlanDrift bool           `json:"plan_drift"`
	StateAge  string         `json:"state_age"`
	Counts    map[string]int `json:"counts"`
	Units     []UnitRow      `json:"units"`
	Minions   []MinionRow    `json:"minions"`
	Loops     []LoopRow      `json:"loops"`
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
			if depSatisfied(plan, &st, d) {
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
	s.Loops = buildLoopRows(plan, &st)
	return s
}

// buildLoopRows reads the expanded plan, so a generated unit is found by its
// Loop tag. A plan that was never expanded has no loop units: the row still
// names the loop and waits.
func buildLoopRows(p Plan, st *State) []LoopRow {
	var rows []LoopRow
	for _, l := range p.Loops {
		row := LoopRow{
			ID:       l.ID,
			Focus:    l.Focus,
			Rounds:   l.Rounds,
			Reviewer: l.Reviewer,
			Fixer:    l.Fixer,
			Pass:     l.Pass,
		}
		for _, u := range p.Units {
			if u.Loop != l.ID {
				continue
			}
			row.Units = append(row.Units, u.ID)
			status := st.UnitStatus(u.ID)
			if status != "planned" {
				row.Started = true
			}
			if u.Kind == "review" && status == "done" {
				row.Passed = true
			}
		}
		row.Round = loopRound(p, st, row.Units, l.Rounds)
		row.State = loopState(p, st, row.Units)
		rows = append(rows, row)
	}
	return rows
}

// loopRound is the round the loop is on now. The first unsettled unit names
// it. A finished loop falls back to its last live round, clamped to the cap,
// because the final review judges the last fix without adding a round.
func loopRound(p Plan, st *State, units []string, cap int) int {
	round := 0
	for _, id := range units {
		status := st.UnitStatus(id)
		// A failed review has had its say, and an unproven fix waits for the
		// next review. Both mean the loop has moved on to its round.
		if status == "done" || status == "canceled" || status == "failed" || status == "unproven" {
			continue
		}
		if u, ok := findUnit(p, id); ok {
			round = u.Round
			break
		}
	}
	if round == 0 {
		for _, id := range units {
			if st.UnitStatus(id) == "canceled" {
				continue
			}
			if u, ok := findUnit(p, id); ok && u.Round > round {
				round = u.Round
			}
		}
	}
	if round < 1 {
		round = 1
	}
	if cap > 0 && round > cap {
		round = cap
	}
	return round
}

// loopState is the loop's current action, most urgent first: working, failed,
// ready, done, waiting. A failed review is called out even when its fix is
// already ready, because the failure is what the operator needs to see.
func loopState(p Plan, st *State, units []string) string {
	if len(units) == 0 {
		return "waiting"
	}
	working, ready, settled := false, false, true
	for _, id := range units {
		switch st.UnitStatus(id) {
		case "working":
			working = true
			settled = false
		case "planned":
			settled = false
			if u, ok := findUnit(p, id); ok && unitReady(p, st, u) {
				ready = true
			}
		case "done", "canceled":
		default:
			settled = false
		}
	}
	switch {
	case working:
		return "working"
	case lastReviewFailed(p, st, units):
		return "failed"
	case ready:
		return "ready"
	case settled:
		return "done"
	default:
		return "waiting"
	}
}

// lastReviewFailed reads the newest review that has an outcome. An unstarted
// or canceled review says nothing about the loop.
func lastReviewFailed(p Plan, st *State, units []string) bool {
	for i := len(units) - 1; i >= 0; i-- {
		u, ok := findUnit(p, units[i])
		if !ok || u.Kind != "review" {
			continue
		}
		switch st.UnitStatus(u.ID) {
		case "planned", "canceled":
			continue
		}
		return st.UnitStatus(u.ID) == "failed"
	}
	return false
}

func unitReady(p Plan, st *State, u Unit) bool {
	for _, d := range u.Deps {
		if !depSatisfied(p, st, d) {
			return false
		}
	}
	return true
}

// markPlanDrift compares the plan on disk with the hash recorded at dispatch.
// A state written before the guard carries no hash, so it cannot drift.
func markPlanDrift(s *Snapshot, planPath string, st State) {
	if st.PlanHash == "" {
		return
	}
	h, err := PlanHash(planPath)
	if err != nil {
		return
	}
	s.PlanHash = h
	s.PlanDrift = h != st.PlanHash
}

func renderText(w io.Writer, s Snapshot) {
	fmt.Fprintf(w, "%s  plan %s  state %s\n", s.Encounter, s.PlanPath, s.StateAge)
	if s.PlanDrift {
		fmt.Fprintf(w, "plan changed since dispatch (%s): re-run dungeon up, or pass --force\n", shortHash(s.PlanHash))
	}
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
				detail = collapseLine(u.Note)
			}
			fmt.Fprintf(w, "  %s  %s  %s  %s  %s\n",
				pad(u.ID, idW), where, pad(u.Pane, paneW), pad(u.Status, stW),
				truncate(detail+": "+collapseLine(u.Goal), 80))
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

	if len(s.Loops) > 0 {
		idW, focW, roundW := 4, 5, 6
		rounds := make([]string, len(s.Loops))
		for i, l := range s.Loops {
			idW = max(idW, len(l.ID))
			focW = max(focW, len(collapseLine(l.Focus)))
			rounds[i] = fmt.Sprintf("round %d/%d", l.Round, l.Rounds)
			roundW = max(roundW, len(rounds[i]))
		}
		fmt.Fprintf(w, "\nLOOPS (%d)\n", len(s.Loops))
		for i, l := range s.Loops {
			fmt.Fprintf(w, "  %s  %s  %s  %s\n",
				pad(l.ID, idW), pad(collapseLine(l.Focus), focW), pad(rounds[i], roundW), l.State)
		}
	}

	if s.StateAge == "none yet" {
		fmt.Fprintf(w, "\nnothing recorded yet. start with:\n  dungeon set unit %s working --note \"brief written\"\n",
			s.Units[0].ID)
	}
}

// collapseLine flattens every whitespace run, newlines included, to one space.
// A goal or note may come from a YAML block scalar, and a table row is one line.
func collapseLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
