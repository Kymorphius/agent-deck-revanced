package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/gorilla/websocket"
)

// matrix/hardened: stable valid row identities leave enough unchanged content
// for a runtime-only whole-row delta to be smaller than a versioned full menu.
func matrixDashboardDeltaMenu() *MenuSnapshot {
	menu := &MenuSnapshot{Profile: "private-publisher-profile", GeneratedAt: time.Now().UTC(), TotalGroups: 1, TotalSessions: 8}
	menu.Items = append(menu.Items, MenuItem{Index: 0, Type: MenuItemTypeGroup, Group: &MenuGroup{Name: "work", Path: "work", Expanded: true}})
	for index := 1; index <= menu.TotalSessions; index++ {
		menu.Items = append(menu.Items, MenuItem{Index: index, Type: MenuItemTypeSession, Level: 1, Path: "work", Session: &MenuSession{
			ID: fmt.Sprintf("stage52-session-%d", index), Title: fmt.Sprintf("worker-%d", index), Tool: "shell", GroupPath: "work",
			Status: session.StatusRunning, LatestPrompt: strings.Repeat("unchanged prompt ", 30),
			Hints: map[string]string{"remove": "old", "keep": "same"}, Tags: []string{"old"},
		}})
	}
	return menu
}

type matrixDashboardMenuEnvelope struct {
	Type        string          `json:"type"`
	Version     string          `json:"version"`
	BaseVersion string          `json:"baseVersion"`
	Data        json.RawMessage `json:"data"`
	encoded     []byte
}

// matrix/hardened: consume CC/keepalive independently so these menu assertions
// never suppress or gate completion delivery on the menu's content version.
func matrixDashboardReadMenuEnvelope(t *testing.T, conn *websocket.Conn) matrixDashboardMenuEnvelope {
	t.Helper()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		kind, payload, err := conn.ReadMessage()
		if err != nil || kind != websocket.TextMessage {
			t.Fatalf("read dashboard menu envelope: kind=%d error=%v", kind, err)
		}
		var envelope matrixDashboardMenuEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatalf("invalid menu envelope %s: %v", payload, err)
		}
		if envelope.Type == "command-center" || envelope.Type == "heartbeat" {
			continue
		}
		envelope.encoded = payload
		return envelope
	}
}

// matrix/hardened: actual WS opt-in is exact; old/unknown clients retain the
// original bytes. Runtime changes remove nested fields through whole-row delta,
// new connections and identical source recovery always start with a full value.
func TestMatrixDashboardWSDeltaOptInLegacyAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		optIn       bool
	}{
		{"legacy", "", false},
		{"unknown", "?menu_protocol=delta-v2", false},
		{"ambiguous", "?menu_protocol=delta-v1&menu_protocol=legacy", false},
		{"delta", "?menu_protocol=delta-v1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &matrixPublisherIntegrationSource{snapshot: matrixDashboardDeltaMenu()}
			srv, ts, _, _, _ := matrixDashboardFixture(t, source, "")
			conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"+tc.query), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			initial := matrixDashboardReadMenuEnvelope(t, conn)
			if initial.Type != "menu" || (initial.Version != "") != tc.optIn || initial.BaseVersion != "" {
				t.Fatalf("initial opt-in contract: %+v", initial)
			}
			next := matrixDashboardDeltaMenu()
			next.Items[1].Session.Status = session.StatusWaiting
			next.Items[1].Session.ModelID = "new-runtime-model"
			next.Items[1].Session.Hints, next.Items[1].Session.Tags = nil, nil
			source.replace(next, nil)
			srv.notifyMenuChanged()
			changed := matrixDashboardReadMenuEnvelope(t, conn)
			if tc.optIn {
				var data menuDeltaData
				if err := json.Unmarshal(changed.Data, &data); err != nil {
					t.Fatal(err)
				}
				if changed.Type != "menu-delta" || changed.BaseVersion != initial.Version || changed.Version == initial.Version || len(data.Changes) != 1 || data.Changes[0].Index != 1 || data.Changes[0].Item.Session.ModelID != "new-runtime-model" || data.Changes[0].Item.Session.Hints != nil || data.Changes[0].Item.Session.Tags != nil {
					t.Fatalf("runtime delta/deletion contract: %+v, %+v", changed, data)
				}
				var raw struct {
					Changes []struct {
						Item struct {
							Session map[string]json.RawMessage `json:"session"`
						} `json:"item"`
					} `json:"changes"`
				}
				if err := json.Unmarshal(changed.Data, &raw); err != nil {
					t.Fatal(err)
				}
				if _, exists := raw.Changes[0].Item.Session["hints"]; exists {
					t.Fatal("whole-row delta retained deleted hints")
				}
				fresh, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"+tc.query), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer fresh.Close()
				first := matrixDashboardReadMenuEnvelope(t, fresh)
				if first.Type != "menu" || first.Version != changed.Version || first.BaseVersion != "" {
					t.Fatalf("new client received a delta: %+v", first)
				}
			} else {
				srv.menuPublisher.mu.Lock()
				want := srv.menuPublisher.current.dashboardMenu
				srv.menuPublisher.mu.Unlock()
				if changed.Type != "menu" || changed.Version != "" || changed.BaseVersion != "" || !bytes.Equal(changed.encoded, want) {
					t.Fatalf("legacy menu envelope changed: %+v", changed)
				}
			}
			source.replace(nil, errors.New("delta-source-error"))
			srv.notifyMenuChanged()
			if failed := matrixDashboardReadMenuEnvelope(t, conn); failed.Type != "error" {
				t.Fatalf("expected source error: %+v", failed)
			}
			source.replace(next, nil)
			srv.notifyMenuChanged()
			recovered := matrixDashboardReadMenuEnvelope(t, conn)
			if recovered.Type != "menu" || recovered.BaseVersion != "" || recovered.Version != changed.Version {
				t.Fatalf("identical recovery did not send full baseline: %+v", recovered)
			}
		})
	}
}

