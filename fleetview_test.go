package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fleetViewFixture(t *testing.T) (FleetModel, fleetViewModel, FleetRow) {
	t.Helper()
	now := time.Now()
	report := now.Add(-2 * time.Hour)
	model := FleetModel{
		Encounters: []FleetEncounter{
			{Name: "alpha", Path: filepath.Join(t.TempDir(), ".dungeon", "alpha", "plan.yaml"), LastTouch: now.Add(-3 * time.Hour)},
			{Name: "beta", Path: filepath.Join(t.TempDir(), ".dungeon", "beta", "plan.yaml"), LastTouch: now.Add(-1 * time.Hour)},
		},
		Rows: []FleetRow{
			{
				Encounter: "alpha", ID: "A", Minion: "w1", Role: "worker",
				Status: "blocked", Pane: "%1", Branch: "fleet/w1",
				Worktree: "/repo/alpha.worktrees/w1", Ahead: 2, Dirty: 1,
				Accept: "go test ./...", LastReport: &report,
				LastTouch: now.Add(-3 * time.Hour),
				WT:        &FleetWT{Added: intp(7), Deleted: intp(3)},
			},
			{
				Encounter: "alpha", ID: "B", Minion: "w2", Role: "worker",
				Status: "working", Pane: "%2", Branch: "fleet/w2",
				LastTouch: now.Add(-3 * time.Hour),
			},
			{
				Encounter: "alpha", ID: "C", Minion: "r1", Role: "reviewer",
				Status: "planned", Branch: "fleet/r1",
				LastTouch: now.Add(-3 * time.Hour),
			},
			{
				Encounter: "alpha", ID: "D", Minion: "w3", Role: "worker",
				Status: "done", Pane: "%4", Branch: "fleet/w3",
				LastTouch: now.Add(-3 * time.Hour),
			},
			{
				Encounter: "beta", ID: "E", Minion: "w4", Role: "worker",
				Status: "done", Branch: "fleet/w4",
				LastTouch: now.Add(-1 * time.Hour),
			},
		},
		Sources: FleetSources{
			Worktrunk: FleetSource{Available: true},
			Hunk:      FleetSource{Available: true},
		},
	}
	m := newFleetViewModel(time.Second, "")
	m.width, m.height = 140, 40
	m.model = model
	m.rebuildOrder(fleetRowKey(model.Rows[0]))
	m.preview = "preview line one\npreview line two"
	m.previewFor = fleetRowKey(model.Rows[0])
	m.previewHeader = "lane %1"
	return model, m, model.Rows[0]
}

func intp(n int) *int { return &n }

