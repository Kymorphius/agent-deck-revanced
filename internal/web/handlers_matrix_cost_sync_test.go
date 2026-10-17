package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/costs"
	"golang.org/x/time/rate"
)

// matrix/hardened: exercise the mounted route and its real Host/CSRF/auth/write
// gates; a synthetic callback cannot access sessions, tmux or private transcripts.
func matrixCostSyncServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.MenuData = &fakeMenuDataLoader{snapshot: &MenuSnapshot{}}
	srv := NewServer(cfg)
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown cost sync fixture: %v", err)
		}
	})
	return srv
}

func matrixCostSyncRequest(method, target, token string) *http.Request {
	req := newLocalRequest(method, target, nil)
	req.Header.Set("Origin", "http://localhost")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestMatrixCostSyncOnlyExplicitPostAndCounts(t *testing.T) {
	srv := matrixCostSyncServer(t, Config{Token: "cost-token", WebMutations: true})
	var calls atomic.Int32
	srv.SetCostSync(func(context.Context) (*costs.SyncResult, error) {
		calls.Add(1)
		return &costs.SyncResult{SessionsScanned: 7, EventsImported: 11, EventsSkipped: 5,
			Errors: []string{"scan /private/transcripts/secret.jsonl", "private-session-id"}}, nil
	})
	// Read queries and the sync GET itself must never trigger transcript work.
	for _, path := range []string{"/api/costs/sync", "/api/costs/summary", "/api/costs/batch?ids=one"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, matrixCostSyncRequest(http.MethodGet, path, "cost-token"))
		if path == "/api/costs/sync" && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET sync = %d, body=%s", rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("GET query triggered synchronization")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, matrixCostSyncRequest(http.MethodPost, "/api/costs/sync", "cost-token"))
	if rec.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("explicit sync: status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
	var response map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 4 || response["sessions_scanned"] != 7 || response["events_imported"] != 11 || response["events_skipped"] != 5 || response["error_count"] != 2 {
		t.Fatalf("count-only response: %+v", response)
	}
	if strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("importer errors leaked through aggregate response")
	}
}

func TestMatrixCostSyncAuthorizationAndWriteGatesBeforeCallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		mutate func(*Server, *http.Request)
		status int
	}{
		{"unauthorized", Config{Token: "cost-token", WebMutations: true}, func(_ *Server, r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		{"query-token", Config{Token: "cost-token", WebMutations: true}, func(_ *Server, r *http.Request) { r.Header.Del("Authorization"); r.URL.RawQuery = "token=cost-token" }, http.StatusUnauthorized},
		{"read-only", Config{Token: "cost-token", WebMutations: true, ReadOnly: true}, nil, http.StatusForbidden},
		{"mutations-disabled", Config{Token: "cost-token"}, nil, http.StatusForbidden},
		{"cross-origin", Config{Token: "cost-token", WebMutations: true}, func(_ *Server, r *http.Request) { r.Header.Set("Origin", "http://foreign.example") }, http.StatusForbidden},
		{"missing-origin", Config{Token: "cost-token", WebMutations: true}, func(_ *Server, r *http.Request) { r.Header.Del("Origin") }, http.StatusForbidden},
		{"cross-referer", Config{Token: "cost-token", WebMutations: true}, func(_ *Server, r *http.Request) {
			r.Header.Del("Origin")
			r.Header.Set("Referer", "http://foreign.example/costs")
		}, http.StatusForbidden},
		{"wrong-host", Config{Token: "cost-token", WebMutations: true}, func(_ *Server, r *http.Request) {
			r.Host = "foreign.example"
			r.Header.Set("Origin", "http://foreign.example")
		}, http.StatusMisdirectedRequest},
		{"rate-limit", Config{Token: "cost-token", WebMutations: true}, func(s *Server, _ *http.Request) { s.mutationLimiter = rate.NewLimiter(0, 0) }, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := matrixCostSyncServer(t, tc.cfg)
			var calls atomic.Int32
			srv.SetCostSync(func(context.Context) (*costs.SyncResult, error) {
				calls.Add(1)
				return &costs.SyncResult{}, nil
			})
			req := matrixCostSyncRequest(http.MethodPost, "/api/costs/sync", "cost-token")
			if tc.mutate != nil {
				tc.mutate(srv, req)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status || calls.Load() != 0 || srv.costSyncRunning.Load() {
				t.Fatalf("write gate: status=%d want=%d calls=%d running=%t body=%s", rec.Code, tc.status, calls.Load(), srv.costSyncRunning.Load(), rec.Body.String())
			}
		})
	}
}

func TestMatrixCostSyncCookieAndRefererUseExistingAuthBoundary(t *testing.T) {
	srv := matrixCostSyncServer(t, Config{Token: "cost-token", WebMutations: true})
	var calls atomic.Int32
	srv.SetCostSync(func(context.Context) (*costs.SyncResult, error) { calls.Add(1); return &costs.SyncResult{}, nil })
	req := matrixCostSyncRequest(http.MethodPost, "/api/costs/sync", "")
	req.AddCookie(&http.Cookie{Name: "agentdeck_token", Value: "cost-token"})
	req.Header.Del("Origin")
	req.Header.Set("Referer", "http://localhost/costs")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("cookie/same-origin referer: status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
}

func TestMatrixCostSyncUnavailableAndFailureArePrivateAndRetryable(t *testing.T) {
	srv := matrixCostSyncServer(t, Config{WebMutations: true})
	for _, tc := range []struct {
		name   string
		set    bool
		result *costs.SyncResult
		err    error
		status int
	}{
		{"not-injected", false, nil, nil, http.StatusServiceUnavailable},
		{"callback-error", true, &costs.SyncResult{Errors: []string{"/private/path"}}, errors.New("scan /private/path: failure"), http.StatusInternalServerError},
		{"nil-result", true, nil, nil, http.StatusInternalServerError},
		{"success-after-error", true, &costs.SyncResult{}, nil, http.StatusOK},
		{"disabled-again", false, nil, nil, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				srv.SetCostSync(func(context.Context) (*costs.SyncResult, error) { return tc.result, tc.err })
			} else {
				srv.SetCostSync(nil)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, matrixCostSyncRequest(http.MethodPost, "/api/costs/sync", ""))
			if rec.Code != tc.status || srv.costSyncRunning.Load() || strings.Contains(rec.Body.String(), "/private/path") {
				t.Fatalf("private retryable sync: status=%d want=%d running=%t body=%s", rec.Code, tc.status, srv.costSyncRunning.Load(), rec.Body.String())
			}
		})
	}
}