// matrix/hardened: skipped content generations cannot be patched from a wrong
// base. A slow writer receives current full state and every observed completion.
func TestMatrixDashboardWSDeltaSlowGapUsesFullAndPreservesCompletion(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: matrixDashboardDeltaMenu()}
	gate := &matrixDashboardWriteGate{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	release := func() { gate.releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	srv, ts, _, _, _ := matrixDashboardFixture(t, source, "", func(listener net.Listener) net.Listener {
		return &matrixDashboardGatedListener{Listener: listener, gate: gate}
	})
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard?menu_protocol=delta-v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("initial menu write did not block")
	}
	for _, state := range []struct {
		title  string
		status session.Status
	}{
		{"completed-during-gap", session.StatusWaiting},
		{"restarted-during-gap", session.StatusRunning},
		{"latest-after-gap", session.StatusRunning},
	} {
		menu := matrixDashboardDeltaMenu()
		menu.Items[1].Session.Title, menu.Items[1].Session.Status = state.title, state.status
		source.replace(menu, nil)
		srv.notifyMenuChanged()
		if _, err := srv.menuPublisher.loadWire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	release()
	initial, _ := matrixDashboardReadState(t, conn)
	if initial.Items[1].Session.Title != "worker-1" {
		t.Fatalf("blocked initial bytes lost ownership: %+v", initial)
	}
	// ReadState intentionally rejects menu-delta; this missed-version update
	// must be a full menu even though current has a valid delta from its predecessor.
	latest, cc := matrixDashboardReadState(t, conn)
	if latest.Items[1].Session.Title != "latest-after-gap" || len(cc.RecentlyCompleted) != 1 || cc.RecentlyCompleted[0].Title != "completed-during-gap" {
		t.Fatalf("gap/full/completion contract: %+v, %+v", latest, cc)
	}
}

// matrix/hardened: exercise the real HTTP upgrade and count handler completion
// so socket cleanup is proven without relying on process-wide goroutine counts.
func matrixDashboardFixture(t *testing.T, source *matrixPublisherIntegrationSource, token string, decorate ...func(net.Listener) net.Listener) (*Server, *httptest.Server, *atomic.Int32, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	originalInterval := dashboardEventsPollInterval
	dashboardEventsPollInterval = time.Hour
	t.Cleanup(func() { dashboardEventsPollInterval = originalInterval })
	srv, hooks, annotations := matrixPublisherIntegrationServer(t, source, token)
	srv.cfg.ReadOnly = true
	completed := &atomic.Int32{}
	active := &atomic.Int32{}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		defer completed.Add(1)
		srv.Handler().ServeHTTP(w, r)
	}))
	if len(decorate) > 0 {
		ts.Listener = decorate[0](ts.Listener)
	}
	ts.Start()
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown dashboard fixture: %v", err)
		}
		ts.Close()
		matrixDashboardWait(t, func() bool { return active.Load() == 0 }, "fixture dashboard handlers to join before restoring tunables")
	})
	return srv, ts, hooks, annotations, completed
}

// matrix/hardened: deterministically stall just the first socket's first WS
// frame; HTTP 101, write deadlines and close still retain real net.Conn semantics.
type matrixDashboardWriteGate struct {
	entered     chan struct{}
	release     chan struct{}
	closed      chan struct{}
	blocked     atomic.Bool
	closeOnce   sync.Once
	releaseOnce sync.Once
	deadline    atomic.Int64
}

type matrixDashboardGatedListener struct {
	net.Listener
	gate     *matrixDashboardWriteGate
	accepted atomic.Int32
}

func (l *matrixDashboardGatedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil || l.accepted.Add(1) != 1 {
		return conn, err
	}
	return &matrixDashboardGatedConn{Conn: conn, gate: l.gate}, nil
}

