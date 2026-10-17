package ui

// matrix/hardened: drive the group-only locator through Home so selection never
// inherits session attach behavior or escapes an explicitly scoped dashboard.

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func matrixGroupSearchHome(t *testing.T) *Home {
	t.Helper()
	h := newMatrixLocalSearchMatchingHome(t, 120, []*session.Instance{
		{ID: "grouped", Title: "unrelated issue", Tool: "codex", ProjectPath: "/work/unrelated", GroupPath: "tools/agent-deck", Status: session.StatusIdle},
		{ID: "metadata", Title: "session-only-secret", Tool: "shell", ProjectPath: "/work/project-only-secret", GroupPath: "other", Status: session.StatusRunning},
	})
	h.groupTree.CreateGroupPath("tools/empty")
	h.groupTree.Groups["tools/agent-deck"].Name = "研发面板"
	for _, group := range h.groupTree.GroupList {
		group.Expanded = false
		h.groupTree.Expanded[group.Path] = false
	}
	h.setHotkeys(resolveHotkeys(nil))
	h.rebuildFlatItems()
	return h
}

func matrixGroupSearchType(t *testing.T, h *Home, query string) {
	t.Helper()
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(query)})
}

func matrixGroupSearchPick(t *testing.T, h *Home, path, remote string) {
	t.Helper()
	for i, item := range h.groupSearch.results {
		if item.Path == path && item.Remote == remote {
			h.groupSearch.cursor = i
			return
		}
	}
	t.Fatalf("group %q on remote %q missing from candidates: %+v", path, remote, h.groupSearch.results)
}

func matrixGroupSearchSelectedRow(t *testing.T, h *Home, path, remote string) {
	t.Helper()
	if h.cursor < 0 || h.cursor >= len(h.flatItems) {
		t.Fatalf("group locator left invalid cursor %d for %d rows", h.cursor, len(h.flatItems))
	}
	item := h.flatItems[h.cursor]
	wantType := session.ItemTypeGroup
	if remote != "" {
		wantType = session.ItemTypeRemoteGroup
	}
	if item.Type != wantType || item.Path != path || item.RemoteName != remote {
		t.Fatalf("selected row = %+v, want group %q on remote %q", item, path, remote)
	}
	if h.isAttaching.Load() || h.embeddedMode {
		t.Fatal("locating a group entered an AI session")
	}
}

func TestMatrixGroupSearchHome_MainKeysAndInputFocus(t *testing.T) {
	if got := resolveHotkeys(nil)[hotkeyGroupSearch]; got != "alt+s" {
		t.Fatalf("group locator default = %q, want alt+s", got)
	}
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'s'}, Alt: true},
		{Type: tea.KeyRunes, Runes: []rune{'ß'}},
	} {
		t.Run(key.String(), func(t *testing.T) {
			h := matrixGroupSearchHome(t)
			focus := &fakeInputMethod{}
			h.inputMethod = focus
			h.Update(tea.FocusMsg{})
			updateMatrixLocalSearchMatchingHome(t, h, key)
			if h.groupSearch == nil || !h.groupSearch.IsVisible() || !h.hasModalVisible() {
				t.Fatalf("%q failed to open a modal group locator", key.String())
			}
			if h.search.IsVisible() || h.globalSearch.IsVisible() || h.inputMethodNavigation() {
				t.Fatal("group locator did not acquire text input exclusively")
			}
			if len(focus.modes) < 2 || focus.modes[len(focus.modes)-1] {
				t.Fatal("opening the locator did not restore the text input method")
			}
			matrixGroupSearchType(t, h, "研发")
			if h.groupSearch.input.Value() != "研发" {
				t.Fatalf("typed Chinese query = %q", h.groupSearch.input.Value())
			}
			selected, ok := h.groupSearch.Selected()
			if !ok || selected.Path != "tools/agent-deck" {
				t.Fatalf("display-name query selected %+v, ok=%v", selected, ok)
			}
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEsc})
			if h.groupSearch.IsVisible() || h.hasModalVisible() || !h.inputMethodNavigation() || !focus.modes[len(focus.modes)-1] {
				t.Fatal("Esc did not return focus to list navigation")
			}
		})
	}
}

