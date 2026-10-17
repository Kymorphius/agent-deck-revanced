package ui

import (
	"fmt"
	"slices"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: exercise the ordinary / route, not just the matcher, so
// searching a group or a loosely typed project still selects its existing row.
func TestMatrixLocalSearchMatching_GroupAndSelection(t *testing.T) {
	first := &session.Instance{ID: "first", Title: "修复", Tool: "codex", ProjectPath: "/work/alpha", GroupPath: "工具/研发"}
	second := &session.Instance{ID: "second", Title: "校验", Tool: "claude", ProjectPath: "/work/beta", GroupPath: "工具/研发"}
	unrelated := &session.Instance{ID: "other", Title: "清理", Tool: "shell", ProjectPath: "/work/omega", GroupPath: "生活"}
	h := newMatrixLocalSearchMatchingHome(t, 120, []*session.Instance{first, second, unrelated})
	for _, group := range h.groupTree.GroupList {
		group.Expanded = false
	}
	h.rebuildFlatItems()

	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !h.search.IsVisible() || h.globalSearch.IsVisible() {
		t.Fatal("/ must open the local search overlay")
	}
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("研发")})
	assertMatrixLocalSearchMatchingIDs(t, h.search, "first", "second")

	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyDown})
	if h.search.Selected() != second {
		t.Fatal("Down must select the second matching session")
	}
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyUp})
	if h.search.Selected() != first {
		t.Fatal("Up must return to the first matching session")
	}
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("selecting a local search result must only locate its row")
	}
	if h.search.IsVisible() || h.getSelectedSession() != second {
		t.Fatal("Enter must close the overlay and select the existing session")
	}
	if len(h.instances) != 3 {
		t.Fatal("search selection changed the registered session list")
	}
	for _, path := range []string{"工具", "工具/研发"} {
		if group := h.groupTree.Groups[path]; group == nil || !group.Expanded {
			t.Fatalf("Enter must reveal the session by expanding %q", path)
		}
	}
}

func TestMatrixLocalSearchMatching_ProjectSeparatorsAndAbbreviation(t *testing.T) {
	for _, query := range []string{"agent deck", "adk"} {
		t.Run(query, func(t *testing.T) {
			target := &session.Instance{ID: "target", Title: "排查", Tool: "codex", ProjectPath: "/work/agent-deck", GroupPath: "工具"}
			h := newMatrixLocalSearchMatchingHome(t, 80, []*session.Instance{
				target,
				{ID: "other", Title: "清理", Tool: "shell", ProjectPath: "/work/toolkit", GroupPath: "生活"},
			})
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(query)})
			assertMatrixLocalSearchMatchingIDs(t, h.search, target.ID)
		})
	}
}

func TestMatrixLocalSearchMatching_RefreshKeepsCurrentQuery(t *testing.T) {
	h := newMatrixLocalSearchMatchingHome(t, 80, []*session.Instance{
		{ID: "old", Title: "排查", Tool: "codex", ProjectPath: "/work/agent-deck", GroupPath: "工具"},
		{ID: "other", Title: "清理", Tool: "shell", ProjectPath: "/work/toolkit", GroupPath: "生活"},
	})
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("agent deck")})
	assertMatrixLocalSearchMatchingIDs(t, h.search, "old")

	// Background registry updates use SetItems while the input remains open.
	// An existing row that moved away must disappear, and a new match must enter.
	h.search.SetItems([]*session.Instance{
		{ID: "old", Title: "排查", Tool: "codex", ProjectPath: "/work/renamed", GroupPath: "工具"},
		{ID: "new", Title: "校验", Tool: "claude", ProjectPath: "/work/agent_deck", GroupPath: "工具"},
		{ID: "other", Title: "清理", Tool: "shell", ProjectPath: "/work/toolkit", GroupPath: "生活"},
	})
	if h.search.input.Value() != "agent deck" || !h.search.IsVisible() {
		t.Fatal("a background refresh must retain the current query and overlay")
	}
	assertMatrixLocalSearchMatchingIDs(t, h.search, "new")
}

func TestMatrixLocalSearchMatching_ScopedRefreshDoesNotLeak(t *testing.T) {
	setXDGTestHome(t)
	s := NewSearch()
	s.SetScopedGroup("团队/研发")
	items := []*session.Instance{
		{ID: "inside", Title: "排查", ProjectPath: "/work/agent-deck", GroupPath: "团队/研发"},
		{ID: "child", Title: "排查", ProjectPath: "/work/agent-deck", GroupPath: "团队/研发/子组"},
		{ID: "outside", Title: "排查", ProjectPath: "/work/agent-deck", GroupPath: "其他"},
	}
	s.SetItems(items)
	s.Show()
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("agent deck")})
	assertMatrixLocalSearchMatchingIDs(t, s, "inside")

	s.SetItems(append(items, &session.Instance{ID: "new-outside", Title: "agent deck", GroupPath: "其他"}))
	assertMatrixLocalSearchMatchingIDs(t, s, "inside")
	if s.input.Value() != "agent deck" {
		t.Fatal("scoped background refresh lost the query")
	}
}

func TestMatrixLocalSearchMatching_ChineseInputFits(t *testing.T) {
	for _, width := range []int{30, 60, 80, 120} {
		t.Run(fmt.Sprintf("%d_columns", width), func(t *testing.T) {
			h := newMatrixLocalSearchMatchingHome(t, width, []*session.Instance{
				{ID: "target", Title: "修复", Tool: "codex", ProjectPath: "/work/alpha", GroupPath: "工具/研发"},
			})
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
			assertDialogFits(t, h.search.View(), width, h.height, "[Enter] Select", "[Esc] Cancel")
			updateMatrixLocalSearchMatchingHome(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("研发")})
			assertMatrixLocalSearchMatchingIDs(t, h.search, "target")
			assertDialogFits(t, h.search.View(), width, h.height, "研发", "修复", "[Enter] Select", "[Esc] Cancel")
		})
	}
}

func newMatrixLocalSearchMatchingHome(t *testing.T, width int, items []*session.Instance) *Home {
	t.Helper()
	setXDGTestHome(t)
	h := NewHome()
	t.Cleanup(func() {
		h.cancel()
		if h.recallSource != nil {
			h.recallSource.Close()
		}
		if h.storage != nil {
			_ = h.storage.Close()
		}
	})
	h.width, h.height, h.initialLoading = width, 30, false
	h.search.SetSize(width, h.height)
	h.instancesMu.Lock()
	h.instances = items
	h.instanceByID = make(map[string]*session.Instance, len(items))
	for _, inst := range items {
		h.instanceByID[inst.ID] = inst
	}
	h.instancesMu.Unlock()
	h.groupTree = session.NewGroupTree(items)
	h.rebuildFlatItems()
	h.search.SetItems(items)
	return h
}

func updateMatrixLocalSearchMatchingHome(t *testing.T, h *Home, msg tea.Msg) {
	t.Helper()
	model, _ := h.Update(msg)
	if model != h {
		t.Fatalf("search interaction replaced Home with %T", model)
	}
}

func assertMatrixLocalSearchMatchingIDs(t *testing.T, s *Search, want ...string) {
	t.Helper()
	got := make([]string, len(s.results))
	for i, inst := range s.results {
		got[i] = inst.ID
	}
	if !slices.Equal(got, want) {
		t.Fatalf("search %q result IDs = %v, want %v", s.input.Value(), got, want)
	}
}
