package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/monitor"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: source fixtures never probe or mutate the host's fleet.
type matrixRuntimeUISource struct {
	mu           sync.Mutex
	latest       session.RuntimeProfileSnapshot
	history      []session.RuntimeProfileSnapshot
	err          error
	historyErr   error
	historyLimit int
	refreshGate  <-chan struct{}
	refreshed    chan struct{}
	loads        atomic.Int32
	refreshes    atomic.Int32
}

func (s *matrixRuntimeUISource) Snapshot(context.Context, string) (*session.RuntimeProfileSnapshot, error) {
	s.loads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	copy := s.latest.Clone()
	return &copy, nil
}

func (s *matrixRuntimeUISource) Observations(_ context.Context, _ string, epoch string, after uint64) ([]session.RuntimeProfileSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.historyErr != nil {
		return nil, s.historyErr
	}
	var out []session.RuntimeProfileSnapshot
	for _, snapshot := range s.history {
		if snapshot.Epoch == epoch && snapshot.Revision > after {
			out = append(out, snapshot.Clone())
			if s.historyLimit > 0 && len(out) == s.historyLimit {
				break
			}
		}
	}
	return out, nil
}

func (s *matrixRuntimeUISource) Refresh(ctx context.Context, _ string) error {
	s.refreshes.Add(1)
	if s.refreshed != nil {
		select {
		case s.refreshed <- struct{}{}:
		default:
		}
	}
	if s.refreshGate != nil {
		select {
		case <-s.refreshGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *matrixRuntimeUISource) replace(snapshot session.RuntimeProfileSnapshot, err error) {
	s.mu.Lock()
	s.latest, s.err = snapshot, err
	if err == nil {
		s.history = append(s.history, snapshot.Clone())
	}
	s.mu.Unlock()
}

func matrixRuntimeUIObservation(inst *session.Instance, revision uint64, status session.Status) session.RuntimeProfileSnapshot {
	now := time.Now()
	return session.RuntimeProfileSnapshot{
		Protocol: session.RuntimeProtocolVersion, Profile: session.DefaultProfile, Epoch: "private-ui-epoch", Revision: revision, ObservedAt: now,
		Sessions: []session.RuntimeSessionSnapshot{{
			ID: inst.ID, Profile: session.DefaultProfile, Epoch: "private-ui-epoch", Revision: revision, ObservedAt: now, Identity: inst.RuntimeIdentity(),
			Status: status, Substate: session.SubstateBackgroundWork, Detail: "child workflow is active",
			ActivityAt: now.Add(-time.Minute), ObservedActivityAt: now.Add(-2 * time.Minute), ObservedActivityKnown: true,
			LastChangeAt: now.Add(-3 * time.Minute), WaitingSince: now.Add(-4 * time.Minute), WaitingSinceKnown: true,
			TmuxAlive: true, TmuxAliveKnown: true, Acknowledged: true,
			PaneTitle: "⠐ shared child work", PaneTitleKnown: true,
			BackgroundWork: tmux.BackgroundWork{Kind: tmux.BackgroundKindWorkflow, Task: "private child workflow", Step: 2, Steps: 4},
			Hook:           &session.HookStatus{Status: "waiting", Event: "Stop", UpdatedAt: now}, HookKnown: true, HookFresh: true,
		}},
	}
}

func matrixRuntimeUIHome(t *testing.T, source *matrixRuntimeUISource, inst *session.Instance) *Home {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := newHomeForSnapshotTest()
	h.ctx, h.cancel, h.profile, h.runtimeSource = ctx, cancel, session.DefaultProfile, source
	h.instances = []*session.Instance{inst}
	h.instanceByID = map[string]*session.Instance{inst.ID: inst}
	h.lastPersistedAutoNameDesc = make(map[string]string)
	inst.SetRuntimeConsumer(true)
	t.Cleanup(func() { cancel(); h.stopRuntimeRefreshWorkers() })
	return h
}

func TestMatrixRuntimeUISharedStatusHasNoLocalFleetProbes(t *testing.T) {
	inst := instWithTmuxName(t, "private-runtime-child", "private-runtime-pane")
	inst.CodexSessionID = "private-owned-native-id"
	identity := inst.RuntimeIdentity()
	tmuxSession := inst.GetTmuxSession()
	source := &matrixRuntimeUISource{}
	snapshot := matrixRuntimeUIObservation(inst, 1, session.StatusRunning)
	snapshot.Sessions[0].CodexSessionID = "private-observed-id-not-registry"
	source.replace(snapshot, nil)
	h := matrixRuntimeUIHome(t, source, inst)
	before := tmux.SubprocessStarts()
	h.backgroundStatusUpdate()
	h.processStatusUpdate(statusUpdateRequest{})
	h.feedHookStatuses(h.instances)
	if calls := tmux.SubprocessStarts() - before; calls != 0 {
		t.Fatalf("configured runtime consumer spawned %d local fleet probes", calls)
	}
	if source.loads.Load() != 2 || inst.GetStatusThreadSafe() != session.StatusRunning || inst.CachedSubstate() != session.SubstateBackgroundWork {
		t.Fatalf("shared status missing: loads=%d status=%s substate=%s", source.loads.Load(), inst.GetStatusThreadSafe(), inst.CachedSubstate())
	}
	if inst.SubstateDetail() != "child workflow is active" || inst.BackgroundWork().Task != "private child workflow" {
		t.Fatal("child detail/background work was lost")
	}
	if at, known := inst.LastObservedActivity(); !known || !at.Equal(snapshot.Sessions[0].ObservedActivityAt) {
		t.Fatalf("observed activity lost: %v, %v", at, known)
	}
	if !inst.RuntimeAcknowledged() || !inst.GetWaitingSince().Equal(snapshot.Sessions[0].WaitingSince) {
		t.Fatal("acknowledgment/waiting activity was lost")
	}
	if inst.RuntimeIdentity() != identity || inst.CodexSessionID != "private-owned-native-id" || inst.GetTmuxSession() != tmuxSession {
		t.Fatal("runtime apply replaced registry identity or interaction object")
	}
	if state := h.getSessionRenderState(inst); state.paneTitle != "shared child work" {
		t.Fatalf("shared pane title not rendered: %+v", state)
	}
}

func TestMatrixRuntimeUISourceFailureKeepsStateAndRetries(t *testing.T) {
	inst := instWithTmuxName(t, "private-runtime-retry", "private-runtime-retry-pane")
	source := &matrixRuntimeUISource{}
	source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
	h := matrixRuntimeUIHome(t, source, inst)
	h.backgroundStatusUpdate()
	source.replace(session.RuntimeProfileSnapshot{}, errors.New("private injected source unavailable"))
	before := tmux.SubprocessStarts()
	h.backgroundStatusUpdate()
	if inst.GetStatusThreadSafe() != session.StatusRunning || h.runtimeWarning() == "" || tmux.SubprocessStarts() != before {
		t.Fatal("source failure lost state, hid the outage, or started local sampling")
	}
	source.replace(matrixRuntimeUIObservation(inst, 2, session.StatusWaiting), nil)
	h.backgroundStatusUpdate()
	if inst.GetStatusThreadSafe() != session.StatusWaiting || h.runtimeWarning() != "" || source.loads.Load() != 3 {
		t.Fatal("successful retry did not replace state and clear the outage")
	}
}

func TestMatrixRuntimeUIAttachRefreshDoesNotBlockAndCoalesces(t *testing.T) {
	inst := instWithTmuxName(t, "private-runtime-attach", "private-runtime-attach-pane")
	gate := make(chan struct{})
	source := &matrixRuntimeUISource{refreshGate: gate, refreshed: make(chan struct{}, 4)}
	source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
	h := matrixRuntimeUIHome(t, source, inst)
	h.backgroundStatusUpdate()
	before := tmux.SubprocessStarts()
	start := time.Now()
	if cmd := h.attachReturnSyncCmd(inst.ID); cmd == nil {
		t.Fatal("attach command missing")
	} else if _, ok := cmd().(attachReturnSyncedMsg); !ok {
		t.Fatal("attach command did not return its immediate repaint message")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("attach return waited on the shared refresh")
	}
	select {
	case <-source.refreshed:
	case <-time.After(time.Second):
		t.Fatal("attach did not request shared refresh")
	}
	h.requestRuntimeRefresh(inst)
	h.requestRuntimeRefresh(inst)
	if source.refreshes.Load() != 1 {
		t.Fatal("concurrent refreshes were not coalesced")
	}
	// A second action during an in-flight refresh requires a newer pass.
	source.replace(matrixRuntimeUIObservation(inst, 2, session.StatusWaiting), nil)
	close(gate)
	h.runtimeState.refreshWorkers.Wait()
	if source.refreshes.Load() != 2 || inst.GetStatusThreadSafe() != session.StatusWaiting || tmux.SubprocessStarts() != before {
		t.Fatalf("late invalidation was lost or local probing resumed: refreshes=%d status=%s", source.refreshes.Load(), inst.GetStatusThreadSafe())
	}
}

func TestMatrixRuntimeUIHistoryPaginationAndGapBaseline(t *testing.T) {
	inst := instWithTmuxName(t, "private-runtime-history", "private-runtime-history-pane")
	source := &matrixRuntimeUISource{historyLimit: 1}
	source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
	h := matrixRuntimeUIHome(t, source, inst)
	h.refreshRuntimeObservations()
	source.replace(matrixRuntimeUIObservation(inst, 2, session.StatusWaiting), nil)
	source.replace(matrixRuntimeUIObservation(inst, 3, session.StatusRunning), nil)
	source.replace(matrixRuntimeUIObservation(inst, 4, session.StatusWaiting), nil)
	h.refreshRuntimeObservations()
	if h.runtimeState.revision != 2 || inst.GetStatusThreadSafe() != session.StatusWaiting {
		t.Fatal("history page skipped directly to the latest snapshot")
	}
	h.refreshRuntimeObservations()
	if h.runtimeState.revision != 3 || inst.GetStatusThreadSafe() != session.StatusRunning {
		t.Fatal("next ordered history page was not applied")
	}
	source.mu.Lock()
	source.historyErr = &monitor.GapError{Epoch: "private-ui-epoch", Oldest: 4, Latest: 4}
	source.mu.Unlock()
	before := len(h.getTransitionTracker().history[inst.ID])
	h.refreshRuntimeObservations()
	if h.runtimeState.revision != 4 || inst.GetStatusThreadSafe() != session.StatusWaiting || len(h.getTransitionTracker().history[inst.ID]) != before || h.runtimeWarning() == "" {
		t.Fatal("gap did not establish an explicit baseline without invented transitions")
	}
}

func TestMatrixRuntimeUISameRevisionAppliesToReloadedInstanceAndFence(t *testing.T) {
	inst := instWithTmuxName(t, "private-runtime-reload", "private-runtime-reload-pane")
	source := &matrixRuntimeUISource{}
	source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
	h := matrixRuntimeUIHome(t, source, inst)
	h.refreshRuntimeObservations()
	reloaded := instWithTmuxName(t, inst.ID, inst.GetTmuxSession().Name)
	reloaded.ExpectRuntimeSnapshot()
	h.instances, h.instanceByID = []*session.Instance{reloaded}, map[string]*session.Instance{reloaded.ID: reloaded}
	h.refreshRuntimeObservations()
	if reloaded.GetStatusThreadSafe() != session.StatusRunning {
		t.Fatal("equal publication revision failed to hydrate a reloaded row")
	}
	reloaded.SetStatusThreadSafe(session.StatusStopped)
	reloaded.SetRuntimeMutationFence(time.Now().Add(time.Second))
	h.refreshRuntimeObservations()
	if reloaded.GetStatusThreadSafe() != session.StatusStopped {
		t.Fatal("pre-mutation source observation overwrote stopped state")
	}
}

func TestMatrixRuntimeUIConstructorInjectsSource(t *testing.T) {
	source := &matrixRuntimeUISource{}
	h := NewHomeWithRuntimeSource("_test", source)
	t.Cleanup(func() { h.cancel(); h.stopRuntimeRefreshWorkers() })
	if h.runtimeSource != source || h.hookWatcher != nil || h.sseWatcher != nil {
		t.Fatal("constructor did not bind shared source before watcher startup")
	}
}

func TestMatrixRuntimeUIOutputWorkerDoesNotProbeOrDiscardDetail(t *testing.T) {
	inst := instWithTmuxName(t, "private-runtime-output", "private-runtime-output-pane")
	source := &matrixRuntimeUISource{}
	source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
	h := matrixRuntimeUIHome(t, source, inst)
	h.logUpdateChan = make(chan *session.Instance, 1)
	h.statusTrigger = make(chan statusUpdateRequest, 1)
	h.backgroundStatusUpdate()
	before := tmux.SubprocessStarts()
	h.logWorkerWg.Add(1)
	go h.logWorker()
	h.logUpdateChan <- inst
	select {
	case request := <-h.statusTrigger:
		h.processStatusUpdate(request)
	case <-time.After(time.Second):
		t.Fatal("output worker did not request a cached observation read")
	}
	if inst.SubstateDetail() != "child workflow is active" || tmux.SubprocessStarts() != before || source.refreshes.Load() != 0 {
		t.Fatal("output discarded accepted detail, launched local sampling, or forced central collection")
	}
	h.cancel()
	h.logWorkerWg.Wait()
	h.stopRuntimeRefreshWorkers()
}

func TestMatrixRuntimeUIRejectsInvalidEnvelopeAsUnavailable(t *testing.T) {
	inst := instWithTmuxName(t, "private-runtime-envelope", "private-runtime-envelope-pane")
	source := &matrixRuntimeUISource{}
	source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
	h := matrixRuntimeUIHome(t, source, inst)
	h.refreshRuntimeObservations()
	bad := matrixRuntimeUIObservation(inst, 2, session.StatusWaiting)
	bad.Sessions[0].Profile = "private-other-profile"
	source.replace(bad, nil)
	before := tmux.SubprocessStarts()
	if h.refreshRuntimeObservations() || h.runtimeWarning() == "" || inst.GetStatusThreadSafe() != session.StatusRunning || tmux.SubprocessStarts() != before {
		t.Fatal("invalid envelope was accepted or caused local fallback")
	}
}

func TestMatrixRuntimeUIOutageIsVisibleInRenderedFrame(t *testing.T) {
	gate := NewGateHome(GateHomeOptions{Width: 100, Height: 30, Profile: session.DefaultProfile})
	gate.home.setRuntimeUnavailable(errors.New("private injected outage"))
	if frame := gate.home.View(); !strings.Contains(frame, "Session status unavailable") || strings.Contains(frame, "private injected outage") {
		t.Fatal("source health was clipped from the frame or leaked an internal error")
	}
}

func TestMatrixRuntimeUIWindowMaintenanceDoesNotWriteCoarseStatus(t *testing.T) {
	inst := instWithTmuxName(t, "private-runtime-window", "private-runtime-window-pane")
	source := &matrixRuntimeUISource{}
	source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
	h := matrixRuntimeUIHome(t, source, inst)
	storage, err := session.NewStorageWithProfile(session.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	h.storage = storage
	db := storage.GetDB()
	t.Cleanup(func() { _ = db.UnregisterInstance(); _ = db.Close() })
	if err := db.RegisterInstance(true); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveInstance(&statedb.InstanceRow{ID: inst.ID, Title: inst.Title, Tool: inst.Tool, Status: string(session.StatusIdle), ProjectPath: "/private-fixture/project"}); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteStatus(inst.ID, string(session.StatusIdle), inst.Tool); err != nil {
		t.Fatal(err)
	}
	h.backgroundStatusUpdate()
	source.replace(session.RuntimeProfileSnapshot{}, errors.New("private injected maintenance outage"))
	h.lastDeadInstanceCleanup = time.Time{}
	h.backgroundStatusUpdate()
	statuses, err := db.ReadAllStatuses()
	if err != nil || statuses[inst.ID].Status != string(session.StatusIdle) {
		t.Fatalf("shared TUI wrote coarse status: %+v, %v", statuses, err)
	}
	if count, err := db.AliveInstanceCount(); err != nil || count != 1 || h.lastDeadInstanceCleanup.IsZero() {
		t.Fatalf("source outage skipped window maintenance: alive=%d, error=%v", count, err)
	}
}

func TestMatrixRuntimeUIHeadlessControllerDoesNotKeepCollectionDemand(t *testing.T) {
	inst := instWithTmuxName(t, "private-headless-runtime", "private-headless-pane")
	source := &matrixRuntimeUISource{}
	source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
	h := matrixRuntimeUIHome(t, source, inst)
	h.SetHeadless(true)
	h.backgroundStatusUpdate()
	if source.loads.Load() != 0 || source.refreshes.Load() != 0 {
		t.Fatal("headless mutation controller maintained collection demand")
	}
	controller := NewHomeWithRuntimeSource(session.DefaultProfile, source, true)
	t.Cleanup(func() { controller.cancel(); controller.stopRuntimeRefreshWorkers() })
	if !controller.headless {
		t.Fatal("constructor did not set controller mode before startup")
	}
}

// matrix/hardened: a retired source error must not poison all future history
// reads. Pagination also must retain the marker's unknown baseline boundary.
func TestMatrixRuntimeUIHistoricalErrorAdvancesCursorAndRecovers(t *testing.T) {
	for _, pageLimit := range []int{0, 1} {
		t.Run(fmt.Sprintf("history-page-limit-%d", pageLimit), func(t *testing.T) {
			inst := instWithTmuxName(t, "private-runtime-error-history", "private-runtime-error-history-pane")
			source := &matrixRuntimeUISource{historyLimit: pageLimit}
			source.replace(matrixRuntimeUIObservation(inst, 1, session.StatusRunning), nil)
			h := matrixRuntimeUIHome(t, source, inst)
			if !h.refreshRuntimeObservations() {
				t.Fatal("initial successful observation unavailable")
			}
			failed := matrixRuntimeUIObservation(inst, 2, session.StatusStopped)
			failed.Error = "private injected historical collection error"
			source.replace(failed, nil)
			if h.refreshRuntimeObservations() || h.runtimeWarning() == "" || h.runtimeState.revision != 1 || inst.GetStatusThreadSafe() != session.StatusRunning {
				t.Fatal("latest source error must preserve state/cursor and surface unavailability")
			}
			source.replace(matrixRuntimeUIObservation(inst, 3, session.StatusWaiting), nil)
			if !h.refreshRuntimeObservations() {
				t.Fatal("historical source error prevented successful recovery")
			}
			if pageLimit == 1 {
				if h.runtimeState.revision != 2 || inst.GetStatusThreadSafe() != session.StatusRunning || !h.runtimeState.historyUnknown {
					t.Fatal("error-only page was applied or did not advance its cursor")
				}
				if !h.refreshRuntimeObservations() {
					t.Fatal("successful next page remained poisoned by an old error")
				}
			}
			if h.runtimeState.revision != 3 || inst.GetStatusThreadSafe() != session.StatusWaiting || h.runtimeWarning() != "" || h.runtimeState.historyUnknown {
				t.Fatal("successful observation did not recover monotonically without a gap")
			}
			if len(h.getTransitionTracker().history[inst.ID]) != 0 {
				t.Fatal("an invented transition crossed the unknown history marker")
			}
			source.replace(matrixRuntimeUIObservation(inst, 4, session.StatusRunning), nil)
			if !h.refreshRuntimeObservations() || h.runtimeState.revision != 4 || len(h.getTransitionTracker().history[inst.ID]) != 1 {
				t.Fatal("ordinary transition observation did not resume after recovery")
			}
		})
	}
}
