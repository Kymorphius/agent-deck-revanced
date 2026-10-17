package ui

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// matrix/hardened: the separate Recall shortcut searches current registered owners.
func TestMatrixManagedRecall_MainKeysSelectScope(t *testing.T) {
	if got := resolveHotkeys(nil)[hotkeyManagedRecall]; got != "alt+r" {
		t.Fatalf("managed Recall default key = %q, want alt+r", got)
	}
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'r'}, Alt: true},
		{Type: tea.KeyRunes, Runes: []rune{'®'}},
	} {
		t.Run(key.String(), func(t *testing.T) {
			h := newMatrixRecallManagementHome(t, 200)
			h.setHotkeys(resolveHotkeys(nil))
			h.globalSearch.Hide()
			h.handleMainKey(key)
			if !h.globalSearch.IsVisible() || h.globalSearch.managed == nil {
				t.Fatalf("%q did not open managed Recall", key.String())
			}
			if !strings.Contains(h.globalSearch.headerLine(), "已管理") || !strings.Contains(h.globalSearch.input.Placeholder, "已管理") {
				t.Fatalf("managed Recall scope is not visible in title/placeholder: %q / %q", h.globalSearch.headerLine(), h.globalSearch.input.Placeholder)
			}
			if frame := ansi.Strip(h.globalSearch.View()); !strings.Contains(frame, "[Enter] Locate") {
				t.Fatalf("managed Recall footer does not describe locating a session:\n%s", frame)
			}
			h.globalSearch.Hide()
			h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
			if !h.globalSearch.IsVisible() || h.globalSearch.managed != nil {
				t.Fatal("G must open the full Recall index after a managed search")
			}
			if frame := ansi.Strip(h.globalSearch.View()); !strings.Contains(frame, "[Enter] Open") {
				t.Fatalf("full Recall footer lost its Open action:\n%s", frame)
			}
		})
	}
}

func TestMatrixManagedRecall_HotkeyCanBeReboundAndDisabled(t *testing.T) {
	h := newMatrixRecallManagementHome(t, 120)
	h.globalSearch.Hide()
	h.setHotkeys(resolveHotkeys(map[string]string{hotkeyManagedRecall: "ctrl+r"}))
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'r'}, Alt: true},
		{Type: tea.KeyRunes, Runes: []rune{'®'}},
	} {
		h.handleMainKey(key)
		if h.globalSearch.IsVisible() {
			t.Fatalf("rebound managed Recall still responds to default key %q", key.String())
		}
	}
	h.handleMainKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !h.globalSearch.IsVisible() || h.globalSearch.managed == nil {
		t.Fatal("configured ctrl+r did not open managed Recall")
	}
	h.globalSearch.Hide()
	h.setHotkeys(resolveHotkeys(map[string]string{hotkeyManagedRecall: ""}))
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'r'}, Alt: true},
		{Type: tea.KeyRunes, Runes: []rune{'®'}},
		{Type: tea.KeyCtrlR},
	} {
		h.handleMainKey(key)
		if h.globalSearch.IsVisible() {
			t.Fatalf("disabled managed Recall still responds to %q", key.String())
		}
	}
}

func TestMatrixManagedRecall_HomeBuildsCurrentScope(t *testing.T) {
	h := newMatrixRecallManagementHome(t, 120)
	setMatrixRecallManagementInstances(h, []*session.Instance{
		{ID: "claude-deck", ClaudeSessionID: "claude-native"},
		{ID: "codex-deck", CodexSessionID: "codex-native"},
		{ID: "both-deck", ClaudeSessionID: "second-claude", CodexSessionID: "second-codex"},
		{ID: "no-native"},
	})
	h.openManagedRecall()
	if h.globalSearch.managed == nil {
		t.Fatal("openManagedRecall did not set a scope")
	}
	want := query.ManagedScope{
		DeckIDs:   []string{"claude-deck", "codex-deck", "both-deck", "no-native"},
		ClaudeIDs: []string{"claude-native", "second-claude"},
		CodexIDs:  []string{"codex-native", "second-codex"},
	}
	assertMatrixManagedScope(t, *h.globalSearch.managed, want)
	// The deferred search command must dispatch through SearchManaged with
	// the captured scope, rather than filtering an already limited all-search.
	src := h.globalSearch.source.(*stubRecall)
	_, cmd := h.globalSearch.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("clock")})
	drain(h.globalSearch, cmd)
	if len(src.managedSearches) != 1 {
		t.Fatalf("managed search dispatches = %d, want 1", len(src.managedSearches))
	}
	assertMatrixManagedScope(t, src.managedSearches[0], want)
	setMatrixRecallManagementInstances(h, nil)
	h.openManagedRecall()
	if h.globalSearch.managed == nil {
		t.Fatal("an empty managed list must still create a restricted scope")
	}
	assertMatrixManagedScope(t, *h.globalSearch.managed, query.ManagedScope{})
}