func TestFleetViewRendersSectionsRowsAndPreview(t *testing.T) {
	_, m, _ := fleetViewFixture(t)
	out := m.View()

	for _, want := range []string{
		"dungeon fleet",
		"BLOCKED 1", "WORKING 1", "READY 1", "SETTLED 2",
		"A", "w1 worker", "%1", "fleet/w1", "+7/-3", "report 2h ago",
		"B", "w2 worker", "%2",
		"1 settled", "1 settled, nothing live",
		"PREVIEW", "preview line one",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	order := []string{"BLOCKED", "WORKING", "READY", "SETTLED"}
	last := -1
	for _, label := range order {
		i := strings.Index(out, label)
		if i <= last {
			t.Fatalf("section %s is out of order in:\n%s", label, out)
		}
		last = i
	}
}

func TestFleetViewWithoutEncountersOrBackend(t *testing.T) {
	m := newFleetViewModel(time.Second, "no-such-mux")
	m.width, m.height = 100, 24
	out := m.View()
	for _, want := range []string{"dungeon fleet", "no encounters under .dungeon", "PREVIEW", "no lane selected"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	t.Chdir(t.TempDir())
	code, stdout, stderr := runCLI("fleet", "--once", "--mux", "no-such-mux", "--width", "100", "--height", "24")
	if code != 0 {
		t.Fatalf("fleet --once exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "no encounters under .dungeon") {
		t.Fatalf("fleet --once should render a clear empty line, got:\n%s", stdout)
	}
}

func TestFleetPreviewFallsBackToTheReportWhenTheLaneIsGone(t *testing.T) {
	dir := t.TempDir()
	encDir := filepath.Join(dir, ".dungeon", "alpha")
	if err := os.MkdirAll(filepath.Join(encDir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(encDir, "reports", "A.md"), []byte("report body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := FleetModel{
		Encounters: []FleetEncounter{{Name: "alpha", Path: filepath.Join(encDir, "plan.yaml")}},
		Rows:       []FleetRow{{Encounter: "alpha", ID: "A", Pane: "%1"}},
	}
	text, key, header, errText := fleetPreview(t.Context(), "no-such-mux", model, fleetRowKey(model.Rows[0]))
	if text != "report body\n" {
		t.Fatalf("preview %q, want the report body", text)
	}
	if key != fleetRowKey(model.Rows[0]) || !strings.Contains(header, "report A") {
		t.Fatalf("preview key %q header %q", key, header)
	}
	if !strings.Contains(errText, "unknown mux") {
		t.Fatalf("the unreadable lane should be named, got %q", errText)
	}
	if !strings.Contains(header, "not live") {
		t.Fatalf("a report standing in for a dead pane must say the lane is not live, got %q", header)
	}

	model.Rows[0].Pane = ""
	text, _, header, errText = fleetPreview(t.Context(), "no-such-mux", model, fleetRowKey(model.Rows[0]))
	if text != "report body\n" || !strings.Contains(header, "report A") {
		t.Fatalf("a gone lane should show the report: text %q header %q", text, header)
	}
	if !strings.Contains(header, "not live") || !strings.Contains(errText, "no live lane") {
		t.Fatalf("a gone lane is not a live one: header %q err %q", header, errText)
	}
}

func boolp(b bool) *bool { return &b }

func fleetLine(t *testing.T, out, want string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no line holding %q in:\n%s", want, out)
	return ""
}

// A source that failed must not render as a quiet zero, and the header must
// name it.
func TestFleetViewNamesTheSourceThatFailed(t *testing.T) {
	now := time.Now()
	model := FleetModel{
		Encounters: []FleetEncounter{{
			Name:      "alpha",
			Path:      filepath.Join(t.TempDir(), ".dungeon", "alpha", "plan.yaml"),
			LastTouch: now,
		}},
		Rows: []FleetRow{{
			Encounter: "alpha", ID: "A", Minion: "w1", Role: "worker", Status: "working",
			Pane: "%1", Branch: "fleet/w1", Worktree: "/repo/alpha.worktrees/w1", LastTouch: now,
		}},
		Sources: FleetSources{
			Worktrunk: FleetSource{Err: "wt: exit status 1"},
			Hunk:      FleetSource{Available: true},
		},
	}
	m := newFleetViewModel(time.Second, "")
	m.width, m.height = 140, 40
	m.model, m.modelAt = model, now.Add(-3*time.Minute)
	out := m.View()
	for _, want := range []string{
		"wt unavailable: wt: exit status 1",
		"hunk ok",
		"snapshot 3m ago",
		"changed is the uncommitted diff",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if row := fleetLine(t, out, "w1 worker"); !strings.Contains(row, "?") {
		t.Fatalf("a failed wt must leave the changed cell unknown, not a zero: %q", row)
	}
}

func TestFleetChangedSaysUnknownNotZero(t *testing.T) {
	cases := []struct {
		name string
		row  FleetRow
		want string
	}{
		{"worktree with no wt fact", FleetRow{Worktree: "/repo/w1"}, "?"},
		{"wt fact with no diff", FleetRow{Worktree: "/repo/w1", WT: &FleetWT{}}, "+?/-?"},
		{"a real zero", FleetRow{Worktree: "/repo/w1", WT: &FleetWT{Added: intp(0), Deleted: intp(0)}}, "+0/-0"},
		{"no worktree at all", FleetRow{}, "-"},
	}
	for _, c := range cases {
		if got := fleetChanged(c.row); got != c.want {
			t.Fatalf("%s: changed %q, want %q", c.name, got, c.want)
		}
	}
}

// LoadState answers an empty state for a missing file and UnitStatus answers
// planned. The view must name the missing state, not claim one.
func TestFleetViewNamesAMissingStateFile(t *testing.T) {
	encDir := filepath.Join(t.TempDir(), ".dungeon", "alpha")
	if err := os.MkdirAll(encDir, 0o755); err != nil {
		t.Fatal(err)
	}
	model := FleetModel{
		Encounters: []FleetEncounter{{Name: "alpha", Path: filepath.Join(encDir, "plan.yaml"), LastTouch: time.Now()}},
		Rows:       []FleetRow{{Encounter: "alpha", ID: "A", Status: "planned"}},
		Sources:    FleetSources{Worktrunk: FleetSource{Available: true}, Hunk: FleetSource{Available: true}},
	}
	m := newFleetViewModel(time.Second, "")
	m.width, m.height = 120, 30
	m.model = model
	out := m.View()
	for _, want := range []string{"no state.json", "alpha", "read as planned"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	if err := os.WriteFile(filepath.Join(encDir, "state.json"), []byte("{\"units\":{}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := m.View(); strings.Contains(out, "no state.json") {
		t.Fatalf("a state file is not a missing one:\n%s", out)
	}
}

// A dead lane standing behind a report must keep saying it is dead.
func TestFleetViewKeepsThePreviewReadErrorBesideTheReport(t *testing.T) {
	_, m, _ := fleetViewFixture(t)
	m.previewErr = "lane unreadable, the report stands in for the pane: pane %1 is gone"
	out := m.View()
	if !strings.Contains(out, "pane %1 is gone") {
		t.Fatalf("the read failure must stand beside the report:\n%s", out)
	}
	if m.preview == "" {
		t.Fatal("the fixture should carry a preview")
	}
}

// Hunk that could not be asked is not a worktree with no session.
func TestFleetHunkActionSaysWhenHunkCouldNotBeAsked(t *testing.T) {
	model, m, row := fleetViewFixture(t)
	model.Sources.Hunk = FleetSource{Err: "hunk: exit status 1"}
	m.model = model
	it := fleetItem{row: model.Rows[0], encounter: row.Encounter, label: row.ID}
	msg, ok := m.hunkCmd(it)().(fleetNoticeMsg)
	if !ok {
		t.Fatal("the hunk action should answer with a notice")
	}
	if !strings.Contains(msg.text, "could not be asked") || !strings.Contains(msg.text, "exit status 1") {
		t.Fatalf("notice %q should say hunk could not be asked", msg.text)
	}
	if strings.Contains(msg.text, "no hunk session") {
		t.Fatalf("notice %q claims something it cannot know", msg.text)
	}

	model.Sources.Hunk = FleetSource{Available: true}
	m.model = model
	msg, _ = m.hunkCmd(it)().(fleetNoticeMsg)
	if !strings.Contains(msg.text, "no hunk session on this worktree") {
		t.Fatalf("an answered source with no session should say so, got %q", msg.text)
	}
}

// A folded row names an encounter. It never carried a pane, a branch or a diff.
func TestFleetViewFoldedSummaryCarriesNoFieldItNeverHad(t *testing.T) {
	_, m, _ := fleetViewFixture(t)
	out := m.View()
	line := fleetLine(t, out, "1 settled")
	for _, bad := range []string{"?"} {
		if strings.Contains(line, bad) {
			t.Fatalf("folded summary drew %q for a field it never carried: %q", bad, line)
		}
	}
	if strings.Contains(line, "-") {
		t.Fatalf("folded summary drew a pane, a branch or a diff it never carried: %q", line)
	}
}

func TestFleetDetailDoesNotPrintZeroAheadDirty(t *testing.T) {
	model, m, _ := fleetViewFixture(t)
	m.model = model
	m.overlay = "detail"

	m.rebuildOrder(fleetRowKey(model.Rows[2])) // planned, no worktree: nothing was recorded
	out := m.View()
	if strings.Contains(out, "0 / 0") {
		t.Fatalf("a lane that recorded neither must not read 0 / 0:\n%s", out)
	}
	if !strings.Contains(out, "never recorded") {
		t.Fatalf("the detail must say ahead and dirty were never recorded:\n%s", out)
	}

	m.rebuildOrder(fleetRowKey(model.Rows[0])) // spawned: 2 ahead, 1 dirty
	out = m.View()
	if !strings.Contains(out, "2 / 1, recorded at spawn") || !strings.Contains(out, "no timestamp") {
		t.Fatalf("a recorded ahead and dirty must show and say where it came from:\n%s", out)
	}
}

func TestFleetDetailShowsStaleChecksAndMergeable(t *testing.T) {
	model, m, _ := fleetViewFixture(t)
	model.Rows[0].WT = &FleetWT{
		Added:   intp(7),
		Deleted: intp(3),
		Checks:  &FleetChecks{Status: "passing", Stale: true},
		PR:      &FleetPR{Number: 42, Review: "approved", Mergeable: boolp(false)},
	}
	m.model = model
	m.overlay = "detail"
	m.rebuildOrder(fleetRowKey(model.Rows[0]))
	out := m.View()
	for _, want := range []string{"passing (stale", "#42 approved", "not mergeable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFleetJSONIsStable(t *testing.T) {
	t.Chdir(t.TempDir())
	code, first, stderr := runCLI("fleet", "--json")
	if code != 0 {
		t.Fatalf("fleet --json exit %d, stderr %q", code, stderr)
	}
	code, second, _ := runCLI("fleet", "--json")
	if code != 0 {
		t.Fatalf("fleet --json second exit %d", code)
	}
	if first != second {
		t.Fatalf("fleet JSON changed between runs:\n%s\n---\n%s", first, second)
	}
	for _, want := range []string{`"encounters": []`, `"rows": []`, `"worktrunk"`, `"hunk"`} {
		if !strings.Contains(first, want) {
			t.Fatalf("missing %q in JSON:\n%s", want, first)
		}
	}
}

func TestFleetViewNamesABrokenPlanUnderBlocked(t *testing.T) {
	now := time.Now()
	model := FleetModel{
		Encounters: []FleetEncounter{{
			Name:      "broken",
			Path:      "/tmp/broken/plan.yaml",
			LastTouch: now,
			Err:       "plan.yaml: unexpected end of file",
		}},
		Rows: []FleetRow{},
	}
	m := newFleetViewModel(time.Second, "")
	m.width, m.height = 120, 24
	m.model = model
	m.rebuildOrder("")
	out := m.View()
	for _, want := range []string{"BLOCKED 1", "unreadable", "broken", "unexpected end of file"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "READY 1") {
		t.Fatalf("a broken plan is blocked, not ready:\n%s", out)
	}
}
