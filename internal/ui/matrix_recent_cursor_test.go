package ui

// matrix/hardened: entering Recent is explicit navigation to the newest visit;
// ordinary refreshes and navigation among the other views retain identity.

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func matrixRecentCursorHome(t *testing.T) *Home {
	t.Helper()
	home := matrixPinnedHome(t)
	now := time.Now()
	for i, inst := range home.instances {
		inst.LastAccessedAt = now.Add(-time.Duration(i+1) * time.Minute)
		inst.Status, inst.Pin = session.StatusRunning, session.PinTop
	}
	home.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {
		{ID: "remote-old", Title: "old remote", Group: "work", Status: "running", Pin: matrixRemotePinPointer(session.PinTop), LastAccessedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)},
	}}
	home.rebuildFlatItems()
	return home
}

func matrixAssertRecentCursorAtNewest(t *testing.T, home *Home, title string) {
	t.Helper()
	if home.groupViewMode != session.GroupViewRecent || home.cursor != 0 || home.viewOffset != 0 {
		t.Fatalf("Recent did not start at top: mode=%v cursor=%d offset=%d", home.groupViewMode, home.cursor, home.viewOffset)
	}
	if len(home.flatItems) == 0 || home.flatItems[0].Session == nil || home.flatItems[0].Session.Title != title {
		t.Fatalf("Recent first row is %s, want session %q", describeCursor(home), title)
	}
	if home.pendingCursorRestore != nil {
		t.Fatal("entering Recent retained a deferred cursor from another view")
	}
}

func TestMatrixRecentViewEntryDiscardsOtherViewCursor(t *testing.T) {
	for _, mode := range []struct {
		name string
		mode session.GroupViewMode
	}{
		{"normal", session.GroupViewNormal},
		{"active top", session.GroupViewActiveTop},
		{"populated top", session.GroupViewPopulatedTop},
		{"pinned", session.GroupViewPinnedOnly},
	} {
		for _, selection := range []string{"local", "group", "remote"} {
			if mode.mode == session.GroupViewPinnedOnly && selection == "group" {
				continue
			}
			t.Run(mode.name+"/"+selection, func(t *testing.T) {
				home := matrixRecentCursorHome(t)
				home.groupViewMode = mode.mode
				home.rebuildFlatItems()
				switch selection {
				case "local":
					home.cursor = sessionIndexByTitle(home, "b1")
				case "remote":
					home.cursor = remoteSessionIndexByID(home, "remote-old")
				case "group":
					home.cursor = -1
					for i, item := range home.flatItems {
						if item.Type == session.ItemTypeGroup && item.Path == "beta" {
							home.cursor = i
							break
						}
					}
				}
				if home.cursor <= 0 {
					t.Fatalf("fixture did not place the old %s selection below first row: %d", selection, home.cursor)
				}
				home.viewOffset = home.cursor
				home.pendingCursorRestore = &uiState{CursorSessionID: matrixPinnedInstance(t, home, "b1").ID}
				home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
				matrixAssertRecentCursorAtNewest(t, home, "a1")
			})
		}
	}
}

func TestMatrixRecentViewCycleFromPinnedStartsNewest(t *testing.T) {
	for _, selection := range []string{"local", "remote"} {
		t.Run(selection, func(t *testing.T) {
			home := matrixRecentCursorHome(t)
			home.groupViewMode = session.GroupViewPinnedOnly
			home.rebuildFlatItems()
			if selection == "local" {
				home.cursor = sessionIndexByTitle(home, "b1")
			} else {
				home.cursor = remoteSessionIndexByID(home, "remote-old")
			}
			if home.cursor <= 0 {
				t.Fatalf("fixture lacks old %s selection: %d", selection, home.cursor)
			}
			home.viewOffset = home.cursor
			home.pendingCursorRestore = &uiState{CursorSessionID: matrixPinnedInstance(t, home, "b1").ID}
			home.Update(plainKeyMsg('t'))
			matrixAssertRecentCursorAtNewest(t, home, "a1")
		})
	}
}

func TestMatrixRecentViewReentryResetsNavigationAndScroll(t *testing.T) {
	for _, route := range []string{"toggle", "cycle"} {
		t.Run(route, func(t *testing.T) {
			home := matrixRecentCursorHome(t)
			home.height = 8
			home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
			for visit := 0; visit < 3; visit++ {
				matrixAssertRecentCursorAtNewest(t, home, "a1")
				home.Update(tea.KeyMsg{Type: tea.KeyEnd})
				if home.cursor == 0 || home.viewOffset == 0 {
					t.Fatalf("navigation did not scroll before reentry: cursor=%d offset=%d", home.cursor, home.viewOffset)
				}
				if route == "toggle" {
					home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
					home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
				} else {
					for step := 0; step < session.GroupViewModeCount; step++ {
						home.Update(plainKeyMsg('t'))
					}
				}
			}
			matrixAssertRecentCursorAtNewest(t, home, "a1")
		})
	}
}

