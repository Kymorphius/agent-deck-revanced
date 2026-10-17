package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: a replica preserves the full verdict without invoking local
// samplers, native binding writers, acknowledgment transitions or sidecar IO.
func TestRuntimeSnapshotCompletePrivateReplica(t *testing.T) {
	i := &Instance{ID: "replica", Tool: "codex", Command: "codex", CodexSessionID: "native", Status: StatusIdle, CreatedAt: time.Now().Add(-time.Hour)}
	now := time.Now()
	row := RuntimeSessionSnapshot{ID: i.ID, Profile: DefaultProfile, Epoch: "epoch-1", Revision: 1, Identity: i.RuntimeIdentity(), ObservedAt: now,
		Status: StatusRunning, Substate: SubstateUsageLimit, Detail: "retry at 20:15", ActivityAt: now.Add(-time.Second),
		ObservedActivityAt: now.Add(-2 * time.Second), ObservedActivityKnown: true, WaitingSince: now.Add(-time.Minute), WaitingSinceKnown: true,
		Acknowledged: true, TmuxAlive: true, TmuxAliveKnown: true, PaneTitle: "live task", PaneTitleKnown: true,
		Hook: &HookStatus{Status: "running", Event: "UserPromptSubmit", UpdatedAt: now, Sequence: 8, DoneSummary: "complete metadata"}, HookKnown: true, HookFresh: true,
		Evidence: StatusEvidence{HookStatus: "running", HookAt: now}, BackgroundWork: tmux.BackgroundWork{Kind: tmux.BackgroundKindWorkflow, Task: "build", Step: 0, Steps: 5},
		AuthHeld: true, AuthRemedy: "login", AuthHold: &AuthHoldRecord{InstanceID: i.ID, Reason: AuthHoldReasonLive, Timestamp: now.Unix()},
		GeminiAnalytics: &GeminiSessionAnalytics{TotalTurns: 3, LastFileModTime: now}, LaunchModel: ParseModelID("gpt-5"), ToolOptions: json.RawMessage(`{"model":"gpt-5"}`), CanFork: true, CanForkKnown: true}
	if !i.ApplyRuntimeSnapshot(DefaultProfile, row) {
		t.Fatal("valid complete snapshot rejected")
	}
	// Mutating transport-owned data cannot affect this consumer.
	row.Hook.DoneSummary = "changed"
	row.AuthHold.Reason = "changed"
	row.GeminiAnalytics.TotalTurns = 9
	row.ToolOptions[0] = 'x'
	if i.Substate() != SubstateUsageLimit || i.CachedSubstate() != SubstateUsageLimit || i.SubstateDetail() != "retry at 20:15" {
		t.Fatal("full quota verdict/details lost")
	}
	if !i.RuntimeAcknowledged() {
		t.Fatal("ack lost")
	}
	if alive, known := i.RuntimeTmuxAlive(); !alive || !known {
		t.Fatal("liveness known/value lost")
	}
	if i.GetStatusThreadSafe() != StatusRunning || !i.LastActivityAt().Equal(now.Add(-time.Second)) || !i.GetWaitingSince().Equal(row.WaitingSince) {
		t.Fatal("status/activity/waiting lost")
	}
	if at, known := i.LastObservedActivity(); !known || !at.Equal(row.ObservedActivityAt) {
		t.Fatal("confirmed activity lost")
	}
	if held, remedy := i.IsAuthHeld(); !held || remedy != "login" {
		t.Fatal("auth verdict lost")
	}
	if i.AuthHold().Reason != AuthHoldReasonLive || !i.AuthHeldCached() {
		t.Fatal("auth ownership lost")
	}
	if i.BackgroundWork().Step != 0 || i.BackgroundWork().Steps != 5 || i.LaunchModelID() != "gpt-5" {
		t.Fatal("background/model verdict lost")
	}
	copy, ok := i.RuntimeReplica()
	if !ok {
		t.Fatal("replica missing")
	}
	if copy.Hook.DoneSummary != "complete metadata" || copy.GeminiAnalytics.TotalTurns != 3 || !copy.GeminiAnalytics.LastFileModTime.Equal(now) || string(copy.ToolOptions) != `{"model":"gpt-5"}` {
		t.Fatal("input ownership leaked")
	}
	copy.Hook.Sequence = 99
	copy.AuthHold.Reason = "other"
	copy2, _ := i.RuntimeReplica()
	if copy2.Hook.Sequence != 8 || copy2.AuthHold.Reason != AuthHoldReasonLive {
		t.Fatal("output ownership leaked")
	}
	if err := i.UpdateStatus(); err != nil || i.Status != StatusRunning {
		t.Fatalf("consumer local sampling changed status: %v", err)
	}
	if i.CodexSessionID != "native" {
		t.Fatal("apply changed native identity")
	}
	if !i.CanFork() {
		t.Fatal("consumer reran filesystem capability lookup instead of the shared verdict")
	}
}

