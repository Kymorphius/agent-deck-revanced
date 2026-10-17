package session

import (
	"testing"
	"time"
)

// matrix/hardened: retained Codex sources distinguish an old completion from a
// new turn, and apply explicit mark-unread without repeating the ack tuple.
func TestRuntimeProducerCodexAcknowledgmentSurvivesRepeatedCompletion(t *testing.T) {
	i := NewInstanceWithTool("runtime-ack", t.TempDir(), "codex")
	if err := i.tmuxSession.Start("sleep 3600"); err != nil {
		t.Skipf("private tmux fixture unavailable: %v", err)
	}
	t.Cleanup(func() { _ = i.tmuxSession.Kill() })
	i.SetRuntimeProducerFence(0)
	i.Status = StatusRunning
	completed := time.Now().Add(-time.Second)
	ackAt := time.Now()
	i.SetRuntimeProducerAcknowledgment(true, ackAt)
	hs := &HookStatus{Status: "waiting", Event: "agent-turn-complete", UpdatedAt: completed, CodexCompletedGeneration: "turn-a"}
	i.UpdateHookStatus(hs)
	for n := 0; n < 3; n++ {
		if err := i.updateStatus(nil, false); err != nil {
			t.Fatal(err)
		}
		if i.GetStatusThreadSafe() != StatusIdle || !i.tmuxSession.IsAcknowledged() {
			t.Fatalf("same completed turn reset ack on pass %d: status=%s", n, i.GetStatusThreadSafe())
		}
		i.UpdateHookStatus(hs)
	}
	row := i.RuntimeSnapshot(DefaultProfile, "ack-epoch", 1, time.Now())
	if !row.Acknowledged || !row.AcknowledgedAt.Equal(ackAt) {
		t.Fatal("ack timestamp not preserved on wire")
	}
	// Mark unread creates a distinct tuple and forces re-evaluation even while
	// the idle tier otherwise throttles unchanged window activity.
	i.SetRuntimeProducerAcknowledgment(false, ackAt.Add(time.Millisecond))
	if err := i.updateStatus(nil, false); err != nil {
		t.Fatal(err)
	}
	if i.GetStatusThreadSafe() != StatusWaiting || i.tmuxSession.IsAcknowledged() {
		t.Fatal("mark unread failed to restore waiting")
	}
	i.SetRuntimeProducerAcknowledgment(true, ackAt.Add(2*time.Millisecond))
	hs2 := *hs
	hs2.UpdatedAt = ackAt.Add(3 * time.Millisecond)
	hs2.CodexCompletedGeneration = "turn-b"
	i.UpdateHookStatus(&hs2)
	if err := i.updateStatus(nil, false); err != nil {
		t.Fatal(err)
	}
	if i.GetStatusThreadSafe() != StatusWaiting || i.tmuxSession.IsAcknowledged() {
		t.Fatal("new completed turn retained old ack")
	}
	// The next unchanged DB tuple must not re-acknowledge the fresh turn.
	i.SetRuntimeProducerAcknowledgment(true, ackAt.Add(2*time.Millisecond))
	if i.tmuxSession.IsAcknowledged() {
		t.Fatal("unchanged producer tuple replay hid new turn")
	}
}

func TestRuntimeProducerLegacyAckKeepsFirstHookBaseline(t *testing.T) {
	i := NewInstanceWithTool("runtime-legacy-ack", t.TempDir(), "codex")
	i.SetRuntimeProducerFence(0)
	i.SetRuntimeProducerAcknowledgment(true, time.Time{})
	first := time.Now()
	i.UpdateHookStatus(&HookStatus{Status: "waiting", Event: "agent-turn-complete", UpdatedAt: first})
	if !i.tmuxSession.IsAcknowledged() {
		t.Fatal("legacy ack lost on first hook baseline")
	}
	i.UpdateHookStatus(&HookStatus{Status: "waiting", Event: "agent-turn-complete", UpdatedAt: first.Add(time.Millisecond)})
	if i.tmuxSession.IsAcknowledged() {
		t.Fatal("legacy ack hid subsequent waiting observation")
	}
}

func TestRuntimeProducerCodexNewGenerationWithinAckSecond(t *testing.T) {
	i := NewInstanceWithTool("runtime-coarse-ack", t.TempDir(), "codex")
	i.SetRuntimeProducerFence(0)
	second := time.Now().Truncate(time.Second)
	first := &HookStatus{Status: "waiting", Event: "agent-turn-complete", UpdatedAt: second, CodexCompletedGeneration: "one"}
	i.UpdateHookStatus(first)
	i.SetRuntimeProducerAcknowledgment(true, second.Add(200*time.Millisecond))
	i.UpdateHookStatus(first)
	if !i.tmuxSession.IsAcknowledged() {
		t.Fatal("repeated generation cleared ack")
	}
	next := *first
	next.CodexCompletedGeneration = "two"
	i.UpdateHookStatus(&next)
	if i.tmuxSession.IsAcknowledged() {
		t.Fatal("new Codex generation inside the same timestamp second retained ack")
	}
	// A cold monitor's first generation can predate the stored ack even though
	// both fall within one timestamp second; it remains a baseline.
	cold := NewInstanceWithTool("runtime-coarse-cold", t.TempDir(), "codex")
	cold.SetRuntimeProducerFence(0)
	cold.SetRuntimeProducerAcknowledgment(true, second.Add(200*time.Millisecond))
	cold.UpdateHookStatus(first)
	if !cold.tmuxSession.IsAcknowledged() {
		t.Fatal("cold first generation reset a later explicit ack")
	}
}
