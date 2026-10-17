package ui

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/monitor"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: the cursor and apply pass are serialized across timer and
// output workers. The warning is immutable so View never waits on monitor I/O.
type runtimeHomeState struct {
	mu             sync.Mutex
	epoch          string
	revision       uint64
	historyUnknown bool
	warning        atomic.Pointer[string]
	requestMu      sync.Mutex
	refreshPending bool
	refreshAgain   bool
	closed         bool
	refreshWorkers sync.WaitGroup
}

func (h *Home) runtimeContext() context.Context {
	if h.ctx != nil {
		return h.ctx
	}
	return context.Background()
}

func (h *Home) runtimeWarning() string {
	if warning := h.runtimeState.warning.Load(); warning != nil {
		return *warning
	}
	return ""
}

func (h *Home) setRuntimeUnavailable(err error) {
	if err == nil {
		h.runtimeState.warning.Store(nil)
		return
	}
	warning := "⚠ Session status unavailable; retrying with last known state"
	if h.runtimeState.warning.Swap(&warning) == nil {
		uiLog.Warn("runtime_monitor_unavailable", slog.String("error", err.Error()))
	}
}

// matrix/hardened: profile snapshots are indexed onto the existing instances.
// No registry fields, tmux objects, focus/input or native bindings are replaced.
func (h *Home) applyRuntimeObservation(snapshot session.RuntimeProfileSnapshot, trackTransitions bool) bool {
	h.instancesMu.RLock()
	defer h.instancesMu.RUnlock()
	changed := false
	tracker := h.getTransitionTracker()
	for _, row := range snapshot.Sessions {
		inst := h.instanceByID[row.ID]
		if inst == nil {
			continue
		}
		oldStatus := inst.GetStatusThreadSafe()
		if !inst.ApplyRuntimeSnapshot(h.profile, row) {
			continue
		}
		newStatus := inst.GetStatusThreadSafe()
		if oldStatus != newStatus {
			changed = true
			if trackTransitions {
				tracker.recordAt(inst.ID, inst.GetTitleThreadSafe(), inst.GetToolThreadSafe(), string(oldStatus), string(newStatus), row.ObservedAt)
			}
		}
	}
	return changed
}

func (h *Home) validRuntimeObservation(snapshot *session.RuntimeProfileSnapshot) error {
	if err := h.validRuntimeObservationEnvelope(snapshot); err != nil {
		return err
	}
	if snapshot.Error != "" {
		return errors.New(snapshot.Error)
	}
	return nil
}

// matrix/hardened: an error publication is still a valid ordered history
// marker. Only a latest-value read requires a successful observation.
func (h *Home) validRuntimeObservationEnvelope(snapshot *session.RuntimeProfileSnapshot) error {
	if snapshot == nil || snapshot.Protocol != session.RuntimeProtocolVersion || snapshot.Profile != h.profile || snapshot.Epoch == "" || snapshot.Revision == 0 || snapshot.ObservedAt.IsZero() {
		return errors.New("invalid runtime profile snapshot")
	}
	seen := make(map[string]bool, len(snapshot.Sessions))
	for _, row := range snapshot.Sessions {
		if row.ID == "" || row.Profile != snapshot.Profile || row.Epoch != snapshot.Epoch || row.Revision != snapshot.Revision || row.ObservedAt.IsZero() || seen[row.ID] {
			return errors.New("invalid runtime session envelope")
		}
		seen[row.ID] = true
	}
	return nil
}