func TestRuntimeSnapshotIdentityRevisionAndMutationFences(t *testing.T) {
	i := &Instance{ID: "guard", Tool: "codex", Command: "codex", Status: StatusRunning, CodexSessionID: "thread-1"}
	now := time.Now()
	row := RuntimeSessionSnapshot{ID: i.ID, Profile: DefaultProfile, Epoch: "one", Revision: 5, Identity: i.RuntimeIdentity(), ObservedAt: now, Status: StatusWaiting}
	if !i.ApplyRuntimeSnapshot(DefaultProfile, row) {
		t.Fatal("seed rejected")
	}
	older := row
	older.Revision = 4
	older.ObservedAt = now.Add(time.Second)
	if i.ApplyRuntimeSnapshot(DefaultProfile, older) {
		t.Fatal("revision rewind accepted")
	}
	older = row
	older.Epoch = "old"
	older.ObservedAt = now.Add(-time.Second)
	if i.ApplyRuntimeSnapshot(DefaultProfile, older) {
		t.Fatal("retired epoch time rewind accepted")
	}
	wrong := row
	wrong.Profile = "other"
	if i.ApplyRuntimeSnapshot(DefaultProfile, wrong) {
		t.Fatal("cross profile accepted")
	}
	i.InvalidateRuntimeSnapshot()
	if i.ApplyRuntimeSnapshot(DefaultProfile, row) {
		t.Fatal("pre mutation probe accepted")
	}
	fresh := row
	fresh.ObservedAt = time.Now().Add(time.Second)
	fresh.Epoch = "new"
	fresh.Revision = 1
	if !i.ApplyRuntimeSnapshot(DefaultProfile, fresh) {
		t.Fatal("fresh daemon epoch rejected")
	}
	i.CodexSessionID = "thread-2"
	if i.ApplyRuntimeSnapshot(DefaultProfile, fresh) {
		t.Fatal("old native binding verdict accepted")
	}
	i.ExpectRuntimeSnapshot()
	if i.CachedSubstate() != SubstateNone || i.Substate() != SubstateNone {
		t.Fatal("missing row ran a local probe")
	}
	if _, known := i.RuntimeTmuxAlive(); known {
		t.Fatal("missing row assumed liveness")
	}
	i.SetRuntimeMutationFence(fresh.ObservedAt.Add(time.Second))
	fresh.Identity = i.RuntimeIdentity()
	if i.ApplyRuntimeSnapshot(DefaultProfile, fresh) {
		t.Fatal("fresh hydration mutation watermark ignored")
	}
}

