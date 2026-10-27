package main

import (
	"path/filepath"
	"strings"

	"github.com/charmbracelet/glamour"
)

// Briefs and reports are markdown. Rendering them as plain lines throws away the
// structure the lane wrote: headings, lists, code, emphasis. The reader renders
// them instead, and falls back to the raw lines when glamour cannot.

func isMarkdown(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdown", ".mkd":
		return true
	}
	return false
}

// The renderer is built once per width and reused. Building one per frame cost
// milliseconds and, worse, a style chosen by probing the terminal, which fights
// the TUI for the same tty: the view froze for seconds on the first render. A
// fixed style and a cached renderer keep the frame path free of both.
var (
	mdRenderer *glamour.TermRenderer
	mdWidth    int
)

func markdownRenderer(width int) *glamour.TermRenderer {
	if mdRenderer != nil && mdWidth == width {
		return mdRenderer
	}
	// notty keeps the structure and drops the per-word colour and padding that
	// the dark style emits: tens of characters of escape code per line, which
	// made the reader heavy to draw and slow to scroll. The view has its own
	// styling; the markdown only has to keep headings, lists and code legible.
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("notty"),
		glamour.WithWordWrap(max(20, width)),
	)
	if err != nil {
		return nil
	}
	mdRenderer, mdWidth = r, width
	return r
}

// renderMarkdown returns styled lines at the given width.
func renderMarkdown(src string, width int) []string {
	r := markdownRenderer(width)
	if r == nil {
		return strings.Split(src, "\n")
	}
	out, err := r.Render(src)
	if err != nil {
		return strings.Split(src, "\n")
	}
	raw := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, line := range raw {
		raw[i] = strings.TrimRight(line, " ")
	}
	return raw
}