// matrix/hardened: failures retain the last accepted replica and retry later;
// there is deliberately no UpdateStatus/tmux/hook fallback in this function.
func (h *Home) refreshRuntimeObservations() bool {
	if h.runtimeSource == nil {
		return false
	}
	h.runtimeState.mu.Lock()
	defer h.runtimeState.mu.Unlock()
	ctx, cancel := context.WithTimeout(h.runtimeContext(), 6*time.Second)
	defer cancel()
	latest, err := h.runtimeSource.Snapshot(ctx, h.profile)
	if err == nil {
		err = h.validRuntimeObservation(latest)
	}
	if err != nil {
		h.setRuntimeUnavailable(err)
		return false
	}
	var observations []session.RuntimeProfileSnapshot
	baseline := h.runtimeState.epoch != latest.Epoch || h.runtimeState.revision == 0
	var gap *monitor.GapError
	if h.runtimeState.epoch == latest.Epoch && h.runtimeState.revision > 0 {
		observations, err = h.runtimeSource.Observations(ctx, h.profile, latest.Epoch, h.runtimeState.revision)
		if err != nil {
			if !errors.As(err, &gap) {
				h.setRuntimeUnavailable(err)
				return false
			}
			// A retained-history gap explicitly establishes a fresh baseline;
			// it cannot fabricate transitions across missing observations.
			latest, err = h.runtimeSource.Snapshot(ctx, h.profile)
			if err == nil {
				err = h.validRuntimeObservation(latest)
			}
			if err != nil {
				h.setRuntimeUnavailable(err)
				return false
			}
			baseline, observations = true, nil
			uiLog.Warn("runtime_observation_history_gap", slog.String("error", gap.Error()))
		}
		// Validate the complete batch before applying any partial history.
		for n := range observations {
			if err := h.validRuntimeObservationEnvelope(&observations[n]); err != nil || observations[n].Epoch != latest.Epoch {
				h.setRuntimeUnavailable(errors.New("invalid runtime observation history"))
				return false
			}
		}
	}
	if baseline || len(observations) == 0 {
		observations = []session.RuntimeProfileSnapshot{*latest}
	}
	if ctx.Err() != nil {
		return false
	}
	sort.SliceStable(observations, func(a, b int) bool { return observations[a].Revision < observations[b].Revision })
	changed := false
	for _, observation := range observations {
		if !baseline && observation.Epoch == h.runtimeState.epoch && observation.Revision < h.runtimeState.revision {
			continue
		}
		if observation.Error != "" {
			// Keep accepted state, retire the marker and re-prime the next
			// successful verdict without inferring an edge across unknown data.
			h.runtimeState.historyUnknown = true
			h.runtimeState.epoch, h.runtimeState.revision = observation.Epoch, observation.Revision
			continue
		}
		// Reapplying an equal revision hydrates newly reloaded instances; the
		// instance itself rejects mutation-stale rows and duplicate edges.
		changed = h.applyRuntimeObservation(observation, !baseline && !h.runtimeState.historyUnknown) || changed
		h.runtimeState.historyUnknown = false
		h.runtimeState.epoch, h.runtimeState.revision = observation.Epoch, observation.Revision
	}
	if gap != nil {
		h.setRuntimeUnavailable(gap)
	} else {
		h.setRuntimeUnavailable(nil)
	}
	if changed {
		h.cachedStatusCounts.valid.Store(false)
		h.publishCurrentSessionStates()
	}
	h.refreshSessionRenderSnapshot(nil)
	return true
}

// matrix/hardened: attach/ack/output events request one shared producer pass;
// even a slow Refresh RPC cannot stall Bubble Tea or a pipe callback.
func (h *Home) requestRuntimeRefresh(inst *session.Instance) {
	if h.runtimeSource == nil {
		return
	}
	h.runtimeState.requestMu.Lock()
	defer h.runtimeState.requestMu.Unlock()
	if h.runtimeState.closed || h.runtimeContext().Err() != nil {
		return
	}
	if inst != nil {
		inst.InvalidateRuntimeSnapshot()
	}
	if h.runtimeState.refreshPending {
		h.runtimeState.refreshAgain = true
		return
	}
	h.runtimeState.refreshPending = true
	h.runtimeState.refreshWorkers.Add(1)
	go func() {
		defer h.runtimeState.refreshWorkers.Done()
		for {
			ctx, cancel := context.WithTimeout(h.runtimeContext(), 6*time.Second)
			if err := h.runtimeSource.Refresh(ctx, h.profile); err != nil {
				h.setRuntimeUnavailable(err)
			} else {
				h.refreshRuntimeObservations()
			}
			cancel()
			h.runtimeState.requestMu.Lock()
			if !h.runtimeState.refreshAgain || h.runtimeState.closed || h.runtimeContext().Err() != nil {
				h.runtimeState.refreshPending = false
				h.runtimeState.requestMu.Unlock()
				return
			}
			h.runtimeState.refreshAgain = false
			h.runtimeState.requestMu.Unlock()
		}
	}()
}

