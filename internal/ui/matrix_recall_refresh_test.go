package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/ingest"
	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/recall/store"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: a retry may arrive before its canceled predecessor releases
// the sweep lock. Give that explicit pass a few paced attempts, then stop.
func TestMatrixRecallRefresh_LockRetryIsBounded(t *testing.T) {
	fastCatchUp(t)
	gs := NewGlobalSearch()
	gs.SetSource(newStubRecall())
	gs.Show()
	t.Cleanup(gs.Hide)
	for attempt := 0; attempt < 4; attempt++ {
		_, cmd := gs.Update(recallRefreshMsg{epoch: gs.epoch, err: store.ErrLocked})
		var retry *recallCatchUpMsg
		if batch, ok := cmd().(tea.BatchMsg); ok {
			for _, child := range batch {
				if tick, ok := child().(recallCatchUpMsg); ok {
					retry = &tick
				}
			}
		}
		if attempt == 3 {
			if gs.sweeping || retry != nil {
				t.Fatal("lock contention retries were not bounded")
			}
			break
		}
		if retry == nil || !retry.ungated || !gs.sweeping {
			t.Fatalf("attempt %d lost the pending manual retry", attempt)
		}
		_, retryCmd := gs.Update(*retry)
		if msg, ok := retryCmd().(recallRefreshMsg); !ok || msg.gated {
			t.Fatal("manual lock retry lost the original ungated pass")
		}
	}
}

// matrix/hardened: deletion/card-only commits and failed older reads also need
// a fresh snapshot, rather than silently losing the pending follow-up.
func TestMatrixRecallRefresh_CommitDuringFailedSearch(t *testing.T) {
	for _, progress := range []ingest.Result{{Parsed: 1}, {Missing: 1}, {Sessions: 1}} {
		gs := NewGlobalSearch()
		src := newStubRecall()
		gs.SetSource(src)
		gs.Show()
		t.Cleanup(gs.Hide)
		gs.query = "clock"
		gs.input.SetValue(gs.query)
		gs.searching = true
		gs.Update(recallRefreshMsg{epoch: gs.epoch, res: progress})
		_, cmd := gs.Update(globalSearchResultsMsg{generation: gs.generation, query: gs.query, err: context.DeadlineExceeded})
		if cmd == nil || !gs.searching || gs.searchDirty || gs.searchErr != "" {
			t.Fatalf("committed progress lost after the older read failed: %+v", progress)
		}
		drain(gs, cmd)
		if len(src.searches) != 1 || len(gs.results) != len(src.hits) {
			t.Fatal("the follow-up did not query the new snapshot")
		}
	}
}

func TestMatrixRecallRefresh_StatusCannotRegress(t *testing.T) {
	gs := NewGlobalSearch()
	src := newStubRecall()
	gs.SetSource(src)
	gs.Show()
	t.Cleanup(gs.Hide)
	old := gs.statusCmd()()
	src.status.Sessions++
	fresh := gs.statusCmd()()
	gs.Update(fresh)
	gs.Update(old)
	if gs.status.Sessions != src.status.Sessions {
		t.Fatal("a late pre-refresh status overwrote the latest index count")
	}
}

