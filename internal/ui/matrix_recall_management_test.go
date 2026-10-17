package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/charmbracelet/x/ansi"
)

// matrix/hardened: Recall's indexed deck link can outlive the registered session. Ownership
// must be resolved against the current list, with the harness deciding which
// native id field is authoritative.
func TestMatrixRecallManagement_MatchesCurrentOwners(t *testing.T) {
	instances := []*session.Instance{
		{ID: "deck-owner", ClaudeSessionID: "deck-native"},
		{ID: "claude-owner", ClaudeSessionID: "claude-native"},
		{ID: "codex-owner", CodexSessionID: "codex-native"},
		{ID: "shared-Claude", ClaudeSessionID: "same-native"},
		{ID: "shared-Codex", CodexSessionID: "same-native"},
		{ID: "empty-fields"},
	}
	tests := []struct {
		name   string
		result GlobalSearchResult
		owner  string
	}{
		{"deck link", GlobalSearchResult{Harness: "claude", DeckID: "deck-owner"}, "deck-owner"},
		{"Claude native", GlobalSearchResult{Harness: "claude", SessionID: "claude-native"}, "claude-owner"},
		{"Codex native", GlobalSearchResult{Harness: "codex", SessionID: "codex-native"}, "codex-owner"},
		{"same native resolves to Claude", GlobalSearchResult{Harness: "claude", SessionID: "same-native"}, "shared-Claude"},
		{"same native resolves to Codex", GlobalSearchResult{Harness: "codex", SessionID: "same-native"}, "shared-Codex"},
		{"stale deck falls back to native", GlobalSearchResult{Harness: "codex", DeckID: "deleted-deck", SessionID: "codex-native"}, "codex-owner"},
		{"stale deck alone", GlobalSearchResult{Harness: "claude", DeckID: "deleted-deck", SessionID: "unregistered"}, ""},
		{"Codex cannot use Claude id", GlobalSearchResult{Harness: "codex", SessionID: "claude-native"}, ""},
		{"Claude cannot use Codex id", GlobalSearchResult{Harness: "claude", SessionID: "codex-native"}, ""},
		{"other harness cannot use native ids", GlobalSearchResult{Harness: "gemini", SessionID: "claude-native"}, ""},
		{"empty Claude native", GlobalSearchResult{Harness: "claude"}, ""},
		{"empty Codex native", GlobalSearchResult{Harness: "codex"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gs := NewGlobalSearch()
			result := tt.result
			gs.results = []*GlobalSearchResult{&result}
			gs.MarkInAgentDeck(instances)
			if result.InAgentDeck != (tt.owner != "") || result.InstanceID != tt.owner {
				t.Fatalf("ownership = managed %v, id %q; want id %q: %+v", result.InAgentDeck, result.InstanceID, tt.owner, result)
			}
		})
	}
}

func TestMatrixRecallManagement_RecomputesAfterOwnerChanges(t *testing.T) {
	gs := NewGlobalSearch()
	result := &GlobalSearchResult{Harness: "codex", DeckID: "removed", SessionID: "native"}
	gs.results = []*GlobalSearchResult{result}
	gs.MarkInAgentDeck([]*session.Instance{{ID: "removed", CodexSessionID: "native"}})
	if !result.InAgentDeck || result.InstanceID != "removed" {
		t.Fatalf("initial owner not marked: %+v", result)
	}
	gs.MarkInAgentDeck(nil)
	if result.InAgentDeck || result.InstanceID != "" {
		t.Fatalf("removed owner left a stale management label: %+v", result)
	}
	gs.MarkInAgentDeck([]*session.Instance{{ID: "replacement", CodexSessionID: "native"}})
	if !result.InAgentDeck || result.InstanceID != "replacement" {
		t.Fatalf("native ownership was not reassigned after removal: %+v", result)
	}
}

