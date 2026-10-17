package web

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: control collector publications independently of gateway
// speed, and count the actual source loads rather than cache reads.
type matrixPublishedPushSource struct {
	mu     sync.Mutex
	status session.Status
	err    error
	loads  int
}

func (s *matrixPublishedPushSource) load() (*MenuSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.err != nil {
		return nil, s.err
	}
	return &MenuSnapshot{Profile: "work", Items: []MenuItem{{Type: MenuItemTypeSession,
		Session: &MenuSession{ID: "session-1", Title: "Build Bot", Status: s.status}}}}, nil
}

func (s *matrixPublishedPushSource) set(status session.Status, err error) {
	s.mu.Lock()
	s.status, s.err = status, err
	s.mu.Unlock()
}

// matrix/hardened: the first gateway send blocks while collector observers
// continue. Endpoint/payload readback verifies recipient epoch/focus semantics.
type matrixPublishedPushSender struct {
	mu        sync.Mutex
	messages  []pushMessage
	endpoints []string
	entered   chan struct{}
	release   chan struct{}
}

func (s *matrixPublishedPushSender) Send(payload []byte, sub pushSubscription) (int, error) {
	var message pushMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.messages = append(s.messages, message)
	s.endpoints = append(s.endpoints, sub.Endpoint)
	first := len(s.messages) == 1
	s.mu.Unlock()
	if first && s.entered != nil {
		close(s.entered)
		<-s.release
	}
	return 200, nil
}

func matrixWaitPublishedPush(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("published push condition did not complete")
}

func matrixStartPublishedPush(t *testing.T, source *matrixPublishedPushSource, store *fakePushStore, sender webPushSender) (*pushService, *menuSnapshotPublisher) {
	t.Helper()
	publisher := newMenuSnapshotPublisher(context.Background(), source.load, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	push := &pushService{
		enabled: true, menuData: publisher, store: store, sender: sender,
		pollInterval: time.Hour, triggerCh: make(chan struct{}, 1),
	}
	done := make(chan struct{})
	go func() { push.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		publisher.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("published push loop did not stop")
		}
	})
	return push, publisher
}

func matrixPublishPushStatus(t *testing.T, source *matrixPublishedPushSource, publisher *menuSnapshotPublisher, status session.Status) {
	t.Helper()
	source.set(status, nil)
	publisher.Invalidate()
	if _, err := publisher.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
}