// matrix/hardened: retry is routed through Home and retains both the visible
// search and the immutable managed scope while replacing abandoned requests.
func TestMatrixRecallRefresh_HomeKeysPreserveQueryAndScope(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyCtrlR, tea.KeyF5} {
		t.Run((tea.KeyMsg{Type: key}).String(), func(t *testing.T) {
			h := newMatrixRecallManagementHome(t, 120)
			src := newStubRecall()
			h.globalSearch.SetSource(src)
			scope := query.ManagedScope{DeckIDs: []string{"auth-fix"}, CodexIDs: []string{src.hits[1].NativeID}}
			h.globalSearch.ShowManaged(scope)
			gs := h.globalSearch
			t.Cleanup(gs.Hide)
			gs.input.SetValue("  clock skew  ")
			gs.query = "clock skew"
			gs.applySearchResults(query.SearchResult{Query: gs.query, Hits: src.hits[:2]})
			gs.cursor = 1
			oldResults, oldContext := gs.results, gs.ctx
			oldEpoch, oldGeneration := gs.epoch, gs.generation
			gs.preview[41], gs.previewT[41], gs.previewE[42] = src.detail[41], true, "stale failure"
			gs.refresh, gs.searchErr = "stale refresh failure", "stale search failure"

			model, cmd := h.Update(tea.KeyMsg{Type: key})
			h = model.(*Home)
			if cmd == nil || !gs.IsVisible() || !gs.searching || !gs.sweeping {
				t.Fatalf("refresh was not scheduled through Home: visible=%v searching=%v sweeping=%v cmd=%v", gs.IsVisible(), gs.searching, gs.sweeping, cmd != nil)
			}
			if gs.input.Value() != "  clock skew  " || gs.query != "clock skew" {
				t.Fatalf("refresh changed input/query: %q / %q", gs.input.Value(), gs.query)
			}
			assertMatrixManagedScope(t, *gs.managed, scope)
			if len(gs.results) != len(oldResults) || gs.results[1] != oldResults[1] || gs.cursor != 1 {
				t.Fatal("refresh discarded the visible results or selection before the asynchronous response")
			}
			if len(src.searches) != 0 || len(src.refreshes) != 0 {
				t.Fatal("refresh performed index work synchronously on the keypress")
			}
			if gs.epoch != oldEpoch+1 || gs.generation != oldGeneration+1 || oldContext.Err() != context.Canceled {
				t.Fatalf("refresh did not replace the request cycle: epoch=%d generation=%d old context=%v", gs.epoch, gs.generation, oldContext.Err())
			}
			if len(gs.preview) != 0 || len(gs.previewT) != 0 || len(gs.previewE) != 0 || gs.refresh != "" || gs.searchErr != "" {
				t.Fatal("refresh retained stale previews or error messages")
			}

			// The search starts without waiting for another keystroke or a
			// debounce. Its new ranking keeps the selected conversation.
			src.hits[0], src.hits[1] = src.hits[1], src.hits[0]
			h = stepHome(t, h, cmd)
			if !reflect.DeepEqual(src.searches, []string{"clock skew"}) || len(src.managedSearches) != 1 {
				t.Fatalf("refresh dispatches: searches=%v managed=%v", src.searches, src.managedSearches)
			}
			assertMatrixManagedScope(t, src.managedSearches[0], scope)
			if len(src.refreshes) != 1 || src.refreshes[0] {
				t.Fatalf("manual refresh passes=%v, want one ungated pass", src.refreshes)
			}
			if h.globalSearch.Selected() == nil || h.globalSearch.Selected().SessID != 42 || gs.cursor != 0 {
				t.Fatalf("same-query refresh lost the selected conversation: cursor=%d selected=%+v", gs.cursor, gs.Selected())
			}
		})
	}
}

// matrix/hardened: callback isolation covers every message which can mutate a
// header, preview or catch-up chain, including multiple rapid manual retries.
func TestMatrixRecallRefresh_OldCyclesAreInert(t *testing.T) {
	gs := NewGlobalSearch()
	src := newStubRecall()
	gs.SetSource(src)
	gs.Show()
	t.Cleanup(gs.Hide)
	gs.input.SetValue("clock")
	gs.query = "clock"
	oldEpoch, oldGeneration := gs.epoch, gs.generation
	gs.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	gs.Update(tea.KeyMsg{Type: tea.KeyF5})
	gs.status, gs.hasStat = query.Status{Sessions: 99, Messages: 123}, true
	gs.refresh = "current cycle"
	gs.results = []*GlobalSearchResult{{SessID: 99, Title: "current result"}}
	for _, epoch := range []uint64{oldEpoch, oldEpoch + 1} {
		messages := []tea.Msg{
			recallStatusMsg{epoch: epoch, status: query.Status{Sessions: 1}},
			recallRefreshMsg{epoch: epoch, res: ingest.Result{Parsed: 1, Deferred: 5}},
			recallRefreshMsg{epoch: epoch, err: errors.New("stale failure")},
			recallPreviewMsg{epoch: epoch, sessID: 99, detail: src.detail[41]},
			recallPreviewMsg{epoch: epoch, sessID: 99, err: errors.New("stale preview failure")},
			recallCatchUpMsg{epoch: epoch},
		}
		for _, msg := range messages {
			if _, cmd := gs.Update(msg); cmd != nil {
				t.Fatalf("old cycle %d callback %T scheduled work", epoch, msg)
			}
		}
	}
	if _, cmd := gs.Update(globalSearchDebounceMsg{query: "clock", generation: oldGeneration}); cmd != nil {
		t.Fatal("an old debounce scheduled another search")
	}
	if _, cmd := gs.Update(globalSearchResultsMsg{query: "clock", generation: oldGeneration, res: query.SearchResult{Hits: src.hits}}); cmd != nil {
		t.Fatal("an old search response scheduled a preview")
	}
	if gs.status.Sessions != 99 || gs.status.Messages != 123 || !gs.hasStat || !gs.sweeping || !gs.searching || gs.refresh != "current cycle" {
		t.Fatalf("old cycle changed current state: status=%+v sweeping=%v searching=%v refresh=%q", gs.status, gs.sweeping, gs.searching, gs.refresh)
	}
	if len(gs.preview) != 0 || len(gs.previewT) != 0 || len(gs.previewE) != 0 || len(gs.results) != 1 || gs.results[0].SessID != 99 {
		t.Fatal("old cycle changed the current preview or results")
	}
	if len(src.searches) != 0 || len(src.refreshes) != 0 {
		t.Fatal("an old callback reached the index")
	}
}

