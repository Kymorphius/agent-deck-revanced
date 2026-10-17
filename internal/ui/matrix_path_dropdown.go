package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
)

// matrix/hardened: bound path menus to the space below the input. Never move
// a tall menu upwards over the path being edited, and keep its selection visible.
func renderPathDropdownBox(content string, border lipgloss.Color, bounds ...int) string {
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(border).Background(dropdownMenuBg()).Padding(0, 1)
	if len(bounds) < 2 {
		return style.Render(content)
	}
	width, height := bounds[0], bounds[1]
	if width < 7 || height < 3 {
		return ""
	}
	innerWidth := width - style.GetHorizontalFrameSize()
	rows := strings.Split(content, "\n")
	focus := 0
	for i, row := range rows {
		if strings.HasPrefix(strings.TrimSpace(stripAnsi(row)), "▶ ") {
			focus = i
		}
		rows[i] = cellTruncate(row, innerWidth, "…")
	}
	sections := dialogSections{body: rows[:len(rows)-1], focus: focus, foot: rows[len(rows)-1:]}
	if height == 3 {
		// A single candidate is more useful than a footer with no candidates.
		sections.foot = nil
	}
	content = fitDialogRows(0, height-style.GetVerticalFrameSize(), sections)
	return style.Width(width - 2).Render(content)
}

// matrix/hardened: abbreviate only display text, retaining the directory tail
// and whole grapheme clusters. The stored/accepted path remains untouched.
func pathDropdownLabel(path string, bounds ...int) string {
	path = sanitizeMatchForDisplay(path)
	if len(bounds) < 2 {
		return path
	}
	width := bounds[0] - 6 // border, padding and selection prefix
	if cellWidth(path) <= width {
		return path
	}
	if width <= 1 {
		return cellTruncate(path, width, "")
	}
	graphemes := uniseg.NewGraphemes(path)
	for graphemes.Next() {
		_, end := graphemes.Positions()
		if cellWidth(path[end:]) <= width-1 {
			return "…" + path[end:]
		}
	}
	return "…"
}
