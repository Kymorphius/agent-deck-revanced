package session

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: two handles model independent monitor and CLI processes;
// no tmux/agent process is required to exercise sidecar writer ordering.
func runtimeAuthHoldFixture(t *testing.T, id string) (*Storage, *Storage, *Instance) {
	t.Helper()
	owner := newTestStorage(t)
	inst := &Instance{ID: id, Title: id, Tool: "claude", Command: "claude", Status: StatusError,
		ProjectPath: t.TempDir(), GroupPath: "work", CreatedAt: time.Now(), LastStartedAt: time.Now().Add(-time.Minute).Truncate(time.Second)}
	if err := owner.SaveWithGroups([]*Instance{inst}, nil); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(owner.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	peer := &Storage{db: db, dbPath: owner.dbPath, profile: owner.profile}
	t.Cleanup(func() { _ = inst.authHoldScopeLocked().clearObserved(true) })
	return owner, peer, inst
}

func runtimeAuthProbe(t *testing.T, storage *Storage, inst *Instance, at time.Time) authHoldScope {
	t.Helper()
	fence, err := storage.db.LastModified()
	if err != nil {
		t.Fatal(err)
	}
	inst.SetRuntimeProducer(true)
	inst.SetRuntimeProducerFence(fence)
	inst.SetRuntimeProducerObservedAt(at)
	inst.mu.Lock()
	scope := inst.authHoldScopeLocked()
	inst.mu.Unlock()
	return scope
}

func runtimeAuthLoad(t *testing.T, storage *Storage) *Instance {
	t.Helper()
	instances, _, err := storage.LoadWithGroups()
	if err != nil || len(instances) != 1 {
		t.Fatalf("load peer: count=%d error=%v", len(instances), err)
	}
	return instances[0]
}

func TestRuntimeAuthHoldDelayedFailureAfterStopCannotRecreateRecord(t *testing.T) {
	owner, peer, inst := runtimeAuthHoldFixture(t, "auth-late-stop")
	scope := runtimeAuthProbe(t, owner, inst, time.Now())
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		<-release
		done <- scope.writeObserved(AuthHoldRecord{InstanceID: inst.ID, Reason: AuthHoldReasonDeath, Evidence: "late 401", Timestamp: time.Now().Unix()})
	}()
	<-started
	if err := peer.db.WriteStatus(inst.ID, string(StatusStopped), inst.Tool); err != nil {
		t.Fatal(err)
	}
	if err := peer.db.Touch(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, statedb.ErrObservationStale) {
		t.Fatalf("late producer result=%v", err)
	}
	if _, err := os.Stat(authHoldRecordPathForProfile(owner.profile, inst.ID)); !os.IsNotExist(err) {
		t.Fatalf("late failure recreated sidecar: %v", err)
	}
	if held, _ := runtimeAuthLoad(t, peer).IsAuthHeld(); held {
		t.Fatal("fresh stopped CLI was held")
	}
}

func TestRuntimeAuthHoldOldHealthyProbeKeepsReplacementFailure(t *testing.T) {
	owner, peer, inst := runtimeAuthHoldFixture(t, "auth-old-healthy")
	oldAt := time.Now().Add(-time.Second)
	runtimeAuthProbe(t, owner, inst, oldAt)
	if !inst.noteAuthHoldLocked(AuthHoldReasonDeath, "old launch 401") {
		t.Fatal("initial hold refused")
	}
	oldHealthy := runtimeAuthProbe(t, owner, inst, oldAt.Add(time.Millisecond))
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { close(started); <-release; done <- oldHealthy.clearObserved(false) }()
	<-started
	replacement := runtimeAuthLoad(t, peer)
	replacement.LastStartedAt = time.Now().Truncate(time.Second)
	replacement.Command = "claude --model replacement"
	if err := peer.SaveWithGroups([]*Instance{replacement}, nil); err != nil {
		t.Fatal(err)
	}
	if replacement.AuthHold() != nil {
		t.Fatal("replacement launch adopted its predecessor's auth record")
	}
	runtimeAuthProbe(t, peer, replacement, time.Now().Add(time.Millisecond))
	if !replacement.noteAuthHoldLocked(AuthHoldReasonDeath, "replacement 401") {
		t.Fatal("replacement hold refused")
	}
	close(release)
	if err := <-done; !errors.Is(err, statedb.ErrObservationStale) {
		t.Fatalf("old healthy result=%v", err)
	}
	if rec := runtimeAuthLoad(t, peer).AuthHold(); rec == nil || rec.Evidence != "replacement 401" {
		t.Fatalf("old healthy removed replacement hold: %+v", rec)
	}
	if inst.AuthHold() != nil {
		t.Fatal("old launch reader adopted the replacement auth record")
	}
}

