package web

// matrix/hardened: real isolated SQLite tests exercise external writes and
// consumer fences; the runtime source is injected without any fleet sampling.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func matrixCatalogueFixture(t *testing.T) (*SessionDataService, *runtimeWebSource, *session.Storage, *statedb.InstanceRow) {
	t.Helper()
	profile := fmt.Sprintf("_test_catalogue_%d", time.Now().UnixNano())
	writer, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	row := &statedb.InstanceRow{ID: "catalogue-session", Title: "persisted", Tool: "shell", Status: "waiting", ProjectPath: t.TempDir(), GroupPath: "work", CreatedAt: time.Now().Add(-time.Hour), ToolData: json.RawMessage(`{"notes":"registry"}`)}
	if err := writer.GetDB().SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	instances, _, err := writer.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	source := &runtimeWebSource{snapshot: session.RuntimeProfileSnapshot{Protocol: session.RuntimeProtocolVersion, Profile: profile, Epoch: "catalogue", Revision: 1, ObservedAt: time.Now(), Sessions: []session.RuntimeSessionSnapshot{{ID: row.ID, Profile: profile, Epoch: "catalogue", Revision: 1, Identity: instances[0].RuntimeIdentity(), ObservedAt: time.Now().Add(time.Hour), Status: session.StatusRunning, Substate: session.SubstateBackgroundWork, Detail: "fresh work", CanForkKnown: true, CanFork: true, LaunchModel: session.ModelInfo{ModelID: "live-model", Model: "Live"}, LatestPrompt: "fresh prompt", ToolOptions: json.RawMessage(`{"live":true}`), BackgroundWork: tmux.BackgroundWork{Kind: tmux.BackgroundKindWorkflow, Task: "fresh", Step: 1, Steps: 4}, GeminiAnalytics: &session.GeminiSessionAnalytics{Model: "fresh"}}}}}
	svc := NewSessionDataService(profile)
	svc.SetRuntimeObservationSource(source)
	t.Cleanup(func() { _ = svc.Close() })
	return svc, source, writer, row
}

func TestMatrixMenuCatalogueReuseAndPrivateOwnership(t *testing.T) {
	svc, source, _, _ := matrixCatalogueFixture(t)
	starts := tmux.SubprocessStarts()
	first, err := svc.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	item := runtimeWebSession(t, first)
	if item.Status != session.StatusRunning || !item.CanFork || item.BackgroundWork == nil {
		t.Fatalf("missing source verdict %+v", item)
	}
	item.Title = "caller edit"
	item.GeminiAnalytics.Model = "caller edit"
	item.ToolOptionsJSON[2] = 'X'
	data := NewMemoryMenuData(svc)
	for range 4 {
		got, err := data.LoadMenuSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		item := runtimeWebSession(t, got)
		if item.Title != "persisted" || item.GeminiAnalytics.Model != "fresh" || string(item.ToolOptionsJSON) != `{"live":true}` {
			t.Fatalf("private ownership lost %+v", item)
		}
	}
	stats := svc.CatalogueDiagnostics()
	if stats.Opens != 1 || stats.Reloads != 1 || stats.Hits != 4 || source.snapshots != 5 {
		t.Fatalf("catalogue did not reuse: %+v, source %d", stats, source.snapshots)
	}
	if tmux.SubprocessStarts() != starts {
		t.Fatal("catalogue consumer ran local tmux queries")
	}
}