func TestRuntimeSnapshotAckDoesNotHideNewTurn(t *testing.T) {
	i := &Instance{ID: "ack", Tool: "codex", Command: "codex", Status: StatusIdle}
	now := time.Now()
	row := RuntimeSessionSnapshot{ID: i.ID, Profile: DefaultProfile, Epoch: "one", Revision: 1, Identity: i.RuntimeIdentity(), ObservedAt: now, Status: StatusIdle, Acknowledged: true}
	if !i.ApplyRuntimeSnapshot(DefaultProfile, row) || !i.RuntimeAcknowledged() {
		t.Fatal("ack snapshot rejected")
	}
	row.Revision = 2
	row.ObservedAt = now.Add(time.Second)
	row.Status = StatusWaiting
	row.Acknowledged = false
	row.WaitingSince = now.Add(time.Second)
	if !i.ApplyRuntimeSnapshot(DefaultProfile, row) || i.RuntimeAcknowledged() {
		t.Fatal("ack incorrectly survived the new waiting turn")
	}
	nm := NewNotificationManager(6, false, false)
	nm.SyncFromInstances([]*Instance{i}, "")
	if !nm.Has(i.ID) {
		t.Fatal("new waiting turn suppressed")
	}
}

func TestRuntimeSnapshotStoppedIdentityAndRegistryReconcile(t *testing.T) {
	i := &Instance{ID: "registry", Tool: "shell", Command: "sh", Title: "old", Status: StatusRunning, lastIdleCheck: time.Now()}
	candidate := &Instance{ID: i.ID, Tool: i.Tool, Command: i.Command, Title: "new", Status: StatusIdle}
	if !i.ReconcileRuntimeRegistry(candidate) || i.Title != "new" || i.lastIdleCheck.IsZero() {
		t.Fatal("presentation reload reset live sampler")
	}
	candidate.Status = StatusStopped
	if i.ReconcileRuntimeRegistry(candidate) {
		t.Fatal("explicit stop retained a running sampler")
	}
	candidate.Status = StatusRunning
	candidate.Account = "other"
	if i.ReconcileRuntimeRegistry(candidate) {
		t.Fatal("account switch reused old sampler")
	}
}

// matrix/hardened: metadata helpers historically execute with i.mu held. A
// replica eligibility check must never try to re-lock that producer mutex.
func TestRuntimeSnapshotLegacyLockedMetadataGetters(t *testing.T) {
	i := &Instance{ID: "locked", Tool: "shell", Status: StatusRunning, CreatedAt: time.Now()}
	i.mu.Lock()
	done := make(chan struct{}, 1)
	go func() { _ = i.GetLastActivityTime(); _ = i.GetSessionIDFromTmux(); done <- struct{}{} }()
	select {
	case <-done:
		i.mu.Unlock()
	case <-time.After(time.Second):
		i.mu.Unlock()
		<-done
		t.Fatal("metadata getter recursively acquired producer mutex")
	}
}

func TestRuntimeSnapshotLaunchTimestampUsesPersistedPrecision(t *testing.T) {
	i := &Instance{ID: "time", Tool: "codex", Command: "codex", Status: StatusRunning, LastStartedAt: time.Now()}
	candidate := &Instance{ID: i.ID, Tool: i.Tool, Command: i.Command, Status: StatusRunning, LastStartedAt: i.LastStartedAt.Truncate(time.Second)}
	if i.RuntimeIdentity() != candidate.RuntimeIdentity() {
		t.Fatal("saved seconds and local start nanoseconds have different launch identities")
	}
}

func TestRuntimeSnapshotExactBindingConfirmationSupportsForkPreflight(t *testing.T) {
	i := &Instance{ID: "confirmed-fork", Tool: "claude", Command: "claude", Status: StatusRunning, ClaudeSessionID: "exact-native", ClaudeDetectedAt: time.Now().Add(-time.Hour)}
	now := time.Now()
	row := RuntimeSessionSnapshot{ID: i.ID, Profile: DefaultProfile, Epoch: "one", Revision: 1, Identity: i.RuntimeIdentity(), ObservedAt: now, Status: StatusRunning, ClaudeSessionID: i.ClaudeSessionID, ClaudeDetectedAt: now, CanFork: true, CanForkKnown: true}
	if !i.ApplyRuntimeSnapshot(DefaultProfile, row) || !i.canForkCurrentSource() {
		t.Fatal("observed exact binding lost fresh action confirmation")
	}
	row.Revision = 2
	row.ObservedAt = now.Add(time.Second)
	row.ClaudeSessionID = "foreign"
	row.ClaudeDetectedAt = now.Add(time.Hour)
	if !i.ApplyRuntimeSnapshot(DefaultProfile, row) {
		t.Fatal("verdict rejected")
	}
	if i.ClaudeSessionID != "exact-native" || !i.ClaudeDetectedAt.Equal(now) {
		t.Fatal("foreign metadata rewrote native binding confirmation")
	}
}

