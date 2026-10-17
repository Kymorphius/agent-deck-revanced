package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: synchronize on observable publications rather than assume a
// collector has completed just because a short polling interval elapsed.
func matrixPublisherPublication(t *testing.T, updates <-chan menuPublication) menuPublication {
	t.Helper()
	select {
	case publication, ok := <-updates:
		if !ok {
			t.Fatal("subscription closed before publication")
		}
		return publication
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for menu publication")
		return menuPublication{}
	}
}

// matrix/hardened: retain nil/error checks in every successful publication so
// an accidental cached error cannot satisfy a lifecycle test.
func matrixPublisherProfile(t *testing.T, updates <-chan menuPublication) string {
	t.Helper()
	publication := matrixPublisherPublication(t, updates)
	if publication.Err != nil || publication.Snapshot == nil {
		t.Fatalf("publication = %#v, want snapshot without error", publication)
	}
	return publication.Snapshot.Profile
}

// matrix/hardened: a busy page's REST reads and streams must share the same
// expensive collection, including its initial in-flight load.
func TestMatrixMenuPublisherConcurrentReadsAndSubscribersShareCollection(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return &MenuSnapshot{Profile: "shared"}, nil
	}, time.Hour)
	defer publisher.Close()

	const readers = 16
	results := make(chan menuPublication, readers)
	startReaders := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(readers)
	for range readers {
		go func() {
			ready.Done()
			<-startReaders
			snapshot, err := publisher.LoadMenuSnapshot()
			results <- menuPublication{Snapshot: snapshot, Err: err}
		}()
	}
	ready.Wait()
	close(startReaders)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("REST reads did not start collection")
	}

	streams := make([]<-chan menuPublication, 4)
	for i := range streams {
		updates, unsubscribe := publisher.Subscribe(time.Hour, nil)
		defer unsubscribe()
		streams[i] = updates
	}
	releaseOnce.Do(func() { close(release) })
	for range readers {
		publication := matrixPublisherPublication(t, results)
		if publication.Err != nil || publication.Snapshot == nil || publication.Snapshot.Profile != "shared" {
			t.Fatalf("REST load = %#v, want shared snapshot", publication)
		}
	}
	for _, updates := range streams {
		if profile := matrixPublisherProfile(t, updates); profile != "shared" {
			t.Fatalf("subscription profile = %q, want shared", profile)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("collector calls = %d, want one across REST reads and streams", got)
	}
}

// matrix/hardened: opening the first stream after a REST request must reuse the
// still-fresh cache instead of doing a second immediate session refresh.
func TestMatrixMenuPublisherFirstSubscriberReusesRESTCache(t *testing.T) {
	var calls atomic.Int32
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		return &MenuSnapshot{Profile: fmt.Sprint(calls.Add(1))}, nil
	}, time.Hour)
	defer publisher.Close()
	if snapshot, err := publisher.LoadMenuSnapshot(); err != nil || snapshot == nil || snapshot.Profile != "1" {
		t.Fatalf("initial load = %#v, %v, want first snapshot", snapshot, err)
	}
	updates, unsubscribe := publisher.Subscribe(time.Hour, nil)
	defer unsubscribe()
	if got := matrixPublisherProfile(t, updates); got != "1" {
		t.Fatalf("first subscriber received profile %q, want cached 1", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first subscriber collected %d times, want cached load only", got)
	}
}

// matrix/hardened: short-interval collection exists only while a consumer needs
// it; repeated unsubscribe calls must remain safe and stop the last stream.
func TestMatrixMenuPublisherDoesNotPollWithoutSubscribers(t *testing.T) {
	const interval = 15 * time.Millisecond
	var calls atomic.Int32
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		return &MenuSnapshot{Profile: fmt.Sprint(calls.Add(1))}, nil
	}, interval)
	defer publisher.Close()
	time.Sleep(4 * interval)
	if got := calls.Load(); got != 0 {
		t.Fatalf("subscriber-free publisher collected %d times", got)
	}
	if _, err := publisher.LoadMenuSnapshot(); err != nil {
		t.Fatalf("REST load: %v", err)
	}
	time.Sleep(4 * interval)
	if got := calls.Load(); got != 1 {
		t.Fatalf("subscriber-free REST cache started polling: %d calls", got)
	}
	updates, unsubscribe := publisher.Subscribe(interval, nil)
	defer unsubscribe()
	for calls.Load() < 3 {
		matrixPublisherProfile(t, updates)
	}
	unsubscribe()
	unsubscribe()
	// Allow an already-started load to finish before checking sustained idleness.
	time.Sleep(2 * interval)
	stoppedAt := calls.Load()
	time.Sleep(4 * interval)
	if got := calls.Load(); got != stoppedAt {
		t.Fatalf("last unsubscribe kept polling: calls %d -> %d", stoppedAt, got)
	}
}

