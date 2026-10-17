package session

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: measure the real consumer getter chain, including nested
// read-only verdicts, rather than a synthetic atomic load or runtimeView call.
type matrixRuntimeGetterResult struct {
	status                           Status
	substate, cachedSubstate         Substate
	detail, hook, description, model string
	remedy, nativeID, genericID      string
	ack, alive, aliveKnown           bool
	observedKnown, hookFresh         bool
	hookKnown, held, heldCached      bool
	canFork                          bool
	waiting, changed, activity       time.Time
	observed, hookAt                 time.Time
	evidence                         StatusEvidence
	background                       tmux.BackgroundWork
	port                             int
}

func matrixRuntimeGetterChain(i *Instance) matrixRuntimeGetterResult {
	var result matrixRuntimeGetterResult
	result.status = i.GetStatusThreadSafe()
	result.substate, result.cachedSubstate = i.Substate(), i.CachedSubstate()
	result.detail = i.SubstateDetail()
	result.ack = i.RuntimeAcknowledged()
	result.alive, result.aliveKnown = i.RuntimeTmuxAlive()
	result.waiting, result.changed = i.GetWaitingSince(), i.GetLastActivityTime()
	result.activity = i.LastActivityAt()
	result.observed, result.observedKnown = i.LastObservedActivity()
	result.evidence = i.StatusEvidence()
	result.hook, result.hookFresh = i.GetHookStatus()
	result.hookAt, result.hookKnown = i.LastHookActivityTime()
	result.description, result.model = i.GetAutoNameDescription(), i.LaunchModelID()
	result.held, result.remedy = i.IsAuthHeld()
	result.heldCached = i.AuthHeldCached()
	result.background = i.BackgroundWork()
	result.canFork = i.CanFork()
	result.nativeID, result.genericID = i.GetSessionIDFromTmux(), i.GetGenericSessionID()
	result.port = i.GetOpenCodePort()
	return result
}

func matrixRuntimeViewFixture(tb testing.TB) (*Instance, RuntimeSessionSnapshot) {
	tb.Helper()
	i := &Instance{ID: "runtime-view", Tool: "codex", Command: "codex", CodexSessionID: "native", Status: StatusRunning}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := RuntimeSessionSnapshot{
		ID: i.ID, Profile: DefaultProfile, Epoch: "view-epoch", Revision: 1, Identity: i.RuntimeIdentity(), ObservedAt: at,
		Status: StatusRunning, Substate: SubstateRunning, Detail: "working", Acknowledged: true,
		ActivityAt: at, LastChangeAt: at, ObservedActivityAt: at, ObservedActivityKnown: true, WaitingSince: at,
		TmuxAlive: true, TmuxAliveKnown: true, HookKnown: true, HookFresh: true,
		Hook:     &HookStatus{Status: "running", Sequence: 1, UpdatedAt: at, DoneSummary: "private"},
		Evidence: StatusEvidence{HookStatus: "running", HookAt: at},
		AuthHeld: true, AuthRemedy: "login", AuthHold: &AuthHoldRecord{InstanceID: i.ID, Reason: AuthHoldReasonLive, Timestamp: 1},
		BackgroundWork:  tmux.BackgroundWork{Kind: tmux.BackgroundKindWorkflow, Task: "build", Step: 1, Steps: 5},
		ClaudeSessionID: "claude-native", OpenCodePort: 1234, AutoNameDescription: "build project",
		GeminiAnalytics: &GeminiSessionAnalytics{TotalTurns: 1, LastFileModTime: at},
		LaunchModel:     ParseModelID("gpt-5"), ToolOptions: json.RawMessage(`{"model":"gpt-5"}`), CanFork: true, CanForkKnown: true,
	}
	if !i.ApplyRuntimeSnapshot(DefaultProfile, row) {
		tb.Fatal("fixture observation rejected")
	}
	return i, row
}

var matrixRuntimeGetterSink matrixRuntimeGetterResult

func BenchmarkMatrixRuntimeViewGetterChain(b *testing.B) {
	i, _ := matrixRuntimeViewFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		matrixRuntimeGetterSink = matrixRuntimeGetterChain(i)
	}
}

