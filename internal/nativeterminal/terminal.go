// Package nativeterminal adapts the durable Windows fleet to the shared session
// terminal interface. It never creates a manager or starts/rebinds a session.
// matrix/hardened: original session business code can use an explicitly verified
// native run without importing a second UI or registry implementation.
package nativeterminal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/terminalbackend"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
)

var (
	ErrInvalidBinding     = errors.New("invalid native terminal binding")
	ErrTargetMismatch     = errors.New("native terminal response belongs to another session or run")
	ErrInvalidResponse    = errors.New("invalid native terminal response")
	ErrHistoryUnavailable = errors.New("native retained output changed during capture")
	ErrObservation        = errors.New("native terminal process observation failed")
)

// Caller performs one request against the given pipe. It must verify server
// identity and close only its own client on cancellation. New uses winipc.Dial,
// which checks the current user's SID on every connection.
type Caller func(context.Context, string, terminalhost.Request) (terminalhost.Response, error)

// Terminal has an immutable binding to one explicitly selected run. Concurrent
// operations cannot silently acquire a later run, even after the manager restarts.
type Terminal struct {
	pipe      string
	sessionID string
	runID     string
	caller    Caller
}

var _ terminalbackend.Backend = (*Terminal)(nil)
var _ terminalbackend.BoundBackend = (*Terminal)(nil)

// Binding returns the immutable record/run identity for the original Instance
// to verify before accepting this transport. Returning a value cannot rebind it.
// matrix/hardened: a valid pipe response alone does not link two session rows.
func (t *Terminal) Binding() terminalbackend.Binding {
	if t == nil {
		return terminalbackend.Binding{}
	}
	return terminalbackend.Binding{SessionID: t.sessionID, RunID: t.runID}
}

// New binds a native run after its caller has checked the manager's version,
// protocol, namespace and selected session/run IDs. It does not perform that
// manager handshake itself. All later calls remain fenced to these exact IDs.
func New(pipe, sessionID, runID string) (*Terminal, error) {
	return NewWithCaller(pipe, sessionID, runID, callPipe)
}

// NewWithCaller has the same binding contract as New and permits offline protocol
// tests. A caller must not retry writes or substitute a current run on failure.
func NewWithCaller(pipe, sessionID, runID string, caller Caller) (*Terminal, error) {
	if err := validatePipe(pipe); err != nil {
		return nil, err
	}
	for _, binding := range []struct{ name, value string }{{"session", sessionID}, {"run", runID}} {
		id, err := uuid.Parse(binding.value)
		if err != nil || id == uuid.Nil || id.String() != binding.value {
			return nil, fmt.Errorf("%w: %s ID must be a canonical nonzero UUID", ErrInvalidBinding, binding.name)
		}
	}
	if caller == nil {
		return nil, fmt.Errorf("%w: caller is required", ErrInvalidBinding)
	}
	return &Terminal{pipe: pipe, sessionID: sessionID, runID: runID, caller: caller}, nil
}

func validatePipe(pipe string) error {
	if !strings.HasPrefix(pipe, winipc.PipePrefix) {
		return fmt.Errorf("%w: %w", ErrInvalidBinding, winipc.ErrInvalidPipePath)
	}
	suffix := strings.TrimPrefix(pipe, winipc.PipePrefix)
	if len(suffix) == 0 || len(suffix) > 128 {
		return fmt.Errorf("%w: %w", ErrInvalidBinding, winipc.ErrInvalidPipePath)
	}
	for _, c := range suffix {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return fmt.Errorf("%w: %w", ErrInvalidBinding, winipc.ErrInvalidPipePath)
	}
	return nil
}

func callPipe(ctx context.Context, pipe string, request terminalhost.Request) (terminalhost.Response, error) {
	conn, err := winipc.Dial(ctx, pipe)
	if err != nil {
		return terminalhost.Response{}, err
	}
	defer conn.Close()
	return terminalhost.Call(ctx, conn, request)
}

func (t *Terminal) call(ctx context.Context, request terminalhost.Request) (terminalhost.Response, error) {
	if t == nil || t.caller == nil {
		return terminalhost.Response{}, ErrInvalidBinding
	}
	if ctx == nil {
		return terminalhost.Response{}, errors.New("native terminal context is required")
	}
	if err := ctx.Err(); err != nil {
		return terminalhost.Response{}, err
	}
	request.SessionID, request.RunID = t.sessionID, t.runID
	response, err := t.caller(ctx, t.pipe, request)
	if contextErr := ctx.Err(); contextErr != nil {
		return terminalhost.Response{}, contextErr
	}
	// A transport failure can have no decoded response. Any decoded target,
	// including an error response exposing a newer run, must still match.
	if err == nil || response.SessionID != "" || response.RunID != "" {
		if response.SessionID != t.sessionID || response.RunID != t.runID {
			return terminalhost.Response{}, errors.Join(ErrTargetMismatch, err)
		}
	}
	if err != nil {
		return terminalhost.Response{}, err
	}
	if !response.OK {
		return terminalhost.Response{}, fmt.Errorf("native terminal: %s", response.Error)
	}
	if response.HostPID <= 0 || response.Running && response.PID == 0 {
		return terminalhost.Response{}, fmt.Errorf("%w: missing observed process identity", ErrInvalidResponse)
	}
	return response, nil
}