// matrix/hardened: parsed sources become searchable before the entire backlog
// finishes. A search already running gets one follow-up without being starved
// by cancellation on every small ingest batch.
func TestMatrixRecallRefresh_DeferredProgressRerunsQuery(t *testing.T) {
	fastCatchUp(t)
	for _, busy := range []bool{false, true} {
		name := "idle_search"
		if busy {
			name = "search_in_flight"
		}
		t.Run(name, func(t *testing.T) {
			src := newStubRecall()
			gs := NewGlobalSearch()
			gs.SetSource(src)
			gs.Show()
			t.Cleanup(gs.Hide)
			gs.input.SetValue("clock")
			gs.query = "clock"
			old := query.SearchResult{Query: gs.query, Hits: src.hits[:1]}
			gs.applySearchResults(old)
			gs.searching = busy
			_, cmd := gs.Update(recallRefreshMsg{epoch: gs.epoch, res: ingest.Result{Parsed: 1, Deferred: 3}})
			drainMatrixRecallBeforeCatchUp(gs, cmd)
			if busy {
				if len(src.searches) != 0 {
					t.Fatal("partial ingest restarted a search that is still in flight")
				}
				_, cmd = gs.Update(globalSearchResultsMsg{query: gs.query, generation: gs.generation, res: old})
				drainMatrixRecallBeforeCatchUp(gs, cmd)
			}
			if !reflect.DeepEqual(src.searches, []string{"clock"}) || len(gs.results) != len(src.hits) {
				t.Fatalf("committed partial progress did not re-search: searches=%v results=%d", src.searches, len(gs.results))
			}
			if !gs.sweeping || len(src.refreshes) != 0 {
				t.Fatalf("test crossed the next catch-up tick: sweeping=%v refreshes=%v", gs.sweeping, src.refreshes)
			}
		})
	}
}

// matrix/hardened: deliver current results before the next background pass so
// the partial-ingest regression cannot pass only after the backlog is empty.
func drainMatrixRecallBeforeCatchUp(gs *GlobalSearch, cmd tea.Cmd) {
	for cmd != nil {
		msg := cmd()
		cmd = nil
		switch msg := msg.(type) {
		case nil, recallCatchUpMsg:
		case tea.BatchMsg:
			for _, child := range msg {
				drainMatrixRecallBeforeCatchUp(gs, child)
			}
		default:
			gs, cmd = gs.Update(msg)
		}
	}
}

func TestMatrixRecallRefresh_NewQueryResetsSelection(t *testing.T) {
	gs := NewGlobalSearch()
	src := newStubRecall()
	gs.SetSource(src)
	gs.Show()
	t.Cleanup(gs.Hide)
	gs.input.SetValue("clock")
	gs.query = "clock"
	gs.applySearchResults(query.SearchResult{Query: gs.query, Hits: src.hits})
	gs.cursor = 2
	gs.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	gs.Update(globalSearchResultsMsg{query: gs.query, generation: gs.generation, res: query.SearchResult{
		Query: gs.query, Hits: []query.Hit{src.hits[1], src.hits[2], src.hits[0]},
	}})
	if gs.cursor != 0 || gs.Selected() == nil || gs.Selected().SessID != 42 {
		t.Fatalf("a different query retained the previous selection: cursor=%d selected=%+v", gs.cursor, gs.Selected())
	}
}

