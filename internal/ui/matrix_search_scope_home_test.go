package ui

// matrix/hardened: session search has two explicit scopes. Global selection
// must reveal its owner, while a view search keeps the operator's filters.

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type matrixSearchScopeView struct {
	status session.Status
	time   session.TimeFilterMode
	mode   session.GroupViewMode
	group  string
}

func matrixSearchScopeState(h *Home) matrixSearchScopeView {
	return matrixSearchScopeView{h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope}
}

func matrixSearchScopeHome(t *testing.T) (*Home, map[string]*session.Instance) {
	t.Helper()
	now := time.Now()
	items := []*session.Instance{
		{ID: "target", Title: "scope hidden target", GroupPath: "Team/Alpha", Status: session.StatusIdle},
		{ID: "pinned", Title: "scope pinned parent", GroupPath: "Team/Alpha", Status: session.StatusRunning, Pin: session.PinTop},
		{ID: "child", Title: "scope child", GroupPath: "Team/Alpha", Status: session.StatusRunning, Pin: session.PinBottom, ParentSessionID: "pinned"},
		{ID: "nested", Title: "scope nested", GroupPath: "Team/Alpha/deep", Status: session.StatusRunning, Pin: session.PinTop},
		{ID: "outside", Title: "scope outside", GroupPath: "Other", Status: session.StatusRunning, Pin: session.PinTop},
		{ID: "old", Title: "scope old", GroupPath: "Team/Alpha", Status: session.StatusRunning, Pin: session.PinTop},
		{ID: "idle", Title: "scope idle", GroupPath: "Team/Alpha", Status: session.StatusIdle, Pin: session.PinTop},
		{ID: "never", Title: "scope unseen", GroupPath: "Team/Alpha", Status: session.StatusRunning, Pin: session.PinTop},
		{ID: "archived", Title: "scope archived", GroupPath: "Team/Alpha", Status: session.StatusRunning, Pin: session.PinTop, ArchivedAt: now},
	}
	byID := make(map[string]*session.Instance, len(items))
	for _, inst := range items {
		inst.Tool, inst.ProjectPath = "codex", "/work/"+inst.ID
		inst.CreatedAt, inst.LastAccessedAt = now.Add(-time.Hour), now.Add(-time.Hour)
		byID[inst.ID] = inst
	}
	for _, id := range []string{"target", "old"} {
		byID[id].CreatedAt, byID[id].LastAccessedAt = now.AddDate(0, 0, -60), now.AddDate(0, 0, -60)
	}
	byID["target"].LastAccessedAt, byID["never"].LastAccessedAt = time.Time{}, time.Time{}
	h := newMatrixLocalSearchMatchingHome(t, 120, items)
	h.setHotkeys(resolveHotkeys(nil))
	h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = "", session.TimeFilterAll, session.GroupViewNormal, ""
	h.keepEmptyFilter = true
	h.rebuildFlatItems()
	return h, byID
}

func matrixSearchScopeOpen(t *testing.T, h *Home, key tea.KeyMsg, query string) {
	t.Helper()
	updateMatrixLocalSearchMatchingHome(t, h, key)
	if !h.search.IsVisible() || h.globalSearch.IsVisible() {
		t.Fatalf("%q did not open the session metadata search", key.String())
	}
	if query != "" {
		updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(query)})
	}
}

func matrixSearchScopeIDs(t *testing.T, h *Home, want ...string) {
	t.Helper()
	got := make([]string, 0, len(h.search.results))
	for _, inst := range h.search.results {
		got = append(got, inst.ID)
	}
	expected := append([]string{}, want...)
	slices.Sort(got)
	slices.Sort(expected)
	if !slices.Equal(got, expected) {
		t.Fatalf("query %q candidates = %v, want %v", h.search.input.Value(), got, expected)
	}
}

func matrixSearchScopeSelected(t *testing.T, h *Home, target *session.Instance) {
	t.Helper()
	if selected := h.getSelectedSession(); selected == nil || selected.ID != target.ID {
		t.Fatalf("search did not locate %q; selected=%+v, cursor=%d, rows=%+v", target.ID, selected, h.cursor, h.flatItems)
	}
	if h.search.IsVisible() || h.isAttaching.Load() || h.embeddedMode {
		t.Fatal("search selection must close the overlay and remain in list navigation")
	}
}

