package main

import "github.com/charmbracelet/lipgloss"

// View clamps the frame to the terminal. A frame one line taller than the
// screen scrolls the alt screen and pushes the header off the top, and the
// operator then loses the counts they were reading. The clamp is the guarantee:
// the renderer may be wrong about its budget, the frame still fits.
func (m viewModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return m.render()
	}
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(m.render())
}