// matrix/hardened: exercise the real append -> bounded sweep -> FTS query ->
// preview path. Retrying the old query alone cannot discover this new turn.
func TestMatrixRecallRefresh_HomeFindsAppendedTranscript(t *testing.T) {
	idx := newRealRecallIndex(t)
	// The read-only fixture does not need a registry; the actual Refresh
	// adapter passes its pointer as an ingest.Registry, so wire an empty
	// registry just as openRecallIndex does under this isolated HOME.
	idx.registry = session.NewRecallRegistry(idx.profile, nil)
	t.Cleanup(idx.registry.Close)
	h := newMatrixRecallManagementHome(t, 120)
	gs := h.globalSearch
	gs.SetSource(idx)
	t.Cleanup(gs.Hide)
	h = stepHome(t, h, gs.Show())
	const keyword = "aurorafinch"
	model, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(keyword)})
	h = stepHome(t, model.(*Home), cmd)
	if gs.query != keyword || len(gs.results) != 0 {
		t.Fatalf("new keyword already matched before append: query=%q results=%+v", gs.query, gs.results)
	}
	const nativeID = "e2e0e2e0-0000-4000-8000-00000000e2e0"
	path := filepath.Join(idx.roots[0].Dir, "projects", "-tmp-proj", nativeID+".jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(`{"type":"user","message":{"role":"user","content":"please also review the aurorafinch rollout"},"uuid":"u2","timestamp":"2026-09-19T12:00:02Z","sessionId":"` + nativeID + `","cwd":"/tmp/proj"}` + "\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append transcript: write=%v close=%v", writeErr, closeErr)
	}
	model, cmd = h.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	h = stepHome(t, model.(*Home), cmd)
	if gs.input.Value() != keyword || gs.query != keyword || len(gs.results) != 1 || gs.results[0].SessionID != nativeID {
		t.Fatalf("manual refresh did not find the appended turn: query=%q results=%+v", gs.query, gs.results)
	}
	detail, ok := gs.preview[gs.results[0].SessID]
	if !ok || len(gs.previewE) != 0 {
		t.Fatalf("fresh hit has no preview: loaded=%v errors=%v", ok, gs.previewE)
	}
	found := false
	for _, message := range detail.Messages {
		found = found || strings.Contains(message.Text, keyword)
	}
	if !found || !strings.Contains(stripAnsi(h.View()), keyword+" rollout") {
		t.Fatalf("preview retained the old transcript after refresh: %+v", detail.Messages)
	}
}

// matrix/hardened: Bubble Tea may finish the immediate read before ingest.
// Both cached and late previews of that older snapshot must yield to the
// detail read after a committed refresh, even within the same manual cycle.
func TestMatrixRecallRefresh_PreviewFollowsCommittedProgress(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "cached_before_commit"
		if late {
			name = "arrives_after_commit"
		}
		t.Run(name, func(t *testing.T) {
			src := newStubRecall()
			src.hits = src.hits[:1]
			gs := NewGlobalSearch()
			gs.SetSource(src)
			gs.Show()
			t.Cleanup(gs.Hide)
			gs.input.SetValue("clock")
			gs.query = "clock"
			gs.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
			_, previewCmd := gs.Update(gs.searchCmd(gs.query)())
			if previewCmd == nil {
				t.Fatal("the immediate search did not request its preview")
			}
			oldPreview := previewCmd()
			if !late {
				gs.Update(oldPreview)
			}
			fresh := src.detail[41]
			fresh.Messages = append(append([]query.Message(nil), fresh.Messages...), query.Message{Seq: 3, Role: "user", Text: "newly appended turn"})
			src.detail[41] = fresh
			_, cmd := gs.Update(recallRefreshMsg{epoch: gs.epoch, res: ingest.Result{Parsed: 1}})
			drain(gs, cmd)
			if late {
				gs.Update(oldPreview)
			}
			if got := gs.preview[41]; len(got.Messages) != 3 || got.Messages[2].Text != "newly appended turn" {
				t.Fatalf("committed progress retained an old preview: %+v", got.Messages)
			}
		})
	}
}

type matrixRecallContextCall struct {
	operation string
	deadline  time.Time
	hasLimit  bool
}

// matrix/hardened: observe actual source arguments instead of testing only the
// overlay's parent context; every database operation needs a bounded child.
type matrixRecallContextSource struct {
	*stubRecall
	calls []matrixRecallContextCall
}

func (s *matrixRecallContextSource) record(operation string, ctx context.Context) {
	deadline, ok := ctx.Deadline()
	s.calls = append(s.calls, matrixRecallContextCall{operation, deadline, ok})
}