// matrix/hardened: published generations remain immutable while producer-owned
// inputs and public API results stay independently mutable.
func TestMatrixRuntimeViewPublicationAndPublicOwnership(t *testing.T) {
	i, input := matrixRuntimeViewFixture(t)
	retained, _ := i.runtimeView()
	input.Hook.DoneSummary = "transport edit"
	input.AuthHold.Reason = "transport edit"
	input.GeminiAnalytics.TotalTurns = 99
	input.ToolOptions[0] = 'x'
	output, ok := i.RuntimeReplica()
	if !ok || output.Hook.DoneSummary != "private" || output.AuthHold.Reason != AuthHoldReasonLive || output.GeminiAnalytics.TotalTurns != 1 || string(output.ToolOptions) != `{"model":"gpt-5"}` {
		t.Fatal("transport input aliases the publication")
	}
	output.Status, output.Hook.DoneSummary = StatusError, "public edit"
	output.AuthHold.Reason = "public edit"
	output.GeminiAnalytics.TotalTurns = 88
	output.ToolOptions[0] = 'y'
	hold := i.AuthHold()
	hold.Reason = "auth API edit"
	fresh, _ := i.RuntimeReplica()
	if fresh.Status != StatusRunning || fresh.Hook.DoneSummary != "private" || fresh.AuthHold.Reason != AuthHoldReasonLive || fresh.GeminiAnalytics.TotalTurns != 1 || string(fresh.ToolOptions) != `{"model":"gpt-5"}` || i.AuthHold().Reason != AuthHoldReasonLive {
		t.Fatal("public output aliases the publication")
	}
	next := fresh.Clone()
	next.Revision, next.ObservedAt, next.Detail = 2, fresh.ObservedAt.Add(time.Second), "next generation"
	next.Hook.DoneSummary = "next generation"
	if !i.ApplyRuntimeSnapshot(DefaultProfile, next) {
		t.Fatal("next observation rejected")
	}
	if retained.Revision != 1 || retained.Detail != "working" || retained.Hook.DoneSummary != "private" || i.SubstateDetail() != "next generation" {
		t.Fatal("publication changed a retained generation or failed to replace it")
	}
}

func TestMatrixRuntimeViewConcurrentPublicationAndPublicCopies(t *testing.T) {
	i, initial := matrixRuntimeViewFixture(t)
	const generations = 200
	var wg sync.WaitGroup
	failures := make(chan string, 5)
	start := make(chan struct{})
	readersReady := make(chan struct{}, 4)
	published := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(published)
		<-start
		for reader := 0; reader < 4; reader++ {
			<-readersReady
		}
		for revision := uint64(2); revision <= generations; revision++ {
			row := initial.Clone()
			row.Revision, row.ObservedAt = revision, initial.ObservedAt.Add(time.Duration(revision)*time.Second)
			row.Hook.Sequence = revision
			row.AuthHold.Timestamp = int64(revision)
			row.GeminiAnalytics.TotalTurns = int(revision)
			if !i.ApplyRuntimeSnapshot(DefaultProfile, row) {
				failures <- fmt.Sprintf("generation %d rejected", revision)
				return
			}
			// These buffers belong to the caller after Apply; writes must not race
			// with readers of the cloned publication.
			row.Hook.Sequence = 0
			row.AuthHold.Timestamp = -1
			row.GeminiAnalytics.TotalTurns = -1
			row.ToolOptions[0] = 'x'
		}
	}()
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			readersReady <- struct{}{}
			for n := 0; ; n++ {
				if n >= generations {
					select {
					case <-published:
						return
					default:
					}
				}
				result := matrixRuntimeGetterChain(i)
				if result.status != StatusRunning || result.substate != SubstateRunning || !result.alive || !result.aliveKnown || !result.held || result.background.Task != "build" || !result.canFork {
					failures <- "getter lost a published verdict"
					return
				}
				copy, ok := i.RuntimeReplica()
				if !ok || copy.Hook.Sequence != copy.Revision || copy.AuthHold.Timestamp != int64(copy.Revision) || copy.GeminiAnalytics.TotalTurns != int(copy.Revision) || string(copy.ToolOptions) != `{"model":"gpt-5"}` {
					failures <- "public replica contains mixed or caller-mutated fields"
					return
				}
				copy.Hook.Sequence, copy.AuthHold.Timestamp = 0, -2
				copy.GeminiAnalytics.TotalTurns = -2
				copy.ToolOptions[0] = 'y'
				hold := i.AuthHold()
				hold.Reason = "reader edit"
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	final, _ := i.RuntimeReplica()
	if final.Revision != generations || final.AuthHold.Reason != AuthHoldReasonLive || final.Hook.Sequence != generations {
		t.Fatal("public mutations escaped into the final publication")
	}
}