func TestMatrixManagedRecall_RealIndexSearchesRegisteredNative(t *testing.T) {
	idx := newRealRecallIndex(t)
	res, err := idx.Search(context.Background(), "heron", recallResultLimit)
	if err != nil || len(res.Hits) != 1 {
		t.Fatalf("fixture search: %v %+v", err, res)
	}
	h := newMatrixRecallManagementHome(t, 120)
	h.globalSearch.SetSource(idx)
	setMatrixRecallManagementInstances(h, []*session.Instance{
		{ID: "current-owner", ClaudeSessionID: res.Hits[0].NativeID},
	})
	h.openManagedRecall()
	_, cmd := h.globalSearch.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("heron")})
	drain(h.globalSearch, cmd)
	if len(h.globalSearch.results) != 1 || h.globalSearch.results[0].SessID != res.Hits[0].SessID {
		t.Fatalf("managed native search did not return the registered transcript: %+v", h.globalSearch.results)
	}
	if frame := ansi.Strip(h.View()); !strings.Contains(frame, "[已管理]") || !strings.Contains(frame, "current-owner") {
		t.Fatalf("managed native search did not locate its current owner:\n%s", frame)
	}
	// Non-nil empty scope searches nothing; it must not degrade to the full
	// Recall index even though the query matches the same stored transcript.
	h.globalSearch.ShowManaged(query.ManagedScope{})
	_, cmd = h.globalSearch.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("heron")})
	drain(h.globalSearch, cmd)
	if len(h.globalSearch.results) != 0 {
		t.Fatalf("empty managed scope leaked full-index hits: %+v", h.globalSearch.results)
	}
}

func TestMatrixManagedRecall_OldMessagesCannotCrossScope(t *testing.T) {
	for _, fromManaged := range []bool{true, false} {
		name := "managed_to_all"
		if !fromManaged {
			name = "all_to_managed"
		}
		t.Run(name, func(t *testing.T) {
			gs := NewGlobalSearch()
			gs.SetSource(newStubRecall())
			scope := query.ManagedScope{DeckIDs: []string{"owner"}}
			if fromManaged {
				gs.ShowManaged(scope)
			} else {
				gs.Show()
			}
			gs.input.SetValue("same query")
			gs.query = "same query"
			oldGeneration := gs.generation
			gs.Hide()
			if fromManaged {
				gs.Show()
			} else {
				gs.ShowManaged(scope)
			}
			gs.input.SetValue("same query")
			gs.query = "same query"
			if gs.generation == oldGeneration {
				t.Fatal("reopening a different scope did not advance the generation")
			}
			gs.Update(globalSearchResultsMsg{query: "same query", generation: gs.generation,
				res: query.SearchResult{Hits: []query.Hit{{SessID: 101, Title: "current scope"}}}})
			gs.Update(globalSearchResultsMsg{query: "same query", generation: oldGeneration,
				res: query.SearchResult{Hits: []query.Hit{{SessID: 102, Title: "stale scope"}}}})
			if len(gs.results) != 1 || gs.results[0].SessID != 101 {
				t.Fatalf("old same-query results overwrote the reopened scope: %+v", gs.results)
			}
			if _, cmd := gs.Update(globalSearchDebounceMsg{query: "same query", generation: oldGeneration}); cmd != nil || gs.searching {
				t.Fatal("old same-query debounce scheduled a search in the reopened scope")
			}
			if (gs.managed != nil) == fromManaged {
				t.Fatal("stale messages changed the active search scope")
			}
		})
	}
}