type matrixDashboardGatedConn struct {
	net.Conn
	gate *matrixDashboardWriteGate
}

func (c *matrixDashboardGatedConn) SetWriteDeadline(deadline time.Time) error {
	var when int64
	if !deadline.IsZero() {
		when = deadline.UnixNano()
	}
	c.gate.deadline.Store(when)
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *matrixDashboardGatedConn) Write(payload []byte) (int, error) {
	if !strings.HasPrefix(string(payload), "HTTP/") && c.gate.blocked.CompareAndSwap(false, true) {
		close(c.gate.entered)
		var timeout <-chan time.Time
		if when := c.gate.deadline.Load(); when != 0 {
			timer := time.NewTimer(time.Until(time.Unix(0, when)))
			defer timer.Stop()
			timeout = timer.C
		}
		select {
		case <-c.gate.release:
		case <-c.gate.closed:
			return 0, net.ErrClosed
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		}
	}
	return c.Conn.Write(payload)
}

func (c *matrixDashboardGatedConn) Close() error {
	c.gate.closeOnce.Do(func() { close(c.gate.closed) })
	return c.Conn.Close()
}

// matrix/hardened: a blocked network writer does not block collection or a
// second page, and its completion backlog survives coalesced state and errors.
func TestMatrixDashboardWSSlowWriterPreservesCompletionsAndFanout(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("initial-running", session.StatusRunning, "shell")}
	gate := &matrixDashboardWriteGate{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	releaseGate := func() { gate.releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(releaseGate)
	srv, ts, hooks, annotations, completed := matrixDashboardFixture(t, source, "", func(listener net.Listener) net.Listener {
		return &matrixDashboardGatedListener{Listener: listener, gate: gate}
	})
	slow, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
	if err != nil {
		t.Fatalf("slow dashboard upgrade: response=%v error=%v", resp, err)
	}
	defer slow.Close()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("slow dashboard did not enter its blocked WS write")
	}
	fast, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
	if err != nil {
		t.Fatalf("fast dashboard upgrade: response=%v error=%v", resp, err)
	}
	defer fast.Close()
	matrixDashboardReadState(t, fast)
	change := func(title string, status session.Status) {
		t.Helper()
		source.replace(matrixPublisherIntegrationSnapshot(title, status, "shell"), nil)
		srv.notifyMenuChanged()
		menu, cc := matrixDashboardReadState(t, fast)
		if menu.Items[1].Session.Title != title || menu.Items[1].Session.Status != status || len(cc.Conductors) != 1 || cc.Conductors[0].Sessions[0].Title != title {
			t.Fatalf("fast page did not advance during blocked writer: %+v, %+v", menu, cc)
		}
	}
	change("finished-while-blocked", session.StatusWaiting)
	change("restarted-while-blocked", session.StatusRunning)
	source.replace(nil, errors.New("temporary blocked-writer-source-error"))
	srv.notifyMenuChanged()
	_ = fast.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, err := fast.ReadMessage()
	if err != nil {
		t.Fatalf("fast page source-error envelope: %v", err)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Type != "error" {
		t.Fatalf("fast page did not see source failure while slow writer blocked: %s, %v", payload, err)
	}
	change("recovered-while-blocked", session.StatusRunning)
	srv.menuPublisher.mu.Lock()
	revision := srv.menuPublisher.revision
	srv.menuPublisher.mu.Unlock()
	if revision != 5 || source.calls.Load() != 5 || hooks.Load() != 4 || annotations.Load() != 4 || matrixDashboardSubscriberCount(srv) != 2 {
		t.Fatalf("blocked network writer held collector or other page: revision=%d subs=%d loads=%d/%d/%d", revision, matrixDashboardSubscriberCount(srv), source.calls.Load(), hooks.Load(), annotations.Load())
	}
	releaseGate()
	initialMenu, _ := matrixDashboardReadState(t, slow)
	if initialMenu.Items[1].Session.Title != "initial-running" {
		t.Fatalf("released first frame changed ownership: %+v", initialMenu)
	}
	menu, cc := matrixDashboardReadState(t, slow)
	if menu.Items[1].Session.Title != "recovered-while-blocked" || len(cc.RecentlyCompleted) != 1 || cc.RecentlyCompleted[0].Title != "finished-while-blocked" || cc.RecentlyCompleted[0].Status != "waiting" {
		t.Fatalf("slow network page lost completion while states/errors were coalesced: %+v, %+v", menu, cc)
	}
	_ = slow.Close()
	_ = fast.Close()
	matrixDashboardWait(t, func() bool { return completed.Load() == 2 && matrixDashboardSubscriberCount(srv) == 0 }, "both blocked/fast dashboard handlers to join")
}

// matrix/hardened: finite read deadlines make broken pushes fail locally;
// dashboard application data is exclusively JSON text with a typed envelope.
func matrixDashboardReadState(t *testing.T, conn *websocket.Conn) (*MenuSnapshot, *CommandCenterSnapshot) {
	t.Helper()
	var menu *MenuSnapshot
	var cc *CommandCenterSnapshot
	for menu == nil || cc == nil {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read dashboard state: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("dashboard message type=%d, want JSON text", messageType)
		}
		var envelope struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatalf("dashboard envelope=%s: %v", payload, err)
		}
		switch envelope.Type {
		case "menu":
			if menu != nil {
				t.Fatalf("duplicate menu envelope before command-center: %s", payload)
			}
			menu = &MenuSnapshot{}
			if err := json.Unmarshal(envelope.Data, menu); err != nil {
				t.Fatalf("menu envelope=%s: %v", payload, err)
			}
		case "command-center":
			if cc != nil {
				t.Fatalf("duplicate command-center envelope before menu: %s", payload)
			}
			cc = &CommandCenterSnapshot{}
			if err := json.Unmarshal(envelope.Data, cc); err != nil {
				t.Fatalf("command-center envelope=%s: %v", payload, err)
			}
		case "heartbeat":
		default:
			t.Fatalf("unexpected dashboard envelope: %s", payload)
		}
	}
	return menu, cc
}

