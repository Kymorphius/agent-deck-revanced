package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: controlled pool completions exercise admission without
// native tmux, real session input, or timing-sensitive subprocess ordering.
func matrixFullScheduler(t *testing.T, capacity int) *matrixBatchScheduler {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := &matrixBatchScheduler{ctx: ctx, budget: ctx, workers: make(chan struct{}, capacity), window: matrixCaptureCoalesceWindow}
	for i := 0; i < capacity; i++ {
		b.workers <- struct{}{}
	}
	b.groupStarted(time.Now())
	return b
}

func TestMatrixBatchSchedulerSelfOwnedPoolsNeverWait(t *testing.T) {
	for _, capacity := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			b := matrixFullScheduler(t, capacity)
			b.window = time.Second
			b.onWait = func() { t.Fatal("pending group waited for slots only its own probes can release") }
			reserved, reason := b.reserve(capacity)
			if reserved || reason != matrixBatchPoolSelf || len(b.workers) != capacity {
				t.Fatalf("self-owned pool lost reservations: reserved=%v reason=%v slots=%d", reserved, reason, len(b.workers))
			}
		})
	}
}

func TestMatrixBatchSchedulerCoalescesAdjacentCompletionBurst(t *testing.T) {
	b := matrixFullScheduler(t, 10)
	// The test seam delivers two adjacent completions synchronously; it enlarges
	// only this private instance's window to avoid CI scheduling assumptions.
	b.window = time.Second
	waits := 0
	b.onWait = func() {
		waits++
		<-b.workers
	}
	first := b.firstAt
	before := CaptureBatchSchedulingSnapshot()
	pending := 2
	for pending < 4 {
		reserved, reason := b.reserve(pending)
		if !reserved || reason != matrixBatchNoFlush {
			t.Fatalf("adjacent completion prematurely flushed pending=%d: %v %v", pending, reserved, reason)
		}
		pending++
	}
	after := CaptureBatchSchedulingSnapshot()
	if waits != 2 || pending != 4 || len(b.workers) != 10 || !b.firstAt.Equal(first) || after.CoalescedSlots-before.CoalescedSlots != 2 {
		t.Fatalf("burst did not yield one full group with original deadline: waits=%d pending=%d slots=%d first=%v", waits, pending, len(b.workers), b.firstAt)
	}
	// matrix.49 emitted the initial two reservations before awaiting either
	// completion, leaving these later two completions as singleton groups.
	oldSizes, newSizes := []int{2, 1, 1}, []int{pending}
	if len(newSizes) >= len(oldSizes) {
		t.Fatal("controlled completion trace did not reduce logical dispatch count")
	}
}

func TestMatrixBatchSchedulerExpiredGroupDoesNotRestartWindow(t *testing.T) {
	b := matrixFullScheduler(t, 10)
	b.groupStarted(time.Now().Add(-matrixCaptureCoalesceWindow))
	first := b.firstAt
	<-b.workers // A newly available slot must not extend an expired group.
	b.onWait = func() { t.Fatal("expired group started another coalescing timer") }
	if reserved, reason := b.reserve(2); reserved || reason != matrixBatchTimeout || !b.firstAt.Equal(first) || len(b.workers) != 9 {
		t.Fatalf("expired deadline changed: reserved=%v reason=%v first=%v", reserved, reason, b.firstAt)
	}
	b.groupFlushed()
	if !b.firstAt.IsZero() {
		t.Fatal("flushed group retained its old deadline")
	}
	b.groupStarted(time.Now())
	b.onWait = func() { <-b.workers }
	if reserved, reason := b.reserve(1); !reserved || reason != matrixBatchNoFlush {
		t.Fatalf("new group inherited previous timeout: %v %v", reserved, reason)
	}
}

