package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// A key printed in the actions box runs its action. Arrow keys and enter still
// choose, but a named shortcut must not need a second press to fire.
func TestFleetActionBoxKeysRunDirectly(t *testing.T) {
	model, m, _ := fleetViewFixture(t)
	m.model = model

	// The beta settled fold is a summary row, the only row the fold action takes.
	for _, it := range m.items() {
		if it.summary && it.encounter == "beta" {
			m.rebuildOrder(fleetItemKey(it))
			break
		}
	}
	it, ok := m.selectedItem()
	if !ok || !it.summary || it.encounter != "beta" {
		t.Fatalf("the cursor must sit on the beta fold, got %+v", it)
	}

	m.overlay = "actions"
	m.actionKey("e")
	if m.overlay != "" {
		t.Fatalf("running an action must close the box, overlay %q", m.overlay)
	}
	if !m.expanded[fleetFoldKey("beta", fleetSectionSettled)] {
		t.Fatal("e must expand the settled fold without an enter")
	}

	// h on a row with no hunk session answers with a notice, not silence.
	m.rebuildOrder(fleetRowKey(model.Rows[0]))
	m.overlay = "actions"
	cmd := m.actionKey("h")
	if cmd == nil {
		t.Fatal("h must run the hunk action")
	}
	msg, ok := cmd().(fleetNoticeMsg)
	if !ok || !strings.Contains(msg.text, "no hunk session") {
		t.Fatalf("h ran nothing useful: %+v", msg)
	}
}

// The box used to print j for jump, which the cursor also used to move down.
// No action key may shadow a motion or an exit key.
func TestFleetActionKeysDoNotCollideWithMotion(t *testing.T) {
	taken := map[string]bool{"j": true, "k": true, "enter": true, "esc": true, " ": true}
	for _, action := range fleetActions {
		if taken[action.key] {
			t.Fatalf("action %q shadows a motion or an exit key", action.label)
		}
	}
}

