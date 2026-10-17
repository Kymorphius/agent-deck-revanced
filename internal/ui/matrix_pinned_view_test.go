package ui

// matrix/hardened: pinned-only is a persistent, collapse-independent view that
// composes with the existing filters and real keyboard dispatch.

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func matrixPinnedHome(t *testing.T) *Home {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	home, _ := buildTwoGroupHome(t)
	t.Cleanup(home.cancel)
	return home
}

func matrixPinnedInstance(t *testing.T, h *Home, title string) *session.Instance {
	t.Helper()
	for _, inst := range h.instances {
		if inst.Title == title {
			return inst
		}
	}
	t.Fatalf("fixture session %q missing", title)
	return nil
}

func matrixAssertPinnedTitles(t *testing.T, h *Home, want ...string) {
	t.Helper()
	got := make([]string, 0, len(h.flatItems))
	for _, item := range h.flatItems {
		if item.Type != session.ItemTypeSession || item.Session == nil || item.CreatingID != "" {
			t.Fatalf("pinned-only retained an unrelated row: %+v", item)
		}
		if item.Session.Pin == session.PinNone {
			t.Fatalf("pinned-only retained unpinned session %q", item.Session.Title)
		}
		got = append(got, item.Session.Title)
	}
	sort.Strings(got)
	expected := append([]string{}, want...)
	sort.Strings(expected)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("pinned titles = %v, want %v", got, expected)
	}
}

func TestMatrixPinnedViewCollapsedMembershipAndSuppression(t *testing.T) {
	home := matrixPinnedHome(t)
	top := matrixPinnedInstance(t, home, "a1")
	top.Pin, top.GroupPath = session.PinTop, "alpha/deep"
	child := matrixPinnedInstance(t, home, "a3")
	child.Pin = session.PinBottom
	child.ParentSessionID = matrixPinnedInstance(t, home, "a2").ID
	archived := matrixPinnedInstance(t, home, "b2")
	archived.Pin, archived.ArchivedAt = session.PinTop, time.Now().UTC()
	home.groupTree = session.NewGroupTree(home.instances)
	for _, path := range []string{"alpha", "beta"} {
		home.groupTree.Groups[path].Expanded = false
		home.groupTree.Expanded[path] = false
	}
	beforeGroups := make(map[string]bool)
	beforeExpanded := make(map[string]bool)
	for path, group := range home.groupTree.Groups {
		beforeGroups[path] = group.Expanded
	}
	for path, expanded := range home.groupTree.Expanded {
		beforeExpanded[path] = expanded
	}
	addRemoteFixture(home)
	home.creatingSessions["creating-pinned-view"] = &CreatingSession{
		ID: "creating-pinned-view", Title: "pending session", Tool: "claude", GroupPath: "alpha",
	}
	home.groupViewMode = session.GroupViewPinnedOnly
	home.rebuildFlatItems()

	matrixAssertPinnedTitles(t, home, "a1", "a3")
	for _, item := range home.flatItems {
		if item.Level != 0 || item.IsSubSession {
			t.Fatalf("pinned-only session is not standalone: %+v", item)
		}
	}
	for path, expanded := range beforeGroups {
		if home.groupTree.Groups[path].Expanded != expanded {
			t.Errorf("pinned-only changed group %q expanded state", path)
		}
	}
	if !reflect.DeepEqual(home.groupTree.Expanded, beforeExpanded) {
		t.Fatal("pinned-only changed saved group folds")
	}
	if child.ParentSessionID == "" {
		t.Fatal("standalone display mutated the session's persisted parent")
	}
}

