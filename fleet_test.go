package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const fleetPlan = `encounter: alpha
repo: /repo/alpha
base: main
minions:
  - {id: w1, role: worker, worktree: w1}
  - {id: w2, role: worker, worktree: w2}
  - {id: r1, role: reviewer, of: w1}
units:
  - id: model
    minion: w1
    paths: [fleet.go]
    goal: Build the read model.
    accept: go test ./...
  - id: docs
    minion: w2
    paths: [README.md]
    goal: Document it.
    accept: go build ./...
  - id: review
    minion: r1
    deps: [model]
    goal: Review it.
`

// fleetFixture writes one encounter under root/.dungeon/alpha and returns its
// directory and the worktree the state records for w1.
func fleetFixture(t *testing.T, root string) (dir, worktree, report string) {
	t.Helper()
	t.Chdir(root)
	dir = filepath.Join(root, ".dungeon", "alpha")
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(planPath, []byte(fleetPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree = filepath.Join(root, "alpha.worktrees", "w1")
	state := State{
		Encounter: "alpha",
		Units: map[string]UnitState{
			"model": {Status: "working", Note: "brief written", At: "2026-01-01T10:00:00Z"},
		},
		Minions: map[string]MinionState{
			"w1": {
				Status:   "spawned",
				Pane:     "%1",
				Worktree: worktree,
				Branch:   "alpha/w1",
				Ahead:    2,
				Dirty:    1,
				At:       "2026-01-01T10:00:00Z",
			},
			"r1": {Status: "spawned", Pane: "%2", At: "2026-01-01T10:00:00Z"},
			"w2": {
				Status:   "spawned",
				Pane:     "%3",
				Worktree: filepath.Join(root, "alpha.worktrees", "w2"),
				Branch:   "alpha/w2",
				At:       "2026-01-01T10:00:00Z",
			},
		},
	}
	statePath := filepath.Join(dir, "state.json")
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	report = filepath.Join(dir, "reports", "model.md")
	if err := os.WriteFile(report, []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(report, base.Add(3*time.Hour), base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(planPath, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(statePath, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return dir, worktree, report
}

func TestFleetModelBuildsRowsFromFixtures(t *testing.T) {
	root := t.TempDir()
	_, worktree, _ := fleetFixture(t, root)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	wt := fmt.Sprintf(`{
  "schema": 2,
  "items": [{
    "branch": "alpha/w1",
    "worktree": {
      "path": %q,
      "changes": {"diff": {"added": 7, "deleted": 3}}
    },
    "pr": {"number": 12, "url": "https://example.test/pr/12", "review": "pending"},
    "checks": {"status": "running", "source": "pr", "stale": true}
  }]
}`, worktree)
	hunk := fmt.Sprintf(`{"sessions": [{"sessionId": "s1", "repoRoot": %q}]}`, worktree)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "wt":
			return []byte(wt), nil
		case "hunk":
			return []byte(hunk), nil
		}
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}

	model := BuildFleetModel(context.Background(), FleetOptions{
		Run:   runner,
		Cache: &FleetCache{},
		Now:   func() time.Time { return base },
	})

	if len(model.Encounters) != 1 || model.Encounters[0].Err != "" {
		t.Fatalf("encounters: %+v", model.Encounters)
	}
	if !model.Encounters[0].LastTouch.Equal(base.Add(time.Hour)) {
		t.Fatalf("last touch %v, want the state's later touch", model.Encounters[0].LastTouch)
	}
	if !model.Sources.Worktrunk.Available || !model.Sources.Hunk.Available {
		t.Fatalf("sources: %+v", model.Sources)
	}
	if len(model.Rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(model.Rows), model.Rows)
	}

	rows := map[string]FleetRow{}
	for _, row := range model.Rows {
		rows[row.ID] = row
	}
	r := rows["model"]
	if r.Encounter != "alpha" || r.Minion != "w1" || r.Role != "worker" {
		t.Fatalf("identity: %+v", r)
	}
	if r.Status != "working" {
		t.Fatalf("status %q must come from the state file", r.Status)
	}
	if r.Pane != "%1" || r.Worktree != worktree || r.Branch != "alpha/w1" || r.Ahead != 2 || r.Dirty != 1 {
		t.Fatalf("state facts: %+v", r)
	}
	if r.Accept != "go test ./..." {
		t.Fatalf("accept %q", r.Accept)
	}
	if r.LastReport == nil || !r.LastReport.Equal(base.Add(3*time.Hour)) {
		t.Fatalf("last report %v, want the report's mtime", r.LastReport)
	}
	if !r.LastTouch.Equal(base.Add(time.Hour)) {
		t.Fatalf("row last touch %v", r.LastTouch)
	}
	if r.WT == nil || r.WT.Added == nil || *r.WT.Added != 7 || r.WT.Deleted == nil || *r.WT.Deleted != 3 {
		t.Fatalf("worktrunk changed lines: %+v", r.WT)
	}
	if r.WT.PR == nil || r.WT.PR.Number != 12 || r.WT.PR.URL == "" || r.WT.PR.Review != "pending" {
		t.Fatalf("worktrunk PR: %+v", r.WT.PR)
	}
	if r.WT.Checks == nil || r.WT.Checks.Status != "running" || !r.WT.Checks.Stale {
		t.Fatalf("worktrunk checks: %+v", r.WT.Checks)
	}
	if r.Review == nil || r.Review.Session != "s1" {
		t.Fatalf("hunk review: %+v", r.Review)
	}

	// w2 has a worktree worktrunk did not report. That is unknown, not zero.
	if w := rows["docs"]; w.WT != nil {
		t.Fatalf("an unreported worktree must not gain invented facts: %+v", w.WT)
	}
	if review := rows["review"]; review.Role != "reviewer" || review.Worktree != "" || review.WT != nil || review.Review != nil {
		t.Fatalf("reviewer row: %+v", review)
	}
}

func TestFleetModelUnavailableSourcesContributeNothing(t *testing.T) {
	root := t.TempDir()
	_, worktree, _ := fleetFixture(t, root)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, exec.ErrNotFound
	}
	model := BuildFleetModel(context.Background(), FleetOptions{
		Run:   runner,
		Cache: &FleetCache{},
		Now:   func() time.Time { return base },
	})

	if model.Sources.Worktrunk.Available || model.Sources.Hunk.Available {
		t.Fatalf("both sources should be unavailable: %+v", model.Sources)
	}
	if len(model.Sources.Worktrunk.Err) == 0 || len(model.Sources.Hunk.Err) == 0 {
		t.Fatalf("an unavailable source should say why: %+v", model.Sources)
	}
	if len(model.Rows) != 3 {
		t.Fatalf("the view must still render rows: %+v", model.Rows)
	}
	for _, row := range model.Rows {
		if row.WT != nil || row.Review != nil {
			t.Fatalf("a failed source must contribute nothing: %+v", row)
		}
		if row.ID == "model" {
			if row.Status != "working" || row.Worktree != worktree || row.Pane != "%1" {
				t.Fatalf("state facts must survive both sources failing: %+v", row)
			}
		}
	}
}

func TestFleetModelCachesWorktrunkForAFewSeconds(t *testing.T) {
	root := t.TempDir()
	_, worktree, _ := fleetFixture(t, root)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	wtCalls := 0
	wt := fmt.Sprintf(`{"schema": 2, "items": [{"worktree": {"path": %q, "changes": {"diff": {"added": 0, "deleted": 0}}}}]}`, worktree)
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "wt":
			wtCalls++
			return []byte(wt), nil
		case "hunk":
			return []byte(`{"sessions": []}`), nil
		}
		return nil, fmt.Errorf("unexpected command %s", name)
	}
	opts := FleetOptions{
		Run:   runner,
		Cache: &FleetCache{},
		Now:   func() time.Time { return now },
	}

	BuildFleetModel(context.Background(), opts)
	BuildFleetModel(context.Background(), opts)
	if wtCalls != 1 {
		t.Fatalf("wt called %d times inside the cache window, want 1", wtCalls)
	}

	now = base.Add(fleetWTCacheTTL)
	BuildFleetModel(context.Background(), opts)
	if wtCalls != 2 {
		t.Fatalf("wt called %d times after the cache expired, want 2", wtCalls)
	}
}