func (h *Home) stopRuntimeRefreshWorkers() {
	h.runtimeState.requestMu.Lock()
	h.runtimeState.closed = true
	h.runtimeState.requestMu.Unlock()
	h.runtimeState.refreshWorkers.Wait()
}

// matrix/hardened: pipe activity only requests a coalesced local cached read;
// it is not a mutation and cannot increase the central collection cadence.
func (h *Home) queueRuntimeStatusRead() {
	select {
	case h.statusTrigger <- statusUpdateRequest{}:
	default:
	}
}

func (h *Home) runtimeWindowDB() *statedb.StateDB {
	if h.storage != nil {
		return h.storage.GetDB()
	}
	return nil
}

// matrix/hardened: only the elected TUI performs existing idle-timeout and
// conductor actions. Headless consumers never capture this ownership.
func (h *Home) runtimeSideEffectsOwner(db *statedb.StateDB) bool {
	if db == nil || h.headless {
		return false
	}
	primary, err := db.ElectPrimary(30 * time.Second)
	return err == nil && primary
}

func (h *Home) backgroundRuntimeStatusUpdate(instances []*session.Instance) {
	db := h.runtimeWindowDB()
	// Window heartbeat/cleanup continue even when the shared source is down.
	if db != nil {
		_ = db.Heartbeat()
		if time.Since(h.lastDeadInstanceCleanup) > 20*time.Second {
			_ = db.CleanDeadInstances(30 * time.Second)
			h.lastDeadInstanceCleanup = time.Now()
		}
	}
	// matrix/hardened: the headless mutation controller is not a display
	// consumer. HTTP demand drives its fallback publisher, so closing every
	// browser must let the shared collector's profile lease expire.
	if h.headless || len(instances) == 0 {
		return
	}
	fresh := h.refreshRuntimeObservations()
	if fresh && h.runtimeSideEffectsOwner(db) {
		if h.idleTimeoutWatcher != nil {
			now := time.Now().UnixNano()
			last := h.idleTimeoutLastTick.Load()
			if (last == 0 || time.Duration(now-last) >= 60*time.Second) && h.idleTimeoutLastTick.CompareAndSwap(last, now) {
				h.idleTimeoutWatcher.Tick(instances)
			}
		}
		h.checkConductorClearOnCompact(instances)
	}
	if db != nil && fresh {
		currentIDs := make(map[string]bool, len(instances))
		for _, inst := range instances {
			currentIDs[inst.ID] = true
			desc, write := shouldPersistAutoNameDesc(inst.GetAutoName(), h.getSessionRenderState(inst).paneTitle, h.lastPersistedAutoNameDesc[inst.ID])
			if write {
				if err := db.WriteAutoNameDescription(inst.ID, desc); err != nil {
					uiLog.Warn("autoname_persist_failed", slog.String("id", inst.ID), slog.String("error", err.Error()))
				} else {
					inst.SetAutoNameDescription(desc)
					h.lastPersistedAutoNameDesc[inst.ID] = desc
				}
			}
		}
		for id := range h.lastPersistedAutoNameDesc {
			if !currentIDs[id] {
				delete(h.lastPersistedAutoNameDesc, id)
			}
		}
	}
	h.syncNotificationsBackground()
	h.lastFullStatusSweep.Store(time.Now().UnixNano())
}
