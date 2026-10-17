package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: terminals do not mark the end of a trackpad gesture. A
// quiet interval separates its delayed inertia reports from a new list scroll.
const detachWheelQuiet = 250 * time.Millisecond

func (h *Home) beginDetachWheelGuard(now time.Time) {
	h.detachWheelUntil = now.Add(detachWheelQuiet)
}

func (h *Home) suppressDetachWheel(msg tea.Msg, now time.Time) bool {
	if h.detachWheelUntil.IsZero() {
		return false
	}
	if h.insertMode || !now.Before(h.detachWheelUntil) {
		h.detachWheelUntil = time.Time{}
		return false
	}
	switch m := msg.(type) {
	case tea.MouseMsg:
		switch m.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown,
			tea.MouseButtonWheelLeft, tea.MouseButtonWheelRight:
			h.detachWheelUntil = now.Add(detachWheelQuiet)
			return true
		case tea.MouseButtonLeft, tea.MouseButtonMiddle, tea.MouseButtonRight:
			// A deliberate click starts fresh interaction; passive motion does not.
			h.detachWheelUntil = time.Time{}
		}
	case tea.KeyMsg:
		// Keep shortcuts immediately responsive, with no sleep or input drain.
		h.detachWheelUntil = time.Time{}
	}
	return false
}