func TestMatrixPinnedViewFiltersComposeAndStayEmpty(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status session.Status
		recent session.TimeFilterMode
		scope  string
		want   []string
	}{
		{"running recent alpha", session.StatusRunning, session.TimeFilter7Days, "alpha", []string{"a1"}},
		{"idle alpha", session.StatusIdle, session.TimeFilterAll, "alpha", []string{"a2"}},
		{"empty status", session.StatusStopped, session.TimeFilterAll, "", nil},
		{"empty recent beta", session.StatusRunning, session.TimeFilter7Days, "beta", nil},
		{"archived", FilterModeArchived, session.TimeFilter7Days, "beta", []string{"b2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := matrixPinnedHome(t)
			for _, inst := range home.instances {
				inst.Status, inst.CreatedAt, inst.LastAccessedAt = session.StatusRunning, now, now
				inst.Pin = session.PinTop
			}
			matrixPinnedInstance(t, home, "a2").Status = session.StatusIdle
			matrixPinnedInstance(t, home, "a3").Pin = session.PinNone
			old := matrixPinnedInstance(t, home, "b1")
			old.CreatedAt, old.LastAccessedAt = now.AddDate(0, 0, -30), now.AddDate(0, 0, -30)
			matrixPinnedInstance(t, home, "b2").ArchivedAt = now
			addRemoteFixture(home)
			home.groupViewMode = session.GroupViewPinnedOnly
			home.statusFilter, home.timeFilter, home.groupScope = tc.status, tc.recent, tc.scope
			home.keepEmptyFilter = false // Even restored filters must remain exact in pinned-only.
			home.rebuildFlatItemsAt(now)

			matrixAssertPinnedTitles(t, home, tc.want...)
			if home.statusFilter != tc.status || home.timeFilter != tc.recent || home.groupScope != tc.scope {
				t.Fatalf("pinned-only changed filters: status=%q time=%v scope=%q", home.statusFilter, home.timeFilter, home.groupScope)
			}
			if home.groupViewMode != session.GroupViewPinnedOnly {
				t.Fatal("an empty result exited pinned-only")
			}
		})
	}
}

func TestMatrixPinnedViewEmptyRecencyDoesNotFallback(t *testing.T) {
	home := matrixPinnedHome(t)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	old := matrixPinnedInstance(t, home, "a1")
	old.Pin = session.PinTop
	old.CreatedAt, old.LastAccessedAt = now.AddDate(0, 0, -30), now.AddDate(0, 0, -30)
	home.groupViewMode, home.timeFilter = session.GroupViewPinnedOnly, session.TimeFilter7Days
	home.keepEmptyFilter = false
	home.rebuildFlatItemsAt(now)
	matrixAssertPinnedTitles(t, home)
	if home.timeFilter != session.TimeFilter7Days {
		t.Fatal("empty pinned-only results cleared the recency filter")
	}
}

func TestMatrixPinnedViewHotkeyDispatchAndSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
	}{
		{"p", plainKeyMsg('p')},
		{"Russian p twin", plainKeyMsg('з')},
		{"ctrl+p", tea.KeyMsg{Type: tea.KeyCtrlP}},
		{"alt+p", altKeyMsg('p')},
		{"mac Option+P", plainKeyMsg('π')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := matrixPinnedHome(t)
			selected := matrixPinnedInstance(t, home, "b1")
			selected.Pin = session.PinBottom
			matrixPinnedInstance(t, home, "a1").Pin = session.PinTop
			home.rebuildFlatItems()
			home.cursor = sessionIndexByTitle(home, "b1")
			home.Update(tc.key)

			if home.groupViewMode != session.GroupViewPinnedOnly {
				t.Fatalf("%s did not enter pinned-only: mode=%v", tc.name, home.groupViewMode)
			}
			matrixAssertPinnedTitles(t, home, "a1", "b1")
			if home.flatItems[home.cursor].Session.ID != selected.ID {
				t.Fatal("toggle lost the selected pinned session")
			}
			home.Update(tc.key)
			if home.groupViewMode != session.GroupViewNormal {
				t.Fatalf("%s did not return to normal: mode=%v", tc.name, home.groupViewMode)
			}
			if home.flatItems[home.cursor].Session == nil || home.flatItems[home.cursor].Session.ID != selected.ID {
				t.Fatal("returning to normal lost the selected pinned session")
			}
		})
	}
}

func TestMatrixPinnedViewHotkeyFromOtherModes(t *testing.T) {
	for _, mode := range []session.GroupViewMode{session.GroupViewActiveTop, session.GroupViewPopulatedTop} {
		home := matrixPinnedHome(t)
		matrixPinnedInstance(t, home, "a1").Pin = session.PinTop
		home.groupViewMode = mode
		home.handleMainKey(tea.KeyMsg{Type: tea.KeyCtrlP})
		if home.groupViewMode != session.GroupViewPinnedOnly {
			t.Fatalf("toggle from %v produced %v, want pinned-only", mode, home.groupViewMode)
		}
		home.handleMainKey(tea.KeyMsg{Type: tea.KeyCtrlP})
		if home.groupViewMode != session.GroupViewNormal {
			t.Fatalf("toggle from pinned-only produced %v, want normal", home.groupViewMode)
		}
	}
}