func TestRuntimeSnapshotTargetedWritesUseOwningProfile(t *testing.T) {
	owner, other := newTestStorage(t), newTestStorage(t)
	i := &Instance{ID: "same-id", Tool: "claude", Command: "claude", Status: StatusIdle, ProjectPath: t.TempDir()}
	seedInstanceRow(t, owner.db, i, `{}`)
	seedInstanceRow(t, other.db, i, `{}`)
	i.restartDB.Store(owner.db)
	prior := statedb.GetGlobal()
	statedb.SetGlobal(other.db)
	t.Cleanup(func() { statedb.SetGlobal(prior) })
	i.MarkAccessed()
	i.lastActivityAt = time.Now().Truncate(time.Second)
	i.persistLastActivity(true)
	i.bindClaudeSessionFromHook("claude-native", "test", "SessionStart", "bind")
	i.bindCodexSessionFromHook("codex-native", "session-start")
	i.bindGeminiSessionFromHook("gemini-native", "SessionStart")
	i.hookLag = hookLagRecord{HookTS: time.Now().Unix()}
	i.persistHookLag()
	i.Tool = "custom"
	i.Command = "custom"
	i.persistGenericSessionIDIfChanged("custom-native")
	rowsA, _ := owner.db.LoadInstances()
	rowsB, _ := other.db.LoadInstances()
	if len(rowsA) != 1 || len(rowsB) != 1 {
		t.Fatal("fixture row lost")
	}
	var a, b map[string]any
	_ = json.Unmarshal(rowsA[0].ToolData, &a)
	_ = json.Unmarshal(rowsB[0].ToolData, &b)
	for _, key := range []string{"claude_session_id", "codex_session_id", "gemini_session_id", "generic_session_id", "last_activity_at", "hook_lag"} {
		if _, ok := a[key]; !ok {
			t.Fatalf("owner missing %s: %s", key, rowsA[0].ToolData)
		}
		if _, ok := b[key]; ok {
			t.Fatalf("other profile changed %s: %s", key, rowsB[0].ToolData)
		}
	}
	if rowsA[0].LastAccessed.IsZero() || !rowsB[0].LastAccessed.IsZero() {
		t.Fatal("access write crossed profiles")
	}
	linksA, _ := owner.db.ListSessionLinks(i.ID)
	linksB, _ := other.db.ListSessionLinks(i.ID)
	if len(linksA) == 0 || len(linksB) != 0 {
		t.Fatal("native recall links crossed profiles")
	}
	// Hydration binds ownership before any monitor sampling takes place.
	loaded, _, err := owner.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("hydrate: %v", err)
	}
	if loaded[0].stateDB() != owner.db || loaded[0].runtimeProfile != "_test" {
		t.Fatal("hydration lost owning profile")
	}
}

