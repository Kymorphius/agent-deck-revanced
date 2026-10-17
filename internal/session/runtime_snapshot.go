package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: the monitor protocol carries complete observed state, not
// the coarse SQLite status shared by older claim_polling consumers.
const RuntimeProtocolVersion = 1

type RuntimeObservationSource interface {
	Snapshot(context.Context, string) (*RuntimeProfileSnapshot, error)
	Refresh(context.Context, string) error
	Observations(context.Context, string, string, uint64) ([]RuntimeProfileSnapshot, error)
}

// matrix/hardened: an explicit gap differs from a transient source failure.
// The transport implements this marker without introducing a package cycle.
type RuntimeObservationGap interface {
	error
	RuntimeObservationGap() bool
}

type RuntimeProfileSnapshot struct {
	Protocol   int                      `json:"protocol"`
	Epoch      string                   `json:"epoch"`
	Revision   uint64                   `json:"revision"`
	Profile    string                   `json:"profile"`
	ObservedAt time.Time                `json:"observed_at"`
	Sessions   []RuntimeSessionSnapshot `json:"sessions"`
	Error      string                   `json:"error,omitempty"`
}

// RuntimeSessionSnapshot is a value-only verdict. ObservedAt is the beginning
// of the source probe, so a slow pre-mutation probe cannot overwrite an edit.
type RuntimeSessionSnapshot struct {
	ID                      string                  `json:"id"`
	Profile                 string                  `json:"profile"`
	Epoch                   string                  `json:"epoch"`
	Revision                uint64                  `json:"revision"`
	Identity                string                  `json:"identity"`
	ObservedAt              time.Time               `json:"observed_at"`
	Error                   string                  `json:"error,omitempty"`
	Status                  Status                  `json:"status"`
	Substate                Substate                `json:"substate"`
	Detail                  string                  `json:"detail,omitempty"`
	Evidence                StatusEvidence          `json:"evidence"`
	Hook                    *HookStatus             `json:"hook,omitempty"`
	HookKnown               bool                    `json:"hook_known"`
	HookFresh               bool                    `json:"hook_fresh"`
	ActivityAt              time.Time               `json:"activity_at"`
	ObservedActivityAt      time.Time               `json:"observed_activity_at"`
	ObservedActivityKnown   bool                    `json:"observed_activity_known"`
	LastChangeAt            time.Time               `json:"last_change_at"`
	WaitingSince            time.Time               `json:"waiting_since"`
	WaitingSinceKnown       bool                    `json:"waiting_since_known"`
	Acknowledged            bool                    `json:"acknowledged"`
	AcknowledgedAt          time.Time               `json:"acknowledged_at"`
	TmuxAlive               bool                    `json:"tmux_alive"`
	TmuxAliveKnown          bool                    `json:"tmux_alive_known"`
	PaneTitle               string                  `json:"pane_title,omitempty"`
	PaneTitleKnown          bool                    `json:"pane_title_known"`
	PaneTitleObservedAt     time.Time               `json:"pane_title_observed_at"`
	BackgroundWork          tmux.BackgroundWork     `json:"background_work"`
	AuthHeld                bool                    `json:"auth_held"`
	AuthRemedy              string                  `json:"auth_remedy,omitempty"`
	AuthHold                *AuthHoldRecord         `json:"auth_hold,omitempty"`
	ClaudeSessionID         string                  `json:"claude_session_id,omitempty"`
	ClaudeDetectedAt        time.Time               `json:"claude_detected_at"`
	ClaudeSessionIDVerified bool                    `json:"claude_session_id_verified"`
	CodexSessionID          string                  `json:"codex_session_id,omitempty"`
	CodexDetectedAt         time.Time               `json:"codex_detected_at"`
	GeminiSessionID         string                  `json:"gemini_session_id,omitempty"`
	GeminiDetectedAt        time.Time               `json:"gemini_detected_at"`
	OpenCodeSessionID       string                  `json:"opencode_session_id,omitempty"`
	OpenCodeDetectedAt      time.Time               `json:"opencode_detected_at"`
	OpenCodePort            int                     `json:"opencode_port,omitempty"`
	CopilotSessionID        string                  `json:"copilot_session_id,omitempty"`
	CopilotDetectedAt       time.Time               `json:"copilot_detected_at"`
	CopilotModel            string                  `json:"copilot_model,omitempty"`
	HermesSessionID         string                  `json:"hermes_session_id,omitempty"`
	HermesHookGeneration    string                  `json:"hermes_hook_generation,omitempty"`
	DeepSeekSessionID       string                  `json:"deepseek_session_id,omitempty"`
	PiSessionID             string                  `json:"pi_session_id,omitempty"`
	PiSessionPath           string                  `json:"pi_session_path,omitempty"`
	GenericSessionID        string                  `json:"generic_session_id,omitempty"`
	GenericDetectedAt       time.Time               `json:"generic_detected_at"`
	GenericSessionTool      string                  `json:"generic_session_tool,omitempty"`
	GenericSessionCommand   string                  `json:"generic_session_command,omitempty"`
	GenericSessionLocation  string                  `json:"generic_session_location,omitempty"`
	GeminiModel             string                  `json:"gemini_model,omitempty"`
	GeminiAnalytics         *GeminiSessionAnalytics `json:"gemini_analytics,omitempty"`
	LatestPrompt            string                  `json:"latest_prompt,omitempty"`
	AutoNameDescription     string                  `json:"auto_name_description,omitempty"`
	LaunchModel             ModelInfo               `json:"launch_model"`
	ToolOptions             json.RawMessage         `json:"tool_options,omitempty"`
	CanFork                 bool                    `json:"can_fork"`
	CanForkKnown            bool                    `json:"can_fork_known"`
}

