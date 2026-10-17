package ui

// matrix/hardened: navigate the Shift+number status views in a stable order,
// including empty results, without changing the group presentation or scope.

import (
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func (h *Home) matrixCycleStatusView(direction int) tea.Cmd {
	views := [...]session.Status{
		"", session.StatusRunning, session.StatusWaiting, session.StatusIdle,
		session.StatusError, FilterModeActive, FilterModeArchived,
	}
	current := h.statusFilter
	if current == FilterModeInterrupted {
		// matrix/hardened: interrupted is a drill-down within Abnormal.
		current = session.StatusError
	}
	index := -1
	for i, view := range views {
		if view == current {
			index = i
			break
		}
	}
	// matrix/hardened: an unlisted search filter re-enters at the nearest end.
	next := 0
	if index >= 0 {
		next = (index + direction + len(views)) % len(views)
	} else if direction < 0 {
		next = len(views) - 1
	}
	// matrix/hardened: % owns Open's hide-stopped substep; left/right visits
	// Open once and always starts with the configured exclusion set.
	h.activeFilterHideStopped = false
	cmd := h.changeStatusFilter(views[next])
	h.syncViewport()
	return cmd
}
