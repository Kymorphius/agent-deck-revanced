package session

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// matrix/hardened: notifications consume every retained observation in order,
// rather than treating the latest coarse status row as a stream of turn edges.
type runtimeObservationCursor struct {
	epoch    string
	revision uint64
	latest   *RuntimeProfileSnapshot
	unknown  bool
}

// SetObservationSource is configured before Run/SyncOnce. A configured source
// failure never starts a second local fleet sampler alongside the monitor.
func (d *TransitionDaemon) SetObservationSource(source RuntimeObservationSource) {
	d.observationSource = source
	d.observationCursors = make(map[string]runtimeObservationCursor)
}

func (d *TransitionDaemon) syncObservedProfile(ctx context.Context, profile string) time.Duration {
	storage := d.getStorage(profile)
	if storage == nil {
		return notifyPollSlow
	}
	if d.observationCursors == nil {
		d.observationCursors = make(map[string]runtimeObservationCursor)
	}
	cursor := d.observationCursors[profile]
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var snapshots []RuntimeProfileSnapshot
	if cursor.epoch == "" {
		snapshot, err := d.observationSource.Snapshot(requestCtx, profile)
		if err != nil || snapshot == nil {
			d.logRuntimeSourceError(profile, err)
			return notifyPollSlow
		}
		snapshots = []RuntimeProfileSnapshot{*snapshot}
	} else {
		var err error
		snapshots, err = d.observationSource.Observations(requestCtx, profile, cursor.epoch, cursor.revision)
		if err != nil {
			d.logRuntimeSourceError(profile, err)
			// matrix/hardened: a temporary transport failure retains the cursor,
			// so completed turns still drain from history after reconnecting.
			var gap RuntimeObservationGap
			if errors.As(err, &gap) && gap.RuntimeObservationGap() {
				d.observationCursors[profile] = runtimeObservationCursor{}
				d.initialized[profile] = false
				delete(d.lastJournaled, profile)
			}
			return notifyPollSlow
		}
	}
	interval := notifyPollSlow
	repeated := len(snapshots) == 0 && cursor.latest != nil
	if repeated {
		snapshots = []RuntimeProfileSnapshot{*cursor.latest}
	}
	for _, snapshot := range snapshots {
		if snapshot.Protocol != RuntimeProtocolVersion || snapshot.Profile != profile || snapshot.Epoch == "" || snapshot.ObservedAt.IsZero() {
			d.logRuntimeSourceError(profile, fmt.Errorf("invalid runtime observation: protocol=%d error=%s", snapshot.Protocol, snapshot.Error))
			return notifyPollSlow
		}
		if snapshot.Epoch == cursor.epoch && snapshot.Revision <= cursor.revision && !repeated {
			continue
		}
		// matrix/hardened: a retained source-error publication is an explicit
		// unknown marker. Advance past it so recovery can drain later turns.
		if snapshot.Error != "" {
			d.logRuntimeSourceError(profile, errors.New(snapshot.Error))
			cursor.epoch, cursor.revision, cursor.unknown = snapshot.Epoch, snapshot.Revision, true
			d.observationCursors[profile] = cursor
			continue
		}
		if cursor.unknown && !repeated {
			d.initialized[profile] = false
			delete(d.lastJournaled, profile)
		}
		instances, _, err := storage.LoadWithGroups()
		if err != nil {
			d.logRuntimeSourceError(profile, err)
			return notifyPollSlow
		}
		byID := make(map[string]*Instance, len(instances))
		fence, err := storage.GetUpdatedAt()
		if err != nil {
			d.logRuntimeSourceError(profile, err)
			return notifyPollSlow
		}
		for _, inst := range instances {
			inst.SetRuntimeConsumer(true)
			inst.SetRuntimeMutationFence(fence)
			byID[inst.ID] = inst
		}
		statuses, substates := map[string]string{}, map[string]string{}
		hooks := make(map[string]*HookStatus)
		candidates := make(map[string]hookTransitionCandidate)
		for _, row := range snapshot.Sessions {
			inst := byID[row.ID]
			if inst == nil || row.Epoch != snapshot.Epoch || row.Revision > snapshot.Revision || !inst.ApplyRuntimeSnapshot(profile, row) {
				continue
			}
			statuses[row.ID] = normalizeStatusString(string(row.Status))
			substates[row.ID] = string(row.Substate)
			if row.HookKnown && row.Hook != nil {
				hs := *row.Hook
				hooks[row.ID] = &hs
				if candidate, ok := terminalHookTransitionCandidate(inst.Tool, &hs); ok {
					candidates[row.ID] = candidate
				}
			}
		}
		// Unavailable rows are unknown, not deletions or new terminal edges.
		// Preserve prior statuses until a matching complete observation arrives.
		for id := range byID {
			if _, ok := statuses[id]; !ok {
				if previous, known := d.lastStatus[profile][id]; known {
					statuses[id] = previous
				}
			}
		}
		interval = d.consumeObservedProfile(profile, instances, byID, statuses, substates, hooks, candidates)
		if !repeated {
			private := snapshot.Clone()
			cursor = runtimeObservationCursor{epoch: snapshot.Epoch, revision: snapshot.Revision, latest: &private}
			d.observationCursors[profile] = cursor
		}
	}
	return interval
}

func (d *TransitionDaemon) logRuntimeSourceError(profile string, err error) {
	if err == nil {
		err = fmt.Errorf("empty runtime observation")
	}
	// Existing rate-limited stall logging avoids a reconnect log flood.
	d.logProbeStall(profile, "", "runtime_source: "+err.Error())
}