// Clone gives each consumer private mutable pointer/slice fields.
func (r RuntimeSessionSnapshot) Clone() RuntimeSessionSnapshot {
	r.ToolOptions = append(json.RawMessage(nil), r.ToolOptions...)
	if r.Hook != nil {
		v := *r.Hook
		r.Hook = &v
	}
	if r.AuthHold != nil {
		v := *r.AuthHold
		r.AuthHold = &v
	}
	if r.GeminiAnalytics != nil {
		// Analytics is a value struct; preserve its private cache timestamp too.
		v := *r.GeminiAnalytics
		r.GeminiAnalytics = &v
	}
	return r
}

func (s RuntimeProfileSnapshot) Clone() RuntimeProfileSnapshot {
	s.Sessions = append([]RuntimeSessionSnapshot(nil), s.Sessions...)
	for n := range s.Sessions {
		s.Sessions[n] = s.Sessions[n].Clone()
	}
	return s
}

// stateDB routes targeted producer writes to the profile that owns this row.
// Global is retained only for legacy instances not hydrated by Storage.
func (i *Instance) stateDB() *statedb.StateDB {
	if db := i.restartDB.Load(); db != nil {
		return db
	}
	if i.hookLagDB != nil {
		return i.hookLagDB
	}
	return statedb.GetGlobal()
}

// matrix/hardened: only the monitor producer opts into registry-CAS writes.
// Ordinary explicit mutators retain their existing write semantics.
func (i *Instance) SetRuntimeProducer(producer bool) {
	i.mu.RLock()
	var expected *statedb.InstanceRow
	if producer && i.storageSnapshot != nil {
		expected = statedb.CloneInstanceRow(i.storageSnapshot.stored)
	}
	i.mu.RUnlock()
	i.runtimeProducerExpected.Store(expected)
	if !producer {
		i.runtimeProducerFence.Store(nil)
	}
}

// matrix/hardened: the collector captures the registry watermark before each
// probe. Targeted writes compare it atomically with the operator's registry.
func (i *Instance) SetRuntimeProducerFence(fence int64) {
	i.runtimeProducerFence.Store(&fence)
}

// matrix/hardened: filesystem observations retain the actual probe-start
// timestamp, independently of the time a late callback eventually finishes.
func (i *Instance) SetRuntimeProducerObservedAt(at time.Time) {
	i.runtimeProducerObservedAt.Store(&at)
}

func (i *Instance) RuntimeProducerObservedAt() time.Time {
	if at := i.runtimeProducerObservedAt.Load(); at != nil {
		return *at
	}
	return time.Time{}
}