func (s *matrixRecallContextSource) Search(ctx context.Context, q string, limit int) (query.SearchResult, error) {
	s.record("search", ctx)
	return s.stubRecall.Search(ctx, q, limit)
}

func (s *matrixRecallContextSource) SearchManaged(ctx context.Context, q string, limit int, scope query.ManagedScope) (query.SearchResult, error) {
	s.record("managed search", ctx)
	return s.stubRecall.SearchManaged(ctx, q, limit, scope)
}

func (s *matrixRecallContextSource) Show(ctx context.Context, id int64, turns int) (query.Detail, error) {
	s.record("preview", ctx)
	return s.stubRecall.Show(ctx, id, turns)
}

func (s *matrixRecallContextSource) Status(ctx context.Context) (query.Status, error) {
	s.record("status", ctx)
	return s.stubRecall.Status(ctx)
}

func (s *matrixRecallContextSource) Refresh(ctx context.Context, gated bool) (ingest.Result, error) {
	s.record("refresh", ctx)
	return s.stubRecall.Refresh(ctx, gated)
}

func TestMatrixRecallRefresh_SourceRequestsHaveDeadlines(t *testing.T) {
	src := &matrixRecallContextSource{stubRecall: newStubRecall()}
	gs := NewGlobalSearch()
	gs.SetSource(src)
	t.Cleanup(gs.Hide)
	start := time.Now()
	drain(gs, gs.Show())
	gs.input.SetValue("clock")
	gs.query = "clock"
	drain(gs, gs.searchCmd(gs.query))
	drain(gs, gs.ShowManaged(query.ManagedScope{DeckIDs: []string{"auth-fix"}}))
	gs.input.SetValue("clock")
	gs.query = "clock"
	drain(gs, gs.searchCmd(gs.query))
	seen := map[string]bool{}
	for _, call := range src.calls {
		seen[call.operation] = true
		if !call.hasLimit || !call.deadline.After(start) || call.deadline.After(start.Add(15*time.Second)) {
			t.Fatalf("%s request has no bounded deadline: %+v", call.operation, call)
		}
	}
	for _, operation := range []string{"status", "refresh", "search", "managed search", "preview"} {
		if !seen[operation] {
			t.Fatalf("source operation %q was never exercised: %+v", operation, src.calls)
		}
	}
}

type matrixRecallBlockingSearch struct {
	*stubRecall
	started chan context.Context
}

func (s *matrixRecallBlockingSearch) Search(ctx context.Context, _ string, _ int) (query.SearchResult, error) {
	s.started <- ctx
	<-ctx.Done()
	return query.SearchResult{}, ctx.Err()
}

// matrix/hardened: a stuck read must release promptly on both manual retry and
// closing the overlay, and its late error must not replace the new search.
func TestMatrixRecallRefresh_CancelsInFlightRequests(t *testing.T) {
	src := &matrixRecallBlockingSearch{stubRecall: newStubRecall(), started: make(chan context.Context, 1)}
	gs := NewGlobalSearch()
	gs.SetSource(src)
	gs.Show()
	t.Cleanup(gs.Hide)
	gs.input.SetValue("clock")
	gs.query = "clock"
	gs.results = []*GlobalSearchResult{{SessID: 99}}
	startSearch := func() <-chan tea.Msg {
		t.Helper()
		cmd := gs.searchCmd(gs.query)
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		select {
		case ctx := <-src.started:
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("in-flight search has no timeout")
			}
		case <-time.After(time.Second):
			t.Fatal("search did not reach the source")
		}
		return done
	}
	awaitCancellation := func(done <-chan tea.Msg) tea.Msg {
		t.Helper()
		select {
		case msg := <-done:
			result, ok := msg.(globalSearchResultsMsg)
			if !ok || !errors.Is(result.err, context.Canceled) {
				t.Fatalf("abandoned read returned %#v, want cancellation", msg)
			}
			return msg
		case <-time.After(time.Second):
			t.Fatal("abandoned source request was not canceled")
			return nil
		}
	}
	old := startSearch()
	gs.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	late := awaitCancellation(old)
	if _, cmd := gs.Update(late); cmd != nil || !gs.searching || gs.searchErr != "" || len(gs.results) != 1 || gs.results[0].SessID != 99 {
		t.Fatal("the canceled search response replaced the current retry state")
	}
	current := startSearch()
	gs.Hide()
	awaitCancellation(current)
}