// matrix/hardened: ordinary CLI probes have no monitor START/fence. Their
// loaded launch must still be current before they can borrow a DB watermark.
func TestRuntimeAuthHoldPlainOldCLIRejectsReplacementGeneration(t *testing.T) {
	for _, replacementHold := range []bool{true, false} {
		t.Run(map[bool]string{true: "new hold", false: "no sidecar yet"}[replacementHold], func(t *testing.T) {
			owner, peer, seed := runtimeAuthHoldFixture(t, "auth-old-plain-cli")
			old := runtimeAuthLoad(t, owner)
			if old.isRuntimeProducer() || !old.RuntimeProducerObservedAt().IsZero() {
				t.Fatal("old CLI fixture unexpectedly has producer timing")
			}
			replacement := runtimeAuthLoad(t, peer)
			replacement.Command = "claude --model ordinary-cli-replacement"
			replacement.LastStartedAt = time.Now().Truncate(time.Second)
			if err := peer.SaveWithGroups([]*Instance{replacement}, nil); err != nil {
				t.Fatal(err)
			}
			if replacementHold {
				runtimeAuthProbe(t, peer, replacement, time.Now())
				if !replacement.noteAuthHoldLocked(AuthHoldReasonDeath, "new generation 401") {
					t.Fatal("replacement could not arm its hold")
				}
			}
			// The old plain probe finishes after replacement save/write; no
			// runtimeAuthProbe call supplies it with a producer timestamp.
			if old.noteAuthHoldLocked(AuthHoldReasonDeath, "late old CLI 401") {
				t.Fatal("old plain CLI obtained replacement write permission")
			}
			old.Status = StatusRunning
			old.clearAuthHoldLocked()
			rec := runtimeAuthLoad(t, peer).AuthHold()
			if replacementHold {
				if rec == nil || rec.Evidence != "new generation 401" {
					t.Fatalf("old plain CLI changed replacement hold: %+v", rec)
				}
			} else if _, err := os.Stat(authHoldRecordPathForProfile(owner.profile, seed.ID)); !os.IsNotExist(err) {
				t.Fatalf("old plain CLI recreated a replacement sidecar: %v", err)
			}
		})
	}
}

