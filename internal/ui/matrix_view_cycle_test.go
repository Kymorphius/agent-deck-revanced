package ui

// matrix/hardened: exercise status-view navigation through the real main-list
// dispatcher so its hotkeys cannot steal text editing or alter other filters.

import (
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestMatrixViewCycleDispatchAndWrap(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyType
		want []session.Status
	}{
		{"next", tea.KeyCtrlL, []session.Status{session.StatusRunning, session.StatusWaiting, session.StatusIdle, session.StatusError, FilterModeActive, FilterModeArchived, ""}},
		{"previous", tea.KeyCtrlH, []session.Status{FilterModeArchived, FilterModeActive, session.StatusError, session.StatusIdle, session.StatusWaiting, session.StatusRunning, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := matrixPinnedHome(t)
			for round := 0; round < 2; round++ {
				for step, want := range tc.want {
					home.Update(tea.KeyMsg{Type: tc.key})
					if home.statusFilter != want || !home.keepEmptyFilter {
						t.Fatalf("round %d step %d: filter=%q keepEmpty=%v, want %q", round, step, home.statusFilter, home.keepEmptyFilter, want)
					}
					if home.activeFilterHideStopped {
						t.Fatal("view cycling entered Open's hide-stopped substep")
					}
				}
			}
		})
	}
}

func TestMatrixViewCycleDrilldownAndUnknownEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		from session.Status
		key  tea.KeyType
		want session.Status
	}{
		{"interrupted previous", FilterModeInterrupted, tea.KeyCtrlH, session.StatusIdle},
		{"interrupted next", FilterModeInterrupted, tea.KeyCtrlL, FilterModeActive},
		{"stopped previous", session.StatusStopped, tea.KeyCtrlH, FilterModeArchived},
		{"stopped next", session.StatusStopped, tea.KeyCtrlL, ""},
		{"unknown previous", session.Status("custom"), tea.KeyCtrlH, FilterModeArchived},
		{"unknown next", session.Status("custom"), tea.KeyCtrlL, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := matrixPinnedHome(t)
			home.statusFilter = tc.from
			home.Update(tea.KeyMsg{Type: tc.key})
			if home.statusFilter != tc.want {
				t.Fatalf("filter=%q, want %q", home.statusFilter, tc.want)
			}
		})
	}
}

func TestMatrixViewCycleOpenSkipsHideStoppedSubstep(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		for _, key := range []tea.KeyType{tea.KeyCtrlH, tea.KeyCtrlL} {
			home := matrixPinnedHome(t)
			home.activeFilterExcludes = map[session.Status]bool{session.StatusError: true}
			home.statusFilter, home.activeFilterHideStopped = FilterModeActive, hidden
			home.Update(tea.KeyMsg{Type: key})
			want := session.StatusError
			if key == tea.KeyCtrlL {
				want = FilterModeArchived
			}
			if home.statusFilter != want || home.activeFilterHideStopped {
				t.Fatalf("leave Open hidden=%v key=%q: filter=%q hidden=%v, want %q/false", hidden, (tea.KeyMsg{Type: key}).String(), home.statusFilter, home.activeFilterHideStopped, want)
			}
			// Returning to Open starts with its configured exclusion set.
			back := tea.KeyCtrlL
			if key == tea.KeyCtrlL {
				back = tea.KeyCtrlH
			}
			home.Update(tea.KeyMsg{Type: back})
			if home.statusFilter != FilterModeActive || home.activeFilterHideStopped || !home.matchesStatusFilter(FilterModeActive, session.StatusStopped) {
				t.Fatal("cycling back to Open retained the hide-stopped substep")
			}
		}
	}
}