func TestMatrixSearchScopeHome_GlobalLocatesAcrossViews(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state matrixSearchScopeView
	}{
		{"pinned", matrixSearchScopeView{mode: session.GroupViewPinnedOnly}},
		{"recent", matrixSearchScopeView{mode: session.GroupViewRecent}},
		{"status", matrixSearchScopeView{status: session.StatusRunning}},
		{"time", matrixSearchScopeView{time: session.TimeFilter7Days}},
		{"group", matrixSearchScopeView{group: "Other"}},
		{"combined", matrixSearchScopeView{session.StatusRunning, session.TimeFilter7Days, session.GroupViewRecent, "Other"}},
		{"archived partition", matrixSearchScopeView{status: FilterModeArchived}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, items := matrixSearchScopeHome(t)
			h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = tc.state.status, tc.state.time, tc.state.mode, tc.state.group
			h.rebuildFlatItems()
			if h.flatItemIndexByID("target") >= 0 {
				t.Fatal("fixture failed to hide global search target")
			}
			before := matrixSearchScopeState(h)
			matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "hidden target")
			matrixSearchScopeIDs(t, h, "target")
			if matrixSearchScopeState(h) != before {
				t.Fatal("opening global search changed the view before selection")
			}
			_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil {
				t.Fatal("locating a managed session scheduled an attach or import")
			}
			matrixSearchScopeSelected(t, h, items["target"])
			if got := matrixSearchScopeState(h); got != (matrixSearchScopeView{}) {
				t.Fatalf("global selection retained a hiding filter: %+v", got)
			}
		})
	}
}

func TestMatrixSearchScopeHome_GlobalArchivePartitionIsPresentationOnly(t *testing.T) {
	h, items := matrixSearchScopeHome(t)
	archivedAt := items["archived"].ArchivedAt
	h.groupViewMode, h.statusFilter, h.timeFilter, h.groupScope = session.GroupViewRecent, session.StatusIdle, session.TimeFilter7Days, "Other"
	h.rebuildFlatItems()
	matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "scope archived")
	matrixSearchScopeIDs(t, h, "archived")
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	matrixSearchScopeSelected(t, h, items["archived"])
	if got := matrixSearchScopeState(h); got != (matrixSearchScopeView{status: FilterModeArchived}) {
		t.Fatalf("archived locator state = %+v", got)
	}
	if !items["archived"].ArchivedAt.Equal(archivedAt) {
		t.Fatal("locating an archived result changed its archive data")
	}
	matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "hidden target")
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	matrixSearchScopeSelected(t, h, items["target"])
	if h.statusFilter != "" || !items["archived"].ArchivedAt.Equal(archivedAt) || items["target"].IsArchived() {
		t.Fatal("returning to an active result changed archive data or retained the archived partition")
	}
}

func TestMatrixSearchScopeHome_GlobalRevealsCollapsedChildAndAncestors(t *testing.T) {
	for _, id := range []string{"child", "nested"} {
		t.Run(id, func(t *testing.T) {
			h, items := matrixSearchScopeHome(t)
			for path, group := range h.groupTree.Groups {
				group.Expanded, h.groupTree.Expanded[path] = false, false
			}
			h.rebuildFlatItems()
			matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "scope "+id)
			matrixSearchScopeIDs(t, h, id)
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEnter})
			matrixSearchScopeSelected(t, h, items[id])
			for _, path := range []string{"Team", "Team/Alpha", items[id].GroupPath} {
				if !h.groupTree.Groups[path].Expanded {
					t.Fatalf("target ancestor %q remained folded", path)
				}
			}
			if id == "child" && items[id].ParentSessionID != "pinned" {
				t.Fatal("revealing a child changed its parent relationship")
			}
		})
	}
}