// matrix/hardened: an unread stream cannot hold the collector or a responsive
// stream hostage when its own bounded publication buffer fills.
func TestMatrixMenuPublisherSlowConsumerDoesNotBlockOtherStreams(t *testing.T) {
	const interval = 10 * time.Millisecond
	var calls atomic.Int32
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		return &MenuSnapshot{Profile: fmt.Sprint(calls.Add(1))}, nil
	}, interval)
	defer publisher.Close()
	_, unsubscribeSlow := publisher.Subscribe(interval, nil)
	defer unsubscribeSlow()
	fast, unsubscribeFast := publisher.Subscribe(interval, nil)
	defer unsubscribeFast()
	seen := make(map[string]bool)
	for len(seen) < 5 {
		seen[matrixPublisherProfile(t, fast)] = true
	}
	if got := calls.Load(); got < 5 {
		t.Fatalf("only %d collections after five distinct fast-stream updates", got)
	}
}

// matrix/hardened: collection errors are visible to streams and REST callers;
// the previous successful value cannot silently masquerade as current data.
func TestMatrixMenuPublisherErrorsNeverReturnStaleSuccess(t *testing.T) {
	wantErr := errors.New("menu storage unavailable")
	var failing atomic.Bool
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		if failing.Load() {
			return &MenuSnapshot{Profile: "stale-on-error"}, wantErr
		}
		return &MenuSnapshot{Profile: "healthy"}, nil
	}, time.Hour)
	defer publisher.Close()
	updates, unsubscribe := publisher.Subscribe(time.Hour, nil)
	defer unsubscribe()
	if profile := matrixPublisherProfile(t, updates); profile != "healthy" {
		t.Fatalf("initial profile = %q, want healthy", profile)
	}
	failing.Store(true)
	publisher.Invalidate()
	publication := matrixPublisherPublication(t, updates)
	if !errors.Is(publication.Err, wantErr) || publication.Snapshot != nil {
		t.Fatalf("error publication = %#v, want nil snapshot and storage error", publication)
	}
	if snapshot, err := publisher.LoadMenuSnapshot(); !errors.Is(err, wantErr) || snapshot != nil {
		t.Fatalf("error REST load = %#v, %v, want nil snapshot and storage error", snapshot, err)
	}
	failing.Store(false)
	publisher.Invalidate()
	if snapshot, err := publisher.LoadMenuSnapshot(); err != nil || snapshot == nil || snapshot.Profile != "healthy" {
		t.Fatalf("recovered load = %#v, %v, want healthy snapshot", snapshot, err)
	}
}