// matrix/hardened: a latest-only fanout channel must not lose two observed
// waiting transitions while the first notification blocks at its gateway.
func TestMatrixPublishedPushSlowSendPreservesTransitions(t *testing.T) {
	source := &matrixPublishedPushSource{status: "running"}
	store := newFakePushStore()
	if err := store.Upsert(context.Background(), matrixPushIdleSubscription("https://push.example/slow-publication")); err != nil {
		t.Fatal(err)
	}
	sender := &matrixPublishedPushSender{entered: make(chan struct{}), release: make(chan struct{})}
	_, publisher := matrixStartPublishedPush(t, source, store, sender)
	t.Cleanup(func() {
		select {
		case <-sender.release:
		default:
			close(sender.release)
		}
	})
	matrixWaitPublishedPush(t, func() bool {
		publisher.mu.Lock()
		defer publisher.mu.Unlock()
		return publisher.revision > 0 && len(publisher.subscribers) == 1
	})
	matrixPublishPushStatus(t, source, publisher, "waiting")
	select {
	case <-sender.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first notification did not start")
	}
	matrixPublishPushStatus(t, source, publisher, "running")
	matrixPublishPushStatus(t, source, publisher, "waiting")
	matrixPublishPushStatus(t, source, publisher, "running")
	close(sender.release)
	matrixWaitPublishedPush(t, func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.messages) == 2
	})
	sender.mu.Lock()
	defer sender.mu.Unlock()
	for _, message := range sender.messages {
		if message.Status != "waiting" {
			t.Fatalf("observed transition became %q", message.Status)
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.loads != 5 {
		t.Fatalf("source loads=%d, want one baseline plus four publications", source.loads)
	}
}

// matrix/hardened: old queued transitions cannot notify a subscriber created
// after an empty interval, including when the gateway was already in flight.
func TestMatrixPublishedPushResubscriptionRejectsOldQueue(t *testing.T) {
	source := &matrixPublishedPushSource{status: "running"}
	store := newFakePushStore()
	oldSub := matrixPushIdleSubscription("https://push.example/old-publication")
	if err := store.Upsert(context.Background(), oldSub); err != nil {
		t.Fatal(err)
	}
	sender := &matrixPublishedPushSender{entered: make(chan struct{}), release: make(chan struct{})}
	push, publisher := matrixStartPublishedPush(t, source, store, sender)
	t.Cleanup(func() {
		select {
		case <-sender.release:
		default:
			close(sender.release)
		}
	})
	matrixWaitPublishedPush(t, func() bool {
		publisher.mu.Lock()
		defer publisher.mu.Unlock()
		return publisher.revision > 0
	})
	matrixPublishPushStatus(t, source, publisher, "waiting")
	<-sender.entered
	matrixPublishPushStatus(t, source, publisher, "running")
	matrixPublishPushStatus(t, source, publisher, "waiting")
	changed := make(chan error, 1)
	go func() {
		if err := push.RemoveSubscriptionByEndpoint(context.Background(), oldSub.Endpoint); err != nil {
			changed <- err
			return
		}
		changed <- push.UpsertSubscription(context.Background(), matrixPushIdleSubscription("https://push.example/new-publication"))
	}()
	select {
	case err := <-changed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber mutation deadlocked behind publisher/gateway")
	}
	if _, err := publisher.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
	close(sender.release)
	matrixPublishPushStatus(t, source, publisher, "running")
	matrixPublishPushStatus(t, source, publisher, "error")
	matrixWaitPublishedPush(t, func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.messages) == 2
	})
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.endpoints[0] != oldSub.Endpoint || sender.endpoints[1] != "https://push.example/new-publication" || sender.messages[1].Status != "error" {
		t.Fatalf("stale queue was replayed: endpoints=%v messages=%v", sender.endpoints, sender.messages)
	}
}

// matrix/hardened: zero push subscribers add no menu demand; last removal
// releases the sole collector even when its original polling interval is long.
func TestMatrixPublishedPushDemandStartsAndStops(t *testing.T) {
	source := &matrixPublishedPushSource{status: "running"}
	store := newFakePushStore()
	push, publisher := matrixStartPublishedPush(t, source, store, &matrixPublishedPushSender{})
	push.TriggerSync()
	if _, err := push.SubscriptionCount(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	loads := source.loads
	source.mu.Unlock()
	if loads != 0 {
		t.Fatalf("zero subscribers loaded %d menus", loads)
	}
	sub := matrixPushIdleSubscription("https://push.example/demand")
	if err := push.UpsertSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	matrixWaitPublishedPush(t, func() bool {
		publisher.mu.Lock()
		defer publisher.mu.Unlock()
		return publisher.revision > 0 && len(publisher.subscribers) == 1
	})
	if err := push.RemoveSubscriptionByEndpoint(context.Background(), sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	matrixWaitPublishedPush(t, func() bool {
		publisher.mu.Lock()
		defer publisher.mu.Unlock()
		return len(publisher.subscribers) == 0 && !publisher.running
	})
}

// matrix/hardened: errors leave the observer's last successful baseline intact;
// recovery emits the real subsequent transition rather than replaying stale data.
func TestMatrixPublishedPushErrorRecovery(t *testing.T) {
	source := &matrixPublishedPushSource{status: "running"}
	store := newFakePushStore()
	if err := store.Upsert(context.Background(), matrixPushIdleSubscription("https://push.example/recovery")); err != nil {
		t.Fatal(err)
	}
	sender := &matrixPublishedPushSender{}
	_, publisher := matrixStartPublishedPush(t, source, store, sender)
	matrixWaitPublishedPush(t, func() bool {
		publisher.mu.Lock()
		defer publisher.mu.Unlock()
		return publisher.revision > 0
	})
	source.set("running", errors.New("source unavailable"))
	publisher.Invalidate()
	if _, err := publisher.LoadMenuSnapshot(); err == nil {
		t.Fatal("failed refresh returned a stale success")
	}
	matrixPublishPushStatus(t, source, publisher, "waiting")
	matrixWaitPublishedPush(t, func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.messages) == 1
	})
}