func TestMatrixSearchScopeHome_GlobalRemovedTargetDoesNotClearView(t *testing.T) {
	h, _ := matrixSearchScopeHome(t)
	h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = session.StatusRunning, session.TimeFilter7Days, session.GroupViewPinnedOnly, "Other"
	h.rebuildFlatItems()
	matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "hidden target")
	matrixSearchScopeIDs(t, h, "target")
	h.instancesMu.Lock()
	h.instances = slices.DeleteFunc(h.instances, func(inst *session.Instance) bool { return inst.ID == "target" })
	delete(h.instanceByID, "target")
	h.instancesMu.Unlock()
	before, cursor := matrixSearchScopeState(h), h.cursor
	rows := append([]session.Item{}, h.flatItems...)
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || h.err == nil || matrixSearchScopeState(h) != before || cursor != h.cursor || !reflect.DeepEqual(rows, h.flatItems) {
		t.Fatalf("stale global result changed the view or failed to report an error: error=%v state=%+v", h.err, matrixSearchScopeState(h))
	}
}

func TestMatrixSearchScopeHome_GlobalUsesLatestRegistryIdentity(t *testing.T) {
	h, items := matrixSearchScopeHome(t)
	matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "hidden target")
	matrixSearchScopeIDs(t, h, "target")
	latest := &session.Instance{ID: "target", Title: "renamed owner", GroupPath: "Moved/Latest", Tool: "codex", Status: session.StatusIdle}
	h.instancesMu.Lock()
	for i, inst := range h.instances {
		if inst.ID == "target" {
			h.instances[i] = latest
		}
	}
	h.instanceByID["target"] = latest
	h.instancesMu.Unlock()
	h.groupTree = session.NewGroupTree(h.instances)
	for path, group := range h.groupTree.Groups {
		group.Expanded, h.groupTree.Expanded[path] = false, false
	}
	if h.search.Selected() != items["target"] {
		t.Fatal("fixture must retain the old search result across the registry refresh")
	}
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	matrixSearchScopeSelected(t, h, latest)
	if h.getSelectedSession() != latest || !h.groupTree.Groups["Moved"].Expanded || !h.groupTree.Groups["Moved/Latest"].Expanded {
		t.Fatal("selection followed stale session metadata instead of the current registry owner")
	}
}

func TestMatrixSearchScopeHome_CancelKeepsViewAndFolds(t *testing.T) {
	for _, key := range []tea.KeyMsg{plainKeyMsg('/'), altKeyMsg('?')} {
		t.Run(key.String(), func(t *testing.T) {
			h, _ := matrixSearchScopeHome(t)
			h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = session.StatusRunning, session.TimeFilter7Days, session.GroupViewRecent, "Team"
			for path, group := range h.groupTree.Groups {
				group.Expanded, h.groupTree.Expanded[path] = false, false
			}
			h.rebuildFlatItems()
			before, cursor, offset := matrixSearchScopeState(h), h.cursor, h.viewOffset
			rows := append([]session.Item{}, h.flatItems...)
			matrixSearchScopeOpen(t, h, key, "scope")
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEsc})
			if h.search.IsVisible() || matrixSearchScopeState(h) != before || cursor != h.cursor || offset != h.viewOffset || !reflect.DeepEqual(rows, h.flatItems) {
				t.Fatal("cancel changed the underlying filtered list")
			}
			for path, group := range h.groupTree.Groups {
				if group.Expanded || h.groupTree.Expanded[path] {
					t.Fatalf("cancel expanded group %q", path)
				}
			}
		})
	}
}

