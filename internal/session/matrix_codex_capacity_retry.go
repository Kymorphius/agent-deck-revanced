package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: explicit opt-in, same-model continuation after a native
// capacity failure. Reuse existing observation ticks; no timer, account switch,
// model switch, broad error retry, or scan on the UI render path is added.
// A short continuation fits narrow panes; the sender never changes the model.
const matrixCapacityRetryPrompt = "继续"

var matrixCapacityRetryDelays = [...]time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}

func matrixIsModelCapacityMessage(message string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(message)), "selected model is at capacity.")
}

func matrixCapacityRetryStatusAllowed(status Status) bool {
	return status == StatusWaiting || status == StatusIdle || status == StatusError
}

type matrixCapacityRetryState struct {
	Version     int       `json:"version"`
	WindowStart time.Time `json:"window_start"`
	Attempts    int       `json:"attempts"`
	Pending     string    `json:"pending_turn,omitempty"`
	Due         time.Time `json:"due,omitempty"`
	Claimed     string    `json:"claimed_turn,omitempty"`
	Claims      [3]string `json:"claimed_turns"`
}

// matrix/hardened: the per-native-thread sidecar limits the entire retry chain
// to three claims per hour and survives monitor/TUI restarts. One native failed
// turn can only be claimed once, even when delivery is uncertain or fails.
func (s *matrixCapacityRetryState) plan(v matrixTurnInterrupt, now time.Time) (string, bool, error) {
	if s.Attempts < 0 || s.Attempts > 3 || s.WindowStart.After(now) {
		return "", false, errors.New("invalid retry state")
	}
	if v.Kind != "server_overloaded" || !matrixIsModelCapacityMessage(v.Message) || v.TurnID == "" || v.At.IsZero() || v.At.After(now) || now.Sub(v.At) > 5*time.Minute {
		return "", false, nil
	}
	// Check claimed before the window: an old turn is never replayed after reset.
	seen := s.Claimed == v.TurnID
	for _, id := range s.Claims {
		if id == v.TurnID {
			seen = true
		}
	}
	if seen {
		return "auto retry already claimed; continue manually if no response", false, nil
	}
	if s.WindowStart.IsZero() || now.Sub(s.WindowStart) >= time.Hour {
		claimed := s.Claimed
		*s = matrixCapacityRetryState{WindowStart: now, Claimed: claimed}
	}
	if s.Attempts >= len(matrixCapacityRetryDelays) {
		return "auto retry exhausted (3/3); continue manually", false, nil
	}
	if s.Pending != v.TurnID {
		s.Pending = v.TurnID
		s.Due = now.Add(matrixCapacityRetryDelays[s.Attempts])
	}
	if now.Before(s.Due) {
		return fmt.Sprintf("auto retry %d/3 scheduled in %ds", s.Attempts+1, int(s.Due.Sub(now).Seconds()+0.999)), false, nil
	}
	return fmt.Sprintf("auto retry %d/3 ready", s.Attempts+1), true, nil
}

func matrixReadCapacityRetryState(path string) (matrixCapacityRetryState, error) {
	var state matrixCapacityRetryState
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return state, err
	}
	if len(data) > 4096 {
		return state, errors.New("oversized retry state")
	}
	err = json.Unmarshal(data, &state)
	if err == nil && (state.Version != 1 || state.WindowStart.IsZero() || state.Attempts < 0 || state.Attempts > 3) {
		err = errors.New("invalid retry state schema")
	}
	if err == nil && state.Pending != "" && (state.Due.IsZero() || state.Due.Before(state.WindowStart)) {
		err = errors.New("invalid retry schedule")
	}
	if err == nil {
		for idx := 0; idx < state.Attempts; idx++ {
			if state.Claims[idx] == "" {
				err = errors.New("incomplete retry claim history")
			}
		}
	}
	return state, err
}

// matrix/hardened: persist and sync a claim before sending. A crash or failed
// keystroke never causes another process to resend that same failed turn.
func matrixWriteCapacityRetryState(path string, state matrixCapacityRetryState) error {
	state.Version = 1
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".capacity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type matrixCapacityRetryTarget interface {
	CapturePaneFresh() (string, error)
	SendKeysAndEnter(string) error
}

func matrixCapacityComposerReady(raw string) bool {
	detector := tmux.NewPromptDetector("codex")
	plain := tmux.StripANSI(raw)
	if !detector.HasPrompt(plain) {
		return false
	}
	sub := detector.ClassifySubstate(plain)
	if sub != tmux.SubstateNone && sub != tmux.SubstateIdleAtEmptyPrompt {
		return false
	}
	draft, visible := send.ComposerDraft(raw, tmux.StripANSI)
	return visible && draft == ""
}