// matrix/hardened: wait on actual subscription ownership/handler completion,
// allowing only the finite scheduling delay needed for socket teardown.
func matrixDashboardWait(t *testing.T, ready func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// matrix/hardened: diagnostics counters are read under the same publisher
// lock, without creating a source read or a second dashboard subscription.
func matrixDashboardSubscriberCount(srv *Server) int {
	srv.menuPublisher.mu.Lock()
	defer srv.menuPublisher.mu.Unlock()
	return len(srv.menuPublisher.subscribers)
}

// matrix/hardened: unauthorized, invalid-origin and failed-upgrade requests
// never subscribe, read hook/annotation state, or load the private menu source.
func TestMatrixDashboardWSRejectsBeforeCollecting(t *testing.T) {
	const token = "dashboard-private-token"
	for _, test := range []struct {
		name, query, bearer, cookie, origin string
		wantStatus                          int
	}{
		{name: "missing-token", wantStatus: http.StatusUnauthorized},
		{name: "wrong-query", query: "?token=wrong", wantStatus: http.StatusUnauthorized},
		{name: "wrong-header", bearer: "wrong", wantStatus: http.StatusUnauthorized},
		{name: "wrong-cookie", cookie: "agentdeck_token=wrong", wantStatus: http.StatusUnauthorized},
		{name: "cross-origin", bearer: token, origin: "https://untrusted.example", wantStatus: http.StatusForbidden},
		{name: "invalid-origin", bearer: token, origin: ":invalid:", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("private", session.StatusRunning, "shell")}
			srv, ts, hooks, annotations, _ := matrixDashboardFixture(t, source, token)
			headers := http.Header{}
			if test.bearer != "" {
				headers.Set("Authorization", "Bearer "+test.bearer)
			}
			if test.cookie != "" {
				headers.Set("Cookie", test.cookie)
			}
			if test.origin != "" {
				headers.Set("Origin", test.origin)
			}
			conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"+test.query), headers)
			if conn != nil {
				_ = conn.Close()
			}
			if resp != nil {
				defer resp.Body.Close()
			}
			if err == nil || resp == nil || resp.StatusCode != test.wantStatus {
				t.Fatalf("denied upgrade error=%v response=%v, want status %d", err, resp, test.wantStatus)
			}
			srv.menuPublisher.mu.Lock()
			refreshes, running, subscribers := srv.menuPublisher.refreshes, srv.menuPublisher.running, len(srv.menuPublisher.subscribers)
			srv.menuPublisher.mu.Unlock()
			if source.calls.Load() != 0 || hooks.Load() != 0 || annotations.Load() != 0 || refreshes != 0 || running || subscribers != 0 {
				t.Fatalf("denied upgrade touched private source: source/hooks/annotations=%d/%d/%d refreshes=%d running=%v subscribers=%d", source.calls.Load(), hooks.Load(), annotations.Load(), refreshes, running, subscribers)
			}
		})
	}
	// Method and non-WebSocket requests must also fail before creating demand.
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("private", session.StatusRunning, "shell")}
	srv, _, hooks, annotations, _ := matrixDashboardFixture(t, source, token)
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		req := newLocalRequest(method, "/ws/dashboard", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "http://"+req.Host)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		wantStatus := http.StatusBadRequest
		if method == http.MethodPost {
			wantStatus = http.StatusMethodNotAllowed
		}
		if w.Code != wantStatus {
			t.Fatalf("non-upgrade %s status=%d body=%s, want %d", method, w.Code, w.Body.String(), wantStatus)
		}
	}
	if source.calls.Load() != 0 || hooks.Load() != 0 || annotations.Load() != 0 || matrixDashboardSubscriberCount(srv) != 0 {
		t.Fatal("method/failed-upgrade rejection created menu demand")
	}
}