func TestMatrixSearchScopeHome_ViewCandidatesComposeAndIgnoreFolds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state matrixSearchScopeView
		want  []string
	}{
		{"normal folded", matrixSearchScopeView{}, []string{"target", "pinned", "child", "nested", "outside", "old", "idle", "never"}},
		{"status time group", matrixSearchScopeView{session.StatusRunning, session.TimeFilter7Days, session.GroupViewNormal, "Team/Alpha"}, []string{"pinned", "child", "nested", "never"}},
		{"pinned time group", matrixSearchScopeView{session.StatusRunning, session.TimeFilter7Days, session.GroupViewPinnedOnly, "Team/Alpha"}, []string{"pinned", "child", "nested", "never"}},
		{"recent time group", matrixSearchScopeView{session.StatusRunning, session.TimeFilter7Days, session.GroupViewRecent, "Team/Alpha"}, []string{"pinned", "child", "nested"}},
		{"archived recent", matrixSearchScopeView{FilterModeArchived, session.TimeFilter7Days, session.GroupViewRecent, "Team/Alpha"}, []string{"archived"}},
		{"empty scope", matrixSearchScopeView{session.StatusStopped, session.TimeFilter7Days, session.GroupViewPinnedOnly, "Team/Alpha"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := matrixSearchScopeHome(t)
			h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = tc.state.status, tc.state.time, tc.state.mode, tc.state.group
			for path, group := range h.groupTree.Groups {
				group.Expanded, h.groupTree.Expanded[path] = false, false
			}
			h.rebuildFlatItems()
			before := matrixSearchScopeState(h)
			matrixSearchScopeOpen(t, h, altKeyMsg('?'), "scope")
			matrixSearchScopeIDs(t, h, tc.want...)
			if matrixSearchScopeState(h) != before {
				t.Fatal("current-view search relaxed the selected filters")
			}
		})
	}
}

func TestMatrixSearchScopeHome_EmptyViewEnterDoesNotChangeFilters(t *testing.T) {
	h, _ := matrixSearchScopeHome(t)
	h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = session.StatusStopped, session.TimeFilter7Days, session.GroupViewRecent, "Team/Alpha"
	h.rebuildFlatItems()
	before, cursor := matrixSearchScopeState(h), h.cursor
	rows := append([]session.Item{}, h.flatItems...)
	matrixSearchScopeOpen(t, h, altKeyMsg('?'), "scope")
	matrixSearchScopeIDs(t, h)
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || matrixSearchScopeState(h) != before || h.cursor != cursor || !reflect.DeepEqual(rows, h.flatItems) {
		t.Fatal("Enter in an empty current-view search changed the underlying filters or rows")
	}
}

func TestMatrixSearchScopeHome_ViewEnterPreservesFilters(t *testing.T) {
	for _, mode := range []session.GroupViewMode{session.GroupViewNormal, session.GroupViewPinnedOnly, session.GroupViewRecent} {
		t.Run(mode.Label(), func(t *testing.T) {
			h, items := matrixSearchScopeHome(t)
			h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = session.StatusRunning, session.TimeFilter7Days, mode, "Team/Alpha"
			for path, group := range h.groupTree.Groups {
				group.Expanded, h.groupTree.Expanded[path] = false, false
			}
			h.rebuildFlatItems()
			before := matrixSearchScopeState(h)
			matrixSearchScopeOpen(t, h, altKeyMsg('?'), "scope nested")
			matrixSearchScopeIDs(t, h, "nested")
			_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
			matrixSearchScopeSelected(t, h, items["nested"])
			if cmd != nil || matrixSearchScopeState(h) != before {
				t.Fatalf("current-view selection changed its scope: before=%+v after=%+v", before, matrixSearchScopeState(h))
			}
		})
	}
}

func TestMatrixSearchScopeHome_ViewRefreshKeepsScopeAndQuery(t *testing.T) {
	h, items := matrixSearchScopeHome(t)
	h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = session.StatusRunning, session.TimeFilter7Days, session.GroupViewRecent, "Team/Alpha"
	h.rebuildFlatItems()
	matrixSearchScopeOpen(t, h, altKeyMsg('?'), "scope nested")
	matrixSearchScopeIDs(t, h, "nested")
	items["nested"].Status = session.StatusIdle
	now := time.Now()
	newInside := &session.Instance{ID: "new-inside", Title: "scope nested new", GroupPath: "Team/Alpha", Status: session.StatusRunning, CreatedAt: now, LastAccessedAt: now}
	newOutside := &session.Instance{ID: "new-outside", Title: "scope nested new", GroupPath: "Other", Status: session.StatusRunning, CreatedAt: now, LastAccessedAt: now}
	h.search.SetItems(append(append([]*session.Instance{}, h.instances...), newInside, newOutside))
	if h.search.input.Value() != "scope nested" || !h.search.IsVisible() {
		t.Fatal("background refresh lost current-view query or modal state")
	}
	matrixSearchScopeIDs(t, h, "new-inside")
}

