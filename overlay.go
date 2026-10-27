package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// The overlay is the per-unit detail pane: what the unit is, what it waits for,
// and the files kept with it. The list can only show a row; a brief or a report
// needs somewhere to be read.

type artifact struct {
	Path  string
	Label string
	Size  int64
	Mod   time.Time
	Err   string
}

// artifacts resolves the files a unit keeps: the ones attached by hand, then the
// brief and report a dispatch writes under the encounter directory, which is
// the plan's own .dungeon/ so they live as long as the plan does.
func (m viewModel) artifacts(u UnitRow) []artifact {
	var out []artifact
	seen := map[string]bool{}

	dir := filepath.Dir(m.statePath)
	// A label is the shortest name that is unambiguous on screen: relative to
	// the state directory when the file lives there, else to home.
	label := func(path string) string {
		if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
		if home, err := os.UserHomeDir(); err == nil {
			if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
				return "~/" + rel
			}
		}
		return path
	}

	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		a := artifact{Path: path, Label: label(path)}
		if info, err := os.Stat(path); err != nil {
			a.Err = "missing"
		} else if info.IsDir() {
			a.Err = "directory"
		} else {
			a.Size, a.Mod = info.Size(), info.ModTime()
		}
		out = append(out, a)
	}

	if us, ok := m.state.Units[u.ID]; ok {
		for _, f := range us.Files {
			add(f)
		}
	}
	add(filepath.Join(dir, "briefs", u.ID+".md"))
	add(filepath.Join(dir, "reports", u.ID+".md"))
	return out
}