func TestMatrixRecentViewFilterChangesStartNewestMatch(t *testing.T) {
	for _, key := range []rune{'!', '*'} {
		t.Run(string(key), func(t *testing.T) {
			home := matrixRecentCursorHome(t)
			home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
			home.height = 8
			for change := 0; change < 2; change++ {
				home.cursor = sessionIndexByTitle(home, "b1")
				home.syncViewport()
				if home.cursor <= 0 || home.viewOffset == 0 {
					t.Fatal("fixture did not scroll to an older session that matches the next filter")
				}
				home.Update(plainKeyMsg(key))
				matrixAssertRecentCursorAtNewest(t, home, "a1")
			}
		})
	}
}

func TestMatrixRecentViewChangesRetainNonRecentSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode session.GroupViewMode
		key  rune
	}{
		{"normal to active", session.GroupViewNormal, 't'},
		{"active to populated", session.GroupViewActiveTop, 't'},
		{"populated to pinned", session.GroupViewPopulatedTop, 't'},
		{"normal status filter", session.GroupViewNormal, '!'},
		{"pinned status filter", session.GroupViewPinnedOnly, '!'},
		{"normal time filter", session.GroupViewNormal, '*'},
		{"pinned time filter", session.GroupViewPinnedOnly, '*'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := matrixRecentCursorHome(t)
			home.groupViewMode = tc.mode
			home.rebuildFlatItems()
			home.cursor = sessionIndexByTitle(home, "b1")
			selected := home.captureSelectedItemIdentity()
			home.Update(plainKeyMsg(tc.key))
			if got := home.captureSelectedItemIdentity(); got != selected {
				t.Fatalf("non-Recent change lost selected session: got=%+v want=%+v", got, selected)
			}
		})
	}
}

func TestMatrixRecentViewBackgroundRebuildRetainsSelectionAfterResort(t *testing.T) {
	for _, selection := range []string{"local", "remote"} {
		t.Run(selection, func(t *testing.T) {
			home := matrixRecentCursorHome(t)
			home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
			if selection == "local" {
				home.cursor = sessionIndexByTitle(home, "b1")
			} else {
				home.cursor = remoteSessionIndexByID(home, "remote-old")
			}
			selected, oldIndex := home.captureSelectedItemIdentity(), home.cursor
			matrixPinnedInstance(t, home, "b2").LastAccessedAt = time.Now()
			if selection == "remote" {
				home.remoteSessions["dev"][0].LastAccessedAt = time.Now().Add(-90 * time.Second).Format(time.RFC3339Nano)
			}
			home.rebuildFlatItemsPreservingSelection(selected)
			if got := home.captureSelectedItemIdentity(); got != selected {
				t.Fatalf("background resort moved selected session: got=%+v want=%+v", got, selected)
			}
			if home.cursor == oldIndex {
				t.Fatal("fixture did not shift the selected session's row during resort")
			}
			if home.flatItems[0].Session == nil || home.flatItems[0].Session.Title != "b2" {
				t.Fatal("background rebuild did not apply the changed visit ordering")
			}
		})
	}
}

// matrix/hardened: a reload requested before entering Recent must not resurrect
// its old local cursor when the newest conversation belongs to another host.
func TestMatrixRecentViewLateReloadRetainsCurrentRemoteIdentity(t *testing.T) {
	home := matrixRecentCursorHome(t)
	home.runtimeSource = &matrixRuntimeUISource{}
	home.cursor = sessionIndexByTitle(home, "b1")
	stale := home.preserveState()
	home.remoteSessions["dev"][0].LastAccessedAt = time.Now().Add(time.Minute).Format(time.RFC3339Nano)
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	selected := home.captureSelectedItemIdentity()
	if home.cursor != 0 || selected.remoteSessionID != "remote-old" {
		t.Fatalf("fixture did not enter Recent on the newest remote: %+v", selected)
	}
	home.Update(loadSessionsMsg{instances: home.instances, restoreState: &stale})
	if got := home.captureSelectedItemIdentity(); got != selected || home.cursor != 0 || home.viewOffset != 0 {
		t.Fatalf("late reload restored the old view's cursor: selected=%+v cursor=%d offset=%d", got, home.cursor, home.viewOffset)
	}

	// A later background reload can move this remote's row without changing
	// which conversation the operator is reading.
	matrixPinnedInstance(t, home, "b2").LastAccessedAt = time.Now().Add(time.Hour)
	home.Update(loadSessionsMsg{instances: home.instances, restoreState: &stale})
	if got := home.captureSelectedItemIdentity(); got != selected || home.cursor == 0 {
		t.Fatalf("background reload followed an ordinal instead of the remote: selected=%+v cursor=%d", got, home.cursor)
	}
}