type runtimeProducerAcknowledgment struct {
	Acknowledged bool
	At           time.Time
}

// matrix/hardened: acknowledgment is replicated once per explicit DB tuple;
// repeating the tuple must not re-acknowledge a new turn observed by the source.
func (i *Instance) SetRuntimeProducerAcknowledgment(ack bool, at time.Time) {
	if previous := i.runtimeProducerAck.Load(); previous != nil && previous.Acknowledged == ack && previous.At.Equal(at) {
		return
	}
	value := &runtimeProducerAcknowledgment{Acknowledged: ack, At: at}
	i.runtimeProducerAck.Store(value)
	i.mu.Lock()
	i.lastIdleCheck = time.Time{}
	sess := i.tmuxSession
	i.mu.Unlock()
	if sess != nil {
		sess.ApplySharedAcknowledged(ack)
	}
}

func (i *Instance) runtimeHookResetsAcknowledgment(hook *HookStatus, previousAt time.Time) bool {
	if hook.Status == "running" {
		return true
	}
	if hook.Status != "waiting" {
		return false
	}
	if ack := i.runtimeProducerAck.Load(); ack != nil {
		if !ack.At.IsZero() {
			return hook.UpdatedAt.After(ack.At)
		}
		// Legacy true acknowledgments have no timestamp. Preserve the first
		// accepted hook baseline, then distinguish subsequent observations.
		return !previousAt.IsZero() && hook.UpdatedAt.After(previousAt)
	}
	return !previousAt.IsZero() && hook.UpdatedAt.After(previousAt)
}

func (i *Instance) producerGuardArgs() []int64 {
	if fence := i.runtimeProducerFence.Load(); fence != nil {
		return []int64{*fence}
	}
	return nil
}

func (i *Instance) isRuntimeProducer() bool {
	return i.runtimeProducerExpected.Load() != nil || i.runtimeProducerFence.Load() != nil
}

func (i *Instance) RuntimeProducerGuard() *statedb.InstanceRow {
	return statedb.CloneInstanceRow(i.runtimeProducerExpected.Load())
}

// RuntimeHookCompatible rejects foreign-thread evidence
// without binding a native ID or applying a hook to a consumer.
func (i *Instance) RuntimeHookCompatible(hs *HookStatus) bool {
	if hs == nil {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !HookStatusTool(i.Tool) {
		return false
	}
	return !i.codexHookFromForeignThread(hs)
}

// matrix/hardened: collector compatibility name retains the same read-only gate.
func (i *Instance) RuntimeHookAccepted(hs *HookStatus) bool { return i.RuntimeHookCompatible(hs) }

func normalizedRuntimeProfile(profile string) string {
	if profile == "" {
		return DefaultProfile
	}
	return profile
}

// matrix/hardened: native-path resolvers are also called with i.mu held by the
// producer. The immutable profile name must therefore be read without locking.
func (i *Instance) runtimeProfileForResolution() string {
	if i != nil {
		if profile := i.runtimeProfileName.Load(); profile != nil {
			return *profile
		}
	}
	return resolvedProcessProfile()
}

func (i *Instance) setRuntimeProfileLocked(profile string) {
	i.runtimeProfile = normalizedRuntimeProfile(profile)
	name := i.runtimeProfile
	i.runtimeProfileName.Store(&name)
}

// RuntimeIdentity excludes presentation and sampled fields, but includes every
// launch/native binding that would make a previously observed verdict unsafe.
func (i *Instance) RuntimeIdentity() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.runtimeIdentityLocked()
}

