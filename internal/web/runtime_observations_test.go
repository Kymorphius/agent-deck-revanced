package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: source fixtures provide observed verdicts without invoking
// producer APIs (RuntimeSnapshot/Substate themselves perform real sampling).
type runtimeWebSource struct {
	mu                   sync.Mutex
	snapshot             session.RuntimeProfileSnapshot
	err                  error
	snapshots, refreshes int
	beforeSnapshot       func()
}

func (s *runtimeWebSource) Snapshot(context.Context, string) (*session.RuntimeProfileSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots++
	if s.beforeSnapshot != nil {
		s.beforeSnapshot()
	}
	if s.err != nil {
		return nil, s.err
	}
	copy := s.snapshot.Clone()
	return &copy, nil
}

func (s *runtimeWebSource) Refresh(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshes++
	return s.err
}

func (s *runtimeWebSource) Observations(context.Context, string, string, uint64) ([]session.RuntimeProfileSnapshot, error) {
	return nil, nil
}

func runtimeWebFixture(t *testing.T) (*SessionDataService, *runtimeWebSource, *session.Instance) {
	t.Helper()
	inst := session.NewInstanceWithGroupAndTool("work", t.TempDir(), "work", "shell")
	inst.ID = "runtime-web-session"
	inst.Status = session.StatusWaiting
	observedAt := time.Now().UTC()
	source := &runtimeWebSource{snapshot: session.RuntimeProfileSnapshot{
		Protocol: session.RuntimeProtocolVersion, Profile: "p", Epoch: "epoch-web", Revision: 7, ObservedAt: observedAt,
		Sessions: []session.RuntimeSessionSnapshot{{
			ID: inst.ID, Profile: "p", Epoch: "epoch-web", Revision: 7, Identity: inst.RuntimeIdentity(), ObservedAt: observedAt,
			Status: session.StatusRunning, Substate: session.SubstateBackgroundWork, Detail: "workflow 2/5",
			TmuxAlive: true, TmuxAliveKnown: true, Acknowledged: false,
			LatestPrompt: "monitor prompt", GeminiModel: "monitor-gemini",
			LaunchModel:     session.ModelInfo{ModelID: "monitor-model", Model: "Monitor", Version: "6"},
			GeminiAnalytics: &session.GeminiSessionAnalytics{Model: "monitor-gemini"},
			BackgroundWork:  tmux.BackgroundWork{Kind: tmux.BackgroundKindWorkflow, Task: "workflow", Step: 2, Steps: 5},
		}},
	}}
	svc := &SessionDataService{
		profile: "p", now: time.Now, refreshLiveState: true,
		openStorage: func(string) (storageLoader, error) {
			return &fakeStorage{instances: []*session.Instance{inst}, groups: []*session.GroupData{{Name: "work", Path: "work", Expanded: true}}}, nil
		},
		loadHookStatuses: func() map[string]*session.HookStatus { t.Fatal("configured runtime consumer loaded hooks"); return nil },
	}
	svc.SetRuntimeObservationSource(source)
	return svc, source, inst
}

func runtimeWebSession(t *testing.T, menu *MenuSnapshot) *MenuSession {
	t.Helper()
	for _, item := range menu.Items {
		if item.Session != nil {
			return item.Session
		}
	}
	t.Fatal("menu has no session")
	return nil
}

