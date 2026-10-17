package ui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: exercise real filters and counts across conflicting verdicts.
func TestMatrixAbnormalFilterAndCounts(t *testing.T) {
	rows := []*session.Instance{}
	snap := map[string]sessionRenderState{}
	add := func(id string, status session.Status, sub session.Substate, archived bool) {
		inst := &session.Instance{ID: id, Title: id, GroupPath: "g", Status: status}
		if archived {
			inst.ArchivedAt = time.Now()
		}
		rows = append(rows, inst)
		snap[id] = sessionRenderState{status: status, substate: sub, matrixArchived: archived}
	}
	add("error", session.StatusError, session.SubstateNone, false)
	add("cut", session.StatusWaiting, session.SubstateTurnInterrupted, false)
	add("both", session.StatusError, session.SubstateTurnInterrupted, false)
	add("quota", session.StatusWaiting, session.SubstateUsageLimit, false)
	add("auth", session.StatusIdle, session.SubstateAuth401, false)
	add("model", session.StatusIdle, session.SubstateModelUnavailable, false)
	add("busy", session.StatusRunning, session.SubstateTurnInterrupted, false)
	add("done", session.StatusWaiting, session.SubstateNone, false)
	add("stop", session.StatusStopped, session.SubstateTurnInterrupted, false)
	add("archive", session.StatusError, session.SubstateTurnInterrupted, true)
	h := matrixInterruptHome(t, snap, rows...)
	r, w, i, s, e := h.countSessionStatuses()
	if r != 1 || w != 1 || i != 0 || s != 2 || e != 6 {
		t.Fatalf("counts=%d,%d,%d,%d,%d", r, w, i, s, e)
	}
	if h.matrixCountTurnInterrupted() != 2 {
		t.Fatal("intersection counted twice or stale/archived interruption leaked")
	}
	if bar := stripAnsi(h.renderFilterBar()); !strings.Contains(bar, "✕ Abnormal 6") {
		t.Fatal(bar)
	}
	_, _ = h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'&'}})
	if got := matrixSessionTitles(h); !reflect.DeepEqual(got, []string{"error", "cut", "both", "quota", "auth", "model"}) {
		t.Fatalf("abnormal rows=%v", got)
	}
	_, _ = h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'~'}})
	if got := matrixSessionTitles(h); !reflect.DeepEqual(got, []string{"cut", "both"}) {
		t.Fatalf("interrupted rows=%v", got)
	}
	// A coarse error transition must not eject the interrupted row from either view.
	snap["cut"] = sessionRenderState{status: session.StatusError, substate: session.SubstateTurnInterrupted}
	h.sessionRenderSnapshot.Store(snap)
	if !h.matchesSessionFilter(session.StatusError, rows[1]) || !h.matchesSessionFilter(FilterModeInterrupted, rows[1]) {
		t.Fatal("error transition lost interruption")
	}
	// Once real work starts, stale interruption evidence stops surfacing.
	snap["cut"] = sessionRenderState{status: session.StatusRunning, substate: session.SubstateTurnInterrupted}
	h.sessionRenderSnapshot.Store(snap)
	if h.matchesSessionFilter(session.StatusError, rows[1]) || h.matchesSessionFilter(FilterModeInterrupted, rows[1]) {
		t.Fatal("running row retained old interruption")
	}
	h.activeFilterExcludes = map[session.Status]bool{session.StatusError: true}
	if h.matchesSessionFilter(FilterModeActive, rows[2]) || h.matchesSessionFilter(FilterModeActive, rows[3]) {
		t.Fatal("open filter did not exclude abnormal rows")
	}
}

func TestMatrixAbnormalRemoteAndEmptyEntry(t *testing.T) {
	h := matrixInterruptHome(t, map[string]sessionRenderState{})
	if bar := stripAnsi(h.renderFilterBar()); !strings.Contains(bar, "✕ Abnormal 0") {
		t.Fatal(bar)
	}
	h.changeStatusFilter(session.StatusError)
	cfg, ok := h.filteredEmptyState()
	if !ok || cfg.Title != "No abnormal sessions" {
		t.Fatalf("empty=%+v", cfg)
	}
	h.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {
		{ID: "cut", Title: "cut", Group: "g", Status: "error", Substate: "turn-interrupted"},
		{ID: "quota", Title: "quota", Group: "g", Status: "waiting", Substate: "usage-limit"},
		{ID: "busy", Title: "busy", Group: "g", Status: "running", Substate: "turn-interrupted"},
		{ID: "old", Title: "old", Group: "g", Status: "error", Substate: "turn-interrupted", Archived: true},
	}}
	h.cachedStatusCounts.valid.Store(false)
	_, _, _, _, n := h.countSessionStatuses()
	if n != 2 || h.matrixCountTurnInterrupted() != 1 {
		t.Fatalf("remote abnormal=%d interrupted=%d", n, h.matrixCountTurnInterrupted())
	}
	h.rebuildFlatItems()
	var ids []string
	for _, item := range h.flatItems {
		if item.RemoteSession != nil {
			ids = append(ids, item.RemoteSession.ID)
		}
	}
	if !reflect.DeepEqual(ids, []string{"cut", "quota"}) {
		t.Fatalf("remote filter=%v", ids)
	}
	h.remoteFromCache = map[string]bool{"dev": true}
	h.cachedStatusCounts.valid.Store(false)
	_, _, _, _, n = h.countSessionStatuses()
	if n != 0 || h.matrixCountTurnInterrupted() != 0 {
		t.Fatal("cached remote data inflated live count")
	}
}

func TestMatrixAbnormalDiagnosticGlyphs(t *testing.T) {
	for _, tc := range []struct {
		sub  session.Substate
		want string
	}{
		{session.SubstateAuth401, "🔒"}, {session.SubstateModelUnavailable, "⚡"},
		{session.SubstateUsageLimit, "✕"}, {session.SubstateTurnInterrupted, "↯"},
	} {
		icon, _ := rowStatusGlyph(session.StatusWaiting, tc.sub, false)
		if icon != tc.want {
			t.Fatalf("%s glyph=%s", tc.sub, icon)
		}
	}
}