func TestMatrixManagedRecall_QueryChangesAdvanceGeneration(t *testing.T) {
	gs := NewGlobalSearch()
	gs.SetSource(newStubRecall())
	gs.ShowManaged(query.ManagedScope{DeckIDs: []string{"owner"}})
	first := gs.generation
	_, cmd := gs.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("clock")})
	if gs.generation <= first || cmd == nil {
		t.Fatalf("query update did not advance generation/schedule debounce: %d -> %d", first, gs.generation)
	}
	msg, ok := matrixManagedRecallDebounce(cmd)
	if !ok || msg.generation != gs.generation || msg.query != "clock" {
		t.Fatalf("debounce did not capture the updated generation: %#v", msg)
	}
	oldGeneration := gs.generation
	gs.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if gs.generation <= oldGeneration {
		t.Fatal("editing an existing query did not advance the generation")
	}
	// Even if a later query is textually equal to an earlier query, its
	// generation is distinct and the earlier async result remains obsolete.
	gs.input.SetValue("clock")
	gs.Update(globalSearchResultsMsg{query: "clock", generation: oldGeneration,
		res: query.SearchResult{Hits: []query.Hit{{SessID: 102, Title: "stale query"}}}})
	if len(gs.results) != 0 {
		t.Fatalf("previous generation's equal-text query was accepted: %+v", gs.results)
	}
}

func matrixManagedRecallDebounce(cmd tea.Cmd) (globalSearchDebounceMsg, bool) {
	if cmd == nil {
		return globalSearchDebounceMsg{}, false
	}
	switch msg := cmd().(type) {
	case globalSearchDebounceMsg:
		return msg, true
	case tea.BatchMsg:
		for _, child := range msg {
			if debounce, ok := matrixManagedRecallDebounce(child); ok {
				return debounce, true
			}
		}
	}
	return globalSearchDebounceMsg{}, false
}

func assertMatrixManagedScope(t *testing.T, got, want query.ManagedScope) {
	t.Helper()
	for _, pair := range []struct {
		name      string
		got, want []string
	}{
		{"deck", got.DeckIDs, want.DeckIDs},
		{"Claude", got.ClaudeIDs, want.ClaudeIDs},
		{"Codex", got.CodexIDs, want.CodexIDs},
	} {
		actual, expected := append([]string{}, pair.got...), append([]string{}, pair.want...)
		sort.Strings(actual)
		sort.Strings(expected)
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s scope = %v, want %v", pair.name, actual, expected)
		}
	}
}

func TestMatrixManagedRecall_TrimmedQuerySearches(t *testing.T) {
	gs := NewGlobalSearch()
	src := newStubRecall()
	gs.SetSource(src)
	gs.ShowManaged(query.ManagedScope{DeckIDs: []string{"auth-fix"}})
	_, cmd := gs.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" clock ")})
	drain(gs, cmd)
	if gs.searching || len(gs.results) != 1 || len(src.searches) != 1 || src.searches[0] != "clock" {
		t.Fatalf("whitespace query failed to apply its scoped result: searching %v, results %+v, searches %v", gs.searching, gs.results, src.searches)
	}
}

func TestMatrixManagedRecall_RemovedOwnerCannotBeImported(t *testing.T) {
	h := newMatrixRecallManagementHome(t, 120)
	inst := &session.Instance{ID: "removed-owner", CodexSessionID: "native"}
	setMatrixRecallManagementInstances(h, []*session.Instance{inst})
	h.openManagedRecall()
	result := &GlobalSearchResult{Harness: "codex", SessionID: "native", DeckID: inst.ID}
	h.globalSearch.results = []*GlobalSearchResult{result}
	setMatrixRecallManagementInstances(h, nil)
	if cmd := h.handleGlobalSearchSelection(result); cmd != nil {
		t.Fatal("managed-only Enter scheduled an import after its owner disappeared")
	}
	if h.err == nil || !strings.Contains(h.err.Error(), "已不在当前 profile") {
		t.Fatalf("removed owner must ask for a new search: %v", h.err)
	}
	h.View()
	if len(h.globalSearch.results) != 0 {
		t.Fatal("removed owner remained in the managed-only result list")
	}
}

func TestMatrixManagedRecall_HelpFollowsBinding(t *testing.T) {
	for _, test := range []struct {
		name    string
		binding string
	}{
		{"default", "alt+r"},
		{"rebound", "ctrl+o"},
		{"disabled", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			help := NewHelpOverlay()
			help.SetSize(200, 300)
			help.SetHotkeys(resolveHotkeys(map[string]string{hotkeyManagedRecall: test.binding}))
			help.Show()
			frame := ansi.Strip(help.View())
			found := false
			for _, line := range strings.Split(frame, "\n") {
				if strings.Contains(line, "Recall search (managed sessions") {
					found = true
					if test.binding == "" || !strings.Contains(line, test.binding) {
						t.Fatalf("help does not match configured binding %q: %s", test.binding, line)
					}
				}
			}
			if found != (test.binding != "") {
				t.Fatalf("managed shortcut help visibility = %v for binding %q", found, test.binding)
			}
		})
	}
}
