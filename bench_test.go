package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A report the size of a real one: headings, lists, paragraphs and code.
func bigReport(lines int) string {
	var b strings.Builder
	for i := 0; b.Len() < 0 && i < 0; i++ {
	}
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "## Step %d\n\n- changed `file%d.go` and ran the build\n- left the rest alone\n\nSome prose about step %d, long enough to wrap at a normal panel width and prove the renderer is doing work.\n\n```sh\ngo test ./...\n```\n\n", i, i, i)
	}
	return b.String()
}

func BenchmarkRenderMarkdown(b *testing.B) {
	src := bigReport(40) // ~400 lines
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := renderMarkdown(src, 100); len(got) == 0 {
			b.Fatal("empty")
		}
	}
}

// Scrolling calls View per keystroke. It must not render markdown at all.
func BenchmarkReaderViewScrolling(b *testing.B) {
	dir := b.TempDir()
	plan := filepath.Join(dir, ".dungeon", "plan.yaml")
	if err := os.MkdirAll(filepath.Dir(plan), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(plan, []byte(statusPlan), 0o644); err != nil {
		b.Fatal(err)
	}
	report := filepath.Join(dir, ".dungeon", "reports", "A.md")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(report, []byte(bigReport(40)), 0o644); err != nil {
		b.Fatal(err)
	}
	st := State{Encounter: "st", Units: map[string]UnitState{}, Minions: map[string]MinionState{}}
	if err := st.Attach("A", report); err != nil {
		b.Fatal(err)
	}
	state := filepath.Join(dir, ".dungeon", "state.json")
	if err := SaveState(state, st); err != nil {
		b.Fatal(err)
	}

	m := newViewModel(plan, state, 0)
	m.width, m.height = 120, 40
	m = m.applyRefresh(loadSnapshot(plan, state))
	m.overlay, m.artIdx = "detail", 0
	m.openArtifact()
	if m.overlay != "reader" {
		b.Fatalf("overlay %q", m.overlay)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.readerTop = i % 20
		_ = m.View()
	}
}
