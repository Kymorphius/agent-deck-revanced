package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: use immutable source replacement with atomic call counts so
// concurrent REST/stream collection is verified without races in the fixture.
type matrixPublisherIntegrationSource struct {
	mu       sync.Mutex
	snapshot *MenuSnapshot
	err      error
	calls    atomic.Int32
}

func (s *matrixPublisherIntegrationSource) LoadMenuSnapshot() (*MenuSnapshot, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot, s.err
}

func (s *matrixPublisherIntegrationSource) replace(snapshot *MenuSnapshot, err error) {
	s.mu.Lock()
	s.snapshot, s.err = snapshot, err
	s.mu.Unlock()
}

// matrix/hardened: an error status is intentionally overlaid by a waiting hook,
// making duplicate or missing overlays visible on every successful endpoint.
func matrixPublisherIntegrationSnapshot(title string, status session.Status, tool string) *MenuSnapshot {
	return &MenuSnapshot{
		Profile: "private-publisher-profile", TotalGroups: 1, TotalSessions: 1,
		Items: []MenuItem{
			{Type: MenuItemTypeGroup, Group: &MenuGroup{Name: "work", Path: "work"}},
			{Type: MenuItemTypeSession, Path: "work", Session: &MenuSession{
				ID: "private-publisher-session", Title: title, Status: status, Tool: tool, GroupPath: "work",
			}},
		},
	}
}

// matrix/hardened: fix the publisher period before creating any demand; all
// counters measure real production overlay wiring, independent of test speed.
func matrixPublisherIntegrationServer(t *testing.T, source *matrixPublisherIntegrationSource, token string) (*Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	srv := NewServer(Config{ListenAddr: "127.0.0.1:0", Profile: "private-publisher-profile", MenuData: source, Token: token})
	srv.menuPublisher.mu.Lock()
	srv.menuPublisher.ttl = time.Hour
	srv.menuPublisher.mu.Unlock()
	hooks, annotations := &atomic.Int32{}, &atomic.Int32{}
	srv.hookStatusLoader = func() map[string]*session.HookStatus {
		hooks.Add(1)
		return map[string]*session.HookStatus{
			"private-publisher-session": {Status: "waiting", Event: "Stop", UpdatedAt: time.Now()},
		}
	}
	srv.annotationLoader = func(string) (map[string]*statedb.InstanceAnnotations, error) {
		annotations.Add(1)
		return map[string]*statedb.InstanceAnnotations{
			"private-publisher-session": {Hints: map[string]string{"headline": "shared annotation"}, Tags: []string{"shared"}},
		}, nil
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown publisher fixture: %v", err)
		}
		srv.annotationReader.close()
	})
	return srv, hooks, annotations
}

// matrix/hardened: issue the actual handler routes in the same request batch;
// each response is isolated while the expensive menu source must be shared.
func matrixPublisherIntegrationRESTBatch(t *testing.T, srv *Server) map[string]string {
	t.Helper()
	paths := []string{"/api/menu", "/api/sessions", "/api/groups", "/api/command-center/status"}
	type result struct {
		path, body string
		status     int
	}
	results := make(chan result, len(paths))
	start := make(chan struct{})
	for _, path := range paths {
		go func() {
			<-start
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, newLocalRequest(http.MethodGet, path, nil))
			results <- result{path: path, body: w.Body.String(), status: w.Code}
		}()
	}
	close(start)
	bodies := make(map[string]string, len(paths))
	for range paths {
		select {
		case got := <-results:
			if got.status != http.StatusOK {
				t.Fatalf("GET %s: status=%d body=%s", got.path, got.status, got.body)
			}
			if !json.Valid([]byte(got.body)) {
				t.Fatalf("GET %s returned invalid JSON: %s", got.path, got.body)
			}
			bodies[got.path] = got.body
		case <-time.After(2 * time.Second):
			t.Fatal("shared REST request batch did not complete")
		}
	}
	return bodies
}

