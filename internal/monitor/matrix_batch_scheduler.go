package monitor

import (
	"context"
	"sync/atomic"
	"time"
)

// matrix/hardened: an actual pending capture group may wait briefly for adjacent
// worker completions, rather than turning each completion into a singleton.
// The window belongs to the group, not each reservation, and exists only while
// an active collection is asking for another worker. It creates no idle timer.
const matrixCaptureCoalesceWindow = 3 * time.Millisecond

type matrixBatchFlushReason uint8

const (
	matrixBatchNoFlush matrixBatchFlushReason = iota
	matrixBatchFull
	matrixBatchTimeout
	matrixBatchPoolSelf
	matrixBatchEnd
	matrixBatchCancel
	matrixBatchBudget
	matrixBatchNonCapture
	matrixBatchOverride
)

// CaptureBatchScheduleStats contains fixed, process-local scheduling aggregates.
// Group-size buckets are ordered 1..4. Logical groups precede socket partition;
// CaptureSocketGroups counts dispatched socket partitions, not tmux starts (a
// fresh raw cache or fallback may change the actual number of native commands).
// matrix/hardened: no argv, socket names, session IDs, content or history.
type CaptureBatchScheduleStats struct {
	LogicalGroups       [4]uint64 `json:"logicalGroups"`
	CaptureSocketGroups [4]uint64 `json:"captureSocketGroups"`
	Full                uint64    `json:"full"`
	Timeout             uint64    `json:"timeout"`
	PoolSelf            uint64    `json:"poolSelf"`
	End                 uint64    `json:"end"`
	Cancel              uint64    `json:"cancel"`
	Budget              uint64    `json:"budget"`
	NonCapture          uint64    `json:"nonCapture"`
	Override            uint64    `json:"override"`
	AdmissionWaits      uint64    `json:"admissionWaits"`
	AdmissionWaitNanos  uint64    `json:"admissionWaitNanos"`
	CoalescedSlots      uint64    `json:"coalescedSlots"`
}

// matrix/hardened: fixed arrays bound cardinality across all profiles/Servers;
// atomics let /status read aggregates without the collector's profile lock.
var matrixBatchScheduling struct {
	logical  [4]atomic.Uint64
	socket   [4]atomic.Uint64
	reasons  [matrixBatchOverride + 1]atomic.Uint64
	waits    atomic.Uint64
	waitNano atomic.Uint64
	joined   atomic.Uint64
}

func CaptureBatchSchedulingSnapshot() CaptureBatchScheduleStats {
	var out CaptureBatchScheduleStats
	for i := range out.LogicalGroups {
		out.LogicalGroups[i] = matrixBatchScheduling.logical[i].Load()
		out.CaptureSocketGroups[i] = matrixBatchScheduling.socket[i].Load()
	}
	out.Full = matrixBatchScheduling.reasons[matrixBatchFull].Load()
	out.Timeout = matrixBatchScheduling.reasons[matrixBatchTimeout].Load()
	out.PoolSelf = matrixBatchScheduling.reasons[matrixBatchPoolSelf].Load()
	out.End = matrixBatchScheduling.reasons[matrixBatchEnd].Load()
	out.Cancel = matrixBatchScheduling.reasons[matrixBatchCancel].Load()
	out.Budget = matrixBatchScheduling.reasons[matrixBatchBudget].Load()
	out.NonCapture = matrixBatchScheduling.reasons[matrixBatchNonCapture].Load()
	out.Override = matrixBatchScheduling.reasons[matrixBatchOverride].Load()
	out.AdmissionWaits = matrixBatchScheduling.waits.Load()
	out.AdmissionWaitNanos = matrixBatchScheduling.waitNano.Load()
	out.CoalescedSlots = matrixBatchScheduling.joined.Load()
	return out
}

func matrixRecordLogicalBatch(size int, reason matrixBatchFlushReason) {
	if size <= 0 || size > len(matrixBatchScheduling.logical) || reason <= matrixBatchNoFlush || reason > matrixBatchOverride {
		return
	}
	matrixBatchScheduling.logical[size-1].Add(1)
	matrixBatchScheduling.reasons[reason].Add(1)
}

func matrixRecordSocketBatch(size int) {
	if size > 0 && size <= len(matrixBatchScheduling.socket) {
		matrixBatchScheduling.socket[size-1].Add(1)
	}
}

type matrixBatchScheduler struct {
	ctx     context.Context
	budget  context.Context
	workers chan struct{}
	window  time.Duration
	firstAt time.Time
	onWait  func() // matrix/hardened: deterministic package-test seam; nil in production.
}

func (b *matrixBatchScheduler) groupStarted(at time.Time) { b.firstAt = at }
func (b *matrixBatchScheduler) groupFlushed()             { b.firstAt = time.Time{} }

func (b *matrixBatchScheduler) interrupted() matrixBatchFlushReason {
	if b.ctx.Err() != nil {
		return matrixBatchCancel
	}
	if b.budget.Err() != nil {
		return matrixBatchBudget
	}
	return matrixBatchNoFlush
}

// reserve returns a flush reason before blocking behind the group's own slots.
// After flushing, the caller may retry with pending=0 for the original ordinary
// bounded worker wait. Only successfully reserved slots enter original probes,
// which remain the sole owners of slot release.
func (b *matrixBatchScheduler) reserve(pending int) (bool, matrixBatchFlushReason) {
	if reason := b.interrupted(); reason != matrixBatchNoFlush {
		return false, reason
	}
	// A deadline also flushes when a worker becomes available after the window;
	// availability must not silently restart or extend the pending group's age.
	if pending > 0 && time.Until(b.firstAt.Add(b.window)) <= 0 {
		return false, matrixBatchTimeout
	}
	select {
	case b.workers <- struct{}{}:
		return true, matrixBatchNoFlush
	default:
	}
	if pending == 0 {
		select {
		case b.workers <- struct{}{}:
			return true, matrixBatchNoFlush
		case <-b.budget.Done():
			return false, b.interrupted()
		case <-b.ctx.Done():
			return false, b.interrupted()
		}
	}
	// A group owning the whole pool cannot await a running probe: none can
	// release a slot until that group starts (including pools of size 1..3).
	if pending >= cap(b.workers) {
		return false, matrixBatchPoolSelf
	}
	left := time.Until(b.firstAt.Add(b.window))
	if left <= 0 {
		return false, matrixBatchTimeout
	}
	timer := time.NewTimer(left)
	defer timer.Stop()
	started := time.Now()
	matrixBatchScheduling.waits.Add(1)
	defer func() { matrixBatchScheduling.waitNano.Add(uint64(time.Since(started))) }()
	if b.onWait != nil {
		b.onWait()
	}
	select {
	case b.workers <- struct{}{}:
		matrixBatchScheduling.joined.Add(1)
		return true, matrixBatchNoFlush
	case <-timer.C:
		return false, matrixBatchTimeout
	case <-b.budget.Done():
		return false, b.interrupted()
	case <-b.ctx.Done():
		return false, b.interrupted()
	}
}
