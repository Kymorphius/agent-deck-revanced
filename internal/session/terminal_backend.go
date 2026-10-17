package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalbackend"
)

// matrix/hardened: the portable contract can also be consumed by a Windows
// transport without importing this package's remaining Unix implementation.
type TerminalBackend = terminalbackend.Backend
type TerminalObservation = terminalbackend.Observation
type TerminalBinding = terminalbackend.Binding
type BoundTerminalBackend = terminalbackend.BoundBackend

var (
	ErrTerminalBackendPending    = errors.New("terminal backend is required but has not been bound")
	ErrTerminalBackendBound      = errors.New("terminal backend is already bound")
	ErrTerminalLaunchUnsupported = errors.New("launch, restart and fork are unavailable for this terminal backend until a native launch plan is installed")
	ErrTerminalWindowUnsupported = errors.New("this terminal backend has no auxiliary tmux windows")
)

const terminalOperationTimeout = 10 * time.Second

// MarkTerminalBackendPending prevents a persisted native descriptor from falling
// through to tmux before its transport has been verified and explicitly bound.
// matrix/hardened: call while hydrating the instance, before publishing it.
func (i *Instance) MarkTerminalBackendPending() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	// matrix/hardened: explicit native construction cannot conceal any legacy
	// terminal handle, whether or not that handle currently has a live process.
	if i.tmuxSession != nil {
		return fmt.Errorf("cannot mark a legacy tmux instance for a native terminal backend")
	}
	if i.terminalBackendExpected {
		return nil
	}
	i.terminalBackendExpected = true
	i.terminalBackendError = ErrTerminalBackendPending
	i.Status = StatusUnknown
	i.stopRevision++
	i.runtimeReplica.Store(nil)
	return nil
}

// BindTerminalBackend installs one immutable, already verified runtime target.
// It neither launches a process nor changes the original session identity.
// matrix/hardened: binding is construction, not a live backend switch. Callers
// must construct a new binding for a replacement run instead of rebinding here.
func (i *Instance) BindTerminalBackend(backend TerminalBackend) error {
	if nilTerminalBackend(backend) {
		return fmt.Errorf("terminal backend must not be nil")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.terminalBackend != nil {
		return ErrTerminalBackendBound
	}
	// matrix/hardened: pending intent is explicit; attaching a backend must not
	// switch a live legacy runtime or a same-named foreign session's identity.
	if !i.terminalBackendExpected || i.tmuxSession != nil {
		return fmt.Errorf("terminal backend binding requires an explicitly pending native instance without a tmux session")
	}
	bound, ok := backend.(BoundTerminalBackend)
	if !ok {
		return fmt.Errorf("terminal backend binding requires an immutable session and run identity")
	}
	binding := bound.Binding()
	if i.ID == "" || binding.SessionID != i.ID || strings.TrimSpace(binding.RunID) == "" {
		return fmt.Errorf("terminal backend binding does not match the original session identity and a nonempty run")
	}
	i.terminalBackend = backend
	i.terminalBackendError = nil
	i.Status = StatusUnknown
	i.stopRevision++
	i.runtimeReplica.Store(nil)
	return nil
}

func nilTerminalBackend(backend TerminalBackend) bool {
	if backend == nil {
		return true
	}
	v := reflect.ValueOf(backend)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// UsesTerminalBackend also returns true for an unbound persisted descriptor.
func (i *Instance) UsesTerminalBackend() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.terminalBackendExpected
}

// TerminalError exposes transport and teardown failures separately from AI
// semantic status. An uncertain probe or stop must never invent a stopped state.
func (i *Instance) TerminalError() error {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.terminalBackendError
}

func (i *Instance) terminalBackendSnapshot() (TerminalBackend, bool, uint64) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.terminalBackend, i.terminalBackendExpected, i.stopRevision
}

func (i *Instance) recordTerminalError(revision uint64, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stopRevision == revision {
		i.terminalBackendError = err
	}
}

// ObserveTerminal returns process facts without applying an AI activity verdict.
func (i *Instance) ObserveTerminal(ctx context.Context) (TerminalObservation, error) {
	if err := ctx.Err(); err != nil {
		return TerminalObservation{}, err
	}
	backend, expected, revision := i.terminalBackendSnapshot()
	if !expected {
		var err error
		backend, err = i.tmuxTerminalBackend()
		if err != nil {
			return TerminalObservation{}, err
		}
	} else if backend == nil {
		i.recordTerminalError(revision, ErrTerminalBackendPending)
		return TerminalObservation{}, ErrTerminalBackendPending
	}
	observation, err := backend.Observe(ctx)
	if expected {
		i.recordTerminalError(revision, err)
	}
	return observation, err
}

