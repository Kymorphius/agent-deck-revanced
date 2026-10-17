package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/health"
)

// matrix/hardened: exercise ordered delivery through the existing journal,
// and prove monitor failures cannot silently reactivate the local sampler.
type runtimeTransitionSource struct {
	initial  RuntimeProfileSnapshot
	batch    []RuntimeProfileSnapshot
	err      error
	requests int
}

func (s *runtimeTransitionSource) Snapshot(context.Context, string) (*RuntimeProfileSnapshot, error) {
	s.requests++
	v := s.initial.Clone()
	return &v, s.err
}
func (s *runtimeTransitionSource) Refresh(context.Context, string) error { return s.err }
func (s *runtimeTransitionSource) Observations(context.Context, string, string, uint64) ([]RuntimeProfileSnapshot, error) {
	s.requests++
	if s.err != nil {
		return nil, s.err
	}
	batch := s.batch
	s.batch = nil
	return batch, s.err
}

type runtimeTestHistoryGap struct{}

func (*runtimeTestHistoryGap) Error() string               { return "retained runtime history gap" }
func (*runtimeTestHistoryGap) RuntimeObservationGap() bool { return true }

type runtimeJournalSink struct {
	mu     sync.Mutex
	events []health.Event
}

func (s *runtimeJournalSink) Append(e health.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func newRuntimeTransitionFixture(t *testing.T) (*TransitionDaemon, *runtimeTransitionSource, *Instance, *runtimeJournalSink) {
	t.Helper()
	storage, err := NewStorageWithProfile(DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	inst := NewInstanceWithTool("runtime-child", t.TempDir(), "codex")
	inst.Status = StatusRunning
	inst.NoTransitionNotify = true
	if err := storage.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatal(err)
	}
	instances, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	inst = instances[0]
	row := RuntimeSessionSnapshot{ID: inst.ID, Profile: DefaultProfile, Epoch: "runtime-test", Revision: 1, Identity: inst.RuntimeIdentity(), ObservedAt: time.Now(), Status: StatusRunning, Substate: SubstateNone, TmuxAliveKnown: true, TmuxAlive: true}
	source := &runtimeTransitionSource{initial: RuntimeProfileSnapshot{Protocol: RuntimeProtocolVersion, Profile: DefaultProfile, Epoch: row.Epoch, Revision: 1, ObservedAt: row.ObservedAt, Sessions: []RuntimeSessionSnapshot{row}}}
	d := NewTransitionDaemon()
	d.storages[DefaultProfile] = storage
	d.SetObservationSource(source)
	sink := &runtimeJournalSink{}
	d.journalWriters[DefaultProfile] = health.NewAsyncWriter(sink, 64)
	t.Cleanup(d.shutdown)
	return d, source, inst, sink
}

func TestRuntimeTransitionConsumerRetainsOrderedEdgesAndDetail(t *testing.T) {
	d, source, _, sink := newRuntimeTransitionFixture(t)
	d.syncObservedProfile(context.Background(), DefaultProfile)
	for n, status := range []Status{StatusWaiting, StatusRunning, StatusWaiting} {
		v := source.initial.Clone()
		v.Revision = uint64(n + 2)
		v.Sessions[0].Revision = v.Revision
		v.Sessions[0].ObservedAt = time.Now()
		v.ObservedAt = v.Sessions[0].ObservedAt
		v.Sessions[0].Status = status
		if n == 2 {
			v.Sessions[0].Substate = SubstateTurnInterrupted
			v.Sessions[0].Detail = "turn_aborted: interrupted"
		}
		source.batch = append(source.batch, v)
	}
	d.syncObservedProfile(context.Background(), DefaultProfile)
	if got := d.observationCursors[DefaultProfile].revision; got != 4 {
		t.Fatalf("cursor=%d", got)
	}
	d.Flush()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 3 {
		t.Fatalf("ordered events=%d, want3: %+v", len(sink.events), sink.events)
	}
	if got := sink.events[2].Detail["substate"]; got != string(SubstateTurnInterrupted) {
		t.Fatalf("substate=%v", got)
	}
}

func TestRuntimeTransitionConsumerSourceFailureDoesNotProbe(t *testing.T) {
	d, source, _, _ := newRuntimeTransitionFixture(t)
	prior := updateInstanceStatus.Load().(statusProbeFunc)
	updateInstanceStatus.Store(statusProbeFunc(func(*Instance) error { t.Error("configured source started local sampling"); return nil }))
	t.Cleanup(func() { updateInstanceStatus.Store(prior) })
	d.syncObservedProfile(context.Background(), DefaultProfile)
	source.err = errors.New("history gap")
	d.syncObservedProfile(context.Background(), DefaultProfile)
	if d.lastStatus[DefaultProfile][source.initial.Sessions[0].ID] != string(StatusRunning) {
		t.Fatal("source error erased last known status")
	}
	if d.observationCursors[DefaultProfile].epoch == "" {
		t.Fatal("temporary failure lost ordered cursor")
	}
	if d.hookWatcher != nil {
		t.Fatal("consumer opened a hook watcher")
	}
}

func TestRuntimeTransitionConsumerRejectsPreMutationObservation(t *testing.T) {
	d, source, inst, _ := newRuntimeTransitionFixture(t)
	if err := d.storages[DefaultProfile].GetDB().Touch(); err != nil {
		t.Fatal(err)
	}
	d.syncObservedProfile(context.Background(), DefaultProfile)
	if _, exists := d.lastStatus[DefaultProfile][inst.ID]; exists {
		t.Fatal("pre-mutation status applied")
	}
	if source.requests != 1 {
		t.Fatalf("source requests=%d", source.requests)
	}
}

func TestRuntimeTransitionConsumerRetriesRetainedHistoryAfterTransportFailure(t *testing.T) {
	d, source, _, sink := newRuntimeTransitionFixture(t)
	d.syncObservedProfile(context.Background(), DefaultProfile)
	for n, status := range []Status{StatusWaiting, StatusRunning} {
		v := source.initial.Clone()
		v.Revision = uint64(n + 2)
		v.Sessions[0].Revision = v.Revision
		v.Sessions[0].ObservedAt = time.Now()
		v.ObservedAt = v.Sessions[0].ObservedAt
		v.Sessions[0].Status = status
		source.batch = append(source.batch, v)
	}
	source.err = errors.New("temporary connection failure")
	d.syncObservedProfile(context.Background(), DefaultProfile)
	if d.observationCursors[DefaultProfile].revision != 1 {
		t.Fatal("transport failure advanced cursor")
	}
	source.err = nil
	d.syncObservedProfile(context.Background(), DefaultProfile)
	d.Flush()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 2 {
		t.Fatalf("reconnected history events=%d want2", len(sink.events))
	}
}

func TestRuntimeTransitionConsumerExplicitGapEstablishesSilentBaseline(t *testing.T) {
	d, source, _, sink := newRuntimeTransitionFixture(t)
	d.syncObservedProfile(context.Background(), DefaultProfile)
	source.err = &runtimeTestHistoryGap{}
	d.syncObservedProfile(context.Background(), DefaultProfile)
	if d.observationCursors[DefaultProfile].epoch != "" || d.initialized[DefaultProfile] {
		t.Fatal("gap did not invalidate baseline")
	}
	source.err = nil
	source.initial.Revision = 20
	source.initial.Sessions[0].Revision = 20
	source.initial.Sessions[0].Status = StatusWaiting
	source.initial.Sessions[0].ObservedAt = time.Now()
	d.syncObservedProfile(context.Background(), DefaultProfile)
	d.Flush()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 0 {
		t.Fatal("inferred a transition across missing history")
	}
}

func TestRuntimeTransitionConsumerSkipsErrorPublicationAndResumes(t *testing.T) {
	d, source, _, sink := newRuntimeTransitionFixture(t)
	d.syncObservedProfile(context.Background(), DefaultProfile)
	errorPub := source.initial.Clone()
	errorPub.Revision = 2
	errorPub.Error = "registry read interrupted"
	errorPub.Sessions = nil
	source.batch = []RuntimeProfileSnapshot{errorPub}
	d.syncObservedProfile(context.Background(), DefaultProfile)
	if d.observationCursors[DefaultProfile].revision != 2 {
		t.Fatal("retained error blocked cursor")
	}
	// A retry of an unflushed completion uses the last accepted observation,
	// while the history cursor continues beyond the error marker.
	d.syncObservedProfile(context.Background(), DefaultProfile)
	if d.observationCursors[DefaultProfile].revision != 2 {
		t.Fatal("retry moved error cursor backwards")
	}
	for n, status := range []Status{StatusWaiting, StatusRunning} {
		v := source.initial.Clone()
		v.Revision = uint64(n + 3)
		v.Sessions[0].Revision = v.Revision
		v.Sessions[0].ObservedAt = time.Now()
		v.ObservedAt = v.Sessions[0].ObservedAt
		v.Sessions[0].Status = status
		source.batch = append(source.batch, v)
	}
	d.syncObservedProfile(context.Background(), DefaultProfile)
	if d.observationCursors[DefaultProfile].revision != 4 {
		t.Fatal("history did not recover")
	}
	d.Flush()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 || sink.events[0].From != "waiting" || sink.events[0].To != "running" {
		t.Fatalf("events across unknown boundary: %+v", sink.events)
	}
}
