package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// matrix/hardened: transport consumers receive no mutable DTO and share the
// same successful generation's encoded bytes and lazy Command Center owner.
func matrixPublisherWireSnapshot(t *testing.T, publication, owned menuPublication) *MenuSnapshot {
	t.Helper()
	if publication.Snapshot != nil || publication.Err != nil || len(publication.encoded) == 0 || len(publication.dashboardMenu) == 0 {
		t.Fatalf("wire publication exposed a DTO or omitted successful data: %#v", publication)
	}
	if len(owned.encoded) == 0 || len(owned.dashboardMenu) == 0 || &publication.encoded[0] != &owned.encoded[0] || &publication.dashboardMenu[0] != &owned.dashboardMenu[0] || publication.Fingerprint != owned.Fingerprint || publication.commandCenter == nil || publication.commandCenter != owned.commandCenter {
		t.Fatal("wire publication copied or changed the owned generation's bytes/derived owner")
	}
	var snapshot MenuSnapshot
	if err := json.Unmarshal(publication.encoded, &snapshot); err != nil {
		t.Fatalf("wire menu encoding: %v", err)
	}
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(publication.dashboardMenu, &envelope); err != nil || envelope.Type != "menu" || !bytes.Equal(envelope.Data, publication.encoded) {
		t.Fatalf("dashboard envelope does not reuse the menu value: type=%q error=%v", envelope.Type, err)
	}
	return &snapshot
}

// matrix/hardened: cached wire publications share encoding while ordinary
// subscribers, load and LoadMenuSnapshot keep complete deep-private DTOs.
func TestMatrixMenuPublisherWireCachedOwnershipAndDerivedIdentity(t *testing.T) {
	want := matrixPublisherNestedSnapshot()
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		return matrixPublisherNestedSnapshot(), nil
	}, time.Hour)
	defer publisher.Close()
	rest, err := publisher.LoadMenuSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	privateA, releaseA := publisher.Subscribe(time.Hour, nil)
	defer releaseA()
	privateB, releaseB := publisher.Subscribe(time.Hour, nil)
	defer releaseB()
	wireA, releaseWireA := publisher.subscribeWire(time.Hour, nil)
	defer releaseWireA()
	wireB, releaseWireB := publisher.subscribeWire(time.Hour, nil)
	defer releaseWireB()
	firstPrivate := matrixPublisherPublication(t, privateA)
	secondPrivate := matrixPublisherPublication(t, privateB)
	firstWire := matrixPublisherPublication(t, wireA)
	secondWire := matrixPublisherPublication(t, wireB)
	publisher.mu.Lock()
	owned := publisher.current
	publisher.mu.Unlock()
	for _, publication := range []menuPublication{firstWire, secondWire} {
		if got := matrixPublisherWireSnapshot(t, publication, owned); !reflect.DeepEqual(got, want) {
			t.Fatalf("cached wire contents changed: %+v", got)
		}
	}
	loaded, err := publisher.load(context.Background())
	if err != nil || loaded.Snapshot == nil || firstPrivate.Snapshot == nil || secondPrivate.Snapshot == nil {
		t.Fatalf("private DTO mode lost its snapshot: load=%#v error=%v", loaded, err)
	}
	for _, snapshot := range []*MenuSnapshot{rest, loaded.Snapshot, firstPrivate.Snapshot, secondPrivate.Snapshot} {
		if !reflect.DeepEqual(snapshot, want) || snapshot == owned.Snapshot {
			t.Fatal("ordinary reader did not receive a complete private snapshot")
		}
	}
	matrixPublisherMutateSnapshot(rest)
	matrixPublisherMutateSnapshot(loaded.Snapshot)
	matrixPublisherMutateSnapshot(firstPrivate.Snapshot)
	if !reflect.DeepEqual(secondPrivate.Snapshot, want) || !reflect.DeepEqual(owned.Snapshot, want) {
		t.Fatal("private nested mutation corrupted owned state or another reader")
	}
	if got := matrixPublisherWireSnapshot(t, firstWire, owned); !reflect.DeepEqual(got, want) {
		t.Fatal("private mutation changed immutable wire content")
	}
	if snapshot, err := publisher.LoadMenuSnapshot(); err != nil || !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("private reader corruption persisted in later loads: %+v, %v", snapshot, err)
	}
}