func TestMatrixMenuCatalogueInvalidRowsCannotRetainOverlay(t *testing.T) {
	for _, kind := range []string{"missing", "error", "identity", "zero-time", "old-time", "source-error", "duplicate", "envelope"} {
		t.Run(kind, func(t *testing.T) {
			svc, source, writer, _ := matrixCatalogueFixture(t)
			if _, err := svc.LoadMenuSnapshot(); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				source.snapshot.Sessions = nil
			case "error":
				source.snapshot.Sessions[0].Error = "probe failed"
			case "identity":
				source.snapshot.Sessions[0].Identity = "old binding"
			case "zero-time":
				source.snapshot.Sessions[0].ObservedAt = time.Time{}
			case "old-time":
				if err := writer.GetDB().Touch(); err != nil {
					t.Fatal(err)
				}
				source.snapshot.Sessions[0].ObservedAt = time.Unix(1, 0)
			case "source-error":
				source.err = errors.New("source unavailable")
			case "duplicate":
				source.snapshot.Sessions = append(source.snapshot.Sessions, source.snapshot.Sessions[0])
			case "envelope":
				source.snapshot.Sessions[0].Revision++
			}
			got, err := svc.LoadMenuSnapshot()
			if kind == "source-error" || kind == "duplicate" || kind == "envelope" {
				if err == nil {
					t.Fatal("source failure promoted to success")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			item := runtimeWebSession(t, got)
			if item.Status != session.StatusWaiting || item.Substate != "" || item.SubstateDetail != "" || item.CanFork || item.BackgroundWork != nil || item.LatestPrompt != "" || item.ModelID != "" || item.GeminiAnalytics != nil {
				t.Fatalf("previous overlay retained %+v", item)
			}
		})
	}
}

func TestMatrixMenuCatalogueExternalWritesAndMutationFence(t *testing.T) {
	svc, source, writer, row := matrixCatalogueFixture(t)
	if _, err := svc.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
	before, err := writer.GetUpdatedAt()
	if err != nil {
		t.Fatal(err)
	}
	row.Title = "external rename"
	if err := writer.GetDB().SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	after, err := writer.GetUpdatedAt()
	if err != nil {
		t.Fatal(err)
	}
	if !before.Equal(after) {
		t.Fatal("fixture unexpectedly touched mutation watermark")
	}
	got, err := svc.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if runtimeWebSession(t, got).Title != row.Title || svc.CatalogueDiagnostics().Reloads != 2 {
		t.Fatal("WAL commit invisible without Touch")
	}
	row.ToolData = json.RawMessage(`{"codex_session_id":"new-native-binding"}`)
	if err := writer.GetDB().SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	got, err = svc.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	item := runtimeWebSession(t, got)
	if item.CodexSessionID != "new-native-binding" || item.Status != session.StatusWaiting || item.Substate != "" {
		t.Fatalf("old identity accepted %+v", item)
	}
	source.beforeSnapshot = func() {
		row.Status = "stopped"
		if e := writer.GetDB().SaveInstance(row); e != nil {
			t.Fatal(e)
		}
		if e := writer.GetDB().Touch(); e != nil {
			t.Fatal(e)
		}
	}
	got, err = svc.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if item = runtimeWebSession(t, got); item.Status != session.StatusStopped || item.Substate != "" {
		t.Fatalf("old source resurrected stop %+v", item)
	}
	source.beforeSnapshot = nil
	if err := writer.GetDB().SetArchived(row.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = svc.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalSessions != 0 {
		t.Fatal("archive not reflected")
	}
}

func TestMatrixMenuCatalogueNonRegistryWritesReuseAndTouchStillFences(t *testing.T) {
	svc, source, writer, _ := matrixCatalogueFixture(t)
	if _, err := svc.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"heartbeat-one", "heartbeat-two", "heartbeat-three"} {
		if err := writer.GetDB().SetMeta("catalogue-fixture-heartbeat", value); err != nil {
			t.Fatal(err)
		}
		got, err := svc.LoadMenuSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if runtimeWebSession(t, got).Status != session.StatusRunning {
			t.Fatal("unrelated commit lost runtime overlay")
		}
	}
	if stats := svc.CatalogueDiagnostics(); stats.Reloads != 1 || stats.Hits != 3 || stats.RegistryReads != 4 {
		t.Fatalf("unrelated writes rebuilt menu %+v", stats)
	}
	if err := writer.GetDB().Touch(); err != nil {
		t.Fatal(err)
	}
	source.snapshot.Sessions[0].ObservedAt = time.Unix(1, 0)
	got, err := svc.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if item := runtimeWebSession(t, got); item.Status != session.StatusWaiting || item.CanFork || item.BackgroundWork != nil {
		t.Fatal("new touch failed to fence old observation")
	}
	if svc.CatalogueDiagnostics().Reloads != 1 {
		t.Fatal("touch-only write unnecessarily rebuilt unchanged catalogue")
	}
}

func matrixReplaceCatalogueDB(t *testing.T, path string) {
	t.Helper()
	other, err := session.NewStorageWithProfile(fmt.Sprintf("_test_replacement_%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	row := &statedb.InstanceRow{ID: "replacement", Title: "replacement", Tool: "shell", Status: "waiting", ProjectPath: t.TempDir(), GroupPath: "work", CreatedAt: time.Now(), ToolData: json.RawMessage(`{}`)}
	if err := other.GetDB().SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	otherPath := other.Path()
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(path+suffix, path+".old"+suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMatrixMenuCatalogueReplacementAtOpenAndHotCache(t *testing.T) {
	for _, when := range []string{"during-open", "hot-cache"} {
		t.Run(when, func(t *testing.T) {
			svc, source, writer, _ := matrixCatalogueFixture(t)
			if when == "hot-cache" {
				if _, err := svc.LoadMenuSnapshot(); err != nil {
					t.Fatal(err)
				}
			}
			path := writer.Path()
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if when == "during-open" {
				original := svc.openStorage
				once := true
				svc.openStorage = func(profile string) (storageLoader, error) {
					opened, err := original(profile)
					if err == nil && once {
						once = false
						matrixReplaceCatalogueDB(t, path)
					}
					return opened, err
				}
				if _, err := svc.LoadMenuSnapshot(); err == nil {
					t.Fatal("mixed storage/observer inodes accepted")
				}
			} else {
				matrixReplaceCatalogueDB(t, path)
			}
			source.snapshot.Sessions = nil
			got, err := svc.LoadMenuSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if item := runtimeWebSession(t, got); item.ID != "replacement" || item.Status != session.StatusWaiting {
				t.Fatalf("old database retained %+v", item)
			}
			if when == "hot-cache" && svc.CatalogueDiagnostics().Replacements != 1 {
				t.Fatal("replacement not detected")
			}
		})
	}
}

func TestMatrixMenuCatalogueOverlayConfigFence(t *testing.T) {
	svc, _, _, _ := matrixCatalogueFixture(t)
	path, err := session.GetUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	prior, priorErr := os.ReadFile(path)
	t.Cleanup(func() {
		if priorErr == nil {
			_ = os.WriteFile(path, prior, 0o600)
		} else if os.IsNotExist(priorErr) {
			_ = os.Remove(path)
		}
	})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
	once := true
	svc.now = func() time.Time {
		if once {
			once = false
			if err := os.WriteFile(path, []byte("# changed during overlay\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return time.Now()
	}
	if _, err := svc.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
	if svc.CatalogueDiagnostics().Reloads < 2 {
		t.Fatal("config changed during overlay escaped fence")
	}
}

type matrixCatalogueBlockedSource struct {
	entered, release chan struct{}
	snapshot         session.RuntimeProfileSnapshot
	cancellable      bool
}

func (s *matrixCatalogueBlockedSource) Snapshot(ctx context.Context, _ string) (*session.RuntimeProfileSnapshot, error) {
	close(s.entered)
	if s.cancellable {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		<-s.release
	}
	v := s.snapshot.Clone()
	return &v, nil
}

func TestMatrixMenuCatalogueSourceGenerationAndContextClose(t *testing.T) {
	for _, kind := range []string{"nil-source", "replacement-source", "context-close"} {
		t.Run(kind, func(t *testing.T) {
			svc, source, _, _ := matrixCatalogueFixture(t)
			blocked := &matrixCatalogueBlockedSource{entered: make(chan struct{}), release: make(chan struct{}), snapshot: source.snapshot.Clone(), cancellable: kind == "context-close"}
			svc.SetRuntimeObservationSource(blocked)
			done := make(chan error, 1)
			go func() { _, err := svc.LoadMenuSnapshot(); done <- err }()
			<-blocked.entered
			if kind == "context-close" {
				if err := svc.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if kind == "nil-source" {
					svc.SetRuntimeObservationSource(nil)
				} else {
					svc.SetRuntimeObservationSource(source)
				}
				close(blocked.release)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("retired source returned success")
				}
			case <-time.After(time.Second):
				t.Fatal("source close/replacement did not finish")
			}
			if kind != "context-close" {
				svc.SetRuntimeObservationSource(source)
				source.snapshot.Sessions[0].LatestPrompt = "replacement source"
				menu, err := svc.LoadMenuSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				if runtimeWebSession(t, menu).LatestPrompt != "replacement source" {
					t.Fatal("retired source state retained")
				}
			}
		})
	}
}

func TestMatrixMenuCatalogueMatchesFreshLegacyOverlay(t *testing.T) {
	for _, kind := range []string{"complete", "unknown-fork-and-no-background"} {
		t.Run(kind, func(t *testing.T) {
			svc, source, writer, _ := matrixCatalogueFixture(t)
			if kind != "complete" {
				source.snapshot.Sessions[0].CanForkKnown = false
				source.snapshot.Sessions[0].BackgroundWork = tmux.BackgroundWork{}
			}
			fixed := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
			svc.now = func() time.Time { return fixed }
			actual, err := svc.LoadMenuSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			instances, groups, err := writer.LoadWithGroups()
			if err != nil {
				t.Fatal(err)
			}
			touch, err := writer.GetUpdatedAt()
			if err != nil {
				t.Fatal(err)
			}
			if err := applyRuntimeObservations(&source.snapshot, svc.Profile(), instances, touch); err != nil {
				t.Fatal(err)
			}
			want := BuildMenuSnapshot(svc.Profile(), instances, groups, fixed)
			// JSON normalization matches the private serialization ownership seam.
			encoded, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var normalized MenuSnapshot
			if err := json.Unmarshal(encoded, &normalized); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, &normalized) {
				t.Fatalf("cached DTO differs from fresh legacy overlay\nactual=%+v\nwant=%+v", runtimeWebSession(t, actual), runtimeWebSession(t, &normalized))
			}
		})
	}
}
func (*matrixCatalogueBlockedSource) Refresh(context.Context, string) error { return nil }
func (*matrixCatalogueBlockedSource) Observations(context.Context, string, string, uint64) ([]session.RuntimeProfileSnapshot, error) {
	return nil, nil
}

func TestMatrixMenuCatalogueCloseDuringUncancelableSource(t *testing.T) {
	svc, source, _, _ := matrixCatalogueFixture(t)
	blocked := &matrixCatalogueBlockedSource{entered: make(chan struct{}), release: make(chan struct{}), snapshot: source.snapshot.Clone()}
	svc.SetRuntimeObservationSource(blocked)
	done := make(chan error, 1)
	go func() { _, err := svc.LoadMenuSnapshot(); done <- err }()
	<-blocked.entered
	closed := make(chan struct{})
	go func() { _ = svc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close waited for uncancelable source")
	}
	close(blocked.release)
	if err := <-done; err == nil {
		t.Fatal("late source revived closed catalogue")
	}
	if _, err := svc.LoadMenuSnapshot(); err == nil {
		t.Fatal("closed reader reopened")
	}
	if got := svc.CatalogueDiagnostics(); !got.Closed || got.Opens != 1 {
		t.Fatalf("close gate lost %+v", got)
	}
}

func TestMatrixMenuCatalogueServerShutdownAndPrivateDiagnostics(t *testing.T) {
	svc, _, _, _ := matrixCatalogueFixture(t)
	srv := NewServer(Config{Profile: svc.Profile(), Token: "catalogue-fixture-token", MenuData: NewMemoryMenuData(svc)})
	unauth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauth, newLocalRequest(http.MethodGet, "/api/diagnostics/menu-catalogue", nil))
	if unauth.Code != http.StatusUnauthorized || svc.CatalogueDiagnostics().Opens != 0 {
		t.Fatal("unauthorized diagnostics activated catalogue")
	}
	if _, err := srv.menuPublisher.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = srv.Shutdown(context.Background()) }()
	}
	wg.Wait()
	if !svc.CatalogueDiagnostics().Closed {
		t.Fatal("Handler-only shutdown retained resources")
	}
	if _, err := svc.LoadMenuSnapshot(); err == nil {
		t.Fatal("shutdown reader reopened")
	}
}