func TestMatrixSearchScopeHome_ViewExpiredTargetDoesNotClearFilters(t *testing.T) {
	h, items := matrixSearchScopeHome(t)
	h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = session.StatusRunning, session.TimeFilter7Days, session.GroupViewRecent, "Team/Alpha"
	h.rebuildFlatItems()
	matrixSearchScopeOpen(t, h, altKeyMsg('?'), "scope nested")
	matrixSearchScopeIDs(t, h, "nested")
	items["nested"].LastAccessedAt = time.Now().AddDate(0, 0, -60)
	before, cursor := matrixSearchScopeState(h), h.cursor
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || h.err == nil || matrixSearchScopeState(h) != before || h.cursor != cursor {
		t.Fatalf("expired view result changed filters or failed to report an error: error=%v state=%+v", h.err, matrixSearchScopeState(h))
	}
}

func TestMatrixSearchScopeHome_InGroupThenSlashRestoresAllCandidates(t *testing.T) {
	h, _ := matrixSearchScopeHome(t)
	for i, item := range h.flatItems {
		if item.Type == session.ItemTypeGroup && item.Path == "Team/Alpha" {
			h.cursor = i
			break
		}
	}
	matrixSearchScopeOpen(t, h, altKeyMsg('/'), "scope")
	matrixSearchScopeIDs(t, h, "target", "pinned", "child", "old", "idle", "never", "archived")
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	matrixSearchScopeOpen(t, h, plainKeyMsg('/'), "scope")
	matrixSearchScopeIDs(t, h, "target", "pinned", "child", "nested", "outside", "old", "idle", "never", "archived")
}

func TestMatrixSearchScopeHome_InGroupSelectionClearsHidingView(t *testing.T) {
	h, items := matrixSearchScopeHome(t)
	h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = session.StatusRunning, session.TimeFilter7Days, session.GroupViewPinnedOnly, "Team/Alpha"
	h.rebuildFlatItems()
	if index := h.flatItemIndexByID("pinned"); index >= 0 {
		h.cursor = index
	} else {
		t.Fatal("fixture must select a pinned session in the desired group")
	}
	matrixSearchScopeOpen(t, h, altKeyMsg('/'), "hidden target")
	matrixSearchScopeIDs(t, h, "target")
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	matrixSearchScopeSelected(t, h, items["target"])
	if matrixSearchScopeState(h) != (matrixSearchScopeView{}) {
		t.Fatal("exact-group selection did not clear filters hiding its target")
	}
}

func TestMatrixSearchScopeHome_DefaultGroupNormalizesUnsetPaths(t *testing.T) {
	for _, key := range []tea.KeyMsg{altKeyMsg('?'), altKeyMsg('/')} {
		t.Run(key.String(), func(t *testing.T) {
			unset := &session.Instance{ID: "unset", Title: "default scope result", Status: session.StatusIdle}
			h := newMatrixLocalSearchMatchingHome(t, 120, []*session.Instance{
				unset,
				{ID: "outside", Title: "other scope result", GroupPath: "Other", Status: session.StatusIdle},
			})
			h.setHotkeys(resolveHotkeys(nil))
			h.groupScope = session.DefaultGroupPath
			h.rebuildFlatItems()
			matrixSearchScopeOpen(t, h, key, "scope result")
			matrixSearchScopeIDs(t, h, "unset")
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEnter})
			matrixSearchScopeSelected(t, h, unset)
			if unset.GroupPath != "" {
				t.Fatal("normalizing the default group for navigation mutated persisted session data")
			}
		})
	}
}

func TestMatrixSearchScopeHome_InGroupNoLocalGroupExplainsFallback(t *testing.T) {
	h, _ := matrixSearchScopeHome(t)
	h.groupViewMode, h.statusFilter = session.GroupViewRecent, session.StatusStopped
	h.rebuildFlatItems()
	if len(h.flatItems) != 0 {
		t.Fatal("fallback fixture needs no selected local group")
	}
	matrixSearchScopeOpen(t, h, altKeyMsg('/'), "scope outside")
	matrixSearchScopeIDs(t, h, "outside")
	if strings.TrimSpace(h.search.notice) == "" {
		t.Fatal("group search fallback to all sessions must explain that there is no selected local group")
	}
}

