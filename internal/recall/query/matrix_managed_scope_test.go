package query

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/recall"
	"github.com/asheshgoplani/agent-deck/internal/recall/store"
)

// matrix/hardened: membership must constrain SQL candidates before either cap.
func matrixManagedStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "recall.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// Seed the real store and FTS surfaces directly so message row order,
// card-only hits and identical native IDs across harnesses are exact.
func matrixManagedSession(t *testing.T, st *store.Store, harness, native, deck, title string, bodies ...string) int64 {
	t.Helper()
	tx, err := st.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	r, err := tx.Exec(`INSERT INTO session(host_uid,harness,native_id,deck_id,title,ended_at)
		VALUES (?,?,?,?,?,?)`, store.LocalHostUID, harness, native, deck, title, 100)
	if err != nil {
		t.Fatal(err)
	}
	sessID, err := r.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO card(sess_id,title) VALUES (?,?)`, sessID, title); err != nil {
		t.Fatal(err)
	}
	for i, body := range bodies {
		r, err := tx.Exec(`INSERT INTO msg(sess_id,src_id,seq,role,class,ts,rec_off,rec_len,nchars,body)
			VALUES (?,0,?,2,0,?,0,0,?,?)`, sessID, i, 100+sessID, len(body), recall.CompressBody([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		msgID, err := r.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO msg_fts(rowid,body) VALUES (?,?)`, msgID, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return sessID
}