// matrix/hardened: push transition observation runs for every successful
// publication, including a cached first subscription, but never for errors.
func TestMatrixMenuPublisherObserverUsesSuccessfulPublications(t *testing.T) {
	wantErr := errors.New("observer load unavailable")
	var failing atomic.Bool
	var calls atomic.Int32
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		if failing.Load() {
			return nil, wantErr
		}
		return &MenuSnapshot{Profile: fmt.Sprint(calls.Add(1))}, nil
	}, time.Hour)
	defer publisher.Close()
	if _, err := publisher.LoadMenuSnapshot(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	observations := make(chan string, 4)
	var droppedObservations atomic.Int32
	updates, unsubscribe := publisher.Subscribe(time.Hour, func(snapshot *MenuSnapshot) {
		select {
		case observations <- snapshot.Profile:
		default:
			droppedObservations.Add(1)
		}
	})
	defer unsubscribe()
	for _, wantProfile := range []string{"1", "2"} {
		if wantProfile == "2" {
			publisher.Invalidate()
		}
		if profile := matrixPublisherProfile(t, updates); profile != wantProfile {
			t.Fatalf("publication profile = %q, want %q", profile, wantProfile)
		}
		select {
		case profile := <-observations:
			if profile != wantProfile {
				t.Fatalf("observed profile = %q, want %q", profile, wantProfile)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("profile %q did not reach observer", wantProfile)
		}
	}
	failing.Store(true)
	publisher.Invalidate()
	publication := matrixPublisherPublication(t, updates)
	if !errors.Is(publication.Err, wantErr) {
		t.Fatalf("error publication = %#v, want load error", publication)
	}
	select {
	case profile := <-observations:
		t.Fatalf("observer ran for failed publication with profile %q", profile)
	default:
	}
	if got := droppedObservations.Load(); got != 0 {
		t.Fatalf("observer received %d unexpected extra successful publications", got)
	}
}

// matrix/hardened: an invalidation during collection must discard the old
// result, then collect the post-mutation state for both streams and REST.
func TestMatrixMenuPublisherInvalidationDiscardsInflightSnapshot(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return &MenuSnapshot{Profile: "before-mutation"}, nil
		}
		return &MenuSnapshot{Profile: "after-mutation"}, nil
	}, time.Hour)
	defer publisher.Close()
	updates, unsubscribe := publisher.Subscribe(time.Hour, nil)
	defer unsubscribe()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not start collection")
	}
	publisher.Invalidate()
	results := make(chan menuPublication, 1)
	go func() {
		snapshot, err := publisher.LoadMenuSnapshot()
		results <- menuPublication{Snapshot: snapshot, Err: err}
	}()
	releaseOnce.Do(func() { close(release) })
	if profile := matrixPublisherProfile(t, updates); profile != "after-mutation" {
		t.Fatalf("stream received obsolete snapshot %q", profile)
	}
	if profile := matrixPublisherProfile(t, results); profile != "after-mutation" {
		t.Fatalf("REST waiter received obsolete snapshot %q", profile)
	}
	if snapshot, err := publisher.LoadMenuSnapshot(); err != nil || snapshot == nil || snapshot.Profile != "after-mutation" {
		t.Fatalf("cached load = %#v, %v, want post-mutation state", snapshot, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("collector calls = %d, want obsolete load plus one fresh load", got)
	}
}

// matrix/hardened: cover every nested mutable wire field so handlers, loader
// owners, and separate streams cannot modify each other's published snapshots.
func matrixPublisherNestedSnapshot() *MenuSnapshot {
	yolo := true
	cpu, memory := "2.0", "4g"
	return &MenuSnapshot{
		Profile: "original", GeneratedAt: time.Unix(1234, 0).UTC(),
		TotalGroups: 1, TotalSessions: 1,
		Items: []MenuItem{
			{Type: MenuItemTypeGroup, Group: &MenuGroup{Name: "work", Path: "work"}},
			{Type: MenuItemTypeSession, Session: &MenuSession{
				ID: "one", Title: "original", Hints: map[string]string{"status": "review"}, Tags: []string{"urgent"},
				GeminiYoloMode: &yolo, Channels: []string{"channel"}, ExtraArgs: []string{"--arg"},
				ToolOptionsJSON: json.RawMessage(`{"mode":"original"}`),
				Sandbox: &session.SandboxConfig{Enabled: true, Image: "image", CPULimit: &cpu, MemoryLimit: &memory,
					ExtraVolumes: map[string]string{"host": "container"}},
				AdditionalPaths: []string{"additional"}, MultiRepoWorktrees: []session.MultiRepoWorktree{{Branch: "branch"}},
				LoadedMCPNames: []string{"mcp"}, GeminiAnalytics: &session.GeminiSessionAnalytics{InputTokens: 5, Model: "model"},
			}},
		},
	}
}

// matrix/hardened: mutate through pointers/maps/slices rather than replacing
// only top-level fields, which would miss shallow-copy cache corruption.
func matrixPublisherMutateSnapshot(snapshot *MenuSnapshot) {
	snapshot.Profile = "changed"
	snapshot.Items[0].Group.Name = "changed"
	snapshot.Items[1].Path = "changed"
	menuSession := snapshot.Items[1].Session
	menuSession.Title = "changed"
	menuSession.Hints["status"] = "changed"
	menuSession.Tags[0] = "changed"
	*menuSession.GeminiYoloMode = false
	menuSession.Channels[0] = "changed"
	menuSession.ExtraArgs[0] = "changed"
	menuSession.ToolOptionsJSON[0] = ' '
	menuSession.Sandbox.Image = "changed"
	*menuSession.Sandbox.CPULimit = "changed"
	*menuSession.Sandbox.MemoryLimit = "changed"
	menuSession.Sandbox.ExtraVolumes["host"] = "changed"
	menuSession.AdditionalPaths[0] = "changed"
	menuSession.MultiRepoWorktrees[0].Branch = "changed"
	menuSession.LoadedMCPNames[0] = "changed"
	menuSession.GeminiAnalytics.InputTokens = 999
}

