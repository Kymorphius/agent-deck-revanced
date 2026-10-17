// Package terminalbackend defines the terminal operations shared by the
// original session model and native Windows transport.
// matrix/hardened: the contract imports no platform or session implementation.
package terminalbackend

import (
	"context"
	"fmt"
)

const (
	MaxInputBytes = 64 * 1024
	MaxCols       = 500
	MaxRows       = 300
)

// Observation records process and output facts. Alive never means that an AI
// agent is working, waiting for a prompt, or ready to accept a message.
// OutputSequence is a monotonic output watermark for this pinned run.
type Observation struct {
	Alive          bool
	PID            uint32
	HostPID        int
	RunID          string
	ExitCode       *uint32
	OutputSequence uint64
}

// Backend is bound to one terminal target. A backend that binds a run identity
// must reject a stale run rather than following a replacement process. Stop returns
// success only after that target's owned process tree has completed teardown.
// Cancelling an operation must never kill the terminal. A legacy teardown may
// finish its already admitted, bounded cleanup after cancellation.
// matrix/hardened: launching and AI semantic status remain separate concerns.
type Backend interface {
	Observe(context.Context) (Observation, error)
	Capture(context.Context, bool) (string, error)
	Send(context.Context, []byte) error
	Resize(context.Context, int, int) error
	Stop(context.Context) error
}

// Binding identifies the immutable target of a backend that can be attached to
// an original session Instance. Its identity is established before any I/O.
// matrix/hardened: the process run is distinct from the persistent session ID.
type Binding struct {
	SessionID string
	RunID     string
}

// BoundBackend supplies an immutable session/run identity. A compatibility
// backend without this identity may serve legacy I/O but cannot be injected as
// a native runtime into an original Instance.
type BoundBackend interface {
	Backend
	Binding() Binding
}

// ValidateInput accepts raw terminal bytes, including Unicode and control input.
func ValidateInput(data []byte) error {
	if len(data) > MaxInputBytes {
		return fmt.Errorf("terminal input exceeds %d bytes", MaxInputBytes)
	}
	return nil
}

func ValidateSize(cols, rows int) error {
	if cols < 1 || cols > MaxCols || rows < 1 || rows > MaxRows {
		return fmt.Errorf("terminal dimensions must be 1..%d by 1..%d", MaxCols, MaxRows)
	}
	return nil
}
