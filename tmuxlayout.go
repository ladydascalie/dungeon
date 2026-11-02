package main

import (
	"encoding/json"
	"fmt"
	"math"
)

// The rail is declared into a window's layout instead of being split into it.
// A split takes its columns from one neighbour, so the pane beside the rail
// shrinks and every other pane keeps its width; repeat the split after a
// restart and the imbalance compounds. A declared layout scales every pane in
// the window by the same factor, and a restart puts the new rail where the old
// one stood without resizing anything.

// windowLayout is tmux's JSON layout for one window, as #{window_layout} prints
// it from tmux 3.4 on.
type windowLayout struct {
	V int        `json:"V"`
	L layoutCell `json:"L"`
}

// layoutCell is one node of that tree: a horizontal split, a vertical split, or
// a pane. Index numbers panes in tree order and must run from zero on every
// write, so a replaced rail is renumbered with the rest.
type layoutCell struct {
	T     string       `json:"t"` // h, v or p
	W     int          `json:"w"`
	H     int          `json:"h"`
	X     int          `json:"x"`
	Y     int          `json:"y"`
	A     bool         `json:"a,omitempty"` // the active pane, exactly one
	I     string       `json:"I,omitempty"` // pane id
	Index *int         `json:"i,omitempty"`
	C     []layoutCell `json:"c,omitempty"`
}

func (c layoutCell) clone() layoutCell {
	out := c
	if c.C != nil {
		out.C = make([]layoutCell, len(c.C))
		for i := range c.C {
			out.C[i] = c.C[i].clone()
		}
	}
	if c.Index != nil {
		index := *c.Index
		out.Index = &index
	}
	return out
}

// scaleInto shrinks every cell by factor and moves the window right by shift.
// Heights are untouched, so a stack stays a stack and only its width changes.
func (c *layoutCell) scaleInto(factor float64, shift int) {
	c.W = scaledWidth(c.W, factor)
	c.X = shift + scaledWidth(c.X, factor)
	for i := range c.C {
		c.C[i].scaleInto(factor, shift)
	}
}

func scaledWidth(n int, factor float64) int {
	return max(1, int(math.Round(float64(n)*factor)))
}

// renumber gives every pane a fresh index in tree order, which tmux refuses to
// accept otherwise.
func (c *layoutCell) renumber(next *int) {
	if c.T == "p" {
		index := *next
		c.Index = &index
		*next = index + 1
		return
	}
	for i := range c.C {
		c.C[i].renumber(next)
	}
}

// clearActive drops the active mark from every pane in the subtree. tmux
// refuses a layout with two active panes, and a nested pane can hold the mark.
func (c *layoutCell) clearActive() {
	c.A = false
	for i := range c.C {
		c.C[i].clearActive()
	}
}

func (c *layoutCell) retarget(old, fresh string) bool {
	if c.T == "p" {
		if c.I == old {
			c.I = fresh
			return true
		}
		return false
	}
	for i := range c.C {
		if c.C[i].retarget(old, fresh) {
			return true
		}
	}
	return false
}

func marshalLayout(root layoutCell) (string, error) {
	encoded, err := json.Marshal(windowLayout{V: 2, L: root})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// railDeclaredLayout puts a rail of width columns in the first column, and
// scales the panes that were there into the rest of the window.
func railDeclaredLayout(lay windowLayout, rail string, width int) (string, error) {
	if lay.L.W <= width+1 {
		return "", fmt.Errorf("a %d column rail leaves no room in a %d column window", width, lay.L.W)
	}
	contentWidth := lay.L.W - width
	factor := float64(contentWidth) / float64(lay.L.W)
	content := lay.L.clone()
	content.scaleInto(factor, width)
	content.W = contentWidth
	content.X = width
	content.clearActive()
	railCell := layoutCell{T: "p", W: width, H: lay.L.H, X: 0, Y: 0, A: true, I: rail}
	root := layoutCell{
		T: "h", W: lay.L.W, H: lay.L.H, X: 0, Y: 0,
		C: []layoutCell{railCell, content},
	}
	next := 0
	root.renumber(&next)
	return marshalLayout(root)
}

// railRetargetedLayout puts a fresh rail where the old one stood, with every
// other width untouched. This is how a restart costs no drift.
func railRetargetedLayout(lay windowLayout, old, fresh string) (string, error) {
	root := lay.L.clone()
	if !root.retarget(old, fresh) {
		return "", fmt.Errorf("the layout has no pane %s", old)
	}
	next := 0
	root.renumber(&next)
	return marshalLayout(root)
}
