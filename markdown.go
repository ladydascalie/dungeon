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

// sparseStyleJSON keeps colour on structure only: headings, links, inline code,
// rules and code blocks. Document color is deliberately absent, so glamour does
// not wrap every prose run in an escape sequence. On a 400 line report at width
// 100 the dark style emitted 64,143 escapes and 521KB; this style emits 680 and
// 45KB. Margins match notty, so the layout does not move.
const sparseStyleJSON = `{
  "document": {"block_prefix": "\n", "block_suffix": "\n", "margin": 2},
  "paragraph": {},
  "list": {"level_indent": 2},
  "heading": {"block_suffix": "\n", "color": "39", "bold": true},
  "h1": {"prefix": " ", "suffix": " ", "color": "228", "background_color": "63", "bold": true},
  "h2": {"prefix": "## "},
  "h3": {"prefix": "### "},
  "h4": {"prefix": "#### "},
  "h5": {"prefix": "##### "},
  "h6": {"prefix": "###### ", "color": "35", "bold": false},
  "text": {},
  "strikethrough": {"crossed_out": true},
  "emph": {"italic": true},
  "strong": {"bold": true},
  "hr": {"color": "240", "format": "\n--------\n"},
  "item": {"block_prefix": "• "},
  "enumeration": {"block_prefix": ". "},
  "task": {"ticked": "[✓] ", "unticked": "[ ] "},
  "link": {"color": "30", "underline": true},
  "link_text": {"color": "35", "bold": true},
  "code": {"prefix": " ", "suffix": " ", "color": "203", "background_color": "236"},
  "code_block": {"color": "244", "margin": 2}
}`

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
	r, err := glamour.NewTermRenderer(
		glamour.WithStylesFromJSONBytes([]byte(sparseStyleJSON)),
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