// matrix/hardened: both modes observe complete DTOs on every successful
// collection; wire errors clear the success value and recovery has a new owner.
func TestMatrixMenuPublisherWireCollectedMixedObserversAndRecovery(t *testing.T) {
	source := &matrixPublisherIntegrationSource{snapshot: matrixPublisherNestedSnapshot()}
	publisher := newMenuSnapshotPublisher(context.Background(), source.LoadMenuSnapshot, time.Hour)
	defer publisher.Close()
	var observed, incomplete atomic.Int32
	observe := func(snapshot *MenuSnapshot) {
		observed.Add(1)
		if snapshot == nil || len(snapshot.Items) != 2 || snapshot.Items[1].Session == nil || snapshot.Items[1].Session.Hints["status"] != "review" || len(snapshot.Items[1].Session.Tags) != 1 || snapshot.Items[1].Session.Sandbox.ExtraVolumes["host"] != "container" {
			incomplete.Add(1)
		}
	}
	private, releasePrivate := publisher.Subscribe(time.Hour, observe)
	defer releasePrivate()
	wire, releaseWire := publisher.subscribeWire(time.Hour, observe)
	defer releaseWire()
	initialPrivate := matrixPublisherPublication(t, private)
	initialWire := matrixPublisherPublication(t, wire)
	publisher.mu.Lock()
	initialOwner := publisher.current
	publisher.mu.Unlock()
	matrixPublisherWireSnapshot(t, initialWire, initialOwner)
	if initialPrivate.Snapshot == nil || initialPrivate.Snapshot == initialOwner.Snapshot || observed.Load() != 2 || incomplete.Load() != 0 {
		t.Fatalf("mixed initial observation/ownership: observed=%d incomplete=%d private=%#v", observed.Load(), incomplete.Load(), initialPrivate)
	}
	wantErr := errors.New("mixed wire source unavailable")
	source.replace(nil, wantErr)
	publisher.Invalidate()
	for _, updates := range []<-chan menuPublication{private, wire} {
		publication := matrixPublisherPublication(t, updates)
		if !errors.Is(publication.Err, wantErr) || publication.Snapshot != nil || publication.encoded != nil || publication.dashboardMenu != nil || publication.commandCenter != nil {
			t.Fatalf("failed collection exposed successful/stale values: %#v", publication)
		}
	}
	if observed.Load() != 2 || incomplete.Load() != 0 {
		t.Fatal("source error invoked a successful-snapshot observer")
	}
	recovered := matrixPublisherNestedSnapshot()
	recovered.Profile = "recovered-wire"
	source.replace(recovered, nil)
	publisher.Invalidate()
	privateRecovery := matrixPublisherPublication(t, private)
	wireRecovery := matrixPublisherPublication(t, wire)
	publisher.mu.Lock()
	recoveredOwner := publisher.current
	publisher.mu.Unlock()
	if got := matrixPublisherWireSnapshot(t, wireRecovery, recoveredOwner); !reflect.DeepEqual(got, recovered) || privateRecovery.Snapshot == nil || !reflect.DeepEqual(privateRecovery.Snapshot, recovered) {
		t.Fatalf("mixed recovery lost full contents: private=%#v wire=%+v", privateRecovery, got)
	}
	if recoveredOwner.commandCenter == initialOwner.commandCenter || &recoveredOwner.encoded[0] == &initialOwner.encoded[0] || observed.Load() != 4 || incomplete.Load() != 0 || source.calls.Load() != 3 {
		t.Fatalf("recovery reused an old owner or changed observer semantics: observed=%d incomplete=%d calls=%d", observed.Load(), incomplete.Load(), source.calls.Load())
	}
	loadedWire, err := publisher.loadWire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	matrixPublisherWireSnapshot(t, loadedWire, recoveredOwner)
}

// matrix/hardened: wire delivery retains the generation guard; an in-flight
// pre-mutation load cannot publish stale bytes or call either mode's observer.
func TestMatrixMenuPublisherWireInflightInvalidationDropsOldGeneration(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls, observations, obsolete atomic.Int32
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		snapshot := matrixPublisherNestedSnapshot()
		if calls.Add(1) == 1 {
			close(started)
			<-release
			snapshot.Profile = "obsolete-wire"
		} else {
			snapshot.Profile = "fresh-wire"
		}
		return snapshot, nil
	}, time.Hour)
	defer publisher.Close()
	observe := func(snapshot *MenuSnapshot) {
		observations.Add(1)
		if snapshot.Profile != "fresh-wire" {
			obsolete.Add(1)
		}
	}
	private, releasePrivate := publisher.Subscribe(time.Hour, observe)
	defer releasePrivate()
	wire, releaseWire := publisher.subscribeWire(time.Hour, observe)
	defer releaseWire()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("mixed subscription did not start load")
	}
	publisher.Invalidate()
	releaseOnce.Do(func() { close(release) })
	privatePublication := matrixPublisherPublication(t, private)
	wirePublication := matrixPublisherPublication(t, wire)
	publisher.mu.Lock()
	owned := publisher.current
	publisher.mu.Unlock()
	if privatePublication.Err != nil || privatePublication.Snapshot == nil || privatePublication.Snapshot.Profile != "fresh-wire" || matrixPublisherWireSnapshot(t, wirePublication, owned).Profile != "fresh-wire" || observations.Load() != 2 || obsolete.Load() != 0 || calls.Load() != 2 {
		t.Fatalf("old generation escaped mixed fanout: private=%#v observations=%d obsolete=%d calls=%d", privatePublication, observations.Load(), obsolete.Load(), calls.Load())
	}
}