// matrix/hardened: after paste, only the exact supplied continuation may be
// submitted. A racing operator draft or menu withholds Enter without clearing it.
func matrixCapacitySubmitReady(raw, message string) bool {
	plain := tmux.StripANSI(raw)
	d := tmux.NewPromptDetector("codex")
	if !d.HasPrompt(plain) {
		return false
	}
	sub := d.ClassifySubstate(plain)
	if sub != tmux.SubstateNone && sub != tmux.SubstateIdleAtEmptyPrompt {
		return false
	}
	body, visible := send.CurrentComposerPrompt(plain)
	return visible && send.NormalizePromptText(body) == send.NormalizePromptText(message)
}

// matrix/hardened: serialize scheduling/claim across processes; validate the
// current native turn and pane again before any input. Never clear a draft,
// approve a menu, press Ctrl+C, restart an agent, or retry uncertain delivery.
func matrixRunCapacityRetry(path string, v matrixTurnInterrupt, now time.Time, target matrixCapacityRetryTarget, valid func() bool) (string, error) {
	lock, err := AcquireConfigFileLockTimeout(path, time.Millisecond)
	if err != nil {
		return "auto retry waiting for owner", nil
	}
	defer lock.Release()
	state, err := matrixReadCapacityRetryState(path)
	if err != nil {
		return "auto retry paused: unreadable state", err
	}
	before := state
	detail, due, err := state.plan(v, now)
	if err != nil {
		return "auto retry paused: invalid state", err
	}
	if detail == "" {
		return "", nil
	}
	if !due {
		if state != before {
			if err = matrixWriteCapacityRetryState(path, state); err != nil {
				return "auto retry paused: state write failed", err
			}
		}
		return detail, nil
	}
	if !valid() {
		return "auto retry cancelled: turn changed", nil
	}
	raw, err := target.CapturePaneFresh()
	if err != nil || !matrixCapacityComposerReady(raw) {
		return "auto retry paused: busy, draft or menu", nil
	}
	if !valid() {
		return "auto retry cancelled: turn changed", nil
	}
	state.Claims[state.Attempts] = v.TurnID
	state.Attempts++
	state.Claimed = v.TurnID
	state.Pending = ""
	if err = matrixWriteCapacityRetryState(path, state); err != nil {
		return "auto retry paused: claim write failed", err
	}
	// matrix/hardened: final fresh draft/turn check after the durable claim.
	if !valid() {
		return "auto retry claimed; cancelled because turn changed", nil
	}
	raw, err = target.CapturePaneFresh()
	if err != nil || !matrixCapacityComposerReady(raw) {
		return "auto retry claimed; cancelled because composer changed", nil
	}
	if err = target.SendKeysAndEnter(matrixCapacityRetryPrompt); err != nil {
		return "auto retry claimed; delivery uncertain, continue manually", err
	}
	return fmt.Sprintf("auto retry %d/3 submitted; awaiting response", state.Attempts), nil
}

// matrix/hardened: fence operator stop/rebind across the final capture and send.
// Pane input is guarded again; a claim is consumed even if the fence refuses it.
type matrixCapacityInstanceTarget struct {
	instance           *Instance
	target             *tmux.Session
	pane               *tmux.Session
	key, rollout, turn string
}

func (t matrixCapacityInstanceTarget) CapturePaneFresh() (string, error) {
	return t.pane.CapturePaneFresh()
}
func (t matrixCapacityInstanceTarget) SendKeysAndEnter(message string) error {
	i := t.instance
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.runtimeConsumer.Load() || i.matrixTurnInterruptKeyLocked() != t.key || i.tmuxSession != t.target || i.IsArchived() || !matrixCapacityRetryStatusAllowed(i.Status) {
		return errors.New("capacity retry session changed")
	}
	// matrix/hardened: use the owner's existing registry generation fence so a
	// deleted/stopped/rebound row in another process cannot receive input.
	fence, db := i.runtimeProducerFence.Load(), i.stateDB()
	if fence == nil || db == nil {
		return errors.New("capacity retry has no observation owner")
	}
	// The DB's busy-retry wrapper may invoke its callback again after commit
	// failure. Input delivery is never replayable, even inside that wrapper.
	attempted := false
	var deliveredErr error
	return db.GuardRuntimeObservation(i.ID, *fence, func() error {
		if attempted {
			return deliveredErr
		}
		cfg, err := LoadUserConfig()
		if err != nil || cfg == nil || !cfg.Codex.CapacityRetry {
			return errors.New("capacity retry disabled")
		}
		latest := matrixInterruptFiles.load("codex", t.rollout)
		if !latest.found || latest.verdict.TurnID != t.turn || latest.verdict.Kind != "server_overloaded" || latest.meta.ThreadSource == "subagent" || latest.meta.ParentThreadID != "" {
			return errors.New("capacity retry native turn changed")
		}
		raw, err := t.pane.CapturePaneFresh()
		if err != nil || !matrixCapacityComposerReady(raw) {
			return errors.New("capacity retry composer changed")
		}
		attempted = true
		deliveredErr = t.pane.SendKeysAndEnterChecked(message, t.pane.CapturePaneFresh, func(raw string, captureErr error) (bool, error) {
			if captureErr != nil || !matrixCapacitySubmitReady(raw, message) {
				return false, errors.New("capacity retry withheld Enter: composer changed")
			}
			cfg, err := LoadUserConfig()
			if err != nil || cfg == nil || !cfg.Codex.CapacityRetry {
				return false, errors.New("capacity retry disabled before Enter")
			}
			latest := matrixInterruptFiles.load("codex", t.rollout)
			if !latest.found || latest.verdict.TurnID != t.turn || latest.verdict.Kind != "server_overloaded" {
				return false, errors.New("capacity retry turn changed before Enter")
			}
			return true, nil
		})
		return deliveredErr
	})
}