func TestMatrixSearchScopeHome_ViewKeysAndInputMethod(t *testing.T) {
	if got := resolveHotkeys(nil)["view_search"]; got != "alt+?" {
		t.Fatalf("view search default = %q, want alt+?", got)
	}
	for _, key := range []tea.KeyMsg{altKeyMsg('?'), plainKeyMsg('¿')} {
		t.Run(key.String(), func(t *testing.T) {
			h, _ := matrixSearchScopeHome(t)
			focus := &fakeInputMethod{}
			h.inputMethod = focus
			h.Update(tea.FocusMsg{})
			matrixSearchScopeOpen(t, h, key, "scope nested")
			matrixSearchScopeIDs(t, h, "nested")
			if h.inputMethodNavigation() || len(focus.modes) < 2 || focus.modes[len(focus.modes)-1] || h.helpOverlay.IsVisible() {
				t.Fatal("view-search shortcut did not acquire text input exclusively")
			}
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEsc})
			if !h.inputMethodNavigation() || !focus.modes[len(focus.modes)-1] {
				t.Fatal("canceling current-view search did not restore list input mode")
			}
		})
	}
}

func TestMatrixSearchScopeHome_ViewKeyReboundAndDisabled(t *testing.T) {
	h, _ := matrixSearchScopeHome(t)
	h.setHotkeys(resolveHotkeys(map[string]string{"view_search": "ctrl+o"}))
	for _, key := range []tea.KeyMsg{altKeyMsg('?'), plainKeyMsg('¿')} {
		updateMatrixLocalSearchMatchingHome(t, h, key)
		if h.search.IsVisible() {
			t.Fatalf("rebound view search still responds to %q", key.String())
		}
	}
	matrixSearchScopeOpen(t, h, tea.KeyMsg{Type: tea.KeyCtrlO}, "scope nested")
	matrixSearchScopeIDs(t, h, "nested")
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	h.setHotkeys(resolveHotkeys(map[string]string{"view_search": ""}))
	for _, key := range []tea.KeyMsg{altKeyMsg('?'), plainKeyMsg('¿'), {Type: tea.KeyCtrlO}} {
		updateMatrixLocalSearchMatchingHome(t, h, key)
		if h.search.IsVisible() {
			t.Fatalf("disabled view search still responds to %q", key.String())
		}
	}
}

func TestMatrixSearchScopeHome_ManagedRecallUsesGlobalLocator(t *testing.T) {
	h, items := matrixSearchScopeHome(t)
	h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope = session.StatusRunning, session.TimeFilter7Days, session.GroupViewRecent, "Other"
	h.rebuildFlatItems()
	h.globalSearch.SetSource(newStubRecall())
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('r'))
	if !h.globalSearch.IsVisible() || h.globalSearch.managed == nil {
		t.Fatal("managed Recall did not open")
	}
	h.globalSearch.results = []*GlobalSearchResult{{Harness: "codex", DeckID: "target", SessionID: "native-target"}}
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	matrixSearchScopeSelected(t, h, items["target"])
	if cmd != nil || h.globalSearch.IsVisible() || matrixSearchScopeState(h) != (matrixSearchScopeView{}) {
		t.Fatal("managed Recall owner selection retained filters or scheduled an import")
	}
}

func TestMatrixSearchScopeHome_ViewDefaultRespectsExistingExplicitKey(t *testing.T) {
	h, _ := matrixSearchScopeHome(t)
	bindings := resolveHotkeys(map[string]string{hotkeyHelp: "alt+?"})
	if bindings[hotkeyViewSearch] != "" {
		t.Fatal("new view search default must yield to an existing explicit binding")
	}
	h.setHotkeys(bindings)
	updateMatrixLocalSearchMatchingHome(t, h, altKeyMsg('?'))
	if h.search.IsVisible() || !h.helpOverlay.IsVisible() {
		t.Fatal("Alt+? must still open the user's explicitly bound help action")
	}
}