// matrix/hardened: the read-only dashboard retains existing WebSocket
// query/header/cookie auth, allowing same-origin browser and no-origin clients.
func TestMatrixDashboardWSAcceptsExistingCredentialForms(t *testing.T) {
	const token = "dashboard-private-token"
	for _, credential := range []string{"query", "header", "cookie", "header-with-wrong-query"} {
		t.Run(credential, func(t *testing.T) {
			source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("authorized", session.StatusRunning, "shell")}
			srv, ts, hooks, annotations, completed := matrixDashboardFixture(t, source, token)
			headers, query := http.Header{}, ""
			switch credential {
			case "query":
				query = "?token=" + token
				headers.Set("Origin", ts.URL)
			case "header":
				headers.Set("Authorization", "Bearer "+token)
			case "cookie":
				headers.Set("Cookie", "agentdeck_token="+token)
				headers.Set("Origin", ts.URL)
			case "header-with-wrong-query":
				query = "?token=wrong"
				headers.Set("Authorization", "Bearer "+token)
			}
			conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"+query), headers)
			if err != nil {
				t.Fatalf("authorized upgrade: response=%v error=%v", resp, err)
			}
			defer conn.Close()
			menu, cc := matrixDashboardReadState(t, conn)
			if menu.TotalSessions != 1 || menu.Items[1].Session.Title != "authorized" || cc.Totals.Running != 1 {
				t.Fatalf("authorized dashboard data: menu=%+v command-center=%+v", menu, cc)
			}
			if matrixDashboardSubscriberCount(srv) != 1 || source.calls.Load() != 1 || hooks.Load() != 1 || annotations.Load() != 1 {
				t.Fatalf("one dashboard did not share one collection: subscribers=%d calls=%d/%d/%d", matrixDashboardSubscriberCount(srv), source.calls.Load(), hooks.Load(), annotations.Load())
			}
			_ = conn.Close()
			matrixDashboardWait(t, func() bool { return matrixDashboardSubscriberCount(srv) == 0 && completed.Load() == 1 }, "authorized dashboard handler to release its subscription")
		})
	}
}

// matrix/hardened: a single page gets both typed states and its completion
// notifications from one subscription and one source refresh per mutation.
func TestMatrixDashboardWSInitialMutationAndCompletionSharePublication(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("running-first", session.StatusRunning, "shell")}
	srv, ts, hooks, annotations, completed := matrixDashboardFixture(t, source, "")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
	if err != nil {
		t.Fatalf("dashboard upgrade: response=%v error=%v", resp, err)
	}
	defer conn.Close()
	menu, cc := matrixDashboardReadState(t, conn)
	if menu.Items[1].Session.Status != session.StatusRunning || cc.Totals.Running != 1 || len(cc.RecentlyCompleted) != 0 {
		t.Fatalf("initial dashboard state=%+v, %+v", menu, cc)
	}
	if matrixDashboardSubscriberCount(srv) != 1 || source.calls.Load() != 1 || hooks.Load() != 1 || annotations.Load() != 1 {
		t.Fatalf("initial dashboard demand duplicated: sub=%d loads=%d/%d/%d", matrixDashboardSubscriberCount(srv), source.calls.Load(), hooks.Load(), annotations.Load())
	}
	if !srv.Idle() || srv.InFlightRequests() != 0 {
		t.Fatalf("open read-only dashboard prevents idle handover: in-flight=%d", srv.InFlightRequests())
	}
	source.replace(matrixPublisherIntegrationSnapshot("finished-now", session.StatusWaiting, "shell"), nil)
	srv.notifyMenuChanged()
	menu, cc = matrixDashboardReadState(t, conn)
	if menu.Items[1].Session.Title != "finished-now" || menu.Items[1].Session.Status != session.StatusWaiting || cc.Totals.Waiting != 1 || len(cc.RecentlyCompleted) != 1 || cc.RecentlyCompleted[0].Title != "finished-now" || cc.RecentlyCompleted[0].Status != "waiting" {
		t.Fatalf("mutation dashboard state lost completion=%+v, %+v", menu, cc)
	}
	if matrixDashboardSubscriberCount(srv) != 1 || source.calls.Load() != 2 || hooks.Load() != 2 || annotations.Load() != 2 {
		t.Fatalf("mutation duplicated page collection: sub=%d loads=%d/%d/%d", matrixDashboardSubscriberCount(srv), source.calls.Load(), hooks.Load(), annotations.Load())
	}
	_ = conn.Close()
	matrixDashboardWait(t, func() bool { return matrixDashboardSubscriberCount(srv) == 0 && completed.Load() == 1 }, "closed dashboard to release its one subscription")
}

