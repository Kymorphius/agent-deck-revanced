package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: the abnormal pill and the "~" subset filter.

func matrixInterruptHome(t *testing.T, snap map[string]sessionRenderState, rows ...*session.Instance) *Home {
	t.Helper()
	h := NewHome()
	h.groupTree = session.NewGroupTree(rows)
	h.instances = rows
	h.sessionRenderSnapshot.Store(snap)
	h.width, h.height = 120, 40
	h.rebuildFlatItems()
	return h
}

func matrixSessionTitles(h *Home) []string {
	var out []string
	for _, it := range h.flatItems {
		if it.Type == session.ItemTypeSession && it.Session != nil {
			out = append(out, it.Session.Title)
		}
	}
	return out
}

func TestMatrixInterruptedPillAndFilter(t *testing.T) {
	rows := []*session.Instance{
		{ID: "cut", Title: "cut-row", GroupPath: "g", Status: session.StatusWaiting},
		{ID: "acked", Title: "acked-row", GroupPath: "g", Status: session.StatusIdle},
		{ID: "busy", Title: "busy-row", GroupPath: "g", Status: session.StatusRunning},
		{ID: "done", Title: "done-row", GroupPath: "g", Status: session.StatusWaiting},
		{ID: "old", Title: "archived-row", GroupPath: "g", Status: session.StatusIdle, ArchivedAt: time.Now()},
	}
	snap := map[string]sessionRenderState{
		"cut":   {status: session.StatusWaiting, substate: session.SubstateTurnInterrupted},
		"acked": {status: session.StatusIdle, substate: session.SubstateTurnInterrupted},
		"busy":  {status: session.StatusRunning, substate: session.SubstateTurnInterrupted},
		"done":  {status: session.StatusWaiting, substate: session.SubstateIdleAtEmptyPrompt},
		"old":   {status: session.StatusIdle, substate: session.SubstateTurnInterrupted, matrixArchived: true},
	}
	h := matrixInterruptHome(t, snap, rows...)

	if got := h.matrixCountTurnInterrupted(); got != 2 {
		t.Fatalf("count = %d, want 2 (waiting + idle; not running, not archived)", got)
	}
	if bar := stripAnsi(h.renderFilterBar()); !strings.Contains(bar, "✕ Abnormal 2") {
		t.Errorf("filter bar has no abnormal 2 pill: %q", bar)
	}

	press := func() { _, _ = h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'~'}}) }
	press()
	if h.statusFilter != FilterModeInterrupted {
		t.Fatalf("~ set filter %q", h.statusFilter)
	}
	if got := strings.Join(matrixSessionTitles(h), ","); got != "cut-row,acked-row" {
		t.Errorf("filtered rows = %q, want cut-row,acked-row", got)
	}
	if bar := stripAnsi(h.renderFilterBar()); !strings.Contains(bar, "Interrupted") {
		t.Errorf("filter bar does not name the filter: %q", bar)
	}
	press()
	if h.statusFilter != "" {
		t.Errorf("second ~ left filter %q", h.statusFilter)
	}
}

// Nothing interrupted: the abnormal pill still shows zero; the subset shows an
// empty list that says what would appear there.
func TestMatrixInterruptedFilter_Empty(t *testing.T) {
	rows := []*session.Instance{{ID: "a", Title: "a-row", GroupPath: "g", Status: session.StatusWaiting}}
	h := matrixInterruptHome(t, map[string]sessionRenderState{"a": {status: session.StatusWaiting}}, rows...)
	if bar := stripAnsi(h.renderFilterBar()); !strings.Contains(bar, "✕ Abnormal 0") {
		t.Errorf("filter bar has no abnormal 0 pill: %q", bar)
	}
	_, _ = h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'~'}})
	if h.statusFilter != FilterModeInterrupted || len(matrixSessionTitles(h)) != 0 {
		t.Fatalf("filter=%q rows=%v", h.statusFilter, matrixSessionTitles(h))
	}
	cfg, ok := h.filteredEmptyState()
	if !ok || cfg.Title != "No interrupted turns" {
		t.Errorf("empty state = %+v, %v", cfg, ok)
	}
}