func (m viewModel) detailOverlay() string {
	u, ok := m.selected()
	if !ok {
		return dimStyle.Render("no units")
	}
	inner := max(20, min(m.width-6, 110))
	var b strings.Builder

	b.WriteString(titleStyle.Render(u.ID) + "  " + statusStyles[u.Status].Render(u.Status) +
		dimStyle.Render(fmt.Sprintf("  %s %s", u.Role, u.Minion)) + "\n\n")
	b.WriteString(wrap(u.Goal, inner) + "\n\n")

	deps := fmt.Sprintf("deps %d/%d", u.DepsDone, u.DepsTotal)
	if len(u.WaitsOn) > 0 {
		deps += " · waits " + strings.Join(u.WaitsOn, " ")
	}
	if u.Pane != "" {
		deps += " · pane " + u.Pane
		if u.Agent != "" {
			deps += " (" + u.Agent + ")"
		}
	}
	b.WriteString(dimStyle.Render(truncate(deps, inner)) + "\n")
	if u.Note != "" {
		b.WriteString(wrap("note: "+u.Note, inner) + "\n")
	}

	files := m.artifacts(u)
	if u.Link != "" {
		b.WriteString("\n" + sectionStyle.Render("LINK") + "\n" + truncate(u.Link, inner) + "\n")
	}
	b.WriteString("\n" + sectionStyle.Render(fmt.Sprintf("FILES %d", len(files))) + "\n")
	for i, a := range files {
		mark := " "
		if i == m.artIdx {
			mark = "▸"
		}
		line := fmt.Sprintf("%s %s", mark, a.Label)
		meta := a.Err
		if meta == "" {
			meta = fmt.Sprintf("%s  %s", humanSize(a.Size), age(a.Mod.UTC().Format(time.RFC3339)))
		}
		line += "  " + dimStyle.Render(meta)
		if i == m.artIdx {
			line = selectedStyle.Render(truncate(fmt.Sprintf("%s %s  %s", mark, a.Label, meta), inner))
		}
		b.WriteString(line + "\n")
	}
	if len(files) == 0 {
		b.WriteString(dimStyle.Render("none. attach one with: dungeon attach "+u.ID+" <path>") + "\n")
	}

	if us, ok := m.state.Units[u.ID]; ok && len(us.History) > 0 {
		b.WriteString("\n" + sectionStyle.Render("HISTORY") + "\n")
		start := max(0, len(us.History)-5)
		for _, c := range us.History[start:] {
			when := c.At
			if t, err := time.Parse(time.RFC3339, c.At); err == nil {
				when = t.Local().Format("15:04")
			}
			line := fmt.Sprintf("%s %s → %s", when, orDash(c.From), c.To)
			if c.Note != "" {
				line += "  " + c.Note
			}
			b.WriteString(dimStyle.Render(wrap(line, inner)) + "\n")
		}
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(inner).
		Render(b.String())
	hint := dimStyle.Render("tab file · enter read · d close")
	return lipgloss.Place(m.width, max(4, m.height-5), lipgloss.Center, lipgloss.Center, box) + "\n" + hint
}

// readerDisplay is what the reader draws: rendered markdown when the file was
// rendered at this width, else the source lines. Nothing here renders.
func (m viewModel) readerDisplay() []string {
	if len(m.readerRendered) > 0 {
		return m.readerRendered
	}
	return m.readerLines
}

// renderReader turns the source into display lines once, when the file is
// opened and again only when the pane's width changes. Rendering in the frame
// path made every keystroke pay for a full markdown parse.
func (m *viewModel) renderReader() {
	m.readerRendered = nil
	if !isMarkdown(m.reader) || len(m.readerLines) == 0 || len(m.readerLines) > 2000 {
		m.readerWidth = 0
		return
	}
	width := max(40, m.width-8)
	if width == m.readerWidth && len(m.readerRendered) > 0 {
		return
	}
	m.readerRendered = renderMarkdown(strings.Join(m.readerLines, "\n"), width)
	m.readerWidth = width
}

func (m viewModel) readerView() string {
	inner := max(20, m.width-6)
	rows := max(3, m.height-8)
	lines := m.readerDisplay()
	body := ""
	if len(lines) == 0 {
		body = dimStyle.Render("empty file")
	} else {
		end := min(len(lines), m.readerTop+rows)
		body = strings.Join(lines[m.readerTop:end], "\n")
	}
	title := fmt.Sprintf("%s  %d lines", filepath.Base(m.reader), len(lines))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(inner).
		Height(rows).
		Render(titleStyle.Render(title) + "\n" + body)
	hint := dimStyle.Render("j/k scroll · esc back · d close")
	return lipgloss.Place(m.width, max(4, m.height-5), lipgloss.Center, lipgloss.Center, box) + "\n" + hint
}

func (m *viewModel) openArtifact() {
	u, ok := m.selected()
	if !ok {
		return
	}
	files := m.artifacts(u)
	if len(files) == 0 || m.artIdx >= len(files) {
		return
	}
	a := files[m.artIdx]
	if a.Err != "" {
		return
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		m.reader, m.readerLines = a.Path, []string{"error: " + err.Error()}
	} else {
		m.reader = a.Path
		m.readerLines = strings.Split(strings.ReplaceAll(string(data), "\t", "    "), "\n")
		if len(m.readerLines) > 5000 {
			m.readerLines = m.readerLines[:5000]
		}
	}
	m.readerTop = 0
	m.overlay = "reader"
	m.renderReader()
}

func (m *viewModel) handleOverlayKey(key string) bool {
	switch m.overlay {
	case "detail":
		switch key {
		case "d":
			m.overlay = ""
		case "esc":
			m.overlay = ""
		case "tab", "l", "right":
			if n := len(m.artifacts(m.mustSelected())); n > 0 {
				m.artIdx = (m.artIdx + 1) % n
			}
		case "shift+tab", "h", "left":
			if n := len(m.artifacts(m.mustSelected())); n > 0 {
				m.artIdx = (m.artIdx + n - 1) % n
			}
		case "enter":
			m.openArtifact()
		default:
			return false
		}
		return true
	case "reader":
		switch key {
		case "j", "down":
			if m.readerTop+1 < len(m.readerDisplay()) {
				m.readerTop++
			}
		case "k", "up":
			if m.readerTop > 0 {
				m.readerTop--
			}
		case "g", "home":
			m.readerTop = 0
		case "G", "end":
			m.readerTop = max(0, len(m.readerDisplay())-1)
		case "esc", "d", "enter":
			m.overlay = "detail"
		default:
			return false
		}
		return true
	}
	return false
}

func (m viewModel) mustSelected() UnitRow {
	u, _ := m.selected()
	return u
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
}