func TestMatrixGroupSearchHome_HotkeyReboundAndDisabled(t *testing.T) {
	h := matrixGroupSearchHome(t)
	h.setHotkeys(resolveHotkeys(map[string]string{hotkeyGroupSearch: "ctrl+o"}))
	for _, key := range []tea.KeyMsg{altKeyMsg('s'), plainKeyMsg('ß')} {
		updateMatrixLocalSearchMatchingHome(t, h, key)
		if h.groupSearch != nil && h.groupSearch.IsVisible() {
			t.Fatalf("rebound group locator still responds to %q", key.String())
		}
	}
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyCtrlO})
	if h.groupSearch == nil || !h.groupSearch.IsVisible() {
		t.Fatal("configured ctrl+o did not open the group locator")
	}
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	h.setHotkeys(resolveHotkeys(map[string]string{hotkeyGroupSearch: ""}))
	for _, key := range []tea.KeyMsg{altKeyMsg('s'), plainKeyMsg('ß')} {
		updateMatrixLocalSearchMatchingHome(t, h, key)
		if h.groupSearch.IsVisible() {
			t.Fatalf("disabled group locator still responds to %q", key.String())
		}
	}
}

func TestMatrixGroupSearchHome_MouseWheelStaysInsideModal(t *testing.T) {
	h := matrixGroupSearchHome(t)
	h.cursor, h.viewOffset, h.previewScrollOffset = 1, 1, 4
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
	if len(h.groupSearch.results) < 2 {
		t.Fatal("wheel fixture needs at least two groups")
	}
	for _, x := range []int{1, 110} {
		h.groupSearch.cursor = 0
		updateMatrixLocalSearchMatchingHome(t, h, tea.MouseMsg{X: x, Y: 10, Button: tea.MouseButtonWheelDown})
		if h.groupSearch.cursor != 1 {
			t.Fatalf("wheel down at x=%d did not move the group selection", x)
		}
		updateMatrixLocalSearchMatchingHome(t, h, tea.MouseMsg{X: x, Y: 10, Button: tea.MouseButtonWheelUp})
		if h.groupSearch.cursor != 0 {
			t.Fatalf("wheel up at x=%d did not restore the group selection", x)
		}
		if h.cursor != 1 || h.viewOffset != 1 || h.previewScrollOffset != 4 || !h.groupSearch.IsVisible() {
			t.Fatalf("modal wheel event at x=%d changed the underlying list or preview", x)
		}
	}
}

func TestMatrixGroupSearchHome_CursorBlinkReachesInput(t *testing.T) {
	h := matrixGroupSearchHome(t)
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
	matrixGroupSearchType(t, h, "研发")
	h.groupSearch.input.Cursor.BlinkSpeed = time.Millisecond
	before := h.groupSearch.input.Cursor.Blink
	msg := h.groupSearch.input.Cursor.BlinkCmd()()
	if _, ok := msg.(cursor.BlinkMsg); !ok {
		t.Fatalf("focused input generated %T instead of a blink", msg)
	}
	selection, _ := h.groupSearch.Selected()
	listCursor, listOffset := h.cursor, h.viewOffset
	_, cmd := h.Update(msg)
	if h.groupSearch.input.Cursor.Blink == before || cmd == nil {
		t.Fatal("Home did not forward the input cursor's blink or continue its timer")
	}
	after, _ := h.groupSearch.Selected()
	if h.groupSearch.input.Value() != "研发" || after != selection || h.cursor != listCursor || h.viewOffset != listOffset {
		t.Fatal("cursor blink changed the query, group selection, or underlying list")
	}
}

func TestMatrixGroupSearchHome_HelpFollowsBinding(t *testing.T) {
	for _, test := range []struct{ name, binding string }{
		{"default", "alt+s"},
		{"rebound", "ctrl+o"},
		{"disabled", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := matrixGroupSearchHome(t)
			h.setHotkeys(resolveHotkeys(map[string]string{hotkeyGroupSearch: test.binding}))
			updateMatrixLocalSearchMatchingHome(t, h, plainKeyMsg('?'))
			if !h.helpOverlay.IsVisible() {
				t.Fatal("? did not open Home's configured help overlay")
			}
			h.helpOverlay.SetSize(200, 300)
			frame := ansi.Strip(h.helpOverlay.View())
			found := false
			for _, line := range strings.Split(frame, "\n") {
				if strings.Contains(line, "Find group by name / path") {
					found = true
					if test.binding == "" || !strings.Contains(line, test.binding) {
						t.Fatalf("group locator help does not match binding %q: %s", test.binding, line)
					}
				}
			}
			if found != (test.binding != "") {
				t.Fatalf("group locator help visibility=%v for binding %q", found, test.binding)
			}
		})
	}
}