func matrixManagedSearch(t *testing.T, s *Searcher, o SearchOptions) SearchResult {
	t.Helper()
	r, err := s.Search(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMatrixManagedScopeNilAndEmpty(t *testing.T) {
	st := matrixManagedStore(t)
	managed := matrixManagedSession(t, st, "claude", "managed", "", "marker", "marker")
	matrixManagedSession(t, st, "codex", "", "", "marker", "marker")
	s := New(st, "")
	for _, tc := range []struct {
		name  string
		scope *ManagedScope
		want  int
	}{
		{"nil searches all", nil, 2},
		{"empty searches none", &ManagedScope{}, 0},
		{"empty IDs cannot match empty columns", &ManagedScope{DeckIDs: []string{""}, ClaudeIDs: []string{""}, CodexIDs: []string{""}}, 0},
		{"duplicates and empty IDs are ignored", &ManagedScope{ClaudeIDs: []string{"", "managed", "managed", ""}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := matrixManagedSearch(t, s, SearchOptions{Query: "marker", Filters: Filters{Managed: tc.scope}})
			if len(res.Hits) != tc.want || res.Candidates != tc.want {
				t.Fatalf("hits %d candidates %d, want %d: %+v", len(res.Hits), res.Candidates, tc.want, res.Hits)
			}
			if tc.want == 1 && res.Hits[0].SessID != managed {
				t.Fatalf("managed hit: %+v", res.Hits)
			}
			rows, err := s.Sessions(context.Background(), Filters{Managed: tc.scope}, 10)
			if err != nil || len(rows) != tc.want {
				t.Fatalf("shared session filter: %v, %d rows, want %d", err, len(rows), tc.want)
			}
		})
	}
}

func TestMatrixManagedScopeUnionAndHarnessIsolation(t *testing.T) {
	st := matrixManagedStore(t)
	claude := matrixManagedSession(t, st, "claude", "same-cl", "stale-cl", "marker", "marker")
	codex := matrixManagedSession(t, st, "codex", "same-co", "stale-co", "marker", "marker")
	deck := matrixManagedSession(t, st, "gemini", "deck-only", "managed-deck", "marker", "marker")
	// Values containing SQL and JSON syntax must remain exact bound IDs.
	const quoted = "native' OR 1=1 -- \"\\会话"
	bound := matrixManagedSession(t, st, "claude", quoted, "", "marker", "marker")
	matrixManagedSession(t, st, "codex", "same-cl", "", "marker", "marker")
	matrixManagedSession(t, st, "claude", "same-co", "", "marker", "marker")
	matrixManagedSession(t, st, "gemini", "same-cl", "", "marker", "marker")
	matrixManagedSession(t, st, "claude", "unmanaged", "other-deck", "marker", "marker")
	scope := &ManagedScope{
		DeckIDs:   []string{"", "managed-deck", "managed-deck"},
		ClaudeIDs: []string{"same-cl", quoted, ""},
		CodexIDs:  []string{"same-co"},
	}
	res := matrixManagedSearch(t, New(st, ""), SearchOptions{Query: "marker", Filters: Filters{Managed: scope}})
	want := map[int64]bool{claude: true, codex: true, deck: true, bound: true}
	if len(res.Hits) != len(want) || res.Candidates != len(want) {
		t.Fatalf("union: %+v", res)
	}
	for _, h := range res.Hits {
		if !want[h.SessID] {
			t.Fatalf("wrong harness or unbound value matched: %+v", h)
		}
		delete(want, h.SessID)
	}
	if len(want) != 0 {
		t.Fatalf("missing union sessions: %v", want)
	}
}

func TestMatrixManagedScopeFiltersBeforeRankingLimit(t *testing.T) {
	st := matrixManagedStore(t)
	managedA := matrixManagedSession(t, st, "claude", "managed-a", "", "", "marker")
	managedB := matrixManagedSession(t, st, "claude", "managed-b", "", "", "marker")
	// More card hits than the result limit outrank both managed body hits.
	for i := range 12 {
		matrixManagedSession(t, st, "claude", fmt.Sprintf("unmanaged-%d", i), "", "marker", "marker", "marker", "marker")
	}
	s := New(st, "")
	all := matrixManagedSearch(t, s, SearchOptions{Query: "marker", Limit: 2})
	if len(all.Hits) != 2 || !all.Hits[0].CardHit || !all.Hits[1].CardHit {
		t.Fatalf("unfiltered ranking fixture: %+v", all.Hits)
	}
	res := matrixManagedSearch(t, s, SearchOptions{Query: "marker", Limit: 2,
		Filters: Filters{Managed: &ManagedScope{ClaudeIDs: []string{"managed-a", "managed-b"}}}})
	if len(res.Hits) != 2 || res.Candidates != 2 {
		t.Fatalf("managed results truncated by higher unmanaged ranks: %+v", res)
	}
	for _, h := range res.Hits {
		if h.SessID != managedA && h.SessID != managedB {
			t.Fatalf("unmanaged ranked hit: %+v", h)
		}
	}
}

func TestMatrixManagedScopeFiltersBeforeBodyCandidateCeiling(t *testing.T) {
	st := matrixManagedStore(t)
	managed := matrixManagedSession(t, st, "codex", "managed", "stale-deck", "", "marker", "marker")
	// These newer rowids consume the entire unfiltered ceiling. There are
	// no card hits to rescue the older managed session after truncation.
	for i := range 8 {
		matrixManagedSession(t, st, "claude", fmt.Sprintf("unmanaged-%d", i), "", "", "marker")
	}
	s := New(st, "")
	all := matrixManagedSearch(t, s, SearchOptions{Query: "marker", Ceiling: 3, Limit: 20})
	if !all.CeilingHit || all.Candidates != 3 {
		t.Fatalf("unfiltered ceiling fixture: %+v", all)
	}
	for _, h := range all.Hits {
		if h.SessID == managed {
			t.Fatalf("managed row unexpectedly inside unfiltered ceiling: %+v", all)
		}
	}
	res := matrixManagedSearch(t, s, SearchOptions{Query: "marker", Ceiling: 3, Limit: 20,
		Filters: Filters{Managed: &ManagedScope{CodexIDs: []string{"managed"}}}})
	if res.CeilingHit || res.Candidates != 2 || len(res.Hits) != 1 || res.Hits[0].SessID != managed || res.Hits[0].BodyHits != 2 {
		t.Fatalf("managed filter applied after body ceiling: %+v", res)
	}
}

func TestMatrixManagedScopeExcludesUnmanagedCardOnlyHits(t *testing.T) {
	st := matrixManagedStore(t)
	managed := matrixManagedSession(t, st, "claude", "managed", "deck", "marker")
	matrixManagedSession(t, st, "codex", "unmanaged", "", "marker")
	res := matrixManagedSearch(t, New(st, ""), SearchOptions{Query: "marker",
		Filters: Filters{Managed: &ManagedScope{DeckIDs: []string{"deck"}}}})
	if res.Candidates != 0 || len(res.Hits) != 1 || res.Hits[0].SessID != managed || !res.Hits[0].CardHit || res.Hits[0].BodyHits != 0 {
		t.Fatalf("card-only scope: %+v", res)
	}
}

func TestMatrixManagedScopeLargeIDSets(t *testing.T) {
	st := matrixManagedStore(t)
	managed := matrixManagedSession(t, st, "claude", "managed", "", "marker", "marker")
	matrixManagedSession(t, st, "claude", "unmanaged", "", "marker", "marker")
	// This exceeds SQLite's usual variable limit and would exceed the
	// expression depth with one OR clause per ID. All three sets are large.
	scope := &ManagedScope{}
	for i := range 40000 {
		id := fmt.Sprintf("missing-%d", i)
		scope.DeckIDs = append(scope.DeckIDs, id)
		scope.ClaudeIDs = append(scope.ClaudeIDs, id)
		scope.CodexIDs = append(scope.CodexIDs, id)
	}
	scope.ClaudeIDs = append(scope.ClaudeIDs, "managed", "managed", "")
	res := matrixManagedSearch(t, New(st, ""), SearchOptions{Query: "marker", Filters: Filters{Managed: scope}})
	if len(res.Hits) != 1 || res.Candidates != 1 || res.Hits[0].SessID != managed {
		t.Fatalf("large bound JSON ID sets: %+v", res)
	}
}