func TestMatrixPinnedViewUnpinLastSessionStaysEmpty(t *testing.T) {
	home := matrixPinnedHome(t)
	last := matrixPinnedInstance(t, home, "a1")
	last.Pin = session.PinBottom
	home.groupViewMode = session.GroupViewPinnedOnly
	home.rebuildFlatItems()
	home.cursor = sessionIndexByTitle(home, "a1")
	home.Update(plainKeyMsg(','))
	if last.Pin != session.PinNone {
		t.Fatalf("unpin did not clear last pin: %q", last.Pin)
	}
	matrixAssertPinnedTitles(t, home)
	if home.groupViewMode != session.GroupViewPinnedOnly || home.cursor != 0 {
		t.Fatalf("unpin changed view or left invalid cursor: mode=%v cursor=%d", home.groupViewMode, home.cursor)
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyHome}, {Type: tea.KeyEnd}, {Type: tea.KeyDown}, {Type: tea.KeyUp}} {
		home.Update(key)
		if home.cursor != 0 || len(home.flatItems) != 0 || home.groupViewMode != session.GroupViewPinnedOnly {
			t.Fatalf("navigation changed the empty view: key=%q mode=%v cursor=%d rows=%d", key.String(), home.groupViewMode, home.cursor, len(home.flatItems))
		}
	}
	// Empty pinned-only still receives the toggle and returns to the full list.
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if home.groupViewMode != session.GroupViewNormal || sessionIndexByTitle(home, "a1") < 0 {
		t.Fatal("could not leave empty pinned-only through its hotkey")
	}
}

func TestMatrixPinnedViewPreferenceSurvivesRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	storage, err := session.NewStorageWithProfile("_matrix_pinned_view")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	before, _ := buildTwoGroupHome(t)
	t.Cleanup(before.cancel)
	before.storage, before.profile = storage, "_matrix_pinned_view"
	before.storageWatcher = nil
	matrixPinnedInstance(t, before, "a1").Pin = session.PinTop
	before.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	// The key action itself must save the mode; a later autosave is not required.
	raw, err := storage.GetDB().GetMeta("ui_state")
	if err != nil {
		t.Fatal(err)
	}
	var state uiState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.GroupViewMode != int(session.GroupViewPinnedOnly) {
		t.Fatalf("persisted group_view_mode=%v, want pinned-only", state.GroupViewMode)
	}
	after, _ := buildTwoGroupHome(t)
	t.Cleanup(after.cancel)
	after.storage, after.profile = storage, "_matrix_pinned_view"
	after.storageWatcher = nil
	matrixPinnedInstance(t, after, "a1").Pin = session.PinTop
	after.loadUIState()
	after.rebuildFlatItems()
	if after.groupViewMode != session.GroupViewPinnedOnly {
		t.Fatalf("restart restored view mode %v, want pinned-only", after.groupViewMode)
	}
	matrixAssertPinnedTitles(t, after, "a1")
}

// matrix/hardened: the aggregate view draws independent rows, including the
// narrow embedded rail's fallback renderer, and exposes its empty-state exit.
func TestMatrixPinnedViewRenderedRowsAndEmptyState(t *testing.T) {
	home := matrixPinnedHome(t)
	matrixPinnedInstance(t, home, "a1").Pin = session.PinTop
	matrixPinnedInstance(t, home, "b1").Pin = session.PinBottom
	home.groupViewMode = session.GroupViewPinnedOnly
	home.rebuildFlatItems()
	for _, embedded := range []bool{false, true} {
		home.embeddedLayout = embedded
		for _, width := range []int{26, 80, 120} {
			view := stripAnsi(home.renderSessionList(width, 24))
			if strings.Contains(view, "├─") || strings.Contains(view, "└─") {
				t.Fatalf("standalone pins drew an orphan connector (embedded=%v width=%d):\n%s", embedded, width, view)
			}
			for _, line := range strings.Split(view, "\n") {
				if cellWidth(line) > width {
					t.Fatalf("pinned row overflows %d columns: %q", width, line)
				}
			}
		}
	}
	state, filtered := home.filteredEmptyState()
	if !filtered || state.Title != "No pinned sessions" || !strings.Contains(strings.Join(state.Hints, " "), "p back to normal view") {
		t.Fatalf("empty pinned view lacks its identity and exit: %+v", state)
	}
}