func TestFleetModelKeepsGoodEncountersWhenOnePlanIsBroken(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	broken := filepath.Join(root, ".dungeon", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte(":::"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, ".dungeon", "good", "plan.yaml")
	writeEncounter(t, good, "good")

	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, exec.ErrNotFound
	}
	model := BuildFleetModel(context.Background(), FleetOptions{
		Run: runner,
		Now: func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
	})

	if len(model.Encounters) != 2 {
		t.Fatalf("got %d encounters, want the broken and the good one: %+v", len(model.Encounters), model.Encounters)
	}
	brokenSeen := false
	for _, enc := range model.Encounters {
		if enc.Err != "" {
			brokenSeen = true
		}
	}
	if !brokenSeen {
		t.Fatalf("the broken plan must be named: %+v", model.Encounters)
	}
	if len(model.Rows) == 0 {
		t.Fatal("the good encounter must still contribute rows")
	}
	for _, row := range model.Rows {
		if row.Encounter != "good" {
			t.Fatalf("a broken plan must not contribute rows: %+v", row)
		}
	}
}

func TestFleetModelWithoutEncountersDoesNotShellOut(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	called := false
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		called = true
		return nil, fmt.Errorf("unexpected command %s", name)
	}
	model := BuildFleetModel(context.Background(), FleetOptions{Run: runner})
	if called {
		t.Fatal("no encounter must not reach a source")
	}
	if len(model.Encounters) != 0 || len(model.Rows) != 0 {
		t.Fatalf("empty fleet: %+v", model)
	}
	if model.Sources.Worktrunk.Available || model.Sources.Hunk.Available {
		t.Fatalf("sources without an encounter are not available: %+v", model.Sources)
	}
}