func TestMatrixCostSyncCancellationRetainsSingleFlightUntilReturn(t *testing.T) {
	srv := matrixCostSyncServer(t, Config{WebMutations: true})
	started, canceled, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv.SetCostSync(func(ctx context.Context) (*costs.SyncResult, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		close(canceled)
		// Model an importer that notices cancellation but cannot release its
		// resources until its currently executing file/read operation returns.
		<-release
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := matrixCostSyncRequest(http.MethodPost, "/api/costs/sync", "").WithContext(ctx)
	first := httptest.NewRecorder()
	go func() { defer close(finished); srv.Handler().ServeHTTP(first, req) }()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() {
		cancel()
		unblock()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("canceled sync handler did not join")
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first sync did not start")
	}
	assertConflict := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, matrixCostSyncRequest(http.MethodPost, "/api/costs/sync", ""))
		if rec.Code != http.StatusConflict || calls.Load() != 1 {
			t.Fatalf("duplicate sync = %d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
		}
	}
	assertConflict()
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not reach callback")
	}
	assertConflict()
	if !srv.costSyncRunning.Load() {
		t.Fatal("cancellation released single-flight before callback returned")
	}
	unblock()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("callback return did not release handler")
	}
	if first.Code != http.StatusInternalServerError || srv.costSyncRunning.Load() {
		t.Fatalf("cancellation completion: status=%d running=%t", first.Code, srv.costSyncRunning.Load())
	}
	srv.SetCostSync(func(context.Context) (*costs.SyncResult, error) {
		calls.Add(1)
		return &costs.SyncResult{}, nil
	})
	retry := httptest.NewRecorder()
	srv.Handler().ServeHTTP(retry, matrixCostSyncRequest(http.MethodPost, "/api/costs/sync", ""))
	if retry.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("sync retry after cancellation: status=%d calls=%d", retry.Code, calls.Load())
	}
}