func TestRuntimeAuthHoldGuardPinsWriterDuringSidecarCallback(t *testing.T) {
	owner, peer, inst := runtimeAuthHoldFixture(t, "auth-writer-pin")
	scope := runtimeAuthProbe(t, owner, inst, time.Now())
	inside, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- scope.guard(func() error {
			close(inside)
			<-release
			return writeAuthHoldRecordAt(authHoldRecordPathForProfile(scope.profile, scope.id), scope.stamp(AuthHoldRecord{InstanceID: scope.id, Reason: AuthHoldReasonDeath, Evidence: "pinned 401", Timestamp: time.Now().Unix()}))
		})
	}()
	<-inside
	stopStarted, stopDone := make(chan struct{}), make(chan error, 1)
	go func() { close(stopStarted); stopDone <- peer.db.WriteStatus(inst.ID, string(StatusStopped), inst.Tool) }()
	<-stopStarted
	select {
	case err := <-stopDone:
		close(release)
		t.Fatalf("stop crossed sidecar writer transaction: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	stopped := runtimeAuthLoad(t, peer)
	stopped.clearAuthHoldLocked()
	if _, err := os.Stat(authHoldRecordPathForProfile(scope.profile, scope.id)); !os.IsNotExist(err) {
		t.Fatalf("committed stop did not clear owned sidecar: %v", err)
	}
}

func TestRuntimeAuthHoldSameLaunchNewerFailureWins(t *testing.T) {
	owner, _, inst := runtimeAuthHoldFixture(t, "auth-same-launch")
	oldAt := time.Now()
	old := runtimeAuthProbe(t, owner, inst, oldAt)
	newer := runtimeAuthProbe(t, owner, inst, oldAt.Add(time.Second))
	if err := newer.writeObserved(AuthHoldRecord{Reason: AuthHoldReasonDeath, Evidence: "newer evidence", Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := old.clearObserved(false); err != nil {
		t.Fatal(err)
	}
	if err := old.writeObserved(AuthHoldRecord{Reason: AuthHoldReasonLive, Evidence: "old evidence", Timestamp: time.Now().Unix()}); !errors.Is(err, statedb.ErrObservationStale) {
		t.Fatalf("old hold overwrote newer evidence: %v", err)
	}
	rec := inst.AuthHold()
	if rec == nil || rec.Evidence != "newer evidence" || rec.Reason != AuthHoldReasonDeath {
		t.Fatalf("newer verdict lost: %+v", rec)
	}
}

func TestRuntimeAuthHoldProfileIsolationAndLegacyCompatibility(t *testing.T) {
	a, b := newTestStorage(t), newTestStorage(t)
	a.profile, b.profile = "auth-profile-a", "auth-profile-b"
	newInstance := func(s *Storage) *Instance {
		i := &Instance{ID: "same-auth-id", Title: "auth", Tool: "claude", Command: "claude", Status: StatusError, ProjectPath: t.TempDir(), GroupPath: "work", CreatedAt: time.Now()}
		if err := s.SaveWithGroups([]*Instance{i}, nil); err != nil {
			t.Fatal(err)
		}
		return i
	}
	ia, ib := newInstance(a), newInstance(b)
	t.Cleanup(func() {
		_ = ia.authHoldScopeLocked().clearObserved(true)
		_ = ib.authHoldScopeLocked().clearObserved(true)
		clearAuthHoldRecord(ia.ID)
	})
	if !ia.noteAuthHoldLocked(AuthHoldReasonDeath, "profile a") || !ib.noteAuthHoldLocked(AuthHoldReasonDeath, "profile b") {
		t.Fatal("profile hold refused")
	}
	if rec := runtimeAuthLoad(t, a).AuthHold(); rec == nil || rec.Evidence != "profile a" {
		t.Fatalf("profile a read crossed namespace: %+v", rec)
	}
	if rec := runtimeAuthLoad(t, b).AuthHold(); rec == nil || rec.Evidence != "profile b" {
		t.Fatalf("profile b read crossed namespace: %+v", rec)
	}
	foreignLegacy := AuthHoldRecord{InstanceID: ia.ID, RuntimeProfile: b.profile, Reason: AuthHoldReasonDeath, Evidence: "foreign old path", Timestamp: time.Now().Unix()}
	if err := writeAuthHoldRecordAt(authHoldRecordPath(ia.ID), foreignLegacy); err != nil {
		t.Fatal(err)
	}
	ia.Status = StatusStopped
	ia.authHoldClearedOnce = true // explicit stop must ignore the healthy one-shot optimization
	ia.clearAuthHoldLocked()
	if rec := ib.AuthHold(); rec == nil || rec.Evidence != "profile b" {
		t.Fatalf("profile a stop removed profile b: %+v", rec)
	}
	if rec, err := readAuthHoldRecord(ia.ID); err != nil || rec == nil || rec.RuntimeProfile != b.profile {
		t.Fatalf("profile a removed foreign legacy record: %+v %v", rec, err)
	}
	ib.Status = StatusStopped
	ib.clearAuthHoldLocked()
	unknownLegacy := AuthHoldRecord{InstanceID: ia.ID, Reason: AuthHoldReasonDeath, Evidence: "anonymous legacy", Timestamp: time.Now().Unix()}
	if err := writeAuthHoldRecordAt(authHoldRecordPath(ia.ID), unknownLegacy); err != nil {
		t.Fatal(err)
	}
	ia.Status, ib.Status = StatusError, StatusError
	if ia.AuthHold() != nil || ib.AuthHold() != nil {
		t.Fatal("nondefault profile adopted ambiguous legacy ownership")
	}
	ia.Status, ib.Status = StatusStopped, StatusStopped
	ia.clearAuthHoldLocked()
	ib.clearAuthHoldLocked()
	if rec, err := readAuthHoldRecord(ia.ID); err != nil || rec == nil || rec.Evidence != "anonymous legacy" {
		t.Fatalf("nondefault profile deleted anonymous legacy: %+v %v", rec, err)
	}
	legacy := newAuthHoldTestInstance(t, "auth-bound-legacy")
	if err := writeAuthHoldRecord(AuthHoldRecord{InstanceID: legacy.ID, Reason: AuthHoldReasonDeath, Evidence: "legacy hold", Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	legacy.restartDB.Store(a.db)
	legacy.setRuntimeProfileLocked(DefaultProfile)
	if rec := legacy.AuthHold(); rec == nil || rec.Evidence != "legacy hold" {
		t.Fatalf("old unprofiled sidecar became unreadable: %+v", rec)
	}
}

func TestRuntimeAuthHoldNativeColdDiscoveryKeepsLaunchHold(t *testing.T) {
	owner, peer, inst := runtimeAuthHoldFixture(t, "auth-native-discovery")
	runtimeAuthProbe(t, owner, inst, time.Now())
	if !inst.noteAuthHoldLocked(AuthHoldReasonDeath, "native discovery auth") {
		t.Fatal("hold refused")
	}
	inst.ClaudeSessionID = "newly-discovered-native"
	inst.GeminiModel = "observed-model"
	if err := owner.db.WriteClaudeSessionBinding(inst.ID, inst.ClaudeSessionID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := owner.db.WriteToolDataExtra(inst.ID, "gemini_model", []byte(`"observed-model"`)); err != nil {
		t.Fatal(err)
	}
	if rec := inst.AuthHold(); rec == nil {
		t.Fatal("native discovery invalidated source hold")
	}
	fresh := runtimeAuthLoad(t, peer)
	if rec := fresh.AuthHold(); rec == nil || rec.Evidence != "native discovery auth" {
		t.Fatalf("fresh CLI lost discovered native hold: %+v", rec)
	}
	if held, _ := fresh.IsAuthHeld(); !held {
		t.Fatal("fresh CLI automatic boot gate lost auth hold")
	}
}

func TestRuntimeAuthHoldLocalStopRejectsSidecarBeforeDBSave(t *testing.T) {
	owner, _, inst := runtimeAuthHoldFixture(t, "auth-local-stop")
	runtimeAuthProbe(t, owner, inst, time.Now())
	if !inst.noteAuthHoldLocked(AuthHoldReasonDeath, "before local stop") {
		t.Fatal("hold refused")
	}
	inst.Status = StatusStopped
	if inst.AuthHold() != nil {
		t.Fatal("local stopped reader trusted pre-save hold")
	}
	if held, _ := inst.IsAuthHeld(); held {
		t.Fatal("local stopped gate held before DB save")
	}
}
