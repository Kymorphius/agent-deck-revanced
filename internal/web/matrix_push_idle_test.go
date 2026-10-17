package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: count full menu loads so idle push polls cannot hide expensive
// session refreshes behind a test fixture or a cached menu implementation.
type matrixPushIdleMenu struct {
	status session.Status
	loads  int
	onLoad func()
}

func (m *matrixPushIdleMenu) LoadMenuSnapshot() (*MenuSnapshot, error) {
	m.loads++
	if m.onLoad != nil {
		m.onLoad()
	}
	return &MenuSnapshot{
		Profile: "work",
		Items: []MenuItem{{
			Type:    MenuItemTypeSession,
			Session: &MenuSession{ID: "session-1", Title: "Build Bot", Status: m.status},
		}},
	}, nil
}

// matrix/hardened: distinguish a failed count from an authoritative empty store.
type matrixPushIdleStore struct {
	*fakePushStore
	countCalls  int
	countError  error
	upsertError error
}

func (s *matrixPushIdleStore) Count(ctx context.Context) (int, error) {
	s.countCalls++
	if s.countError != nil {
		return 0, s.countError
	}
	return s.fakePushStore.Count(ctx)
}

func (s *matrixPushIdleStore) Upsert(ctx context.Context, sub pushSubscription) error {
	if s.upsertError != nil {
		return s.upsertError
	}
	return s.fakePushStore.Upsert(ctx, sub)
}

// matrix/hardened: retain normal focus/send behavior once a subscriber exists.
func matrixPushIdleSubscription(endpoint string) pushSubscription {
	focused := false
	return pushSubscription{
		Endpoint: endpoint, ClientFocused: &focused,
		Keys: pushSubscriptionKeys{P256DH: "key", Auth: "auth"},
	}
}

// matrix/hardened: repeated subscriber-free ticks never enter the menu loader.
func TestMatrixPushIdleSkipsSnapshotsWithNoSubscriptions(t *testing.T) {
	menu := &matrixPushIdleMenu{status: "waiting"}
	store := &matrixPushIdleStore{fakePushStore: newFakePushStore()}
	push := &pushService{
		menuData: menu, store: store,
		initialized: true, lastStatus: map[string]string{"session-1": "running"},
	}
	for i := 0; i < 5; i++ {
		push.syncOnce(context.Background())
	}
	if menu.loads != 0 || store.countCalls != 5 {
		t.Fatalf("idle polls loaded %d snapshots and checked subscriptions %d times", menu.loads, store.countCalls)
	}
	if push.initialized || len(push.lastStatus) != 0 {
		t.Fatalf("idle baseline retained: initialized=%v, statuses=%v", push.initialized, push.lastStatus)
	}
}