func TestMatrixViewCyclePreservesPresentationAndScope(t *testing.T) {
	for _, mode := range []session.GroupViewMode{session.GroupViewNormal, session.GroupViewActiveTop, session.GroupViewPopulatedTop, session.GroupViewPinnedOnly, session.GroupViewRecent} {
		home := matrixPinnedHome(t)
		now := time.Now()
		for _, inst := range home.instances {
			inst.Status, inst.Pin = session.StatusRunning, session.PinTop
			inst.CreatedAt, inst.LastAccessedAt = now, now
		}
		home.groupViewMode, home.timeFilter, home.groupScope = mode, session.TimeFilter7Days, "alpha"
		home.rebuildFlatItems()
		for _, key := range []tea.KeyType{tea.KeyCtrlL, tea.KeyCtrlH} {
			home.Update(tea.KeyMsg{Type: key})
			if home.groupViewMode != mode || home.timeFilter != session.TimeFilter7Days || home.groupScope != "alpha" {
				t.Fatalf("key=%q changed mode/time/scope: %v/%v/%q", (tea.KeyMsg{Type: key}).String(), home.groupViewMode, home.timeFilter, home.groupScope)
			}
			for _, item := range home.flatItems {
				if item.Session != nil && item.Session.GroupPath != "alpha" {
					t.Fatalf("cycling leaked a session outside the group scope: %q", item.Session.Title)
				}
			}
		}
	}
}

func TestMatrixViewCycleKeepsEmptyResultsAndSelection(t *testing.T) {
	home := matrixPinnedHome(t)
	for _, inst := range home.instances {
		inst.Status = session.StatusRunning
	}
	home.rebuildFlatItems()
	selected := matrixPinnedInstance(t, home, "b1")
	home.cursor = sessionIndexByTitle(home, selected.Title)
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	if got := home.getSelectedSession(); got == nil || got.ID != selected.ID {
		t.Fatal("entering a matching status view lost the selected session")
	}
	for _, want := range []session.Status{session.StatusWaiting, session.StatusIdle, session.StatusError} {
		home.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
		if home.statusFilter != want || len(home.flatItems) != 0 || home.cursor != 0 {
			t.Fatalf("empty view did not stay exact: filter=%q rows=%d cursor=%d, want %q/0/0", home.statusFilter, len(home.flatItems), home.cursor, want)
		}
	}
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlH})
	if home.statusFilter != session.StatusIdle || len(home.flatItems) != 0 {
		t.Fatal("previous shortcut could not leave an empty result")
	}
}

func TestMatrixViewCycleRecentStartsAtNewestMatch(t *testing.T) {
	home := matrixPinnedHome(t)
	now := time.Now()
	for i, inst := range home.instances {
		inst.Status, inst.LastAccessedAt = session.StatusRunning, now.Add(-time.Duration(i)*time.Minute)
	}
	home.groupViewMode = session.GroupViewRecent
	home.rebuildFlatItems()
	home.cursor = sessionIndexByTitle(home, "b1")
	home.viewOffset = home.cursor
	home.pendingCursorRestore = &uiState{CursorSessionID: matrixPinnedInstance(t, home, "b1").ID}
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	if home.statusFilter != session.StatusRunning || home.groupViewMode != session.GroupViewRecent || home.cursor != 0 || home.viewOffset != 0 || home.pendingCursorRestore != nil {
		t.Fatalf("Recent did not start at newest match: filter=%q mode=%v cursor=%d offset=%d pending=%v", home.statusFilter, home.groupViewMode, home.cursor, home.viewOffset, home.pendingCursorRestore)
	}
	if got := home.getSelectedSession(); got == nil || got.Title != "a1" {
		t.Fatal("Recent did not select the newest matching visit")
	}
}

func TestMatrixViewCycleKeepsDirectShortcuts(t *testing.T) {
	for _, tc := range []struct {
		key  rune
		want session.Status
	}{{'0', ""}, {'!', session.StatusRunning}, {'@', session.StatusWaiting}, {'#', session.StatusIdle}, {'$', session.StatusError}, {'&', session.StatusError}, {'%', FilterModeActive}, {'^', FilterModeArchived}, {'~', FilterModeInterrupted}} {
		t.Run(string(tc.key), func(t *testing.T) {
			home := matrixPinnedHome(t)
			if tc.key == '0' {
				home.statusFilter = session.StatusRunning
			}
			home.Update(plainKeyMsg(tc.key))
			if home.statusFilter != tc.want {
				t.Fatalf("direct key %c: filter=%q, want %q", tc.key, home.statusFilter, tc.want)
			}
		})
	}
}

