package main

import (
	"fmt"
	"strings"
)

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

// rowParts splits a row into the columns' worth of text. The id and the lane are
// identifiers, so the painter keeps them bold; the goal is the hint.
func (m viewModel) rowParts(u UnitRow, prefix, suffix, fold string) rowCells {
	join := ""
	if len(u.Inputs) > 1 {
		join = fmt.Sprintf(" ∧%d", len(u.Inputs))
	}
	where := u.Minion
	if u.Pane != "" {
		where += " " + u.Pane
	}
	head := prefix + fold
	lead := ""
	if head != "" {
		lead = " " // air between the fold mark and the id
	}
	lane := strings.TrimLeft(join+" "+pad(truncate(where, 22), whereWidth(m.snap)), " ")
	laneWidth := 0
	if lane != "" {
		laneWidth = runeLen(lane) + 1
	}
	rest := strings.TrimLeft(lane+" "+collapseSpace(u.Goal), " ")
	return rowCells{
		head:  head,
		lead:  lead,
		id:    pad(u.ID, idWidth(m.snap)),
		rest:  rest,
		right: linkMark(u) + m.statusText(u) + suffix,
		lane:  laneWidth,
	}
}

// metricsOf sizes the two columns for the rows that are about to be drawn: the
// left column as wide as its longest line, the right column as wide as its
// longest status, and a small fixed gap between them.
func (m viewModel) metricsOf(rows []rowCells, avail int) rowMetrics {
	const pad = 2
	left, right := 0, 0
	for _, c := range rows {
		left = max(left, runeLen(c.head+c.lead+c.id+" "+c.rest))
		right = max(right, runeLen(c.right))
	}
	left = min(left, max(20, avail-right-pad))
	return rowMetrics{left: left, right: right, pad: pad}
}

// rowLine draws one row against those columns: the left text padded to the
// column, the gap, then the status right aligned inside its own column.
func (m viewModel) rowLine(u UnitRow, width int, prefix, suffix, fold string, mt rowMetrics) string {
	c := m.rowParts(u, prefix, suffix, fold)

	fixed := runeLen(c.head) + runeLen(c.lead) + runeLen(c.id) + 1
	if mt.left < fixed {
		plain := truncate(c.head+c.lead+c.id, max(4, mt.left))
		return m.paintRow(u, plain, "", "", "", 0)
	}
	// The padding belongs to the rest of the row, not to the line: paintRow
	// rebuilds the row from its parts, so spaces added to the assembled string
	// would be dropped and the status would drift left on short rows.
	rest := truncate(c.rest, max(0, mt.left-fixed))
	rest += strings.Repeat(" ", max(0, mt.left-fixed-runeLen(rest)))
	plain := c.head + c.lead + c.id + " " + rest

	right := truncate(c.right, mt.right)
	if mt.right > runeLen(right) {
		right = strings.Repeat(" ", mt.right-runeLen(right)) + right
	}
	return m.paintRow(u, plain, c.head+c.lead, c.id, rest, min(c.lane, runeLen(rest))) +
		strings.Repeat(" ", mt.pad) + m.paintRight(u, right)
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
