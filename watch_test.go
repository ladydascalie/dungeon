package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// drainWatch eats the extra events a single write produces until the watcher
// has been quiet for a while.
func drainWatch(out <-chan watchEvent, quiet time.Duration) {
	for {
		select {
		case <-out:
		case <-time.After(quiet):
			return
		}
	}
}

func waitWatchPath(t *testing.T, out <-chan watchEvent, base string, within time.Duration) watchEvent {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		select {
		case ev := <-out:
			if ev.Err != nil {
				t.Fatalf("watch error: %v", ev.Err)
			}
			if filepath.Base(ev.Path) == base {
				return ev
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("no watch event for %s within %s", base, within)
		}
	}
}

func TestWatchWakesOnStateReportAndPlan(t *testing.T) {
	dungeon := filepath.Join(t.TempDir(), ".dungeon")
	encounter := filepath.Join(dungeon, "alpha")
	for _, dir := range []string{encounter, filepath.Join(encounter, "reports"), filepath.Join(encounter, "briefs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan watchEvent, 64)
	if _, err := startWatch(ctx, out, watchTreeDirs(dungeon), nil); err != nil {
		t.Fatalf("startWatch: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dungeon, "plan.yaml"), []byte("encounter: alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitWatchPath(t, out, "plan.yaml", 3*time.Second)

	if err := os.WriteFile(filepath.Join(encounter, "state.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitWatchPath(t, out, "state.json", 3*time.Second)

	if err := os.WriteFile(filepath.Join(encounter, "reports", "model.md"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitWatchPath(t, out, "model.md", 3*time.Second)

	if err := os.WriteFile(filepath.Join(encounter, "briefs", "model.md"), []byte("brief\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitWatchPath(t, out, "model.md", 3*time.Second)
	drainWatch(out, 100*time.Millisecond)

	// A file the rail does not read must not wake it.
	if err := os.WriteFile(filepath.Join(encounter, "notes.txt"), []byte("noise\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-out:
		t.Fatalf("an unrelated write woke the rail: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestWatchWakesOnOSCRecordButNotItsTempFile(t *testing.T) {
	osc := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan watchEvent, 64)
	if _, err := startWatch(ctx, out, nil, []string{osc}); err != nil {
		t.Fatalf("startWatch: %v", err)
	}

	if err := os.WriteFile(osc+"/_1.json.tmp", []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-out:
		t.Fatalf("the temporary OSC write woke the rail: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
	drainWatch(out, 100*time.Millisecond)

	if err := os.WriteFile(osc+"/_1.json", []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitWatchPath(t, out, "_1.json", 3*time.Second)
}