func TestRuntimeWebConfiguredSourceHasNoFleetProbes(t *testing.T) {
	svc, _, inst := runtimeWebFixture(t)
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "probes")
	for _, name := range []string{"tmux", "ps"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nprintf probe >> \"$AD_WEB_RUNTIME_PROBE_LOG\"\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AD_WEB_RUNTIME_PROBE_LOG", marker)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	menu, err := svc.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	got := runtimeWebSession(t, menu)
	if got.Status != session.StatusRunning || got.Substate != string(session.SubstateBackgroundWork) || got.SubstateDetail != "workflow 2/5" {
		t.Fatalf("incomplete monitor verdict: %+v", got)
	}
	if got.LatestPrompt != "monitor prompt" || got.ModelID != "monitor-model" || got.GeminiModel != "monitor-gemini" || got.BackgroundWork == nil {
		t.Fatalf("runtime metadata lost: %+v", got)
	}
	if alive, known := inst.RuntimeTmuxAlive(); !alive || !known {
		t.Fatalf("monitor liveness lost: %v %v", alive, known)
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("Web ran fleet probes: %q", data)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestRuntimeWebSourceErrorsNeverFallBack(t *testing.T) {
	for _, kind := range []string{"error", "protocol", "profile", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			svc, source, _ := runtimeWebFixture(t)
			switch kind {
			case "error":
				source.err = errors.New("monitor unavailable")
			case "protocol":
				source.snapshot.Protocol++
			case "profile":
				source.snapshot.Profile = "other"
			case "duplicate":
				source.snapshot.Sessions = append(source.snapshot.Sessions, source.snapshot.Sessions[0])
			}
			if _, err := svc.LoadMenuSnapshot(); err == nil {
				t.Fatal("configured source failure was hidden")
			}
		})
	}
}

func TestRuntimeWebMissingIdentityAndPreMutationRowsStayUnknown(t *testing.T) {
	for _, kind := range []string{"missing", "identity", "pre-mutation"} {
		t.Run(kind, func(t *testing.T) {
			svc, source, inst := runtimeWebFixture(t)
			switch kind {
			case "missing":
				source.snapshot.Sessions = nil
			case "identity":
				source.snapshot.Sessions[0].Identity = "old-native-binding"
			case "pre-mutation":
				watermark := source.snapshot.ObservedAt.Add(time.Second)
				svc.openStorage = func(string) (storageLoader, error) {
					return &fakeStorage{instances: []*session.Instance{inst}, updatedAt: watermark}, nil
				}
			}
			menu, err := svc.LoadMenuSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if got := runtimeWebSession(t, menu); got.Substate != "" || got.SubstateDetail != "" {
				t.Fatalf("unknown row inherited refinements: %+v", got)
			}
			if _, known := inst.RuntimeTmuxAlive(); known {
				t.Fatal("unknown row gained liveness")
			}
		})
	}
}

func TestRuntimeWebMutationDuringSnapshotFencesEarlierProbe(t *testing.T) {
	svc, source, inst := runtimeWebFixture(t)
	storage := &fakeStorage{instances: []*session.Instance{inst}}
	svc.openStorage = func(string) (storageLoader, error) { return storage, nil }
	source.beforeSnapshot = func() {
		// Simulate a stop committed while the shared source request was in flight.
		storage.updatedAt = source.snapshot.ObservedAt.Add(time.Second)
		inst.SetStatusThreadSafe(session.StatusStopped)
	}
	menu, err := svc.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeWebSession(t, menu); got.Status != session.StatusStopped || got.Substate != "" {
		t.Fatalf("earlier probe overwrote the in-flight stop: %+v", got)
	}
}

