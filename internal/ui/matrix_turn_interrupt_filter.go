package ui

import (
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/charmbracelet/lipgloss"
)

// matrix/hardened: a place to look for interrupted turns (see
// internal/session/matrix_turn_interrupt.go). The filter bar always carries a
// unified abnormal pill; "~" drills down to sessions whose last turn stopped
// before it finished.

// FilterKeyInterrupted toggles the interrupted-turn filter.
const FilterKeyInterrupted = "~"

// FilterModeInterrupted is the filter value for interrupted turns (not a real
// session status).
const FilterModeInterrupted session.Status = "matrix-interrupted"

// matrixShowsTurnInterrupted is the one rule for "this row is an interrupted
// turn", shared by the row glyph, the pill count and the filter: the substate
// says so and the light is waiting, idle or error. A running row is left alone because
// the verdict is a few seconds old and a new turn may already be under way.
func matrixShowsTurnInterrupted(status session.Status, substate session.Substate) bool {
	return substate == session.SubstateTurnInterrupted &&
		(status == session.StatusWaiting || status == session.StatusIdle || status == session.StatusError)
}

// matchesSessionFilter is matchesStatusFilter for a session, adding the
// interrupted and abnormal filters, which also need the diagnostic substate.
func (h *Home) matchesSessionFilter(filter session.Status, inst *session.Instance) bool {
	if inst.IsArchived() && (filter == FilterModeInterrupted || filter == session.StatusError) {
		return false
	}
	state, ok := h.getSessionRenderSnapshot()[inst.ID]
	if !ok {
		state.status, state.substate = inst.GetStatusThreadSafe(), inst.CachedSubstate()
	}
	if filter == FilterModeInterrupted {
		return matrixShowsTurnInterrupted(state.status, state.substate)
	}
	return h.matchesStatusFilter(filter, matrixDisplayStatus(state.status, state.substate, inst.IsArchived()))
}

// matrixCountTurnInterrupted counts interrupted turns from the render snapshot
// (lock-free, like countSessionStatuses).
func (h *Home) matrixCountTurnInterrupted() int {
	n := 0
	for _, state := range h.getSessionRenderSnapshot() {
		if !state.matrixArchived && !state.archivedSuperseded && matrixShowsTurnInterrupted(state.status, state.substate) {
			n++
		}
	}
	// Remote freshness gates match countSessionStatuses; cached/offline data
	// must not manufacture a current abnormal count.
	h.remoteSessionsMu.RLock()
	defer h.remoteSessionsMu.RUnlock()
	for name, rows := range h.remoteSessions {
		if state, known := h.remotePolls[name]; h.remoteFromCache[name] || (known && state.LastPollStatus != "ok") {
			continue
		}
		for _, row := range rows {
			if !row.Archived && matrixShowsTurnInterrupted(session.Status(row.Status), session.Substate(row.Substate)) {
				n++
			}
		}
	}
	return n
}

// matrixInterruptedPill renders the "↯ N" filter-bar pill: highlighted while
// the filter is on, red when there is something to see, dim at 0.
func (h *Home) matrixInterruptedPill(dim lipgloss.Style) string {
	label := fmt.Sprintf("↯ %d", h.matrixCountTurnInterrupted())
	switch {
	case h.statusFilter == FilterModeInterrupted:
		return lipgloss.NewStyle().Foreground(ColorBg).Background(ColorRed).Bold(true).Padding(0, 1).Render(label)
	case label != "↯ 0":
		return lipgloss.NewStyle().Foreground(ColorRed).Background(ColorSurface).Padding(0, 1).Render(label)
	default:
		return dim.Render(label)
	}
}

// matrixInterruptedEmptyState is the empty list under the interrupted filter.
func matrixInterruptedEmptyState() EmptyStateConfig {
	return EmptyStateConfig{
		Icon:                "↯",
		Title:               "No interrupted turns",
		Subtitle:            "A turn cut short by an API error or an abort shows here until you continue it",
		ShowSubtitleMinimal: true,
		ShowAllHintsMinimal: true,
		Hints:               []string{FilterKeyInterrupted + " or 0 show all"},
	}
}

// matrix/hardened: one presentation bucket for faults and interrupted turns.
// Canonical process status and diagnostic substate remain unchanged. Current
// running work wins over old diagnostics; explicit stop/archive keeps its meaning.
func matrixDisplayStatus(status session.Status, sub session.Substate, archived bool) session.Status {
	if archived {
		return session.StatusStopped
	}
	if status == session.StatusRunning || status == session.StatusStopped {
		return status
	}
	if status == session.StatusError || matrixShowsTurnInterrupted(status, sub) {
		return session.StatusError
	}
	if status == session.StatusWaiting || status == session.StatusIdle {
		switch sub {
		case session.SubstateAuth401, session.SubstateModelUnavailable, session.SubstateUsageLimit:
			return session.StatusError
		}
	}
	return statusBucket(status)
}

// matrix/hardened: remote rows use the same classifier as local rows.
func (h *Home) matrixMatchesRemoteFilter(filter session.Status, row session.RemoteSessionInfo) bool {
	if filter == "" || filter == FilterModeArchived {
		return true
	}
	if row.Archived {
		return false
	}
	if filter == FilterModeInterrupted {
		return matrixShowsTurnInterrupted(session.Status(row.Status), session.Substate(row.Substate))
	}
	return h.matchesStatusFilter(filter, matrixDisplayStatus(session.Status(row.Status), session.Substate(row.Substate), false))
}