// matrix/hardened: bounded queues report overflow explicitly and restart from
// a fresh baseline; epoch changes discard old queued observations entirely.
func TestMatrixPublishedPushQueueBoundAndEpoch(t *testing.T) {
	q := &publishedPushQueue{}
	menu := func(status session.Status) *MenuSnapshot {
		return &MenuSnapshot{Items: []MenuItem{{Type: MenuItemTypeSession, Session: &MenuSession{ID: "one", Status: status}}}}
	}
	q.observe(menu("running"), 1)
	for n := 0; n <= maxPublishedPushTransitions; n++ {
		q.observe(menu("waiting"), 1)
		q.observe(menu("running"), 1)
	}
	pending, overflow := q.drain(1)
	if !overflow || len(pending) != maxPublishedPushTransitions {
		t.Fatalf("queue bound pending=%d, overflow=%v", len(pending), overflow)
	}
	q.observe(menu("waiting"), 1)
	if pending, _ := q.drain(1); len(pending) != 0 {
		t.Fatal("overflow resync replayed the first fresh baseline")
	}
	q.observe(menu("running"), 1)
	q.observe(menu("waiting"), 1)
	if pending, _ := q.drain(2); len(pending) != 0 {
		t.Fatal("old epoch survived a new subscription period")
	}
}

// matrix/hardened: expose the completed subscription write while an old
// accepted observer still owns the publisher's publication boundary.
type matrixPublishedPushWriteSignalStore struct {
	*fakePushStore
	written chan struct{}
}

func (s *matrixPublishedPushWriteSignalStore) Upsert(ctx context.Context, sub pushSubscription) error {
	if err := s.fakePushStore.Upsert(ctx, sub); err != nil {
		return err
	}
	close(s.written)
	return nil
}

// matrix/hardened: resetting the epoch before invalidating would let the old
// callback tag an old running snapshot as the newly subscribed baseline and
// falsely notify the waiting state that happened during the empty interval.
func TestMatrixPublishedPushEpochCommitsWithInvalidation(t *testing.T) {
	source := &matrixPublishedPushSource{status: "running"}
	publisher := newMenuSnapshotPublisher(context.Background(), source.load, time.Hour)
	t.Cleanup(publisher.Close)
	store := &matrixPublishedPushWriteSignalStore{fakePushStore: newFakePushStore(), written: make(chan struct{})}
	push := &pushService{enabled: true, menuData: publisher, store: store}
	queue := &publishedPushQueue{}
	entered, gate := make(chan struct{}), make(chan struct{})
	var first sync.Once
	updates, release := publisher.Subscribe(time.Hour, func(snapshot *MenuSnapshot) {
		// Test-only gate fixes the precise old-publication/epoch race boundary.
		first.Do(func() { close(entered); <-gate })
		queue.observe(snapshot, push.publishedEpoch.Load())
	})
	t.Cleanup(release)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	<-entered
	source.set("waiting", nil)
	changed := make(chan error, 1)
	go func() {
		changed <- push.UpsertSubscription(context.Background(), matrixPushIdleSubscription("https://push.example/epoch-boundary"))
	}()
	<-store.written
	select {
	case err := <-changed:
		t.Fatalf("subscription bypassed the old publication boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if epoch := push.publishedEpoch.Load(); epoch != 0 {
		t.Fatalf("old accepted publication could observe the new epoch %d", epoch)
	}
	close(gate)
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.LoadMenuSnapshot(); err != nil {
		t.Fatal(err)
	}
	if pending, _ := queue.drain(push.publishedEpoch.Load()); len(pending) != 0 {
		t.Fatal("idle-interval waiting status became a notification for the new subscriber")
	}
	select {
	case publication := <-updates:
		if publication.Err != nil {
			t.Fatal(publication.Err)
		}
	default:
		t.Fatal("fresh baseline was not published")
	}
}