// matrix/hardened: releasing one mode leaves the other active, and Close
// terminates existing/new channels in both modes without resurrecting demand.
func TestMatrixMenuPublisherWireMixedReleaseAndClose(t *testing.T) {
	publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) {
		return matrixPublisherNestedSnapshot(), nil
	}, time.Hour)
	defer publisher.Close()
	private, releasePrivate := publisher.Subscribe(time.Hour, nil)
	wire, releaseWire := publisher.subscribeWire(time.Hour, nil)
	defer releasePrivate()
	defer releaseWire()
	matrixPublisherPublication(t, private)
	matrixPublisherPublication(t, wire)
	releasePrivate()
	releasePrivate()
	select {
	case _, open := <-private:
		if open {
			t.Fatal("released private channel remained open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("private release did not close its channel")
	}
	publisher.Invalidate()
	publication := matrixPublisherPublication(t, wire)
	if publication.Err != nil || publication.Snapshot != nil || len(publication.encoded) == 0 {
		t.Fatalf("private release stopped the active wire stream: %#v", publication)
	}
	privateAgain, releaseAgain := publisher.Subscribe(time.Hour, nil)
	defer releaseAgain()
	matrixPublisherPublication(t, privateAgain)
	publisher.Close()
	for _, updates := range []<-chan menuPublication{wire, privateAgain} {
		select {
		case _, open := <-updates:
			if open {
				t.Fatal("Close left a mixed-mode channel open")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Close did not close a mixed-mode channel")
		}
	}
	releaseWire()
	releaseWire()
	for _, subscribe := range []func(time.Duration, func(*MenuSnapshot)) (<-chan menuPublication, func()){publisher.Subscribe, publisher.subscribeWire} {
		updates, release := subscribe(time.Hour, nil)
		defer release()
		select {
		case _, open := <-updates:
			if open {
				t.Fatal("closed publisher accepted new mixed-mode demand")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("new subscription on closed publisher was left open")
		}
	}
	if publication, err := publisher.loadWire(context.Background()); err == nil || publication.Snapshot != nil || len(publication.encoded) != 0 {
		t.Fatalf("closed wire load returned a publication: %#v, %v", publication, err)
	}
}

// matrix/hardened: isolate steady fanout cost from source I/O/timers. Both
// modes deliver the same owned 64-session value through production offerLocked;
// allocation metrics measure private DTO decoding, not an asserted CPU saving.
func BenchmarkMatrixMenuPublisherFanoutMode(b *testing.B) {
	snapshot := matrixPublisherNestedSnapshot()
	snapshot.Items = snapshot.Items[:1]
	for i := range 64 {
		item := matrixPublisherNestedSnapshot().Items[1]
		item.Session.ID = fmt.Sprintf("benchmark-%d", i)
		snapshot.Items = append(snapshot.Items, item)
	}
	snapshot.TotalSessions = 64
	for _, privateDTO := range []bool{true, false} {
		mode := "wire"
		if privateDTO {
			mode = "private"
		}
		for _, count := range []int{1, 4, 16} {
			b.Run(fmt.Sprintf("%s/fanout-%d", mode, count), func(b *testing.B) {
				publisher := newMenuSnapshotPublisher(context.Background(), func() (*MenuSnapshot, error) { return snapshot, nil }, time.Hour)
				defer publisher.Close()
				if _, err := publisher.LoadMenuSnapshot(); err != nil {
					b.Fatal(err)
				}
				publisher.mu.Lock()
				owned := publisher.current
				publisher.mu.Unlock()
				channels := make([]chan menuPublication, count)
				for i := range channels {
					channels[i] = make(chan menuPublication, 1)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					publisher.mu.Lock()
					for _, updates := range channels {
						publisher.offerLocked(updates, owned, privateDTO)
					}
					publisher.mu.Unlock()
					for _, updates := range channels {
						publication := <-updates
						if (publication.Snapshot != nil) != privateDTO || publication.Err != nil {
							b.Fatal("fanout delivered the wrong mode")
						}
					}
				}
				b.StopTimer()
			})
		}
	}
}