func TestMatrixGroupSearchHome_OnlyGroupFieldsAndEmptyGroups(t *testing.T) {
	for _, query := range []string{"session-only-secret", "project-only-secret"} {
		t.Run(query, func(t *testing.T) {
			h := matrixGroupSearchHome(t)
			updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
			matrixGroupSearchType(t, h, query)
			if len(h.groupSearch.results) != 0 {
				t.Fatalf("session metadata leaked into group-only results: %+v", h.groupSearch.results)
			}
			before := append([]session.Item(nil), h.flatItems...)
			cursor := h.cursor
			_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil || !h.groupSearch.IsVisible() || h.cursor != cursor || !slices.Equal(before, h.flatItems) {
				t.Fatal("Enter without a group match must leave the locator open and list unchanged")
			}
		})
	}
	for _, query := range []string{"tools", "empty", "研发面板", "agent deck", "adk"} {
		t.Run(query, func(t *testing.T) {
			h := matrixGroupSearchHome(t)
			updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
			matrixGroupSearchType(t, h, query)
			path := "tools/agent-deck"
			if query == "empty" {
				path = "tools/empty"
			} else if query == "tools" {
				path = "tools"
			}
			matrixGroupSearchPick(t, h, path, "")
			_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil || h.groupSearch.IsVisible() || len(h.instances) != 2 {
				t.Fatal("group selection must only close the locator and locate a row")
			}
			matrixGroupSearchSelectedRow(t, h, path, "")
			for _, parent := range []string{"tools", path} {
				if !h.groupTree.Groups[parent].Expanded {
					t.Fatalf("group selection did not expand %q", parent)
				}
			}
		})
	}
}

func TestMatrixGroupSearchHome_CancelPreservesFiltersAndScope(t *testing.T) {
	h := matrixGroupSearchHome(t)
	h.statusFilter, h.timeFilter = FilterModeArchived, session.TimeFilter7Days
	h.groupViewMode, h.groupScope = session.GroupViewPinnedOnly, "tools"
	h.embeddedLayout, h.sidebarMode = true, sidebarFlat
	h.keepEmptyFilter = true
	h.rebuildFlatItems()
	before := append([]session.Item(nil), h.flatItems...)
	cursor := h.cursor
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
	matrixGroupSearchType(t, h, "empty")
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	if h.statusFilter != FilterModeArchived || h.timeFilter != session.TimeFilter7Days || h.groupViewMode != session.GroupViewPinnedOnly || h.groupScope != "tools" || h.sidebarMode != sidebarFlat || !h.keepEmptyFilter || h.cursor != cursor || !slices.Equal(before, h.flatItems) {
		t.Fatal("canceling group search changed the list presentation or filters")
	}
}

func TestMatrixGroupSearchHome_RevealsGroupAcrossViews(t *testing.T) {
	for _, mode := range []string{"status", "time", "pinned", "archived", "embedded", "combined"} {
		t.Run(mode, func(t *testing.T) {
			h := matrixGroupSearchHome(t)
			h.groupScope = "tools"
			switch mode {
			case "status":
				h.statusFilter = session.StatusRunning
			case "time":
				h.timeFilter = session.TimeFilterToday
			case "pinned":
				h.groupViewMode = session.GroupViewPinnedOnly
			case "archived":
				h.statusFilter = FilterModeArchived
			case "embedded":
				h.embeddedLayout, h.sidebarMode = true, sidebarFlat
			case "combined":
				h.statusFilter, h.timeFilter = FilterModeArchived, session.TimeFilterToday
				h.groupViewMode = session.GroupViewPinnedOnly
				h.embeddedLayout, h.sidebarMode = true, sidebarFlat
			}
			h.rebuildFlatItems()
			updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
			matrixGroupSearchType(t, h, "empty")
			matrixGroupSearchPick(t, h, "tools/empty", "")
			_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil || h.groupSearch.IsVisible() {
				t.Fatal("Enter did not finish locating the group")
			}
			matrixGroupSearchSelectedRow(t, h, "tools/empty", "")
			if h.statusFilter != "" || h.timeFilter != session.TimeFilterAll || h.groupViewMode != session.GroupViewNormal || h.embeddedLayout && h.sidebarMode != sidebarGrouped {
				t.Fatal("successful group location did not restore an unfiltered grouped presentation")
			}
			if h.groupScope != "tools" {
				t.Fatalf("group selection changed explicit scope to %q", h.groupScope)
			}
			for _, item := range h.flatItems {
				if !pathInScope(item.Path, h.groupScope) {
					t.Fatalf("group selection revealed out-of-scope row %+v", item)
				}
			}
		})
	}
}