func (i *Instance) matrixMaybeRetryCodexCapacity() {
	// matrix/hardened: observation consumers never act, even when invoked directly.
	if i.runtimeConsumer.Load() || i.runtimeProducerFence.Load() == nil {
		return
	}
	now := time.Now()
	i.mu.Lock()
	m := &i.matrixInterrupt
	if !IsCodexCompatible(i.Tool) || i.IsSSH() || i.IsArchived() || !matrixCapacityRetryStatusAllowed(i.Status) || !m.found || m.verdict.Kind != "server_overloaded" {
		m.retryDetail = ""
		i.mu.Unlock()
		return
	}
	if now.Sub(m.retryCheckedAt) < 5*time.Second {
		i.mu.Unlock()
		return
	}
	m.retryCheckedAt = now
	verdict, key, target := m.verdict, m.key, i.tmuxSession
	i.mu.Unlock()
	// Cached configuration is only consulted for a fresh capacity candidate.
	cfg, cfgErr := LoadUserConfig()
	if cfgErr != nil || cfg == nil || !cfg.Codex.CapacityRetry || verdict.At.IsZero() || verdict.TurnID == "" || verdict.At.After(now) || now.Sub(verdict.At) > 5*time.Minute {
		i.mu.Lock()
		i.matrixInterrupt.retryDetail = ""
		i.mu.Unlock()
		return
	}
	if target == nil {
		return
	}
	rollout := CodexRolloutPathForInstance(i)
	if rollout == "" {
		return
	}
	rollout, err := filepath.EvalSymlinks(rollout)
	if err != nil {
		return
	}
	rollout, err = filepath.Abs(rollout)
	if err != nil {
		return
	}
	root, err := GetAgentDeckDir()
	if err != nil {
		return
	}
	dir := filepath.Join(root, "capacity-retries")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return
	}
	hash := sha256.Sum256([]byte(rollout))
	path := filepath.Join(dir, hex.EncodeToString(hash[:])+".json")
	valid := func() bool {
		i.mu.RLock()
		eligible := !i.runtimeConsumer.Load() && i.matrixTurnInterruptKeyLocked() == key && i.tmuxSession == target && !i.IsArchived() && matrixCapacityRetryStatusAllowed(i.Status)
		i.mu.RUnlock()
		if !eligible {
			return false
		}
		latest := matrixInterruptFiles.load("codex", rollout)
		return latest.found && latest.meta.ThreadSource != "subagent" && latest.meta.ParentThreadID == "" && latest.verdict.TurnID == verdict.TurnID && latest.verdict.Kind == verdict.Kind
	}
	socket := target.SocketName
	if socket == "" {
		socket = tmux.DefaultSocketName()
	}
	detail, _ := matrixRunCapacityRetry(path, verdict, now, matrixCapacityInstanceTarget{instance: i, target: target, key: key, rollout: rollout, turn: verdict.TurnID,
		// matrix/hardened: pin capture and delivery to the managed agent pane;
		// switching to an auxiliary window must never redirect the retry.
		pane: &tmux.Session{Name: target.Name + ":0.0", SocketName: socket, VimMode: target.VimMode}}, valid)
	i.mu.Lock()
	if i.matrixInterrupt.key == key && i.matrixInterrupt.verdict.TurnID == verdict.TurnID {
		i.matrixInterrupt.retryDetail = detail
	}
	i.mu.Unlock()
}