// matrix/hardened: a late producer rebind must not overwrite an operator's
// committed account/command/native binding after its probe began.
func TestRuntimeSnapshotProducerWriteMutationFence(t *testing.T) {
	s := newTestStorage(t)
	i := &Instance{ID: "late-source", Tool: "codex", Command: "codex", Status: StatusRunning, ProjectPath: t.TempDir()}
	if err := s.SaveWithGroups([]*Instance{i}, NewGroupTree([]*Instance{i})); err != nil {
		t.Fatal(err)
	}
	fence, err := s.GetUpdatedAt()
	if err != nil {
		t.Fatal(err)
	}
	i.SetRuntimeProducer(true)
	i.SetRuntimeProducerFence(fence.UnixNano())
	i.bindCodexSessionFromHook("first-native", "session-start")
	loaded, _, err := s.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v", err)
	}
	loaded[0].Command = "new-codex"
	loaded[0].Account = "new-account"
	loaded[0].CodexSessionID = "operator-native"
	if err := s.SaveWithGroups(loaded, NewGroupTree(loaded)); err != nil {
		t.Fatal(err)
	}
	i.bindCodexSessionFromHook("late-native", "session-start")
	i.lastActivityAt = time.Now()
	i.persistLastActivity(true)
	rows, err := s.db.LoadInstances()
	if err != nil || len(rows) != 1 {
		t.Fatalf("read: %v", err)
	}
	var data struct {
		Codex    string `json:"codex_session_id"`
		Activity int64  `json:"last_activity_at"`
	}
	_ = json.Unmarshal(rows[0].ToolData, &data)
	if data.Codex != "operator-native" || rows[0].Command != "new-codex" || rows[0].Account != "new-account" || data.Activity != 0 {
		t.Fatalf("late producer overwrote operator edit: row=%+v tool=%s", rows[0], rows[0].ToolData)
	}
}

// matrix/hardened: a cold native discovery becomes durable before the producer
// publishes its identity. A consumer reload then accepts the complete verdict.
func TestRuntimeSnapshotColdNativeDiscoveryAndTaint(t *testing.T) {
	s := newTestStorage(t)
	i := &Instance{ID: "cold-native", Tool: "claude", Command: "claude", Status: StatusRunning, ProjectPath: t.TempDir(), GroupPath: DefaultGroupPath}
	if err := s.SaveWithGroups([]*Instance{i}, NewGroupTree([]*Instance{i})); err != nil {
		t.Fatal(err)
	}
	fence, err := s.GetUpdatedAt()
	if err != nil {
		t.Fatal(err)
	}
	i.SetRuntimeProducer(true)
	i.SetRuntimeProducerFence(fence.UnixNano())
	i.ClaudeSessionID = "discovered"
	i.ClaudeDetectedAt = time.Now()
	i.claudeSessionIDsFromDiskScan = map[string]bool{"discovered": true}
	row := i.RuntimeSnapshot("_test", "epoch", 1, time.Now())
	if row.Error != "" || row.ClaudeSessionIDVerified {
		t.Fatalf("native discovery/taint: %+v", row)
	}
	loaded, _, err := s.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v", err)
	}
	if loaded[0].ClaudeSessionID != "discovered" || !loaded[0].claudeSessionIDIsUnverified() {
		t.Fatal("cold native identity/taint not durable")
	}
	if !loaded[0].ApplyRuntimeSnapshot("_test", row) {
		t.Fatalf("consumer rejected persisted identity: source=%s consumer=%s", row.Identity, loaded[0].RuntimeIdentity())
	}
	if !i.ReconcileRuntimeRegistry(loaded[0]) {
		t.Fatal("native persistence unnecessarily discarded live sampler")
	}
	if err := i.persistRuntimeNativeBindings(); err != nil {
		t.Fatal(err)
	}
	// A completed registry mutation invalidates the outstanding producer fence.
	if err := s.db.Touch(); err != nil {
		t.Fatal(err)
	}
	i.ClaudeSessionID = "late-discovery"
	late := i.RuntimeSnapshot("_test", "epoch", 2, time.Now())
	if late.Error == "" {
		t.Fatal("obsolete native discovery was published without an error")
	}
	loaded, _, _ = s.LoadWithGroups()
	if loaded[0].ClaudeSessionID != "discovered" {
		t.Fatal("obsolete native discovery overwrote registry")
	}
}