// matrix/hardened: recovery must re-emit identical successful state so clients
// can clear an unavailable indicator even when no completion or data changed.
func TestMatrixDashboardWSRecoveryReemitsIdenticalState(t *testing.T) {
	original := matrixPublisherIntegrationSnapshot("unchanged-success", session.StatusWaiting, "shell")
	source := &matrixPublisherIntegrationSource{snapshot: original}
	srv, ts, _, _, completed := matrixDashboardFixture(t, source, "")
	build := srv.commandCenterBuilder
	srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
		cc := build(menu)
		cc.GeneratedAt = "2026-10-12T00:00:00Z"
		return cc
	}
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
	if err != nil {
		t.Fatalf("dashboard upgrade: response=%v error=%v", resp, err)
	}
	defer conn.Close()
	firstMenu, firstCC := matrixDashboardReadState(t, conn)
	if len(firstCC.RecentlyCompleted) != 0 {
		t.Fatal("first waiting state invented a completion")
	}
	source.replace(nil, errors.New("temporary source failure"))
	srv.notifyMenuChanged()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read transient error: %v", err)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Type != "error" {
		t.Fatalf("transient source error did not send an error envelope: %s, %v", payload, err)
	}
	source.replace(original, nil)
	srv.notifyMenuChanged()
	menu, cc := matrixDashboardReadState(t, conn)
	if !reflect.DeepEqual(menu, firstMenu) || !reflect.DeepEqual(cc, firstCC) || len(cc.RecentlyCompleted) != 0 {
		t.Fatalf("identical recovery state changed or invented a completion: menu=%+v command-center=%+v", menu, cc)
	}
	_ = conn.Close()
	matrixDashboardWait(t, func() bool { return completed.Load() == 1 && matrixDashboardSubscriberCount(srv) == 0 }, "identically recovered dashboard to close")
}

// matrix/hardened: only the exact read-only dashboard route is excluded from
// active-request accounting; terminal work and lookalike paths remain counted.
func TestMatrixDashboardWSIdleAccountingUsesExactRoute(t *testing.T) {
	for _, test := range []struct {
		path     string
		isStream bool
	}{
		{"/ws/dashboard", true},
		{"/ws/dashboard/anything", false},
		{"/ws/dashboard-other", false},
		{"/ws/session/worker", false},
		{"/api/menu", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			if got := isStreamPath(test.path); got != test.isStream {
				t.Fatalf("isStreamPath(%q)=%v, want %v", test.path, got, test.isStream)
			}
			srv := &Server{}
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			handler := srv.trackInFlight(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				close(entered)
				<-release
			}))
			go func() {
				defer close(finished)
				handler.ServeHTTP(httptest.NewRecorder(), newLocalRequest(http.MethodGet, test.path, nil))
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				close(release)
				t.Fatal("request did not reach accounting handler")
			}
			idle, inFlight := srv.Idle(), srv.InFlightRequests()
			close(release)
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("released accounting request did not return")
			}
			wantInFlight := int64(1)
			if test.isStream {
				wantInFlight = 0
			}
			if idle != test.isStream || inFlight != wantInFlight || !srv.Idle() || srv.InFlightRequests() != 0 {
				t.Fatalf("accounting %q idle=%v inFlight=%d, want %v/%d; after return=%d", test.path, idle, inFlight, test.isStream, wantInFlight, srv.InFlightRequests())
			}
		})
	}
}

// matrix/hardened: eight tabs fan out from one collector instead of consuming
// sixteen separate menu and Command Center subscriptions or source polls.
func TestMatrixDashboardWSMultiplePagesShareSourceAndReleaseDemand(t *testing.T) {
	const pages = 8
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("shared-pages", session.StatusRunning, "shell")}
	srv, ts, hooks, annotations, completed := matrixDashboardFixture(t, source, "")
	// matrix/hardened: artifact synthesis also belongs to the shared
	// publication; only completion encoding is private to each connection.
	var derived atomic.Int32
	build := srv.commandCenterBuilder
	srv.commandCenterBuilder = func(menu *MenuSnapshot) *CommandCenterSnapshot {
		derived.Add(1)
		return build(menu)
	}
	connections := make([]*websocket.Conn, 0, pages)
	for range pages {
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
		if err != nil {
			t.Fatalf("dashboard upgrade: response=%v error=%v", resp, err)
		}
		defer conn.Close()
		connections = append(connections, conn)
		matrixDashboardReadState(t, conn)
	}
	if matrixDashboardSubscriberCount(srv) != pages || source.calls.Load() != 1 || hooks.Load() != 1 || annotations.Load() != 1 {
		t.Fatalf("%d pages did not share publication: subscribers=%d loads=%d/%d/%d", pages, matrixDashboardSubscriberCount(srv), source.calls.Load(), hooks.Load(), annotations.Load())
	}
	if got := derived.Load(); got != 1 {
		t.Fatalf("%d pages synthesized Command Center %d times, want one shared build", pages, got)
	}
	source.replace(matrixPublisherIntegrationSnapshot("shared-change", session.StatusWaiting, "shell"), nil)
	srv.notifyMenuChanged()
	for _, conn := range connections {
		menu, cc := matrixDashboardReadState(t, conn)
		if menu.Items[1].Session.Title != "shared-change" || len(cc.RecentlyCompleted) != 1 {
			t.Fatalf("page did not get its independent completion: %+v, %+v", menu, cc)
		}
	}
	if source.calls.Load() != 2 || hooks.Load() != 2 || annotations.Load() != 2 {
		t.Fatalf("fanout mutation repeated collection: %d/%d/%d", source.calls.Load(), hooks.Load(), annotations.Load())
	}
	if got := derived.Load(); got != 2 {
		t.Fatalf("mutation synthesized Command Center %d times, want one build per publication", got)
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	matrixDashboardWait(t, func() bool { return matrixDashboardSubscriberCount(srv) == 0 && completed.Load() == pages }, "all dashboard handlers to return")
	matrixDashboardWait(t, func() bool {
		srv.menuPublisher.mu.Lock()
		defer srv.menuPublisher.mu.Unlock()
		return !srv.menuPublisher.running && srv.menuPublisher.loading == nil
	}, "last-page disconnect to stop collection")
}