// fleetSplitFixture puts one real plan on disk, opens it from a unit row, and
// drains the load, so a test starts on a drawn split.
func fleetSplitFixture(t *testing.T) fleetViewModel {
	t.Helper()
	planPath := writePlan(t, statusPlan)
	if err := SaveState(StatePath(planPath), State{
		Encounter: "st",
		Units:     map[string]UnitState{},
		Minions:   map[string]MinionState{},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	model := FleetModel{
		Encounters: []FleetEncounter{{Name: "st", Path: planPath, LastTouch: now}},
		Rows: []FleetRow{
			{Encounter: "st", ID: "A", Minion: "w1", Role: "worker", Status: "working", Pane: "%1", LastTouch: now},
			{Encounter: "st", ID: "B", Minion: "w1", Role: "worker", Status: "ready", LastTouch: now},
		},
		Sources: FleetSources{Worktrunk: FleetSource{Available: true}, Hunk: FleetSource{Available: true}},
	}
	m := newFleetViewModel(time.Second, "no-such-mux")
	m.width, m.height = 120, 40
	m.model = model
	m.rebuildOrder(fleetRowKey(model.Rows[1])) // B

	cmd := m.key("enter")
	if cmd == nil {
		t.Fatal("enter must load the encounter")
	}
	next, _ := m.Update(cmd())
	return next.(fleetViewModel)
}

// Enter on any row opens that encounter under the fleet: the split keeps the
// fleet on top and the whole dungeon view underneath.
func TestFleetEnterOpensThePlanUnderTheFleet(t *testing.T) {
	m := fleetSplitFixture(t)
	if m.plan == nil || m.focus != "plan" {
		t.Fatalf("enter must open and focus the plan: open %v focus %q", m.plan != nil, m.focus)
	}
	if m.planLoading {
		t.Fatal("the drained load must leave the plan loaded")
	}
	if got := m.plan.selectedID(); got != "B" {
		t.Fatalf("the plan cursor must land on the unit the fleet was on, got %q", got)
	}

	out := m.View()
	for _, want := range []string{
		"dungeon fleet",
		"PLAN · tab fleet",
		"j/k move · enter detail · space actions",
		"READY 1", "2 waiting",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PREVIEW") {
		t.Fatalf("the plan replaces the preview in the split:\n%s", out)
	}
	if lines := len(strings.Split(out, "\n")); lines > m.height {
		t.Fatalf("the split runs %d lines past a %d row frame:\n%s", lines, m.height, out)
	}
	if fleetAt := strings.Index(out, "dungeon fleet"); fleetAt < 0 || fleetAt > strings.Index(out, "READY 1") {
		t.Fatalf("the fleet must stay above the plan:\n%s", out)
	}
}

// A plan with no state still exists on disk, so the split must read as planned
// rather than refuse to open.
func TestFleetEnterOpensAPlanWithoutState(t *testing.T) {
	planPath := writePlan(t, statusPlan)
	m := newFleetViewModel(time.Second, "no-such-mux")
	m.width, m.height = 120, 40
	m.model = FleetModel{
		Encounters: []FleetEncounter{{Name: "st", Path: planPath, LastTouch: time.Now()}},
		Rows:       []FleetRow{{Encounter: "st", ID: "A", Status: "planned"}},
		Sources:    FleetSources{Worktrunk: FleetSource{Available: true}, Hunk: FleetSource{Available: true}},
	}
	m.rebuildOrder(fleetRowKey(m.model.Rows[0]))
	cmd := m.key("enter")
	if cmd == nil || m.plan == nil {
		t.Fatal("enter must open a plan even without a state file")
	}
	next, _ := m.Update(cmd())
	mm := next.(fleetViewModel)
	if mm.plan.err != nil {
		t.Fatalf("a missing state is not an unreadable plan: %v", mm.plan.err)
	}
}

// Tab moves the keyboard between the panes. Esc in a plan with nothing open
// hands it back and closes the split; esc inside an overlay only closes that.
func TestFleetSplitTabAndEsc(t *testing.T) {
	m := fleetSplitFixture(t)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(fleetViewModel)
	if m.focus != "fleet" {
		t.Fatalf("tab must move the focus to the fleet, got %q", m.focus)
	}
	if !strings.Contains(m.View(), "FLEET · tab plan") {
		t.Fatal("the footer must name the pane that holds the focus")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(fleetViewModel)
	if m.focus != "plan" {
		t.Fatalf("tab must move the focus back to the plan, got %q", m.focus)
	}
	if !strings.Contains(m.View(), "PLAN · tab fleet") {
		t.Fatal("the footer must name the plan pane when it holds the focus")
	}

	// Esc inside an overlay closes the overlay, never the split.
	m.plan.overlay = "detail"
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(fleetViewModel)
	if m.plan == nil {
		t.Fatal("esc inside a plan overlay must not close the split")
	}
	if m.plan.overlay != "" {
		t.Fatalf("esc must close the plan overlay first, overlay %q", m.plan.overlay)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(fleetViewModel)
	if m.plan != nil || m.focus != "fleet" {
		t.Fatalf("esc at rest must close the split: open %v focus %q", m.plan != nil, m.focus)
	}
	if strings.Contains(m.View(), "PREVIEW") == false {
		t.Fatal("closing the split must give the preview its rows back")
	}
}

// An unreadable plan has no unit to draw, so enter says why instead of opening
// an empty frame.
func TestFleetEnterOnAnUnreadablePlanSaysWhy(t *testing.T) {
	m := newFleetViewModel(time.Second, "")
	m.width, m.height = 120, 30
	m.model = FleetModel{Encounters: []FleetEncounter{{
		Name:      "broken",
		Path:      filepath.Join(t.TempDir(), ".dungeon", "broken", "plan.yaml"),
		LastTouch: time.Now(),
		Err:       "plan.yaml: unexpected end of file",
	}}}
	m.rebuildOrder("")
	it, ok := m.selectedItem()
	if !ok || !it.broken {
		t.Fatalf("the broken encounter must be selectable, got %+v", it)
	}
	if cmd := m.key("enter"); cmd != nil {
		t.Fatal("a plan that will not read must not start a load")
	}
	if m.plan != nil {
		t.Fatal("a plan that will not read must not open")
	}
	if !strings.Contains(m.View(), "the plan will not read") {
		t.Fatalf("enter must say why the plan did not open:\n%s", m.View())
	}
}
