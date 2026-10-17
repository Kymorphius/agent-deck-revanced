package nativeui

// matrix/hardened: the Windows IME needs the terminal cursor at the focused
// cell, even though Bubble Tea draws its own cursor and ends at the footer.

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	cursorProbeStart = "\x1b]777;agent-deck-native-cursor-probe;start\a"
	cursorProbeEnd   = "\x1b]777;agent-deck-native-cursor-probe;end\a"
)

type cursorAnchor struct{ row, column int }

type cursorFrame struct {
	anchor              cursorAnchor
	width, height, rows int
}

// cursorInputView marks the exact cursor cell in a value copy. Transforming
// both cursor styles covers the visible and blink-hidden phases without
// changing the original input's position, viewport offsets, or blink state.
func cursorInputView(input textinput.Model) string {
	if !input.Focused() {
		return input.View()
	}
	probeStyle := func(style lipgloss.Style) lipgloss.Style {
		transform := style.GetTransform()
		return style.Transform(func(value string) string {
			if transform != nil {
				value = transform(value)
			}
			return cursorProbeStart + value + cursorProbeEnd
		})
	}
	input.Cursor.Style = probeStyle(input.Cursor.Style)
	input.Cursor.TextStyle = probeStyle(input.Cursor.TextStyle)
	return input.View()
}

// clipCursorLines locates a cursor after the form viewport has selected its
// visible lines. An invisible or partially clipped cursor never gets an IME
// anchor. Probe metadata is removed before returning any rendered content.
func clipCursorLines(lines []string, width, height int) ([]string, cursorAnchor) {
	anchor := cursorAnchor{}
	count := 0
	for row := range lines {
		line := lines[row]
		start, end := strings.Index(line, cursorProbeStart), strings.Index(line, cursorProbeEnd)
		if start >= 0 && end >= start+len(cursorProbeStart) {
			count += strings.Count(line, cursorProbeStart)
			column := ansi.StringWidth(line[:start]) + 1
			cellWidth := max(1, ansi.StringWidth(line[start+len(cursorProbeStart):end]))
			if row < height && column > 0 && column+cellWidth-1 <= width {
				anchor = cursorAnchor{row: row + 1, column: column}
			}
		}
		line = strings.ReplaceAll(strings.ReplaceAll(line, cursorProbeStart, ""), cursorProbeEnd, "")
		lines[row] = clipped(line, width)
	}
	if count != 1 {
		anchor = cursorAnchor{}
	}
	return lines, anchor
}