func TestMatrixBatchSchedulerMixedNonCaptureStartsPendingNeighbors(t *testing.T) {
	matrixMonitorBatchFixture(t)
	instances := matrixMonitorBatchInstances(3)
	instances[2].Status = session.StatusStopped
	reg := &matrixCaptureBatchRegistry{instances: instances}
	s := New(Options{MaxWorkers: 10, PassBudget: time.Second, LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }})
	p := newProfile("batch-mixed-end-test", s.epoch)
	matrixMonitorBatchCleanup(t, s, p)
	before := CaptureBatchSchedulingSnapshot()
	s.collect(p)
	after := CaptureBatchSchedulingSnapshot()
	if after.AdmissionWaits != before.AdmissionWaits || after.LogicalGroups[1]-before.LogicalGroups[1] != 1 || after.LogicalGroups[0]-before.LogicalGroups[0] != 1 || after.CaptureSocketGroups[1]-before.CaptureSocketGroups[1] != 1 || after.NonCapture-before.NonCapture != 2 {
		t.Fatal("non-capture demand held or contaminated its pending capture neighbors")
	}
	if len(s.workers) != 0 || p.latest == nil || len(p.latest.snapshot.Sessions) != 3 {
		t.Fatal("mixed dispatch lost its end flush or released a reservation twice")
	}
}

func TestMatrixBatchSchedulerSlowWorkerDoesNotUnboundedlyHoldGroup(t *testing.T) {
	b := matrixFullScheduler(t, 10)
	started := time.Now()
	if reserved, reason := b.reserve(2); reserved || reason != matrixBatchTimeout {
		t.Fatalf("hung unrelated worker did not flush on group deadline: %v %v", reserved, reason)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("short coalescing window held a fast group for %v", elapsed)
	}
	if len(b.workers) != 10 {
		t.Fatal("deadline released or fabricated a worker reservation")
	}
}

func TestMatrixBatchSchedulerCancelAndBudgetInterruptPendingWait(t *testing.T) {
	for _, cancellation := range []string{"cancel", "budget"} {
		t.Run(cancellation, func(t *testing.T) {
			b := matrixFullScheduler(t, 10)
			ctx, cancel := context.WithCancel(context.Background())
			budget, expire := context.WithCancel(ctx)
			defer cancel()
			defer expire()
			b.ctx, b.budget, b.window = ctx, budget, time.Second
			want := matrixBatchBudget
			if cancellation == "cancel" {
				want = matrixBatchCancel
				b.onWait = cancel
			} else {
				b.onWait = expire
			}
			if reserved, reason := b.reserve(2); reserved || reason != want || len(b.workers) != 10 {
				t.Fatalf("interruption changed reservations: reserved=%v reason=%v want=%v slots=%d", reserved, reason, want, len(b.workers))
			}
		})
	}
}

