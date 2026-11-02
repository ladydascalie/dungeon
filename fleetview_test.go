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

	model.Rows[0].Pane = ""
	text, _, header, errText = fleetPreview(t.Context(), "no-such-mux", model, fleetRowKey(model.Rows[0]))
	if text != "report body\n" || !strings.Contains(header, "report A") || errText != "" {
		t.Fatalf("a gone lane should show the report: text %q header %q err %q", text, header, errText)
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