func TestMatrixGroupSearchHome_ScopeCannotBeEscaped(t *testing.T) {
	h := matrixGroupSearchHome(t)
	h.groupScope = "tools"
	h.remoteSessions = map[string][]session.RemoteSessionInfo{"box": {{ID: "remote", Group: "tools", Title: "remote session"}}}
	h.remoteGroups = map[string][]string{"box": {"tools"}}
	h.rebuildFlatItems()
	for _, item := range h.groupSearchItems() {
		if item.Remote != "" || !pathInScope(item.Path, "tools") {
			t.Fatalf("group corpus escaped explicit local scope: %+v", item)
		}
	}
	before := append([]session.Item(nil), h.flatItems...)
	cursor := h.cursor
	for _, item := range []GroupSearchItem{
		{Name: "other", Path: "other"},
		{Name: "tools", Path: "remotes/box/tools", Remote: "box"},
	} {
		if h.locateSearchedGroup(item) {
			t.Fatalf("stale/untrusted out-of-scope candidate was located: %+v", item)
		}
	}
	if h.groupScope != "tools" || h.cursor != cursor || !slices.Equal(before, h.flatItems) {
		t.Fatal("rejecting an out-of-scope result changed the list")
	}
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
	matrixGroupSearchType(t, h, "empty")
	matrixGroupSearchPick(t, h, "tools/empty", "")
	h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	matrixGroupSearchSelectedRow(t, h, "tools/empty", "")
	for _, item := range h.flatItems {
		if !pathInScope(item.Path, "tools") {
			t.Fatalf("locating a scoped group revealed an unrelated remote row: %+v", item)
		}
	}
}

func TestMatrixGroupSearchHome_RemoteIdentityAndCollapsedAncestors(t *testing.T) {
	h := matrixGroupSearchHome(t)
	h.remoteSessions = map[string][]session.RemoteSessionInfo{
		"east":       {{ID: "east-session", Group: "team/agent-deck", Title: "east issue", Status: "idle"}},
		"west":       {{ID: "west-session", Group: "team/agent-deck", Title: "west issue", Status: "idle"}},
		"empty-host": {},
	}
	h.remoteGroups = map[string][]string{"east": {"team/agent-deck", "team/empty"}, "west": {"team/agent-deck"}}
	for _, path := range []string{"remotes/east", "remotes/east/team", "remotes/east/team/agent-deck", "remotes/west"} {
		h.remoteGroupsCollapsed[path] = true
	}
	h.rebuildFlatItems()
	items := h.groupSearchItems()
	for _, want := range []GroupSearchItem{
		{Path: "remotes/east", Remote: "east"},
		{Path: "remotes/east/team", Remote: "east"},
		{Path: "remotes/east/team/empty", Remote: "east"},
		{Path: "remotes/empty-host", Remote: "empty-host"},
	} {
		if !slices.ContainsFunc(items, func(item GroupSearchItem) bool { return item.Path == want.Path && item.Remote == want.Remote }) {
			t.Fatalf("remote corpus omitted root/implicit ancestor/empty group %+v", want)
		}
	}
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
	matrixGroupSearchType(t, h, "agent deck")
	for _, remote := range []string{"east", "west"} {
		if !slices.ContainsFunc(h.groupSearch.results, func(item GroupSearchItem) bool {
			return item.Path == "remotes/"+remote+"/team/agent-deck" && item.Remote == remote
		}) {
			t.Fatalf("same-named group on %q disappeared from results", remote)
		}
	}
	matrixGroupSearchPick(t, h, "remotes/east/team/agent-deck", "east")
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || h.groupSearch.IsVisible() {
		t.Fatal("remote group location launched a command or left search open")
	}
	matrixGroupSearchSelectedRow(t, h, "remotes/east/team/agent-deck", "east")
	for _, path := range []string{"remotes/east", "remotes/east/team", "remotes/east/team/agent-deck"} {
		if h.remoteGroupsCollapsed[path] {
			t.Fatalf("remote ancestor %q remained folded", path)
		}
	}
	if !h.remoteGroupsCollapsed["remotes/west"] {
		t.Fatal("locating east expanded the other host")
	}
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
	matrixGroupSearchType(t, h, "empty-host")
	matrixGroupSearchPick(t, h, "remotes/empty-host", "empty-host")
	_, cmd = h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("locating an empty remote host started a remote operation")
	}
	matrixGroupSearchSelectedRow(t, h, "remotes/empty-host", "empty-host")
	// Explicit remote scopes also retain host identity while excluding the
	// other host and local tree from both the corpus and the revealed list.
	h.groupScope = "remotes/east/team"
	for _, item := range h.groupSearchItems() {
		if item.Remote != "east" || !pathInScope(item.Path, h.groupScope) {
			t.Fatalf("remote-scoped corpus leaked a group: %+v", item)
		}
	}
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
	matrixGroupSearchType(t, h, "empty")
	matrixGroupSearchPick(t, h, "remotes/east/team/empty", "east")
	_, cmd = h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || h.groupScope != "remotes/east/team" {
		t.Fatal("scoped remote location started an operation or changed explicit scope")
	}
	matrixGroupSearchSelectedRow(t, h, "remotes/east/team/empty", "east")
	for _, item := range h.flatItems {
		if !pathInScope(item.Path, h.groupScope) {
			t.Fatalf("remote-scoped location revealed an unrelated row: %+v", item)
		}
	}
}