// matrix/hardened: this socket never accepts terminal input/resize or any
// other application frame; protocol size limits are enforced before decoding.
func TestMatrixDashboardWSRejectsEveryApplicationMessage(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    int
		payload []byte
		code    int
	}{
		{"empty-text", websocket.TextMessage, nil, websocket.ClosePolicyViolation},
		{"input", websocket.TextMessage, []byte(`{"type":"input","data":"typed text"}`), websocket.ClosePolicyViolation},
		{"resize", websocket.TextMessage, []byte(`{"type":"resize","cols":1,"rows":1}`), websocket.ClosePolicyViolation},
		{"unknown", websocket.TextMessage, []byte(`{"type":"anything"}`), websocket.ClosePolicyViolation},
		{"binary", websocket.BinaryMessage, []byte{0, 1, 2}, websocket.ClosePolicyViolation},
		{"empty-binary", websocket.BinaryMessage, nil, websocket.ClosePolicyViolation},
		{"oversize-text", websocket.TextMessage, []byte(strings.Repeat("x", 1025)), websocket.CloseMessageTooBig},
		{"oversize-binary", websocket.BinaryMessage, make([]byte, 1025), websocket.CloseMessageTooBig},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("read-only", session.StatusRunning, "shell")}
			srv, ts, _, _, completed := matrixDashboardFixture(t, source, "")
			conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
			if err != nil {
				t.Fatalf("dashboard upgrade: response=%v error=%v", resp, err)
			}
			defer conn.Close()
			matrixDashboardReadState(t, conn)
			if err := conn.WriteMessage(test.kind, test.payload); err != nil {
				t.Fatalf("write application frame: %v", err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					if !websocket.IsCloseError(err, test.code) {
						t.Fatalf("application frame closed with %v, want code %d", err, test.code)
					}
					break
				}
				t.Fatalf("application frame was accepted with response %s", payload)
			}
			matrixDashboardWait(t, func() bool { return matrixDashboardSubscriberCount(srv) == 0 && completed.Load() == 1 }, "rejected application frame to release handler and subscription")
			if source.calls.Load() != 1 {
				t.Fatalf("client application frame triggered extra source collection: %d", source.calls.Load())
			}
		})
	}
}