func TestRuntimeWebDefaultConsumerUsesReadOnlyRegistry(t *testing.T) {
	profile := "_test_runtime_web_readonly"
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	// A configured consumer must not run the writer constructor's pruning.
	hookPath := filepath.Join(session.GetHooksDir(), "readonly-consumer-sentinel.json")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte(`{"status":"waiting","ts":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ancient := time.Unix(1, 0)
	if err := os.Chtimes(hookPath, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	source := &runtimeWebSource{snapshot: session.RuntimeProfileSnapshot{Protocol: session.RuntimeProtocolVersion, Profile: profile, Epoch: "readonly", Revision: 1, ObservedAt: time.Now()}}
	svc := NewSessionDataService(profile)
	svc.SetRuntimeObservationSource(source)
	if _, err := svc.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("consumer pruned hook artifact: %v", err)
	}
}

func TestRuntimeWebConfiguredKeepsInjectedStorage(t *testing.T) {
	fixture, source, _ := runtimeWebFixture(t)
	svc := NewSessionDataService("p")
	svc.openStorage = fixture.openStorage
	svc.SetRuntimeObservationSource(source)
	if _, err := svc.LoadMenuSnapshot(); err != nil {
		t.Fatalf("configured source bypassed injected factory: %v", err)
	}
}

func TestRuntimeWebServerPreservesMonitorVerdictAndPrivateREST(t *testing.T) {
	svc, source, _ := runtimeWebFixture(t)
	srv := NewServer(Config{Profile: "p", Token: "runtime-token", MenuData: NewMemoryMenuData(svc), RuntimeObservationSource: source})
	t.Cleanup(func() { srv.cancelBase(); srv.menuPublisher.Close() })
	srv.annotationLoader = nil
	srv.hookStatusLoader = func() map[string]*session.HookStatus { t.Fatal("server reapplied raw Stop hook"); return nil }
	unauthorized := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthorized, newLocalRequest(http.MethodGet, "/api/sessions", nil))
	if unauthorized.Code != http.StatusUnauthorized || source.snapshots != 0 {
		t.Fatalf("auth did not precede collection: code=%d snapshots=%d", unauthorized.Code, source.snapshots)
	}
	first, err := srv.menuPublisher.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	runtimeWebSession(t, first).SubstateDetail = "consumer edit"
	runtimeWebSession(t, first).GeminiAnalytics.Model = "consumer model edit"
	second, err := srv.menuPublisher.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := runtimeWebSession(t, second); got.SubstateDetail != "workflow 2/5" || got.Status != session.StatusRunning {
		t.Fatalf("REST snapshot ownership or verdict lost: %+v", got)
	}
	if got := runtimeWebSession(t, second); got.GeminiAnalytics.Model != "monitor-gemini" {
		t.Fatalf("REST metadata aliases another consumer: %+v", got.GeminiAnalytics)
	}
}

func TestRuntimeWebArchivesDoNotCollectRuntime(t *testing.T) {
	svc, source, inst := runtimeWebFixture(t)
	inst.ArchivedAt = time.Now()
	menu, err := svc.LoadArchivedMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if source.snapshots != 0 || menu.TotalSessions != 1 {
		t.Fatalf("archive created runtime demand: snapshots=%d sessions=%d", source.snapshots, menu.TotalSessions)
	}
	if got := runtimeWebSession(t, menu); got.Substate != "" || got.SubstateDetail != "" {
		t.Fatalf("archive received live refinements: %+v", got)
	}
}

func TestRuntimeWebMutationRefreshRequiresAuthorization(t *testing.T) {
	svc, source, _ := runtimeWebFixture(t)
	srv := NewServer(Config{Profile: "p", Token: "runtime-token", WebMutations: true, MenuData: svc, RuntimeObservationSource: source})
	t.Cleanup(func() { srv.cancelBase(); srv.menuPublisher.Close() })
	srv.SetMutator(&fakeMutator{stopSessionFn: func(string) error { return nil }})
	unauthorized := httptest.NewRecorder()
	unauthorizedReq := newLocalRequest(http.MethodPost, "/api/sessions/runtime-web-session/stop", nil)
	unauthorizedReq.Header.Set("Origin", "http://localhost")
	srv.Handler().ServeHTTP(unauthorized, unauthorizedReq)
	if (unauthorized.Code != http.StatusUnauthorized && unauthorized.Code != http.StatusForbidden) || source.refreshes != 0 || source.snapshots != 0 {
		t.Fatalf("unauthorized mutation touched monitor: code=%d refresh=%d snapshot=%d", unauthorized.Code, source.refreshes, source.snapshots)
	}
	req := newLocalRequest(http.MethodPost, "/api/sessions/runtime-web-session/stop", nil)
	req.Header.Set("Authorization", "Bearer runtime-token")
	req.Header.Set("Origin", "http://localhost")
	authorized := httptest.NewRecorder()
	srv.Handler().ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK || source.refreshes != 1 {
		t.Fatalf("mutation failed to refresh monitor: code=%d refresh=%d body=%s", authorized.Code, source.refreshes, authorized.Body.String())
	}
}
