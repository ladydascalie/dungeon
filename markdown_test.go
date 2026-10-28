package main

import (
	"strings"
	"testing"
)

// A real report is about 400 lines. The sparse style must stay near notty's
// cost: colour on structure, not on every prose run. The dark style emitted
// 64,143 escapes and 521KB here.
func TestSparseMarkdownBudget(t *testing.T) {
	src := bigReport(40)
	if got := strings.Count(src, "\n"); got < 400 {
		t.Fatalf("report has %d lines, want at least 400", got)
	}

	lines := renderMarkdown(src, 100)
	if len(lines) < 40 {
		t.Fatalf("rendered %d lines, want at least 40", len(lines))
	}
	out := strings.Join(lines, "\n")
	escapes := strings.Count(out, "\x1b[")
	head := len(strings.Join(lines[:40], "\n"))
	t.Logf("lines=%d escapes=%d bytes=%d first40=%d", len(lines), escapes, len(out), head)

	if escapes >= 5000 {
		t.Errorf("escape sequences = %d, want below 5000", escapes)
	}
	if len(out) >= 70000 {
		t.Errorf("bytes = %d, want below 70000", len(out))
	}
	if head >= 8000 {
		t.Errorf("first 40 lines = %d bytes, want below 8000", head)
	}
}

// Colour belongs on structure. Prose stays plain, which is what keeps the
// escape count down.
func TestSparseMarkdownColoursStructure(t *testing.T) {
	out := strings.Join(renderMarkdown("# Heading\n\n[link](https://example.com)\n\n`code`\n", 100), "\n")
	if !strings.Contains(out, "\x1b[") {
		t.Fatal("rendered markdown has no colour at all")
	}
}