// matrix/hardened: stop after the last removal and prime, without replaying idle
// history, when the first subscriber returns; later transitions still notify.
func TestMatrixPushIdleResubscriptionPrimesBaseline(t *testing.T) {
	ctx := context.Background()
	menu := &matrixPushIdleMenu{status: "running"}
	store := newFakePushStore()
	sender := &fakePushSender{}
	push := &pushService{
		enabled: true, menuData: menu, store: store, sender: sender,
		triggerCh: make(chan struct{}, 1),
	}
	sub := matrixPushIdleSubscription("https://push.example/idle")
	upsertAndSync := func() {
		t.Helper()
		if err := push.UpsertSubscription(ctx, sub); err != nil {
			t.Fatal(err)
		}
		select {
		case <-push.triggerCh:
		default:
			t.Fatal("successful subscription did not request an immediate baseline sync")
		}
		push.syncOnce(ctx)
	}
	upsertAndSync()
	if menu.loads != 1 || len(sender.payloads) != 0 || !push.initialized {
		t.Fatalf("initial prime: loads=%d, sends=%d, initialized=%v", menu.loads, len(sender.payloads), push.initialized)
	}
	menu.status = "waiting"
	push.syncOnce(ctx)
	if len(sender.payloads) != 1 {
		t.Fatalf("active waiting transition sent %d notifications", len(sender.payloads))
	}
	if err := push.RemoveSubscriptionByEndpoint(ctx, sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	menu.status = "running"
	push.syncOnce(ctx)
	push.syncOnce(ctx)
	if menu.loads != 2 || push.initialized {
		t.Fatalf("empty store kept polling: loads=%d, initialized=%v", menu.loads, push.initialized)
	}
	menu.status = "idle"
	upsertAndSync()
	if menu.loads != 3 || len(sender.payloads) != 1 || push.lastStatus["session-1"] != "idle" {
		t.Fatalf("resubscription replayed history: loads=%d, sends=%d, statuses=%v", menu.loads, len(sender.payloads), push.lastStatus)
	}
	menu.status = "error"
	push.syncOnce(ctx)
	if len(sender.payloads) != 2 {
		t.Fatalf("active error transition sent %d total notifications", len(sender.payloads))
	}
}

// matrix/hardened: a zero-subscriber interval must reset the baseline even when
// removal and resubscription both happen before the next polling tick.
func TestMatrixPushIdleResubscriptionWithoutEmptyPoll(t *testing.T) {
	for _, status := range []session.Status{"waiting", "error"} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			menu := &matrixPushIdleMenu{status: "running"}
			sender := &fakePushSender{}
			push := &pushService{enabled: true, menuData: menu, store: newFakePushStore(), sender: sender}
			sub := matrixPushIdleSubscription("https://push.example/no-empty-poll")
			if err := push.UpsertSubscription(ctx, sub); err != nil {
				t.Fatal(err)
			}
			push.syncOnce(ctx)
			if err := push.RemoveSubscriptionByEndpoint(ctx, sub.Endpoint); err != nil {
				t.Fatal(err)
			}
			menu.status = status
			if err := push.UpsertSubscription(ctx, sub); err != nil {
				t.Fatal(err)
			}
			push.syncOnce(ctx)
			if len(sender.payloads) != 0 || push.lastStatus["session-1"] != string(status) {
				t.Fatalf("resubscription replayed idle history: sends=%d, statuses=%v", len(sender.payloads), push.lastStatus)
			}
			menu.status = "running"
			push.syncOnce(ctx)
			menu.status = status
			push.syncOnce(ctx)
			if len(sender.payloads) != 1 {
				t.Fatalf("new active transition sent %d notifications", len(sender.payloads))
			}
		})
	}
}