// CaptureTerminal captures the visible screen, or retained scrollback when full
// is true. The original Preview methods retain their historical tmux behavior.
func (i *Instance) CaptureTerminal(ctx context.Context, full bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	backend, expected, revision := i.terminalBackendSnapshot()
	if !expected {
		var err error
		backend, err = i.tmuxTerminalBackend()
		if err != nil {
			return "", err
		}
	} else if backend == nil {
		i.recordTerminalError(revision, ErrTerminalBackendPending)
		return "", ErrTerminalBackendPending
	}
	content, err := backend.Capture(ctx, full)
	if expected {
		i.recordTerminalError(revision, err)
	}
	return content, err
}

// SendTerminal forwards raw bytes. It does not infer prompt readiness or submit
// a message on the caller's behalf; Enter and escape sequences are explicit.
func (i *Instance) SendTerminal(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := terminalbackend.ValidateInput(data); err != nil {
		return err
	}
	backend, expected, revision := i.terminalBackendSnapshot()
	if !expected {
		var err error
		backend, err = i.tmuxTerminalBackend()
		if err != nil {
			return err
		}
	} else if backend == nil {
		i.recordTerminalError(revision, ErrTerminalBackendPending)
		return ErrTerminalBackendPending
	}
	err := backend.Send(ctx, data)
	if expected {
		i.recordTerminalError(revision, err)
	}
	return err
}

func (i *Instance) ResizeTerminal(ctx context.Context, cols, rows int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := terminalbackend.ValidateSize(cols, rows); err != nil {
		return err
	}
	backend, expected, revision := i.terminalBackendSnapshot()
	if !expected {
		var err error
		backend, err = i.tmuxTerminalBackend()
		if err != nil {
			return err
		}
	} else if backend == nil {
		i.recordTerminalError(revision, ErrTerminalBackendPending)
		return ErrTerminalBackendPending
	}
	err := backend.Resize(ctx, cols, rows)
	if expected {
		i.recordTerminalError(revision, err)
	}
	return err
}

func (i *Instance) terminalLaunchGuard() error {
	if i.UsesTerminalBackend() {
		return ErrTerminalLaunchUnsupported
	}
	return nil
}

func (i *Instance) terminalHasUpdated() bool {
	backend, _, revision := i.terminalBackendSnapshot()
	if backend == nil {
		i.recordTerminalError(revision, ErrTerminalBackendPending)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalOperationTimeout)
	defer cancel()
	observation, err := backend.Observe(ctx)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stopRevision != revision {
		return false
	}
	i.terminalBackendError = err
	if err != nil {
		return false
	}
	// matrix/hardened: concurrent probes may complete out of order. A pinned
	// run's monotonic watermark cannot move backward when an older read returns.
	updated := observation.OutputSequence > i.terminalOutputSequence
	if updated {
		i.terminalOutputSequence = observation.OutputSequence
	}
	return updated
}

// updateTerminalStatus deliberately applies no activity status. Current native
// process facts cannot establish a model turn, prompt readiness, or task state.
func (i *Instance) updateTerminalStatus() error {
	ctx, cancel := context.WithTimeout(context.Background(), terminalOperationTimeout)
	defer cancel()
	_, err := i.ObserveTerminal(ctx)
	return err
}

func (i *Instance) stopTerminalBackend() error {
	// matrix/hardened: serialize exact-run stops and retain a completed stop even
	// if a second caller arrives after the transport has retired that run.
	i.terminalStopMu.Lock()
	defer i.terminalStopMu.Unlock()
	i.mu.RLock()
	if i.terminalStopConfirmed {
		i.mu.RUnlock()
		return nil
	}
	backend, revision := i.terminalBackend, i.stopRevision
	i.mu.RUnlock()
	if backend == nil {
		i.recordTerminalError(revision, ErrTerminalBackendPending)
		return ErrTerminalBackendPending
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalOperationTimeout)
	defer cancel()
	err := backend.Stop(ctx)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stopRevision != revision {
		return fmt.Errorf("terminal stop verdict was superseded")
	}
	i.terminalBackendError = err
	if err != nil {
		return fmt.Errorf("failed to stop terminal backend: %w", err)
	}
	i.Status = StatusStopped
	i.terminalStopConfirmed = true
	i.stopRevision++
	i.clearAuthHoldLocked()
	return nil
}