// matrix/hardened: the direct view shortcut obeys the same explicit binding
// and disable semantics as the other list actions.
func TestMatrixPinnedViewHotkeyRebindAndDisable(t *testing.T) {
	home := matrixPinnedHome(t)
	home.setHotkeys(resolveHotkeys(map[string]string{hotkeyTogglePinnedView: "alt+p"}))
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if home.groupViewMode != session.GroupViewNormal {
		t.Fatal("default shortcut remained active after rebinding")
	}
	home.Update(altKeyMsg('p'))
	if home.groupViewMode != session.GroupViewPinnedOnly {
		t.Fatal("rebound shortcut did not open pinned view")
	}
	home.groupViewMode = session.GroupViewNormal
	home.setHotkeys(resolveHotkeys(map[string]string{hotkeyTogglePinnedView: ""}))
	for _, key := range []tea.KeyMsg{plainKeyMsg('p'), plainKeyMsg('з'), {Type: tea.KeyCtrlP}, altKeyMsg('p'), plainKeyMsg('π')} {
		home.Update(key)
		if home.groupViewMode != session.GroupViewNormal {
			t.Fatalf("disabled shortcut %q opened pinned view", key.String())
		}
	}
}

// matrix/hardened: p opens pinned view even for multi-repo sessions; their
// dedicated path editor remains reachable through its configurable Alt+L key.
func TestMatrixPinnedViewKeepsMultiRepoEditorAccessible(t *testing.T) {
	home := matrixPinnedHome(t)
	inst := matrixPinnedInstance(t, home, "a1")
	inst.MultiRepoEnabled, inst.Pin = true, session.PinTop
	inst.AdditionalPaths = []string{t.TempDir()}
	home.rebuildFlatItems()
	home.cursor = sessionIndexByTitle(home, "a1")
	home.Update(plainKeyMsg('p'))
	if home.groupViewMode != session.GroupViewPinnedOnly || home.editPathsDialog.IsVisible() {
		t.Fatal("p must toggle pinned view without opening the multi-repo editor")
	}
	home.Update(altKeyMsg('l'))
	if !home.editPathsDialog.IsVisible() || home.groupViewMode != session.GroupViewPinnedOnly {
		t.Fatal("Alt+L did not open the multi-repo editor in the pinned view")
	}
	home.editPathsDialog.Hide()
	home.setHotkeys(resolveHotkeys(map[string]string{hotkeyEditPaths: "ctrl+k"}))
	home.Update(altKeyMsg('l'))
	if home.editPathsDialog.IsVisible() {
		t.Fatal("Alt+L remained active after rebinding the path editor")
	}
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if !home.editPathsDialog.IsVisible() {
		t.Fatal("rebound key did not open the multi-repo editor")
	}
	home.editPathsDialog.Hide()
	home.setHotkeys(resolveHotkeys(map[string]string{hotkeyEditPaths: ""}))
	home.Update(altKeyMsg('l'))
	if home.editPathsDialog.IsVisible() {
		t.Fatal("disabled path editor still opened")
	}
}

// matrix/hardened: Ctrl+P selects a view, Ctrl+N has no navigation role, and
// disabling/rebinding the view must not revive the old movement aliases.
func TestMatrixPinnedControlKeysDoNotNavigate(t *testing.T) {
	for _, overrides := range []map[string]string{
		{hotkeyTogglePinnedView: ""},
		{hotkeyTogglePinnedView: "alt+p"},
	} {
		home := matrixPinnedHome(t)
		home.setHotkeys(resolveHotkeys(overrides))
		home.cursor = sessionIndexByTitle(home, "b1")
		before := home.cursor
		for _, key := range []tea.KeyType{tea.KeyCtrlN, tea.KeyCtrlP} {
			home.navCount.n = 3
			home.Update(tea.KeyMsg{Type: key})
			if home.cursor != before || home.groupViewMode != session.GroupViewNormal {
				t.Fatalf("%s moved cursor or changed view: cursor=%d mode=%v", (tea.KeyMsg{Type: key}).String(), home.cursor, home.groupViewMode)
			}
		}
	}
	home := matrixPinnedHome(t)
	home.cursor = sessionIndexByTitle(home, "b1")
	before := home.cursor
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	if home.cursor != before || home.groupViewMode != session.GroupViewNormal {
		t.Fatal("default Ctrl+N moved cursor or changed view")
	}
}