// No explicit MarkInAgentDeck call is allowed here: the ordinary Home.View
// path must resolve management labels when async Recall results first arrive,
// and must re-resolve them while the same overlay remains open.
func TestMatrixRecallManagement_HomeViewRefreshesLabels(t *testing.T) {
	h := newMatrixRecallManagementHome(t, 120)
	inst := &session.Instance{ID: "owner", CodexSessionID: "native"}
	setMatrixRecallManagementInstances(h, []*session.Instance{inst})
	h.globalSearch.applySearchResults(query.SearchResult{Hits: []query.Hit{
		{SessID: 1, Harness: "codex", NativeID: "native", DeckID: "old-deck", Title: "Codex"},
	}})
	result := h.globalSearch.results[0]
	if result.InAgentDeck || result.InstanceID != "" {
		t.Fatal("new search result was pre-marked, so this test would not prove Home.View wiring")
	}
	first := ansi.Strip(h.View())
	if !result.InAgentDeck || result.InstanceID != inst.ID || !strings.Contains(first, "[已管理]") || !strings.Contains(first, inst.ID) {
		t.Fatalf("first Home.View did not mark the current Codex owner: %+v\n%s", result, first)
	}
	setMatrixRecallManagementInstances(h, nil)
	afterRemoval := ansi.Strip(h.View())
	if result.InAgentDeck || result.InstanceID != "" || strings.Contains(afterRemoval, "[已管理]") || !strings.Contains(afterRemoval, "[未管理]") || !strings.Contains(afterRemoval, "未管理（当前列表）") {
		t.Fatalf("Home.View retained ownership after removing the session: %+v\n%s", result, afterRemoval)
	}
}

func TestMatrixRecallManagement_LabelsAndPreviewFitTerminal(t *testing.T) {
	for _, width := range []int{80, 120, 200} {
		t.Run(fmt.Sprintf("%d_columns", width), func(t *testing.T) {
			h := newMatrixRecallManagementHome(t, width)
			setMatrixRecallManagementInstances(h, []*session.Instance{
				{ID: "deck-owner"},
				{ID: "codex-owner", CodexSessionID: "codex-native"},
			})
			h.globalSearch.applySearchResults(query.SearchResult{Hits: []query.Hit{
				{SessID: 1, Harness: "claude", NativeID: "claude-native", DeckID: "deck-owner", Title: "Deck"},
				{SessID: 2, Harness: "codex", NativeID: "codex-native", Title: "Codex"},
				{SessID: 3, Harness: "claude", NativeID: "loose-native", DeckID: "deleted-deck", Title: "Loose"},
			}})
			labels := []string{"[已管理]", "[已管理]", "[未管理]"}
			titles := []string{"Deck", "Codex", "Loose"}
			for cursor := range h.globalSearch.results {
				h.globalSearch.cursor = cursor
				frame := ansi.Strip(h.View())
				lines := strings.Split(frame, "\n")
				// All three rows retain their labels whether selected or not.
				// Check the left pane's next line so the selected preview cannot
				// accidentally satisfy a missing result-row label.
				for i, title := range titles {
					found := false
					for lineNo, line := range lines {
						pane := strings.Split(line, "│")
						if len(pane) < 3 || !strings.Contains(pane[1], title) || lineNo+1 >= len(lines) {
							continue
						}
						nextPane := strings.Split(lines[lineNo+1], "│")
						if len(nextPane) >= 3 && strings.Contains(nextPane[1], labels[i]) {
							found = true
							break
						}
					}
					if !found {
						t.Fatalf("cursor %d: row %q missing label %q:\n%s", cursor, title, labels[i], frame)
					}
				}
				selected := h.globalSearch.results[cursor]
				if selected.InAgentDeck {
					if !strings.Contains(frame, selected.InstanceID) || !strings.Contains(frame, "已管理（当前列表）") {
						t.Fatalf("cursor %d: preview omitted management owner %q:\n%s", cursor, selected.InstanceID, frame)
					}
				} else if !strings.Contains(frame, "未管理（当前列表）") {
					t.Fatalf("cursor %d: preview omitted current-list scope:\n%s", cursor, frame)
				}
				for lineNo, line := range strings.Split(frame, "\n") {
					if cells := ansi.StringWidth(line); cells > width {
						t.Fatalf("cursor %d: line %d uses %d cells on %d-column terminal:\n%s", cursor, lineNo+1, cells, width, line)
					}
				}
			}
			// Chinese titles exercise cell width rather than byte/rune length.
			h.globalSearch.results[0].Title = strings.Repeat("中文标题", 30)
			for lineNo, line := range strings.Split(ansi.Strip(h.View()), "\n") {
				if cells := ansi.StringWidth(line); cells > width {
					t.Fatalf("Chinese title: line %d uses %d cells on %d-column terminal:\n%s", lineNo+1, cells, width, line)
				}
			}
		})
	}
}