func (i *Instance) runtimeIdentityLocked() string {
	name := ""
	if i.tmuxSession != nil {
		name = i.tmuxSession.Name
	}
	identity := struct {
		ID, Profile, Tool, Account, Command, Wrapper, Project, Worktree, MultiRepoDir, SSHHost, SSHPath, Socket, Tmux string
		Claude, Codex, Gemini, OpenCode, Copilot, Pi, PiPath, Generic, GenericTool, GenericCommand, GenericLocation   string
		Group, Parent, ParentPath, ContextLevel, GeminiModel, CopilotModel, DeepSeekTask                              string
		Stopped, IsConductor, MultiRepo, CopilotAllowAll, IdentityDisabled, Passthrough, InheritTelegram              bool
		Started                                                                                                       int64
		GeminiYolo, ExitToShell, LaunchShell                                                                          *bool
		Sandbox                                                                                                       *SandboxConfig
		Options                                                                                                       json.RawMessage
		ExtraArgs, Channels, Plugins, Paths                                                                           []string
	}{ID: i.ID, Profile: normalizedRuntimeProfile(i.runtimeProfile), Tool: i.Tool, Account: i.Account, Command: i.Command, Wrapper: i.Wrapper,
		Project: i.ProjectPath, Worktree: i.WorktreePath, MultiRepoDir: i.MultiRepoTempDir, SSHHost: i.SSHHost, SSHPath: i.SSHRemotePath, Socket: i.TmuxSocketName, Tmux: name,
		Claude: i.ClaudeSessionID, Codex: i.CodexSessionID, Gemini: i.GeminiSessionID, OpenCode: i.OpenCodeSessionID, Copilot: i.CopilotSessionID, Pi: i.PiSessionID, PiPath: i.PiSessionPath,
		Generic: i.GenericSessionID, GenericTool: i.GenericSessionTool, GenericCommand: i.GenericSessionCommand, GenericLocation: i.GenericSessionLocation,
		Group: i.GroupPath, Parent: i.ParentSessionID, ParentPath: i.ParentProjectPath, ContextLevel: i.ContextLevel, GeminiModel: i.runtimeIdentityGeminiModelLocked(), CopilotModel: i.CopilotModel, DeepSeekTask: i.DeepSeekTask,
		Stopped: i.Status == StatusStopped, IsConductor: i.IsConductor, MultiRepo: i.MultiRepoEnabled, CopilotAllowAll: i.CopilotAllowAll, IdentityDisabled: i.IdentityInjectionDisabled, Passthrough: i.SubcommandPassthrough, InheritTelegram: i.InheritTelegramEnv,
		// matrix/hardened: tool_data persists seconds; local mutation fences keep
		// nanosecond precision so same-second restart probes are still rejected.
		Started: i.LastStartedAt.Unix(), GeminiYolo: i.runtimeIdentityGeminiYoloLocked(), ExitToShell: i.ExitToShell, LaunchShell: i.LaunchShell, Sandbox: i.Sandbox,
		Options: i.ToolOptionsJSON, ExtraArgs: i.ExtraArgs, Channels: i.Channels, Plugins: i.Plugins, Paths: i.AdditionalPaths}

	data, _ := json.Marshal(identity)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// matrix/hardened: Gemini's discovered model is volatile metadata, whereas an
// explicit Gemini launch override is stored outside ToolOptionsJSON. Only its
// registry value may identify a producer generation.
func (i *Instance) runtimeIdentityGeminiModelLocked() string {
	if i.isRuntimeProducer() {
		if expected := i.runtimeProducerExpected.Load(); expected != nil {
			var stored struct {
				GeminiModel string `json:"gemini_model"`
			}
			_ = json.Unmarshal(expected.ToolData, &stored)
			return stored.GeminiModel
		}
	}
	return i.GeminiModel
}

// matrix/hardened: live Gemini environment echoes are observed metadata;
// explicit persisted permission overrides identify the launch generation.
func (i *Instance) runtimeIdentityGeminiYoloLocked() *bool {
	if i.isRuntimeProducer() {
		if expected := i.runtimeProducerExpected.Load(); expected != nil {
			var stored struct {
				GeminiYolo *bool `json:"gemini_yolo_mode"`
			}
			_ = json.Unmarshal(expected.ToolData, &stored)
			return stored.GeminiYolo
		}
	}
	return i.GeminiYoloMode
}

// RuntimeSnapshot is a producer operation: resolve the complete substate first,
// then copy fields without holding the instance lock across IO.
func (i *Instance) RuntimeSnapshot(profile, epoch string, revision uint64, sampledAt time.Time) RuntimeSessionSnapshot {
	persistError := i.persistRuntimeNativeBindings()
	sub := i.Substate()
	detail := i.SubstateDetail()
	held, remedy := i.IsAuthHeld()
	hold := i.AuthHold()
	observed, observedKnown := i.LastObservedActivity()
	change := i.GetLastActivityTime()
	waiting := i.GetWaitingSince()
	waitingKnown := false
	if sess := i.tmuxSession; sess != nil {
		waitingKnown = !sess.GetWaitingSince().IsZero()
	}
	model := i.LaunchModelInfo()
	canFork := i.CanFork()
	alive, known := false, true
	ack := false
	var title string
	titleKnown := false
	var titleObservedAt time.Time
	if sess := i.tmuxSession; sess != nil {
		// matrix/hardened: the pass refreshed this batch once. An entry proves
		// session existence, including a retained dead pane, without per-row exec.
		if strings.TrimSpace(sess.SocketName) == tmux.DefaultSocketName() {
			if pane, at, ok := tmux.GetCachedPaneInfoSnapshot(sess.Name); ok && time.Since(at) <= 5*time.Second {
				title, titleKnown, titleObservedAt = pane.Title, true, at
				alive, known = true, true
			}
		}
		if !titleKnown {
			var err error
			alive, err = sess.ProbeExists()
			known = err == nil
		}
		ack = sess.IsAcknowledged()
	}
	i.mu.RLock()
	r := RuntimeSessionSnapshot{ID: i.ID, Profile: normalizedRuntimeProfile(profile), Epoch: epoch, Revision: revision, Identity: i.runtimeIdentityLocked(), ObservedAt: sampledAt,
		Status: i.Status, Substate: sub, Detail: detail, Evidence: StatusEvidence{HookStatus: i.hookStatus, HookEvent: i.hookEvent, HookAt: i.hookLastUpdate, SSEStatus: i.sseStatus, SSEAt: i.sseLastUpdate},
		HookKnown: i.hookStatus != "", ActivityAt: i.lastActivityAt, ObservedActivityAt: observed, ObservedActivityKnown: observedKnown, LastChangeAt: change,
		WaitingSince: waiting, WaitingSinceKnown: waitingKnown, Acknowledged: ack, TmuxAlive: alive, TmuxAliveKnown: known, PaneTitle: title, PaneTitleKnown: titleKnown,
		AuthHeld: held, AuthRemedy: remedy, AuthHold: hold, ClaudeSessionID: i.ClaudeSessionID, ClaudeDetectedAt: i.ClaudeDetectedAt,
		CodexSessionID: i.CodexSessionID, CodexDetectedAt: i.CodexDetectedAt, GeminiSessionID: i.GeminiSessionID, GeminiDetectedAt: i.GeminiDetectedAt,
		OpenCodeSessionID: i.OpenCodeSessionID, OpenCodeDetectedAt: i.OpenCodeDetectedAt, OpenCodePort: i.OpenCodePort, CopilotSessionID: i.CopilotSessionID, CopilotDetectedAt: i.CopilotDetectedAt, CopilotModel: i.CopilotModel, HermesSessionID: i.HermesSessionID, HermesHookGeneration: i.HermesHookGeneration, DeepSeekSessionID: i.DeepSeekSessionID, PiSessionID: i.PiSessionID, PiSessionPath: i.PiSessionPath,
		GenericSessionID: i.GenericSessionID, GenericDetectedAt: i.GenericDetectedAt, GenericSessionTool: i.GenericSessionTool, GenericSessionCommand: i.GenericSessionCommand, GenericSessionLocation: i.GenericSessionLocation,
		GeminiModel: i.GeminiModel, GeminiAnalytics: i.GeminiAnalytics, LatestPrompt: i.LatestPrompt, AutoNameDescription: i.autoNameDescription, LaunchModel: model, ToolOptions: i.ToolOptionsJSON, CanFork: canFork, CanForkKnown: true}
	if titleKnown {
		r.PaneTitleObservedAt = titleObservedAt
	}
	if ack := i.runtimeProducerAck.Load(); ack != nil {
		r.AcknowledgedAt = ack.At
	}
	if i.bgWorkActive && i.Status == StatusRunning {
		r.BackgroundWork = i.bgWork
	}
	if i.hookStatus != "" {
		r.Hook = &HookStatus{Status: i.hookStatus, SessionID: i.hookSessionID, Event: i.hookEvent, UpdatedAt: i.hookLastUpdate,
			CodexStartedGeneration: i.codexStartedGeneration, CodexCompletedGeneration: i.codexCompletedGeneration, CodexStartedSessionID: i.codexStartedSessionID, CodexCompletedSessionID: i.codexCompletedSessionID}
		r.HookFresh = time.Since(i.hookLastUpdate) < hookFastPathFreshnessForTool(i.Tool, i.hookStatus)
	}
	if persistError != nil {
		r.Error = persistError.Error()
	}
	r = r.Clone()
	i.mu.RUnlock()
	i.claudeSessionIDsFromDiskScanMu.Lock()
	r.ClaudeSessionIDVerified = r.ClaudeSessionID != "" && !i.claudeSessionIDsFromDiskScan[r.ClaudeSessionID]
	i.claudeSessionIDsFromDiskScanMu.Unlock()
	return r
}

// ApplyRuntimeSnapshot changes only the volatile replica. Native bindings stay
// registry-owned; a producer rebind requires a registry reload before apply.
func (i *Instance) ApplyRuntimeSnapshot(profile string, row RuntimeSessionSnapshot) bool {
	profile = normalizedRuntimeProfile(profile)
	i.mu.Lock()
	defer i.mu.Unlock()
	if row.ID != i.ID || normalizedRuntimeProfile(row.Profile) != profile ||
		(i.runtimeProfile != "" && i.runtimeProfile != profile) || row.Identity != i.runtimeIdentityLocked() ||
		row.ObservedAt.IsZero() || row.ObservedAt.Before(i.runtimeInvalidatedAt) || row.Error != "" {
		return false
	}
	if prior := i.runtimeReplica.Load(); prior != nil {
		if row.ObservedAt.Before(prior.ObservedAt) || (row.Epoch == prior.Epoch && row.Revision < prior.Revision) {
			return false
		}
	}
	copy := row.Clone()
	i.runtimeReplica.Store(&copy)
	i.runtimeConsumer.Store(true)
	// Status is mirrored for existing memory-only consumers reading the public
	// field; no persisted/native/sampling state is overwritten.
	i.Status = row.Status
	// matrix/hardened: timestamps are confirmations of the existing exact
	// binding. Action preflights need this freshness without rebinding IDs or
	// forcing a SQLite write on every source poll.
	if row.ClaudeSessionID != "" && row.ClaudeSessionID == i.ClaudeSessionID {
		i.ClaudeDetectedAt = row.ClaudeDetectedAt
	}
	if row.CodexSessionID != "" && row.CodexSessionID == i.CodexSessionID {
		i.CodexDetectedAt = row.CodexDetectedAt
	}
	if row.GeminiSessionID != "" && row.GeminiSessionID == i.GeminiSessionID {
		i.GeminiDetectedAt = row.GeminiDetectedAt
	}
	if row.OpenCodeSessionID != "" && row.OpenCodeSessionID == i.OpenCodeSessionID {
		i.OpenCodeDetectedAt = row.OpenCodeDetectedAt
	}
	if row.CopilotSessionID != "" && row.CopilotSessionID == i.CopilotSessionID {
		i.CopilotDetectedAt = row.CopilotDetectedAt
	}
	if row.GenericSessionID != "" && row.GenericSessionID == i.GenericSessionID {
		i.GenericDetectedAt = row.GenericDetectedAt
	}
	return true
}

func (i *Instance) runtimeView() (*RuntimeSessionSnapshot, bool) {
	// matrix/hardened: some legacy metadata helpers run under i.mu. Atomic
	// immutable publication keeps those getter calls safe and lock-free. The
	// private result is borrowed read-only; mutable exports must still Clone.
	if row := i.runtimeReplica.Load(); row != nil {
		return row, i.runtimeConsumer.Load()
	}
	return nil, i.runtimeConsumer.Load()
}

// SetRuntimeConsumer marks missing/failed observations as unknown without IO.
// Enabling leaves the mutation fence unchanged so a current source row can apply.
func (i *Instance) SetRuntimeConsumer(consumer bool) {
	i.mu.Lock()
	i.runtimeConsumer.Store(consumer)
	if !consumer {
		i.runtimeReplica.Store(nil)
		i.runtimeInvalidatedAt = time.Time{}
	}
	i.mu.Unlock()
}

// matrix/hardened: fresh registry consumers await a verdict without probing or
// advancing the mutation fence beyond the snapshot they are about to apply.
func (i *Instance) ExpectRuntimeSnapshot() {
	i.mu.Lock()
	i.runtimeReplica.Store(nil)
	i.runtimeConsumer.Store(true)
	i.mu.Unlock()
}

// RuntimeReplica returns a private value for consumers requiring extra metadata.
func (i *Instance) RuntimeReplica() (RuntimeSessionSnapshot, bool) {
	if row, _ := i.runtimeView(); row != nil {
		return row.Clone(), true
	}
	return RuntimeSessionSnapshot{}, false
}

func (i *Instance) ClearRuntimeSnapshot() {
	i.mu.Lock()
	i.runtimeReplica.Store(nil)
	i.runtimeConsumer.Store(false)
	i.runtimeInvalidatedAt = time.Time{}
	i.mu.Unlock()
}

func (i *Instance) InvalidateRuntimeSnapshot() {
	i.mu.Lock()
	i.runtimeInvalidatedAt = time.Now()
	i.runtimeReplica.Store(nil)
	i.mu.Unlock()
}

// matrix/hardened: a storage hydration carries its registry mutation watermark;
// old source probes must not overwrite a stop/edit committed by another process.
func (i *Instance) SetRuntimeMutationFence(fence time.Time) {
	i.mu.Lock()
	if fence.After(i.runtimeInvalidatedAt) {
		i.runtimeInvalidatedAt = fence
		if row := i.runtimeReplica.Load(); row != nil && row.ObservedAt.Before(fence) {
			i.runtimeReplica.Store(nil)
		}
	}
	i.mu.Unlock()
}

func (i *Instance) RuntimeAcknowledged() bool {
	if row, consumer := i.runtimeView(); row != nil {
		return row.Acknowledged
	} else if consumer {
		return false
	}
	return i.tmuxSession != nil && i.tmuxSession.IsAcknowledged()
}

func (i *Instance) RuntimeTmuxAlive() (bool, bool) {
	if row, consumer := i.runtimeView(); row != nil {
		return row.TmuxAlive, row.TmuxAliveKnown
	} else if consumer {
		return false, false
	}
	return false, false
}

// ReconcileRuntimeRegistry retains producer throttles when only persisted
// presentation fields changed. A launch/binding change must replace the source.
func (i *Instance) ReconcileRuntimeRegistry(candidate *Instance) bool {
	if candidate == nil || candidate == i {
		return candidate == i
	}
	candidate.mu.RLock()
	defer candidate.mu.RUnlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.runtimeIdentityLocked() != candidate.runtimeIdentityLocked() {
		return false
	}
	i.Title, i.GroupPath, i.Order, i.Pin = candidate.Title, candidate.GroupPath, candidate.Order, candidate.Pin
	i.ArchivedAt, i.SupersededBy, i.Supersedes = candidate.ArchivedAt, candidate.SupersededBy, candidate.Supersedes
	i.Notes, i.Color, i.AutoName, i.Favorite = candidate.Notes, candidate.Color, candidate.AutoName, candidate.Favorite
	i.TitleLocked, i.NoTransitionNotify, i.IdleTimeoutSecs = candidate.TitleLocked, candidate.NoTransitionNotify, candidate.IdleTimeoutSecs
	i.ParentSessionID, i.ParentProjectPath = candidate.ParentSessionID, candidate.ParentProjectPath
	i.LastAccessedAt = candidate.LastAccessedAt
	if candidate.lastActivityAt.After(i.lastActivityAt) {
		i.lastActivityAt = candidate.lastActivityAt
	}
	if candidate.autoNameDescription != "" {
		i.autoNameDescription = candidate.autoNameDescription
	}
	i.storageSnapshot = candidate.storageSnapshot
	if i.runtimeProducerExpected.Load() != nil && candidate.storageSnapshot != nil {
		i.runtimeProducerExpected.Store(statedb.CloneInstanceRow(candidate.storageSnapshot.stored))
	}
	return true
}