func TestMatrixViewCycleHotkeyRebindAndDisable(t *testing.T) {
	home := matrixPinnedHome(t)
	if home.actionKey(hotkeyPreviousView) != "ctrl+h" || home.actionKey(hotkeyNextView) != "ctrl+l" || home.actionKey(hotkeyEditPaths) != "alt+l" {
		t.Fatal("default view or path-editor shortcuts do not match the advertised controls")
	}
	home.setHotkeys(resolveHotkeys(map[string]string{hotkeyPreviousView: "f7", hotkeyNextView: "f8"}))
	for _, key := range []tea.KeyType{tea.KeyCtrlH, tea.KeyCtrlL} {
		home.Update(tea.KeyMsg{Type: key})
		if home.statusFilter != "" {
			t.Fatalf("released default %q still changed view", (tea.KeyMsg{Type: key}).String())
		}
	}
	home.Update(tea.KeyMsg{Type: tea.KeyF8})
	if home.statusFilter != session.StatusRunning {
		t.Fatal("rebound next-view shortcut did not work")
	}
	home.Update(tea.KeyMsg{Type: tea.KeyF7})
	if home.statusFilter != "" {
		t.Fatal("rebound previous-view shortcut did not work")
	}
	home.setHotkeys(resolveHotkeys(map[string]string{hotkeyPreviousView: "", hotkeyNextView: ""}))
	for _, key := range []tea.KeyType{tea.KeyCtrlH, tea.KeyCtrlL, tea.KeyF7, tea.KeyF8} {
		home.Update(tea.KeyMsg{Type: key})
		if home.statusFilter != "" {
			t.Fatalf("disabled shortcut %q still changed view", (tea.KeyMsg{Type: key}).String())
		}
	}
}

func TestMatrixViewCycleDoesNotStealTextEditing(t *testing.T) {
	for _, overlay := range []string{"search", "group dialog"} {
		t.Run(overlay, func(t *testing.T) {
			home := matrixPinnedHome(t)
			home.statusFilter = session.StatusRunning
			var value func() string
			if overlay == "search" {
				home.Update(plainKeyMsg('/'))
				home.search.input.SetValue("abc")
				home.search.input.CursorEnd()
				value = func() string { return home.search.input.Value() }
			} else {
				home.groupDialog.Show()
				home.groupDialog.nameInput.SetValue("abc")
				home.groupDialog.nameInput.CursorEnd()
				value = func() string { return home.groupDialog.nameInput.Value() }
			}
			for _, tc := range []struct {
				key  tea.KeyType
				want string
			}{{tea.KeyCtrlH, "ab"}, {tea.KeyBackspace, "a"}, {tea.KeyCtrlL, "a"}} {
				home.Update(tea.KeyMsg{Type: tc.key})
				if got := value(); got != tc.want || home.statusFilter != session.StatusRunning {
					t.Fatalf("%q: text=%q filter=%q, want %q/running", (tea.KeyMsg{Type: tc.key}).String(), got, home.statusFilter, tc.want)
				}
			}
		})
	}
	home := matrixPinnedHome(t)
	home.statusFilter = session.StatusRunning
	home.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if home.statusFilter != session.StatusRunning {
		t.Fatal("DEL backspace in the main list changed view")
	}
}

// matrix/hardened: preserve pre-existing user shortcuts when adding defaults,
// and do not advertise a navigation chord that now belongs to another action.
func TestMatrixViewCycleDefaultsYieldToExplicitBindings(t *testing.T) {
	for _, tc := range []struct {
		action     string
		key        string
		navigation string
		label      string
	}{
		{hotkeyEditPaths, "ctrl+l", hotkeyNextView, "Next status view"},
		{hotkeyHelp, "ctrl+h", hotkeyPreviousView, "Previous status view"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			home := matrixPinnedHome(t)
			home.setHotkeys(resolveHotkeys(map[string]string{tc.action: tc.key}))
			if home.actionKey(tc.navigation) != "" || home.normalizeMainKey(tc.key) != defaultHotkeyBindings[tc.action] {
				t.Fatal("navigation default took an explicit user shortcut")
			}
			help := NewHelpOverlay()
			help.SetHotkeys(home.hotkeys)
			help.SetSize(200, 300)
			help.Show()
			if strings.Contains(stripAnsi(help.View()), tc.label) {
				t.Fatal("help advertised navigation on a shortcut owned by another action")
			}
		})
	}
}