// matrix/hardened: validate the resulting overlays, not only call counts, so
// route-specific regressions cannot pass by returning an incomplete snapshot.
func matrixPublisherIntegrationAssertREST(t *testing.T, bodies map[string]string, title string) {
	t.Helper()
	var menu MenuSnapshot
	if err := json.Unmarshal([]byte(bodies["/api/menu"]), &menu); err != nil {
		t.Fatal(err)
	}
	var sessions struct {
		Sessions []*MenuSession `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(bodies["/api/sessions"]), &sessions); err != nil {
		t.Fatal(err)
	}
	if len(menu.Items) != 2 || menu.Items[1].Session == nil || len(sessions.Sessions) != 1 {
		t.Fatalf("unexpected menu/session results: %+v, %+v", menu, sessions)
	}
	for _, got := range []*MenuSession{menu.Items[1].Session, sessions.Sessions[0]} {
		if got.Title != title || got.Status != session.StatusWaiting || got.Hints["headline"] != "shared annotation" || len(got.Tags) != 1 || got.Tags[0] != "shared" {
			t.Fatalf("shared overlays not present: %+v", got)
		}
	}
	var groups groupsListResponse
	if err := json.Unmarshal([]byte(bodies["/api/groups"]), &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 1 || groups.Groups[0].Path != "work" {
		t.Fatalf("group route result: %+v", groups)
	}
	var cc CommandCenterSnapshot
	if err := json.Unmarshal([]byte(bodies["/api/command-center/status"]), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Totals.Waiting != 1 || len(cc.Conductors) != 1 || len(cc.Conductors[0].Sessions) != 1 || cc.Conductors[0].Sessions[0].Title != title {
		t.Fatalf("command center did not use overlaid shared publication: %+v", cc)
	}
}

// matrix/hardened: 1, 4 and 16 tabs do the same source/hook/annotation work for
// each publication, including the mutation-triggered refresh before TTL expiry.
func TestMatrixMenuPublisherIntegrationRESTFanoutAndMutation(t *testing.T) {
	for _, subscriberCount := range []int{1, 4, 16} {
		t.Run(fmt.Sprintf("subscribers-%d", subscriberCount), func(t *testing.T) {
			initial := matrixPublisherIntegrationSnapshot("before", session.StatusError, "claude")
			source := &matrixPublisherIntegrationSource{snapshot: initial}
			srv, hooks, annotations := matrixPublisherIntegrationServer(t, source, "")
			streams := make([]<-chan menuPublication, subscriberCount)
			for i := range streams {
				updates, unsubscribe := srv.menuPublisher.Subscribe(time.Hour, nil)
				t.Cleanup(unsubscribe)
				streams[i] = updates
			}
			bodies := matrixPublisherIntegrationRESTBatch(t, srv)
			matrixPublisherIntegrationAssertREST(t, bodies, "before")
			for _, updates := range streams {
				publication := matrixPublisherPublication(t, updates)
				if publication.Err != nil || publication.Snapshot == nil || publication.Snapshot.Items[1].Session.Title != "before" || publication.Snapshot.Items[1].Session.Status != session.StatusWaiting {
					t.Fatalf("initial stream did not share overlaid publication: %#v", publication)
				}
			}
			if source.calls.Load() != 1 || hooks.Load() != 1 || annotations.Load() != 1 {
				t.Fatalf("initial calls: source=%d hooks=%d annotations=%d, want 1 each", source.calls.Load(), hooks.Load(), annotations.Load())
			}
			if initial.Items[1].Session.Status != session.StatusError || initial.Items[1].Session.Hints != nil || initial.Items[1].Session.Tags != nil {
				t.Fatal("production overlays mutated the loader-owned source snapshot")
			}
			source.replace(matrixPublisherIntegrationSnapshot("after", session.StatusError, "claude"), nil)
			srv.notifyMenuChanged()
			for _, updates := range streams {
				publication := matrixPublisherPublication(t, updates)
				if publication.Err != nil || publication.Snapshot == nil || publication.Snapshot.Items[1].Session.Title != "after" {
					t.Fatalf("mutation did not promptly reach stream: %#v", publication)
				}
			}
			matrixPublisherIntegrationAssertREST(t, matrixPublisherIntegrationRESTBatch(t, srv), "after")
			if source.calls.Load() != 2 || hooks.Load() != 2 || annotations.Load() != 2 {
				t.Fatalf("mutation calls: source=%d hooks=%d annotations=%d, want 2 each", source.calls.Load(), hooks.Load(), annotations.Load())
			}
		})
	}
}

// matrix/hardened: a slow Command Center stream retains completion history
// while its one pending snapshot is replaced and even while the source fails.
func TestMatrixMenuPublisherIntegrationCCSlowConsumerAndRecovery(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("baseline", session.StatusWaiting, "shell")}
	publisher := newMenuSnapshotPublisher(context.Background(), source.LoadMenuSnapshot, time.Hour)
	defer publisher.Close()
	first := &ccPublicationTracker{}
	updates, release := publisher.Subscribe(time.Hour, first.observe)
	defer release()
	matrixPublisherProfile(t, updates)
	if pending, err := first.drain(); err != nil || len(pending) != 0 {
		t.Fatalf("initial waiting snapshot invented a completion: %+v, %v", pending, err)
	}
	change := func(title string, status session.Status) {
		t.Helper()
		source.replace(matrixPublisherIntegrationSnapshot(title, status, "shell"), nil)
		publisher.Invalidate()
		if snapshot, err := publisher.LoadMenuSnapshot(); err != nil || snapshot.Items[1].Session.Status != status {
			t.Fatalf("change to %q: %#v, %v", status, snapshot, err)
		}
	}
	change("started", session.StatusRunning)
	second := &ccPublicationTracker{}
	_, releaseSecond := publisher.Subscribe(time.Hour, second.observe)
	defer releaseSecond()
	change("finished", session.StatusWaiting)
	change("restarted", session.StatusRunning)
	wantErr := errors.New("private-source-error")
	source.replace(nil, wantErr)
	publisher.Invalidate()
	if snapshot, err := publisher.LoadMenuSnapshot(); !errors.Is(err, wantErr) || snapshot != nil {
		t.Fatalf("failed source load = %#v, %v", snapshot, err)
	}
	publication := matrixPublisherPublication(t, updates)
	if publication.Snapshot != nil || !errors.Is(publication.Err, wantErr) {
		t.Fatalf("stream did not expose source error: %#v", publication)
	}
	if first.overflowed() || second.overflowed() {
		t.Fatal("source failure falsely triggered completion overflow")
	}
	change("recovered", session.StatusRunning)
	for _, tracker := range []*ccPublicationTracker{first, second} {
		pending, err := tracker.drain()
		if err != nil || len(pending) != 1 || pending[0].ID != "private-publisher-session" || pending[0].Title != "finished" || pending[0].Status != "waiting" {
			t.Fatalf("slow stream lost pending completion during later states/error: %+v, %v", pending, err)
		}
		at, err := time.Parse(time.RFC3339, pending[0].At)
		if err != nil {
			t.Fatalf("invalid completion time %q: %v", pending[0].At, err)
		}
		if _, offset := at.Zone(); offset != 0 {
			t.Fatalf("completion timestamp is not UTC: %q", pending[0].At)
		}
		if pending, err := tracker.drain(); err != nil || len(pending) != 0 {
			t.Fatalf("draining one connection did not consume only its own backlog: %+v, %v", pending, err)
		}
	}
	late := &ccPublicationTracker{}
	_, releaseLate := publisher.Subscribe(time.Hour, late.observe)
	defer releaseLate()
	if pending, err := late.drain(); err != nil || len(pending) != 0 {
		t.Fatalf("new connection inherited an older connection's history: %+v, %v", pending, err)
	}
}

// matrix/hardened: exactly 512 pending completions are deliverable; overflow is
// explicit on the 513th transition and never silently truncates into success.
func TestMatrixMenuPublisherIntegrationCCCompletionOverflowBoundary(t *testing.T) {
	for _, count := range []int{512, 513} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			tracker := &ccPublicationTracker{}
			running, completed := &MenuSnapshot{}, &MenuSnapshot{}
			for i := range count {
				id := fmt.Sprintf("worker-%03d", i)
				running.Items = append(running.Items, MenuItem{Type: MenuItemTypeSession, Session: &MenuSession{ID: id, Title: id, Status: session.StatusRunning}})
				completed.Items = append(completed.Items, MenuItem{Type: MenuItemTypeSession, Session: &MenuSession{ID: id, Title: id, Status: session.StatusIdle}})
			}
			tracker.observe(running)
			tracker.observe(running)
			tracker.observe(completed)
			tracker.observe(completed)
			if tracker.overflowed() != (count > 512) {
				t.Fatalf("overflowed=%v for %d transitions", tracker.overflowed(), count)
			}
			pending, err := tracker.drain()
			if count == 512 {
				if err != nil || len(pending) != count {
					t.Fatalf("boundary drain = %d completions, %v", len(pending), err)
				}
				for i, completion := range pending {
					if completion.ID != fmt.Sprintf("worker-%03d", i) || completion.Status != "idle" {
						t.Fatalf("completion %d changed order or state: %+v", i, completion)
					}
				}
			} else {
				if err == nil || pending != nil || !strings.Contains(err.Error(), "requires resync") {
					t.Fatalf("overflow drain silently succeeded/truncated: %+v, %v", pending, err)
				}
				tracker.observe(&MenuSnapshot{})
				if pending, err := tracker.drain(); !tracker.overflowed() || pending != nil || err == nil {
					t.Fatalf("overflow recovered without reconnect: %+v, %v", pending, err)
				}
			}
		})
	}
}

// matrix/hardened: only an observed running-to-waiting/idle edge is a
// completion; vanished session IDs cannot retain a false running baseline.
func TestMatrixMenuPublisherIntegrationCCDoesNotInventCompletions(t *testing.T) {
	tracker := &ccPublicationTracker{}
	for _, status := range []session.Status{session.StatusWaiting, session.StatusWaiting, session.StatusIdle, session.StatusWaiting, session.StatusRunning, session.StatusError, session.StatusRunning, session.StatusStopped} {
		tracker.observe(matrixPublisherIntegrationSnapshot("unchanged", status, "shell"))
	}
	tracker.observe(matrixPublisherIntegrationSnapshot("removed", session.StatusRunning, "shell"))
	tracker.observe(&MenuSnapshot{Items: []MenuItem{{Type: MenuItemTypeGroup}, {Type: MenuItemTypeSession}}})
	tracker.observe(matrixPublisherIntegrationSnapshot("reused-id", session.StatusWaiting, "shell"))
	if pending, err := tracker.drain(); err != nil || len(pending) != 0 {
		t.Fatalf("tracker invented a completion without an observed edge: %+v, %v", pending, err)
	}
}

// matrix/hardened: diagnostics are authenticated metadata only and never cause
// collection; rejecting a request must happen before touching private sources.
func TestMatrixMenuPublisherIntegrationDiagnosticsAuthAndSafeCounters(t *testing.T) {
	const token = "private-diagnostic-token"
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("private-diagnostic-title", session.StatusRunning, "shell")}
	srv, hooks, annotations := matrixPublisherIntegrationServer(t, source, token)
	for _, request := range []struct {
		method, target, bearer string
		wantStatus             int
	}{
		{http.MethodGet, "/api/diagnostics/menu-publisher", "", http.StatusUnauthorized},
		{http.MethodGet, "/api/diagnostics/menu-publisher?token=" + token, "", http.StatusUnauthorized},
		{http.MethodGet, "/api/diagnostics/menu-publisher", "wrong-token", http.StatusUnauthorized},
		{http.MethodPost, "/api/diagnostics/menu-publisher", token, http.StatusMethodNotAllowed},
	} {
		req := newLocalRequest(request.method, request.target, nil)
		if request.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+request.bearer)
		}
		req.Header.Set("Origin", "http://"+req.Host)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != request.wantStatus {
			t.Fatalf("%s %s: status=%d body=%s, want %d", request.method, request.target, w.Code, w.Body.String(), request.wantStatus)
		}
	}
	if source.calls.Load() != 0 || hooks.Load() != 0 || annotations.Load() != 0 {
		t.Fatalf("rejected diagnostics caused source work: %d/%d/%d", source.calls.Load(), hooks.Load(), annotations.Load())
	}
	readDiagnostics := func() map[string]any {
		t.Helper()
		req := newLocalRequest(http.MethodGet, "/api/diagnostics/menu-publisher", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("diagnostics status=%d body=%s", w.Code, w.Body.String())
		}
		var counters map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &counters); err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{"revision": true, "subscribers": true, "refreshCount": true, "errorCount": true, "running": true, "loading": true, "dirty": true, "hasError": true, "pushOverflowCount": true, "commandCenterBuildCount": true}
		if len(counters) != len(allowed) {
			t.Fatalf("diagnostics exposed unexpected fields: %+v", counters)
		}
		for key, value := range counters {
			if !allowed[key] {
				t.Fatalf("diagnostics exposed private/unexpected field %q", key)
			}
			switch value.(type) {
			case float64, bool:
			default:
				t.Fatalf("diagnostic %q has non-counter metadata: %T", key, value)
			}
		}
		for _, secret := range []string{token, "private-publisher-profile", "private-publisher-session", "private-diagnostic-title", "shared annotation", "private-diagnostic-error"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("diagnostics disclosed fixture secret %q", secret)
			}
		}
		return counters
	}
	initial := readDiagnostics()
	if initial["refreshCount"] != float64(0) || initial["revision"] != float64(0) || initial["subscribers"] != float64(0) || initial["commandCenterBuildCount"] != float64(0) || source.calls.Load() != 0 {
		t.Fatalf("idle diagnostics triggered or invented work: %+v, loads=%d", initial, source.calls.Load())
	}
	updates, release := srv.menuPublisher.Subscribe(time.Hour, nil)
	defer release()
	matrixPublisherProfile(t, updates)
	active := readDiagnostics()
	if active["refreshCount"] != float64(1) || active["revision"] != float64(1) || active["subscribers"] != float64(1) || active["running"] != true || active["dirty"] != false || active["hasError"] != false || source.calls.Load() != 1 {
		t.Fatalf("active diagnostics counters disagree with collection: %+v, loads=%d", active, source.calls.Load())
	}
	// matrix/hardened: count only the shared derived build, not every reader
	// asking for Command Center data or the menu's independent source refresh.
	for range 2 {
		if _, err := srv.loadCommandCenterSnapshot(nil); err != nil {
			t.Fatalf("shared diagnostic Command Center build: %v", err)
		}
	}
	derived := readDiagnostics()
	if active["commandCenterBuildCount"] != float64(0) || derived["commandCenterBuildCount"] != float64(1) || source.calls.Load() != 1 {
		t.Fatalf("derived build counter counted readers or triggered another source load: %+v, loads=%d", derived, source.calls.Load())
	}
	wantErr := errors.New("private-diagnostic-error")
	source.replace(nil, wantErr)
	srv.notifyMenuChanged()
	if snapshot, err := srv.menuPublisher.LoadMenuSnapshot(); snapshot != nil || !errors.Is(err, wantErr) {
		t.Fatalf("diagnostic source-error fixture = %#v, %v", snapshot, err)
	}
	failed := readDiagnostics()
	if failed["refreshCount"] != float64(2) || failed["revision"] != float64(2) || failed["errorCount"] != float64(1) || failed["hasError"] != true || failed["pushOverflowCount"] != float64(0) || failed["commandCenterBuildCount"] != float64(1) || source.calls.Load() != 2 || hooks.Load() != 1 || annotations.Load() != 1 {
		t.Fatalf("failed diagnostics counters disagree with collection: %+v, calls=%d/%d/%d", failed, source.calls.Load(), hooks.Load(), annotations.Load())
	}
}

// matrix/hardened: actual menu and Command Center HTTP streams share their
// initial collection and both receive mutation invalidation without TTL expiry.
func TestMatrixMenuPublisherIntegrationHTTPStreamsShareInitialAndChange(t *testing.T) {
	originalMenuInterval, originalCCInterval := menuEventsPollInterval, commandCenterPollInterval
	menuEventsPollInterval, commandCenterPollInterval = time.Hour, time.Hour
	defer func() { menuEventsPollInterval, commandCenterPollInterval = originalMenuInterval, originalCCInterval }()
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("waiting-first", session.StatusWaiting, "shell")}
	srv, hooks, annotations := matrixPublisherIntegrationServer(t, source, "")
	ts := httptest.NewServer(srv.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer ts.Close()
	defer cancel()
	type stream struct {
		name   string
		reader *bufio.Reader
	}
	streams := make([]stream, 0, 2)
	for _, entry := range []struct{ path, event string }{{"/events/menu", "menu"}, {"/events/command-center", "command-center"}} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+entry.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open %s: %v", entry.path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("open %s: status=%d headers=%v", entry.path, resp.StatusCode, resp.Header)
		}
		reader := bufio.NewReader(resp.Body)
		event, payload, err := readSSEEvent(reader)
		if err != nil || event != entry.event || !strings.Contains(payload, "waiting-first") {
			t.Fatalf("initial %s event=%q payload=%s error=%v", entry.path, event, payload, err)
		}
		if entry.event == "command-center" {
			var initial CommandCenterSnapshot
			if err := json.Unmarshal([]byte(payload), &initial); err != nil || len(initial.RecentlyCompleted) != 0 {
				t.Fatalf("fresh Command Center falsely notified old waiting state: %+v, %v", initial, err)
			}
		}
		streams = append(streams, stream{name: entry.event, reader: reader})
	}
	if source.calls.Load() != 1 || hooks.Load() != 1 || annotations.Load() != 1 {
		t.Fatalf("HTTP streams repeated initial work: %d/%d/%d", source.calls.Load(), hooks.Load(), annotations.Load())
	}
	source.replace(matrixPublisherIntegrationSnapshot("now-running", session.StatusRunning, "shell"), nil)
	srv.notifyMenuChanged()
	for _, stream := range streams {
		event, payload, err := readSSEEvent(stream.reader)
		if err != nil || event != stream.name || !strings.Contains(payload, "now-running") {
			t.Fatalf("mutation %s event=%q payload=%s error=%v", stream.name, event, payload, err)
		}
	}
	if source.calls.Load() != 2 || hooks.Load() != 2 || annotations.Load() != 2 {
		t.Fatalf("HTTP stream mutation repeated shared work: %d/%d/%d", source.calls.Load(), hooks.Load(), annotations.Load())
	}
}
