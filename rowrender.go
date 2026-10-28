package main

import (
	"fmt"
	"strings"
)

// displayRow is the text a row shows, which is not always the unit's own text.
// A loop summary row names the loop and its round progress, and a generated
// child hides the long goal and the loop id prefix. The card still reads the
// unit, so nothing is lost.
type displayRow struct {
	u    UnitRow
	id   string // display id
	goal string // display goal
	lane string // lane column; empty leaves it out
}

// rowCells splits a row where the eye styles it: the head (connectors and fold
// mark), the id, the lane (minion and pane), the goal, and the status. The lane
// and goal travel together in rest so the layout can truncate one string.
type rowCells struct {
	head  string
	lead  string
	id    string
	rest  string
	right string
	lane  int // rune length of the lane prefix inside rest, separator included
}

// collapseSpace flattens every whitespace run, newlines included, to a single
// space. A goal may come from a YAML block scalar, and the row is one line.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// laneText is the minion and its live pane, or the pane hint with no agent.
func laneText(u UnitRow) string {
	where := u.Minion
	if u.Pane != "" {
		where += " " + u.Pane
	}
	return where
}

// unitRow is a display row for a snapshot unit.
func unitRow(u UnitRow) displayRow {
	return displayRow{u: u, id: u.ID, goal: u.Goal, lane: laneText(u)}
}

// rowParts splits a row into the columns' worth of text. The id and the lane are
// identifiers, so the painter keeps them bold; the goal is the hint.
func (m viewModel) rowParts(d displayRow, prefix, suffix, fold string) rowCells {
	u := d.u
	join := ""
	if len(u.Inputs) > 1 {
		join = fmt.Sprintf("∧%d", len(u.Inputs))
	}
	lane := d.lane
	if lane != "" {
		lane = pad(truncate(lane, 22), whereWidth(m.snap))
	}
	mark := []string{}
	for _, p := range []string{join, lane} {
		if p != "" {
			mark = append(mark, p)
		}
	}
	pieces := append(mark, collapseSpace(d.goal))
	laneWidth := 0
	if len(mark) > 0 {
		laneWidth = runeLen(strings.Join(mark, " ")) + 1
	}
	return rowCells{
		head:  prefix + fold,
		lead:  leadFor(prefix + fold),
		id:    pad(d.id, idWidth(m.snap)),
		rest:  strings.Join(pieces, " "),
		right: linkMark(u) + m.statusText(u) + suffix,
		lane:  laneWidth,
	}
}

func leadFor(head string) string {
	if head != "" {
		return " " // air between the fold mark and the id
	}
	return ""
}

// metricsOf sizes the two columns for the rows that are about to be drawn. The
// status column keeps its own width and the small gap; the left column takes
// the rest of the frame, so the table reaches the edge at every terminal width.
func (m viewModel) metricsOf(rows []rowCells, avail int) rowMetrics {
	const pad = 2
	right := 0
	for _, c := range rows {
		right = max(right, runeLen(c.right))
	}
	left := max(1, avail-right-pad)
	return rowMetrics{left: left, right: right, pad: pad}
}

// rowLine draws one row against those columns: the left text padded to the
// column, the gap, then the status right aligned inside its own column. The
// status is drawn even when the left column is too narrow for the id, so a
// narrow terminal truncates the goal harder and never the status.
func (m viewModel) rowLine(d displayRow, width int, prefix, suffix, fold string, mt rowMetrics) string {
	c := m.rowParts(d, prefix, suffix, fold)

	right := truncate(c.right, mt.right)
	if mt.right > runeLen(right) {
		right = strings.Repeat(" ", mt.right-runeLen(right)) + right
	}
	tail := strings.Repeat(" ", mt.pad) + m.paintRight(d.u, right)

	fixed := runeLen(c.head) + runeLen(c.lead) + runeLen(c.id) + 1
	if mt.left < fixed {
		plain := pad(truncate(c.head+c.lead+c.id, mt.left), mt.left)
		return m.paintRow(d.u, plain, "", "", "", 0) + tail
	}
	// The padding belongs to the rest of the row, not to the line: paintRow
	// rebuilds the row from its parts, so spaces added to the assembled string
	// would be dropped and the status would drift left on short rows.
	rest := truncate(c.rest, max(0, mt.left-fixed))
	rest += strings.Repeat(" ", max(0, mt.left-fixed-runeLen(rest)))
	plain := c.head + c.lead + c.id + " " + rest

	return m.paintRow(d.u, plain, c.head+c.lead, c.id, rest, min(c.lane, runeLen(rest))) + tail
}

// paintRow styles the left side. The selected row is plain text inside reverse
// video, so nesting colours inside the highlight cannot fight it. The id and
// lane stay bold; the goal is faint.
func (m viewModel) paintRow(u UnitRow, plain, head, id, rest string, lane int) string {
	if u.ID == m.selectedID() {
		return selectedStyle.Render(plain)
	}
	if head == "" && id == "" {
		return plain
	}
	r := []rune(rest)
	lane = min(lane, len(r))
	return dimStyle.Render(head) + boldStyle.Render(id) + " " +
		boldStyle.Render(string(r[:lane])) + dimStyle.Render(string(r[lane:]))
}
