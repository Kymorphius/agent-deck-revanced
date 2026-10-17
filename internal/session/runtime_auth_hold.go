package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/safeio"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: legacy records keep their original per-ID path. Every
// storage-bound instance writes to its own profile namespace instead.
func authHoldRecordPathForProfile(profile, instanceID string) string {
	if profile == "" {
		return authHoldRecordPath(instanceID)
	}
	return filepath.Join(authHoldDir(), "profiles", url.PathEscape(profile), url.PathEscape(instanceID)+".json")
}

// authHoldScope is captured while i.mu is held. Guard callbacks use this value
// only, never a getter that would take the same mutex or access the pinned DB.
type authHoldScope struct {
	id, profile, identity string
	stopped               bool
	started, probeAt      time.Time
	producer              bool
	db                    *statedb.StateDB
	fence                 *int64
}

func (i *Instance) authHoldScopeLocked() authHoldScope {
	probeAt := i.RuntimeProducerObservedAt()
	if probeAt.IsZero() {
		probeAt = time.Now()
	}
	scope := authHoldScope{id: i.ID, profile: i.runtimeProfile, stopped: i.Status == StatusStopped,
		started: i.LastStartedAt, probeAt: probeAt, producer: i.isRuntimeProducer(), db: i.restartDB.Load()}
	if scope.profile != "" {
		scope.identity = i.authHoldLaunchIdentityLocked()
	}
	if fence := i.runtimeProducerFence.Load(); fence != nil {
		value := *fence
		scope.fence = &value
	}
	return scope
}

// matrix/hardened: native conversation discovery and observed Gemini model
// echoes do not replace the launch. Explicit edits are fenced by the registry;
// the persisted options, account and start time identify the auth generation.
func (i *Instance) authHoldLaunchIdentityLocked() string {
	name := ""
	if i.tmuxSession != nil {
		name = i.tmuxSession.Name
	}
	launch := struct {
		ID, Profile, Tool, Account, Command, Wrapper, Project, Worktree, MultiRepoDir, Host, RemotePath, Socket, Tmux string
		Group, Parent, ContextLevel, DeepSeekTask                                                                     string
		Started                                                                                                       int64
		Options                                                                                                       json.RawMessage
		ExtraArgs, Channels, Plugins, Paths                                                                           []string
		Sandbox                                                                                                       *SandboxConfig
		IsConductor, MultiRepo, IdentityDisabled, Passthrough                                                         bool
		ExitToShell, LaunchShell                                                                                      *bool
	}{ID: i.ID, Profile: i.runtimeProfile, Tool: i.Tool, Account: i.Account, Command: i.Command, Wrapper: i.Wrapper,
		Project: i.ProjectPath, Worktree: i.WorktreePath, MultiRepoDir: i.MultiRepoTempDir, Host: i.SSHHost, RemotePath: i.SSHRemotePath,
		Socket: i.TmuxSocketName, Tmux: name, Group: i.GroupPath, Parent: i.ParentSessionID, ContextLevel: i.ContextLevel, DeepSeekTask: i.DeepSeekTask,
		Started: i.LastStartedAt.Unix(), Options: i.ToolOptionsJSON, ExtraArgs: i.ExtraArgs, Channels: i.Channels, Plugins: i.Plugins, Paths: i.AdditionalPaths,
		Sandbox: i.Sandbox, IsConductor: i.IsConductor, MultiRepo: i.MultiRepoEnabled, IdentityDisabled: i.IdentityInjectionDisabled,
		Passthrough: i.SubcommandPassthrough, ExitToShell: i.ExitToShell, LaunchShell: i.LaunchShell}
	data, _ := json.Marshal(launch)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s authHoldScope) owns(rec *AuthHoldRecord) bool {
	if rec == nil || (rec.InstanceID != "" && rec.InstanceID != s.id) {
		return false
	}
	return rec.RuntimeProfile == "" || rec.RuntimeProfile == s.profile
}

// matrix/hardened: an anonymous historical per-ID record belongs to the old
// default namespace. Nondefault profiles require positive legacy ownership.
func (s authHoldScope) ownsLegacy(rec *AuthHoldRecord) bool {
	if !s.owns(rec) {
		return false
	}
	return s.profile == "" || s.profile == DefaultProfile || rec.RuntimeProfile == s.profile
}