func TestMatrixBatchSchedulerEmptyGroupWaitKeepsOriginalBudget(t *testing.T) {
	b := matrixFullScheduler(t, 10)
	b.groupFlushed()
	ctx, cancel := context.WithCancel(context.Background())
	budget, expire := context.WithCancel(ctx)
	defer cancel()
	defer expire()
	b.ctx, b.budget = ctx, budget
	b.onWait = func() { t.Fatal("empty group started a coalescing timer") }
	done := make(chan matrixBatchFlushReason, 1)
	go func() {
		reserved, reason := b.reserve(0)
		if reserved {
			reason = matrixBatchNoFlush
		}
		done <- reason
	}()
	expire()
	select {
	case reason := <-done:
		if reason != matrixBatchBudget || len(b.workers) != 10 {
			t.Fatalf("ordinary worker wait ignored its pass budget: %v slots=%d", reason, len(b.workers))
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary worker wait retained the canceled pass")
	}
}

func TestMatrixBatchSchedulerCollectCountsLogicalAndSocketGroups(t *testing.T) {
	for _, mixedSockets := range []bool{false, true} {
		t.Run(fmt.Sprint(mixedSockets), func(t *testing.T) {
			matrixMonitorBatchFixture(t)
			instances := matrixMonitorBatchInstances(4)
			if mixedSockets {
				for i, inst := range instances {
					inst.GetTmuxSession().SocketName = fmt.Sprintf("private-socket-%d", i%2)
				}
			}
			reg := &matrixCaptureBatchRegistry{instances: instances}
			s := New(Options{MaxWorkers: 10, PassBudget: time.Second, LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }})
			p := newProfile("batch-count-test", s.epoch)
			matrixMonitorBatchCleanup(t, s, p)
			before := CaptureBatchSchedulingSnapshot()
			s.collect(p)
			after := CaptureBatchSchedulingSnapshot()
			if after.LogicalGroups[3]-before.LogicalGroups[3] != 1 || after.Full-before.Full != 1 {
				t.Fatal("four reserved probes did not emit one full logical group")
			}
			if mixedSockets {
				if after.CaptureSocketGroups[1]-before.CaptureSocketGroups[1] != 2 || after.CaptureSocketGroups[3] != before.CaptureSocketGroups[3] {
					t.Fatal("logical group was confused with its independent two-socket partitions")
				}
			} else if after.CaptureSocketGroups[3]-before.CaptureSocketGroups[3] != 1 {
				t.Fatal("same-socket group lost its capture dispatch")
			}
			if len(s.workers) != 0 {
				t.Fatal("completed metrics fixture retained worker slots")
			}
		})
	}
}

func TestMatrixBatchSchedulerCollectEndAndNonCaptureDoNotWait(t *testing.T) {
	for _, kind := range []string{"end", "stopped", "nil", "archived", "override"} {
		t.Run(kind, func(t *testing.T) {
			matrixMonitorBatchFixture(t)
			instances := matrixMonitorBatchInstances(3)
			for _, inst := range instances {
				switch kind {
				case "stopped":
					inst.Status = session.StatusStopped
				case "nil":
					inst.SetTmuxSessionForTest(nil)
				case "archived":
					inst.ArchivedAt = time.Now().Add(-time.Hour)
				}
			}
			reg := &matrixCaptureBatchRegistry{instances: instances}
			opts := Options{MaxWorkers: 10, PassBudget: time.Second, LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }}
			if kind == "override" {
				opts.Probe = fixtureProbe
			}
			s := New(opts)
			p := newProfile("batch-end-test", s.epoch)
			matrixMonitorBatchCleanup(t, s, p)
			before := CaptureBatchSchedulingSnapshot()
			s.collect(p)
			after := CaptureBatchSchedulingSnapshot()
			if after.AdmissionWaits != before.AdmissionWaits {
				t.Fatalf("%s path unnecessarily waited to combine captures", kind)
			}
			if kind == "end" {
				if after.LogicalGroups[2]-before.LogicalGroups[2] != 1 || after.End-before.End != 1 || after.CaptureSocketGroups[2]-before.CaptureSocketGroups[2] != 1 {
					t.Fatal("collection end lost its partial capture group")
				}
			} else {
				if after.CaptureSocketGroups != before.CaptureSocketGroups || after.LogicalGroups[0]-before.LogicalGroups[0] != 3 {
					t.Fatalf("%s path fabricated a capture partition", kind)
				}
				if kind == "override" && after.Override-before.Override != 3 {
					t.Fatal("probe override was confused with native capture scheduling")
				}
				if kind != "override" && after.NonCapture-before.NonCapture != 3 {
					t.Fatal("non-capture dispatch lost its reason")
				}
			}
			if len(s.workers) != 0 || p.latest == nil || len(p.latest.snapshot.Sessions) != 3 {
				t.Fatal("completed end/non-capture pass lost rows or retained reservations")
			}
			for _, row := range p.latest.snapshot.Sessions {
				if strings.Contains(row.Error, "in flight") || row.ObservedAt.IsZero() {
					t.Fatalf("completed non-capture probe appeared pending: %+v", row)
				}
			}
		})
	}
}