// matrix/hardened: immutable cache ownership applies in both directions: the
// source loader and every consumer each retain only their own deep copy.
func TestMatrixMenuPublisherSnapshotsAreDeeplyIsolated(t *testing.T) {
	source := matrixPublisherNestedSnapshot()
	want := matrixPublisherNestedSnapshot()
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		return source, nil
	}, time.Hour)
	defer publisher.Close()
	loaded, err := publisher.LoadMenuSnapshot()
	if err != nil || loaded == nil {
		t.Fatalf("initial load = %#v, %v", loaded, err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("initial deep copy differs from loader snapshot: %#v", loaded)
	}
	matrixPublisherMutateSnapshot(loaded)
	matrixPublisherMutateSnapshot(source)
	first, unsubscribeFirst := publisher.Subscribe(time.Hour, nil)
	defer unsubscribeFirst()
	second, unsubscribeSecond := publisher.Subscribe(time.Hour, nil)
	defer unsubscribeSecond()
	firstPublication := matrixPublisherPublication(t, first)
	secondPublication := matrixPublisherPublication(t, second)
	if firstPublication.Err != nil || firstPublication.Snapshot == nil || secondPublication.Err != nil || secondPublication.Snapshot == nil {
		t.Fatalf("subscriptions failed: %#v, %#v", firstPublication, secondPublication)
	}
	if !reflect.DeepEqual(firstPublication.Snapshot, want) || !reflect.DeepEqual(secondPublication.Snapshot, want) {
		t.Fatal("REST consumer or loader mutation corrupted a subscriber snapshot")
	}
	matrixPublisherMutateSnapshot(firstPublication.Snapshot)
	if !reflect.DeepEqual(secondPublication.Snapshot, want) {
		t.Fatal("one stream mutation corrupted another stream")
	}
	loaded, err = publisher.LoadMenuSnapshot()
	if err != nil || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("consumer mutation corrupted cached REST result: %#v, %v", loaded, err)
	}
}

// matrix/hardened: shutdown must release REST waiters and streams even though
// the loader API cannot cancel its in-flight call; that late result is ignored.
func TestMatrixMenuPublisherShutdownCancelsWaitersAndDiscardsLateLoad(t *testing.T) {
	for _, shutdown := range []string{"close", "context"} {
		t.Run(shutdown, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			release := make(chan struct{})
			finished := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			publisher := newMenuSnapshotPublisher(ctx, func() (*MenuSnapshot, error) {
				close(started)
				<-release
				defer close(finished)
				return &MenuSnapshot{Profile: "late"}, nil
			}, time.Hour)
			defer publisher.Close()
			results := make(chan menuPublication, 1)
			go func() {
				snapshot, err := publisher.LoadMenuSnapshot()
				results <- menuPublication{Snapshot: snapshot, Err: err}
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("loader did not start")
			}
			updates, unsubscribe := publisher.Subscribe(time.Hour, nil)
			defer unsubscribe()
			stopped := make(chan struct{})
			go func() {
				if shutdown == "close" {
					publisher.Close()
				} else {
					cancel()
				}
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown blocked on uncancellable loader")
			}
			publication := matrixPublisherPublication(t, results)
			if publication.Snapshot != nil || publication.Err == nil {
				t.Fatalf("shutdown REST waiter = %#v, want cancellation error", publication)
			}
			select {
			case publication, ok := <-updates:
				if ok {
					t.Fatalf("stream published during shutdown instead of closing: %#v", publication)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown did not close subscription")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("released loader did not complete")
			}
			if snapshot, err := publisher.LoadMenuSnapshot(); snapshot != nil || err == nil {
				t.Fatalf("closed publisher accepted late result: %#v, %v", snapshot, err)
			}
			closedUpdates, closedUnsubscribe := publisher.Subscribe(time.Hour, nil)
			defer closedUnsubscribe()
			select {
			case publication, ok := <-closedUpdates:
				if ok {
					t.Fatalf("closed publisher registered a stream: %#v", publication)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("closed publisher left a new stream open")
			}
		})
	}
}