func (s authHoldScope) accepts(rec *AuthHoldRecord) bool {
	if !s.owns(rec) {
		return false
	}
	// Unbound legacy exit-0 auth deaths retain the old gate contract. A bound
	// registry stop is explicit authority and must never be held by a sidecar.
	if s.profile != "" && s.stopped {
		return false
	}
	if rec.RuntimeIdentity != "" && rec.RuntimeIdentity != s.identity {
		return false
	}
	if s.profile != "" && !s.started.IsZero() {
		if rec.ObservedAtNS > 0 {
			return !rec.ObservedAt().Before(s.started)
		}
		if rec.Timestamp > 0 && rec.Timestamp < s.started.Unix() {
			return false
		}
	}
	return true
}

func (s authHoldScope) readCurrent() (*AuthHoldRecord, string, error) {
	path := authHoldRecordPathForProfile(s.profile, s.id)
	rec, err := readAuthHoldRecordAt(path)
	if err != nil || rec != nil {
		if !s.accepts(rec) {
			return nil, path, err
		}
		return rec, path, err
	}
	if s.profile != "" {
		path = authHoldRecordPath(s.id)
		rec, err = readAuthHoldRecordAt(path)
		if !s.ownsLegacy(rec) || !s.accepts(rec) {
			return nil, path, err
		}
	}
	return rec, path, err
}

func (i *Instance) readAuthHoldLocked() *AuthHoldRecord {
	rec, _, err := i.authHoldScopeLocked().readCurrent()
	if err != nil {
		return nil
	}
	return rec
}

func (s authHoldScope) guard(action func() error) error {
	if s.profile == "" && !s.producer {
		return action()
	}
	if s.stopped || s.db == nil {
		return statedb.ErrObservationStale
	}
	var fence int64
	if s.fence != nil {
		fence = *s.fence
	} else {
		if s.producer {
			return statedb.ErrObservationStale
		}
		// matrix/hardened: a plain CLI may finish a slow probe after restart.
		// Reading the newest watermark alone would lend that old instance the
		// replacement's write permission. Read it before the current registry,
		// verify the captured launch, then let the transaction recheck the pair.
		value, err := s.db.LastModified()
		if err != nil {
			return err
		}
		fence = value
		current, _, err := (&Storage{db: s.db, profile: s.profile}).LoadWithGroups()
		if err != nil {
			return err
		}
		matches := false
		for _, inst := range current {
			if inst.ID != s.id {
				continue
			}
			inst.mu.RLock()
			matches = inst.Status != StatusStopped && inst.authHoldLaunchIdentityLocked() == s.identity
			inst.mu.RUnlock()
			break
		}
		if !matches {
			return statedb.ErrObservationStale
		}
	}
	return s.db.GuardRuntimeObservation(s.id, fence, action)
}

func (s authHoldScope) stamp(rec AuthHoldRecord) AuthHoldRecord {
	rec.InstanceID = s.id
	if s.profile != "" {
		rec.RuntimeProfile, rec.RuntimeIdentity, rec.ObservedAtNS = s.profile, s.identity, s.probeAt.UnixNano()
	}
	return rec
}

func (s authHoldScope) writeObserved(rec AuthHoldRecord) error {
	return s.guard(func() error {
		path := authHoldRecordPathForProfile(s.profile, s.id)
		current, err := readAuthHoldRecordAt(path)
		if err != nil {
			return err
		}
		// A more recent failure must keep both its evidence and its timestamp,
		// including when two observations belong to the same launch generation.
		if current != nil && (!s.owns(current) || current.ObservedAt().After(s.probeAt)) {
			return statedb.ErrObservationStale
		}
		return writeAuthHoldRecordAt(path, s.stamp(rec))
	})
}

func (s authHoldScope) clearObserved(explicitStop bool) error {
	action := func() error {
		paths := []string{authHoldRecordPathForProfile(s.profile, s.id)}
		if s.profile != "" {
			paths = append(paths, authHoldRecordPath(s.id))
		}
		for _, path := range paths {
			rec, err := readAuthHoldRecordAt(path)
			if err != nil {
				return err
			}
			if rec == nil || !s.owns(rec) {
				continue
			}
			if path == authHoldRecordPath(s.id) && !s.ownsLegacy(rec) {
				continue
			}
			if !explicitStop && ((rec.RuntimeIdentity != "" && rec.RuntimeIdentity != s.identity) || rec.ObservedAt().After(s.probeAt)) {
				continue
			}
			if err := safeio.SafeRemove(path, safeio.RemoveOptions{}); err != nil {
				return err
			}
		}
		return nil
	}
	if explicitStop {
		return action()
	}
	return s.guard(action)
}

func authHoldObservationStale(err error) bool { return errors.Is(err, statedb.ErrObservationStale) }