// Observe reports process liveness only. It does not infer an AI's task status
// from a PID, output activity, or a successful wire request.
func (t *Terminal) Observe(ctx context.Context) (terminalbackend.Observation, error) {
	response, err := t.call(ctx, terminalhost.Request{Op: "status"})
	if err != nil {
		return terminalbackend.Observation{}, err
	}
	observation := terminalbackend.Observation{Alive: response.Running, PID: response.PID, HostPID: response.HostPID, RunID: response.RunID, OutputSequence: response.HeadOffset}
	if response.ExitCode != nil {
		code := *response.ExitCode
		observation.ExitCode = &code
	}
	if response.Error != "" {
		return observation, fmt.Errorf("%w: %s", ErrObservation, response.Error)
	}
	return observation, nil
}

// Capture returns safe plain text. With full=false it uses the host's emulated
// current screen, never replaying raw VT queries in the caller's terminal.
// full=true collects the raw output retained at the first snapshot (at most
// terminalhost.HistoryBytes), then removes terminal controls. This is bounded
// process output, not a complete transcript or reconstructed scrollback screen.
// Output evicted while paging is an explicit error, never a silently gapped log.
func (t *Terminal) Capture(ctx context.Context, full bool) (string, error) {
	response, err := t.call(ctx, terminalhost.Request{Op: "capture"})
	if err != nil {
		return "", err
	}
	if !full {
		return plainText(response.Screen), nil
	}
	if err := validateCapture(response, 0); err != nil {
		return "", err
	}
	start, limit := response.StartOffset, response.HeadOffset
	if limit-start > terminalhost.HistoryBytes {
		return "", fmt.Errorf("%w: retained output exceeds its bound", ErrInvalidResponse)
	}
	output := make([]byte, 0, int(limit-start))
	for pages := 1; pages <= terminalhost.HistoryBytes/terminalhost.MaxOutputBytes+1; pages++ {
		end := min(response.EndOffset, limit)
		output = append(output, response.Output[:end-response.StartOffset]...)
		if end == limit {
			return plainText(string(output)), nil
		}
		if end == response.StartOffset {
			return "", fmt.Errorf("%w: capture cursor made no progress", ErrInvalidResponse)
		}
		if pages == terminalhost.HistoryBytes/terminalhost.MaxOutputBytes+1 {
			return "", fmt.Errorf("%w: capture exceeded bounded page count", ErrInvalidResponse)
		}
		response, err = t.call(ctx, terminalhost.Request{Op: "capture", After: end})
		if err != nil {
			return "", err
		}
		if err := validateCapture(response, end); err != nil {
			return "", err
		}
		if response.StartOffset != end {
			return "", ErrHistoryUnavailable
		}
		if response.HeadOffset < limit {
			return "", fmt.Errorf("%w: output sequence regressed", ErrInvalidResponse)
		}
	}
	return "", fmt.Errorf("%w: capture exceeded bounded page count", ErrInvalidResponse)
}

func validateCapture(response terminalhost.Response, after uint64) error {
	if response.StartOffset < after || response.EndOffset < response.StartOffset || response.EndOffset > response.HeadOffset ||
		response.EndOffset-response.StartOffset != uint64(len(response.Output)) || len(response.Output) > terminalhost.MaxOutputBytes {
		return fmt.Errorf("%w: inconsistent capture cursor", ErrInvalidResponse)
	}
	return nil
}

func plainText(text string) string {
	text = ansi.Strip(text)
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	return strings.Map(func(c rune) rune {
		if c < 32 && c != '\n' && c != '\t' || c >= 127 && c < 160 {
			return -1
		}
		return c
	}, text)
}

func (t *Terminal) Send(ctx context.Context, data []byte) error {
	if err := terminalbackend.ValidateInput(data); err != nil {
		return err
	}
	_, err := t.call(ctx, terminalhost.Request{Op: "send", Data: append([]byte(nil), data...)})
	return err
}

func (t *Terminal) Resize(ctx context.Context, cols, rows int) error {
	if err := terminalbackend.ValidateSize(cols, rows); err != nil {
		return err
	}
	_, err := t.call(ctx, terminalhost.Request{Op: "resize", Cols: cols, Rows: rows})
	return err
}

func (t *Terminal) Stop(ctx context.Context) error {
	response, err := t.call(ctx, terminalhost.Request{Op: "stop"})
	if err != nil {
		return err
	}
	if response.Running {
		return fmt.Errorf("%w: stop acknowledged a live process", ErrInvalidResponse)
	}
	return nil
}