// matrix/hardened: source failures are explicit sanitized envelopes, while
// the existing connection recovers without publishing an old successful state.
func TestMatrixDashboardWSSourceErrorAndRecovery(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("before-error", session.StatusRunning, "shell")}
	srv, ts, hooks, annotations, completed := matrixDashboardFixture(t, source, "")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
	if err != nil {
		t.Fatalf("dashboard upgrade: response=%v error=%v", resp, err)
	}
	defer conn.Close()
	matrixDashboardReadState(t, conn)
	source.replace(nil, errors.New("private-source-error-content"))
	srv.notifyMenuChanged()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	kind, payload, err := conn.ReadMessage()
	if err != nil || kind != websocket.TextMessage {
		t.Fatalf("read error envelope: kind=%d payload=%s error=%v", kind, payload, err)
	}
	var envelope struct {
		Type string   `json:"type"`
		Data apiError `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Type != "error" || envelope.Data.Code != "SNAPSHOT_UNAVAILABLE" || strings.Contains(string(payload), "private-source-error-content") || strings.Contains(string(payload), "before-error") {
		t.Fatalf("source failure returned stale/private data: envelope=%+v payload=%s error=%v", envelope, payload, err)
	}
	if matrixDashboardSubscriberCount(srv) != 1 || source.calls.Load() != 2 || hooks.Load() != 1 || annotations.Load() != 1 {
		t.Fatalf("failed source closed/duplicated dashboard demand: sub=%d calls=%d/%d/%d", matrixDashboardSubscriberCount(srv), source.calls.Load(), hooks.Load(), annotations.Load())
	}
	source.replace(matrixPublisherIntegrationSnapshot("after-error", session.StatusWaiting, "shell"), nil)
	srv.notifyMenuChanged()
	menu, cc := matrixDashboardReadState(t, conn)
	if menu.Items[1].Session.Title != "after-error" || len(cc.RecentlyCompleted) != 1 || cc.RecentlyCompleted[0].Title != "after-error" {
		t.Fatalf("recovery lost running baseline across source failure: %+v, %+v", menu, cc)
	}
	if source.calls.Load() != 3 || hooks.Load() != 2 || annotations.Load() != 2 {
		t.Fatalf("recovery repeated source/overlay work: %d/%d/%d", source.calls.Load(), hooks.Load(), annotations.Load())
	}
	_ = conn.Close()
	matrixDashboardWait(t, func() bool { return completed.Load() == 1 && matrixDashboardSubscriberCount(srv) == 0 }, "recovered dashboard to close")
}

// matrix/hardened: protocol ping/pong remains allowed on the server-push-only
// socket, and heartbeats keep a responsive read loop alive beyond pongWait.
func TestMatrixDashboardWSProtocolKeepaliveAndHeartbeat(t *testing.T) {
	oldHeartbeat, oldPongWait := dashboardHeartbeatInterval, dashboardPongWait
	dashboardHeartbeatInterval, dashboardPongWait = 75*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { dashboardHeartbeatInterval, dashboardPongWait = oldHeartbeat, oldPongWait })
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("keepalive", session.StatusRunning, "shell")}
	srv, ts, _, _, completed := matrixDashboardFixture(t, source, "")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
	if err != nil {
		t.Fatalf("dashboard upgrade: response=%v error=%v", resp, err)
	}
	defer conn.Close()
	matrixDashboardReadState(t, conn)
	pong := make(chan string, 1)
	conn.SetPongHandler(func(value string) error {
		select {
		case pong <- value:
		default:
		}
		return nil
	})
	heartbeats := make(chan string, 16)
	readDone := make(chan error, 1)
	go func() {
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				readDone <- err
				return
			}
			var envelope struct {
				Type string `json:"type"`
				Data struct {
					Time string `json:"time"`
				} `json:"data"`
			}
			if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Type != "heartbeat" {
				readDone <- errors.New("unexpected keepalive application envelope")
				return
			}
			select {
			case heartbeats <- envelope.Data.Time:
			default:
			}
		}
	}()
	if err := conn.WriteControl(websocket.PingMessage, []byte("dashboard-probe"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-pong:
		if value != "dashboard-probe" {
			t.Fatalf("protocol pong payload=%q", value)
		}
	case err := <-readDone:
		t.Fatalf("protocol frame closed the socket: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not answer protocol ping")
	}
	select {
	case timestamp := <-heartbeats:
		if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
			t.Fatalf("heartbeat timestamp=%q: %v", timestamp, err)
		}
	case err := <-readDone:
		t.Fatalf("socket closed before heartbeat: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("dashboard did not send heartbeat")
	}
	time.Sleep(600 * time.Millisecond)
	select {
	case err := <-readDone:
		t.Fatalf("responsive peer was reaped despite protocol pongs: %v", err)
	default:
	}
	if matrixDashboardSubscriberCount(srv) != 1 || source.calls.Load() != 1 {
		t.Fatalf("keepalive duplicated or ended collector demand: sub=%d loads=%d", matrixDashboardSubscriberCount(srv), source.calls.Load())
	}
	_ = conn.Close()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("closed keepalive client did not release its test reader")
	}
	matrixDashboardWait(t, func() bool { return completed.Load() == 1 && matrixDashboardSubscriberCount(srv) == 0 }, "keepalive socket to join after close")
}

// matrix/hardened: shutdown closes idle sockets and joins their read loops,
// including peers that send no application data and would otherwise block.
func TestMatrixDashboardWSShutdownReleasesIdleReader(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherIntegrationSnapshot("shutdown", session.StatusRunning, "shell")}
	srv, ts, _, _, completed := matrixDashboardFixture(t, source, "")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/dashboard"), nil)
	if err != nil {
		t.Fatalf("dashboard upgrade: response=%v error=%v", resp, err)
	}
	defer conn.Close()
	matrixDashboardReadState(t, conn)
	shutdown := make(chan error, 1)
	go func() { shutdown <- srv.Shutdown(context.Background()) }()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown waited indefinitely for dashboard reader")
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("dashboard socket remained open after shutdown")
	}
	matrixDashboardWait(t, func() bool { return completed.Load() == 1 && matrixDashboardSubscriberCount(srv) == 0 }, "shutdown to close and join dashboard handler")
}