// matrix/hardened: an old snapshot finishing after subscription changes cannot
// overwrite the reset baseline; serialize the entire snapshot with mutations.
func TestMatrixPushIdleInflightSnapshotCannotRestoreBaseline(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	menu := &matrixPushIdleMenu{status: "waiting", onLoad: func() {
		close(entered)
		<-release
	}}
	store := newFakePushStore()
	sub := matrixPushIdleSubscription("https://push.example/inflight")
	if err := store.Upsert(ctx, sub); err != nil {
		t.Fatal(err)
	}
	push := &pushService{
		enabled: true, menuData: menu, store: store,
		initialized: true, lastStatus: map[string]string{"session-1": "running"},
	}
	syncDone := make(chan struct{})
	go func() {
		push.syncTransitions(ctx)
		close(syncDone)
	}()
	<-entered
	mutationDone := make(chan error, 1)
	go func() {
		if err := push.RemoveSubscriptionByEndpoint(ctx, sub.Endpoint); err != nil {
			mutationDone <- err
			return
		}
		mutationDone <- push.UpsertSubscription(ctx, sub)
	}()
	select {
	case err := <-mutationDone:
		t.Fatalf("subscription mutations interleaved with the in-flight snapshot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-syncDone
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if push.initialized || len(push.lastStatus) != 0 {
		t.Fatalf("old snapshot restored the idle baseline: initialized=%v, statuses=%v", push.initialized, push.lastStatus)
	}
}

// matrix/hardened: transitions captured before the empty interval cannot read
// a newly subscribed recipient list and replay the old notification.
func TestMatrixPushIdleOldTransitionsDoNotNotifyNewSubscribers(t *testing.T) {
	ctx := context.Background()
	store := newFakePushStore()
	oldSub := matrixPushIdleSubscription("https://push.example/old")
	if err := store.Upsert(ctx, oldSub); err != nil {
		t.Fatal(err)
	}
	menu := &matrixPushIdleMenu{status: "waiting"}
	sender := &fakePushSender{}
	push := &pushService{
		enabled: true, menuData: menu, store: store, sender: sender,
		initialized: true, lastStatus: map[string]string{"session-1": "running"},
	}
	transitions, epoch := push.syncTransitions(ctx)
	if len(transitions) != 1 {
		t.Fatalf("captured %d transitions, want 1", len(transitions))
	}
	if err := push.RemoveSubscriptionByEndpoint(ctx, oldSub.Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := push.UpsertSubscription(ctx, matrixPushIdleSubscription("https://push.example/new")); err != nil {
		t.Fatal(err)
	}
	push.notifySubscribers(ctx, transitions[0], epoch)
	push.syncOnce(ctx)
	if len(sender.payloads) != 0 {
		t.Fatalf("old transition replayed to the new subscriber: sends=%d", len(sender.payloads))
	}
}

// matrix/hardened: block a gateway response to verify mutation locks are not
// held through network sends, which may take far longer than local snapshots.
type matrixPushIdleBlockingSender struct {
	entered chan struct{}
	release chan struct{}
}

func (s *matrixPushIdleBlockingSender) Send(_ []byte, _ pushSubscription) (int, error) {
	close(s.entered)
	<-s.release
	return 200, nil
}

// matrix/hardened: removing/replacing a subscriber stays responsive while an
// already-started request completes; its captured recipient cannot change.
func TestMatrixPushIdleNetworkSendDoesNotHoldMutationLock(t *testing.T) {
	ctx := context.Background()
	store := newFakePushStore()
	oldSub := matrixPushIdleSubscription("https://push.example/slow")
	if err := store.Upsert(ctx, oldSub); err != nil {
		t.Fatal(err)
	}
	sender := &matrixPushIdleBlockingSender{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-sender.release:
		default:
			close(sender.release)
		}
	}()
	push := &pushService{
		enabled: true, menuData: &matrixPushIdleMenu{status: "waiting"}, store: store, sender: sender,
		initialized: true, lastStatus: map[string]string{"session-1": "running"},
	}
	syncDone := make(chan struct{})
	go func() {
		push.syncOnce(ctx)
		close(syncDone)
	}()
	<-sender.entered
	mutationDone := make(chan error, 1)
	go func() {
		if err := push.RemoveSubscriptionByEndpoint(ctx, oldSub.Endpoint); err != nil {
			mutationDone <- err
			return
		}
		mutationDone <- push.UpsertSubscription(ctx, matrixPushIdleSubscription("https://push.example/replacement"))
	}()
	select {
	case err := <-mutationDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a blocked network send held the subscription mutation lock")
	}
	if push.initialized || len(push.lastStatus) != 0 {
		t.Fatal("subscription replacement did not clear the baseline")
	}
	close(sender.release)
	<-syncDone
}

// matrix/hardened: a storage error must preserve the last valid baseline and
// avoid an expensive menu refresh until subscriber availability is known.
func TestMatrixPushIdleCountErrorPreservesBaseline(t *testing.T) {
	menu := &matrixPushIdleMenu{status: "waiting"}
	store := &matrixPushIdleStore{
		fakePushStore: newFakePushStore(), countError: errors.New("unreadable subscriptions"),
	}
	push := &pushService{
		menuData: menu, store: store,
		initialized: true, lastStatus: map[string]string{"session-1": "running"},
	}
	push.syncOnce(context.Background())
	if menu.loads != 0 || !push.initialized || push.lastStatus["session-1"] != "running" {
		t.Fatalf("failed count changed baseline: loads=%d, initialized=%v, statuses=%v", menu.loads, push.initialized, push.lastStatus)
	}
}

// matrix/hardened: preserve the pre-existing snapshot-only nil-store semantics.
func TestMatrixPushIdleNilStoreStillPrimesSnapshot(t *testing.T) {
	menu := &matrixPushIdleMenu{status: "running"}
	push := &pushService{menuData: menu}
	push.syncOnce(context.Background())
	if menu.loads != 1 || !push.initialized || push.lastStatus["session-1"] != "running" {
		t.Fatalf("nil-store baseline changed: loads=%d, initialized=%v, statuses=%v", menu.loads, push.initialized, push.lastStatus)
	}
}

// matrix/hardened: removal between Count and List prevents sending; the next
// idle tick clears the baseline instead of continuing expensive menu refreshes.
func TestMatrixPushIdleRemovalBetweenCountAndLoad(t *testing.T) {
	ctx := context.Background()
	store := newFakePushStore()
	sub := matrixPushIdleSubscription("https://push.example/removed")
	if err := store.Upsert(ctx, sub); err != nil {
		t.Fatal(err)
	}
	menu := &matrixPushIdleMenu{status: "waiting", onLoad: func() {
		if err := store.RemoveByEndpoint(ctx, sub.Endpoint); err != nil {
			t.Fatal(err)
		}
	}}
	sender := &fakePushSender{}
	push := &pushService{
		menuData: menu, store: store, sender: sender,
		initialized: true, lastStatus: map[string]string{"session-1": "running"},
	}
	push.syncOnce(ctx)
	push.syncOnce(ctx)
	if menu.loads != 1 || len(sender.payloads) != 0 || push.initialized {
		t.Fatalf("late removal: loads=%d, sends=%d, initialized=%v", menu.loads, len(sender.payloads), push.initialized)
	}
}

// matrix/hardened: failed writes neither register a subscriber nor trigger sync.
func TestMatrixPushIdleFailedUpsertDoesNotTriggerSync(t *testing.T) {
	writeError := errors.New("write failed")
	store := &matrixPushIdleStore{fakePushStore: newFakePushStore(), upsertError: writeError}
	push := &pushService{enabled: true, store: store, triggerCh: make(chan struct{}, 1)}
	if err := push.UpsertSubscription(context.Background(), matrixPushIdleSubscription("https://push.example/fail")); !errors.Is(err, writeError) {
		t.Fatalf("UpsertSubscription error = %v, want %v", err, writeError)
	}
	if len(push.triggerCh) != 0 {
		t.Fatal("failed subscription requested a sync")
	}
}

// matrix/hardened: the idle gate uses the same authoritative subscriptions as
// notification delivery, including fresh writes, removals, and corrupt files.
func TestMatrixPushIdleFileStoreCountMatchesList(t *testing.T) {
	ctx := context.Background()
	store := &pushSubscriptionFileStore{path: filepath.Join(t.TempDir(), "subscriptions.json")}
	check := func(want int) {
		t.Helper()
		count, err := store.Count(ctx)
		if err != nil {
			t.Fatal(err)
		}
		subs, err := store.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if count != want || len(subs) != want {
			t.Fatalf("count=%d, listed=%d, want %d", count, len(subs), want)
		}
	}
	check(0)
	for i, endpoint := range []string{"https://push.example/one", "https://push.example/one", "https://push.example/two"} {
		if err := store.Upsert(ctx, matrixPushIdleSubscription(endpoint)); err != nil {
			t.Fatal(err)
		}
		check([]int{1, 1, 2}[i])
	}
	for i, endpoint := range []string{"https://push.example/one", "https://push.example/two"} {
		if err := store.RemoveByEndpoint(ctx, endpoint); err != nil {
			t.Fatal(err)
		}
		check(1 - i)
	}
	if err := os.WriteFile(store.path, []byte("invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Count(ctx); err == nil {
		t.Fatal("Count treated corrupt subscriptions as empty")
	}
	if _, err := store.List(ctx); err == nil {
		t.Fatal("List accepted corrupt subscriptions")
	}
}