// matrix/hardened: supplementary aliases yield to explicit bindings on either
// side of the action order, and disappear when the primary action is rebound.
func TestMatrixPinnedViewAliasRemapAndConflict(t *testing.T) {
	home := matrixPinnedHome(t)
	home.setHotkeys(resolveHotkeys(map[string]string{hotkeyTogglePinnedView: "ctrl+g"}))
	for _, key := range []tea.KeyMsg{plainKeyMsg('p'), plainKeyMsg('з'), {Type: tea.KeyCtrlP}, altKeyMsg('p'), plainKeyMsg('π')} {
		home.Update(key)
		if home.groupViewMode != session.GroupViewNormal {
			t.Fatalf("default alias %q survived rebinding", key.String())
		}
	}
	home.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	if home.groupViewMode != session.GroupViewPinnedOnly {
		t.Fatal("rebound shortcut did not open pinned view")
	}
	for _, action := range []string{hotkeyNewSession, hotkeyCopyPane} {
		home := matrixPinnedHome(t)
		home.setHotkeys(resolveHotkeys(map[string]string{action: "alt+p"}))
		for _, raw := range []string{"alt+p", "π"} {
			if got := home.normalizeMainKey(raw); got != defaultHotkeyBindings[action] {
				t.Fatalf("%s did not retain explicit %s: got %q", action, raw, got)
			}
		}
		home.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
		if home.groupViewMode != session.GroupViewPinnedOnly {
			t.Fatal("explicit Alt+P conflict disabled Ctrl+P")
		}
	}
}

func TestMatrixPinnedViewHelpShowsOnlyActiveAlias(t *testing.T) {
	for _, tc := range []struct {
		overrides map[string]string
		want      string
	}{
		{nil, "p/ctrl+p/alt+p"},
		{map[string]string{hotkeyCopyPane: "alt+p"}, "p/ctrl+p"},
		{map[string]string{hotkeyCopyPane: "ctrl+p"}, "p/alt+p"},
		{map[string]string{hotkeyTogglePinnedView: "ctrl+g"}, "ctrl+g"},
		{map[string]string{hotkeyTogglePinnedView: ""}, ""},
	} {
		h := NewHelpOverlay()
		h.SetHotkeys(resolveHotkeys(tc.overrides))
		h.Show()
		h.SetSize(200, 200)
		found := false
		for _, line := range strings.Split(h.View(), "\n") {
			if strings.Contains(line, "Toggle pinned-only view") {
				found = true
				if tc.want == "" || !strings.Contains(line, tc.want) || (strings.Contains(line, "/alt+p") != strings.Contains(tc.want, "/alt+p")) || (strings.Contains(line, "/ctrl+p") != strings.Contains(tc.want, "/ctrl+p")) {
					t.Fatalf("unexpected pinned help row: %q", line)
				}
				break
			}
		}
		if found != (tc.want != "") {
			t.Fatalf("pinned help row visibility = %v, wanted binding %q", found, tc.want)
		}
	}
}

// matrix/hardened: parent navigation must not silently alter hidden group folds.
func TestMatrixPinnedViewLeftKeepsGroupExpansion(t *testing.T) {
	home := matrixPinnedHome(t)
	matrixPinnedInstance(t, home, "a1").Pin = session.PinTop
	home.groupViewMode = session.GroupViewPinnedOnly
	home.rebuildFlatItems()
	home.cursor = sessionIndexByTitle(home, "a1")
	expanded := home.groupTree.Groups["alpha"].Expanded
	home.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if home.groupTree.Groups["alpha"].Expanded != expanded {
		t.Fatal("left navigation changed a hidden group's fold state")
	}
	matrixAssertPinnedTitles(t, home, "a1")
}
