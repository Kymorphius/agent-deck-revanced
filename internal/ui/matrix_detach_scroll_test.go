package ui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: model a long inertia tail without sleeps. Each report
// arrives before the quiet interval ends, even after the initial deadline.
func TestDetachWheelInertiaWaitsForQuiet(t *testing.T) {
	h := &Home{}
	now := time.Unix(100, 0)
	h.beginDetachWheelGuard(now)
	for i := 1; i <= 20; i++ {
		now = now.Add(100 * time.Millisecond)
		if !h.suppressDetachWheel(tea.MouseMsg{Button: tea.MouseButtonWheelDown}, now) {
			t.Fatalf("inertia report %d leaked into dashboard", i)
		}
	}
	if h.suppressDetachWheel(tea.MouseMsg{Button: tea.MouseButtonWheelUp}, now.Add(detachWheelQuiet)) {
		t.Fatal("new scroll after quiet interval was suppressed")
	}
}

func TestDetachWheelPreservesFreshInteraction(t *testing.T) {
	now := time.Unix(100, 0)
	for _, msg := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyDown},
		tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress},
	} {
		h := &Home{}
		h.beginDetachWheelGuard(now)
		if h.suppressDetachWheel(msg, now) || !h.detachWheelUntil.IsZero() {
			t.Fatalf("fresh interaction was not released: %v", msg)
		}
	}
	h := &Home{}
	h.beginDetachWheelGuard(now)
	h.suppressDetachWheel(tea.MouseMsg{Action: tea.MouseActionMotion}, now)
	h.suppressDetachWheel(tea.WindowSizeMsg{Width: 80, Height: 24}, now)
	if !h.suppressDetachWheel(tea.MouseMsg{Button: tea.MouseButtonWheelDown}, now) {
		t.Fatal("passive motion or resize released old inertia")
	}
	h.insertMode = true
	if h.suppressDetachWheel(tea.MouseMsg{Button: tea.MouseButtonWheelDown}, now) {
		t.Fatal("wheel input in a new session was suppressed")
	}
}

func TestDetachWheelCtrlQDoesNotScrollOverview(t *testing.T) {
	h, _, _ := armHomeWithOneSession(t)
	h.insertMode, h.embeddedMode = true, true
	h.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if h.insertMode || h.detachWheelUntil.IsZero() {
		t.Fatal("Ctrl+Q did not exit session with wheel guard armed")
	}
	assertDetachWheelKeepsSelection(t, h)
}

func TestDetachWheelLegacyReturnDoesNotScrollOverview(t *testing.T) {
	h, _, _ := armHomeWithOneSession(t)
	h.Update(statusUpdateMsg{})
	if h.detachWheelUntil.IsZero() {
		t.Fatal("legacy attach return did not arm wheel guard")
	}
	assertDetachWheelKeepsSelection(t, h)
}

func assertDetachWheelKeepsSelection(t *testing.T, h *Home) {
	t.Helper()
	h.cursor = 0
	h.previewScrollOffset = 7
	for _, button := range []tea.MouseButton{tea.MouseButtonWheelDown, tea.MouseButtonWheelUp} {
		h.Update(tea.MouseMsg{Button: button})
		if h.cursor != 0 || h.previewScrollOffset != 7 {
			t.Fatal("post-detach wheel moved list selection or preview")
		}
	}
	// End the quiet interval and verify a new list scroll actually navigates.
	h.detachWheelUntil = time.Now().Add(-time.Second)
	h.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if h.cursor != 1 {
		t.Fatalf("fresh list scroll did not move selection: %d", h.cursor)
	}
}