func TestMatrixGroupSearchHome_StaleCandidatesNeverJumpToAnotherGroup(t *testing.T) {
	for _, change := range []string{"removed", "renamed"} {
		t.Run(change, func(t *testing.T) {
			h := matrixGroupSearchHome(t)
			item := GroupSearchItem{Name: "empty", Path: "tools/empty"}
			if change == "removed" {
				h.groupTree.DeleteGroup(item.Path)
			} else if err := h.groupTree.RenameGroup(item.Path, "renamed"); err != nil {
				t.Fatal(err)
			}
			h.statusFilter, h.timeFilter = FilterModeArchived, session.TimeFilter7Days
			h.groupViewMode, h.keepEmptyFilter = session.GroupViewPinnedOnly, true
			h.embeddedLayout, h.sidebarMode = true, sidebarFlat
			h.rebuildFlatItems()
			before, cursor := append([]session.Item(nil), h.flatItems...), h.cursor
			if h.locateSearchedGroup(item) || h.cursor != cursor || !slices.Equal(before, h.flatItems) {
				t.Fatal("stale group identity changed the list or located a different group")
			}
			if h.statusFilter != FilterModeArchived || h.timeFilter != session.TimeFilter7Days || h.groupViewMode != session.GroupViewPinnedOnly || !h.keepEmptyFilter || h.sidebarMode != sidebarFlat {
				t.Fatal("rejecting a stale candidate changed view filters")
			}
		})
	}
	h := matrixGroupSearchHome(t)
	h.remoteSessions = map[string][]session.RemoteSessionInfo{"west": {}}
	h.remoteGroups = map[string][]string{"west": {"team/agent-deck"}}
	h.rebuildFlatItems()
	for _, item := range []GroupSearchItem{
		{Name: "agent-deck", Path: "remotes/east/team/agent-deck", Remote: "east"},
		{Name: "agent-deck", Path: "remotes/west/team/agent-deck", Remote: "east"},
	} {
		if h.locateSearchedGroup(item) {
			t.Fatalf("missing or mismatched remote identity was accepted: %+v", item)
		}
	}
	h = matrixGroupSearchHome(t)
	h.statusFilter, h.timeFilter = FilterModeArchived, session.TimeFilter7Days
	h.groupViewMode = session.GroupViewPinnedOnly
	h.rebuildFlatItems()
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('s'))
	matrixGroupSearchType(t, h, "empty")
	matrixGroupSearchPick(t, h, "tools/empty", "")
	h.groupTree.DeleteGroup("tools/empty")
	before, cursor := append([]session.Item(nil), h.flatItems...), h.cursor
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || h.isAttaching.Load() || h.cursor != cursor || !slices.Equal(before, h.flatItems) || h.statusFilter != FilterModeArchived || h.timeFilter != session.TimeFilter7Days || h.groupViewMode != session.GroupViewPinnedOnly {
		t.Fatal("Enter on a removed group changed the view or launched a session")
	}
}
