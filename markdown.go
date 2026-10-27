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

// renderMarkdown returns styled lines at the given width. Glamour picks a style
// from the terminal, so a piped frame comes out plain and a real pane gets the
// styled one, from the same call.
func renderMarkdown(src string, width int) []string {
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(max(20, width)),
	)
	if err != nil {
		return strings.Split(src, "\n")
	}
	out, err := r.Render(src)
	if err != nil {
		return strings.Split(src, "\n")
	}
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}