func TestMatrixRecallManagement_DeckLinkWinsForMarkAndEnter(t *testing.T) {
	h := newMatrixRecallManagementHome(t, 120)
	// The native owner deliberately comes first: a single combined loop
	// would select it before discovering the stronger explicit deck link.
	nativeOwner := session.NewInstanceWithTool("native", t.TempDir(), "codex")
	nativeOwner.CodexSessionID = "shared-native"
	deckOwner := session.NewInstanceWithTool("deck", t.TempDir(), "codex")
	setMatrixRecallManagementInstances(h, []*session.Instance{nativeOwner, deckOwner})
	result := &GlobalSearchResult{Harness: "codex", SessionID: "shared-native", DeckID: deckOwner.ID}
	h.globalSearch.results = []*GlobalSearchResult{result}
	h.View()
	if !result.InAgentDeck || result.InstanceID != deckOwner.ID {
		t.Fatalf("mark selected the native owner instead of the deck link: %+v", result)
	}
	if cmd := h.handleGlobalSearchSelection(result); cmd != nil {
		t.Fatal("managed result should jump immediately, without scheduling an import")
	}
	if item := h.flatItems[h.cursor]; item.Session == nil || item.Session.ID != result.InstanceID {
		t.Fatalf("Enter target differs from the displayed owner %q: %+v", result.InstanceID, item)
	}
}

func TestMatrixRecallManagement_EmptyNativeDoesNotJump(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			h := newMatrixRecallManagementHome(t, 120)
			inst := session.NewInstanceWithTool("empty native", t.TempDir(), harness)
			setMatrixRecallManagementInstances(h, []*session.Instance{inst})
			result := &GlobalSearchResult{Harness: harness, Missing: true}
			h.globalSearch.results = []*GlobalSearchResult{result}
			h.View()
			if result.InAgentDeck || result.InstanceID != "" {
				t.Fatalf("empty native id was marked as registered: %+v", result)
			}
			// Missing avoids executing any import command. A valid unowned
			// result reports the missing transcript; the old empty-id match
			// silently jumped to this unrelated session instead.
			if cmd := h.handleGlobalSearchSelection(result); cmd != nil {
				t.Fatal("missing transcript should report its error without scheduling an import")
			}
			if h.err == nil || !strings.Contains(h.err.Error(), "transcript file is gone") {
				t.Fatalf("empty native id jumped to an unrelated session instead of reporting the missing transcript: %v", h.err)
			}
		})
	}
}

func newMatrixRecallManagementHome(t *testing.T, width int) *Home {
	t.Helper()
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
	h.width, h.height, h.initialLoading = width, 40, false
	h.globalSearch.SetSource(newStubRecall())
	h.globalSearch.SetSize(width, h.height)
	h.globalSearch.Show()
	return h
}

func setMatrixRecallManagementInstances(h *Home, instances []*session.Instance) {
	h.instancesMu.Lock()
	h.instances = instances
	h.instanceByID = make(map[string]*session.Instance, len(instances))
	for _, inst := range instances {
		h.instanceByID[inst.ID] = inst
	}
	h.instancesMu.Unlock()
	h.groupTree = session.NewGroupTree(instances)
	h.rebuildFlatItems()
}
