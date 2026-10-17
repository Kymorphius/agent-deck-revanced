package session

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/asheshgoplani/agent-deck/internal/terminalbackend"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: the compatibility adapter keeps the existing tmux object and
// its process-tree teardown. The original Preview/Exists/Kill paths are unchanged.
type tmuxTerminalBackend struct {
	session  *tmux.Session
	name     string
	socket   string
	mu       sync.Mutex
	output   string
	sequence uint64
}

func (i *Instance) tmuxTerminalBackend() (TerminalBackend, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.terminalBackendExpected {
		return nil, ErrTerminalBackendPending
	}
	if i.tmuxSession == nil {
		return nil, fmt.Errorf("tmux session not initialized")
	}
	if b := i.terminalTmuxAdapter; b != nil && b.session == i.tmuxSession && b.name == i.tmuxSession.Name && b.socket == i.tmuxSession.SocketName {
		return b, nil
	}
	b := &tmuxTerminalBackend{session: i.tmuxSession, name: i.tmuxSession.Name, socket: i.tmuxSession.SocketName}
	i.terminalTmuxAdapter = b
	return b, nil
}

func (b *tmuxTerminalBackend) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.session == nil || b.name == "" {
		return fmt.Errorf("tmux session not initialized")
	}
	if b.session.Name != b.name || b.session.SocketName != b.socket {
		return fmt.Errorf("tmux terminal target changed")
	}
	return nil
}

func (b *tmuxTerminalBackend) Observe(ctx context.Context) (TerminalObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, terminalOperationTimeout)
	defer cancel()
	if err := b.check(ctx); err != nil {
		return TerminalObservation{}, err
	}
	_, err := tmux.ExecContext(ctx, b.socket, "has-session", "-t", "="+b.name).Output()
	if err != nil {
		if ctx.Err() != nil {
			return TerminalObservation{}, ctx.Err()
		}
		// matrix/hardened: a failed command alone does not prove process death.
		// Only authoritative missing diagnostics are accepted; indeterminate
		// failures remain visible to the caller.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if strings.HasPrefix(stderr, "can't find session:") || strings.HasPrefix(stderr, "no server running on ") ||
				(strings.Contains(stderr, "error connecting to") && strings.Contains(stderr, "No such file or directory")) {
				return TerminalObservation{}, nil
			}
		}
		if err != nil {
			return TerminalObservation{}, fmt.Errorf("tmux terminal probe: %w", err)
		}
	}
	facts, err := tmux.ExecContext(ctx, b.socket, "display-message", "-p", "-t", "="+b.name+":^", "#{pane_dead}|#{pane_pid}|#{pane_dead_status}").Output()
	if err != nil {
		return TerminalObservation{}, fmt.Errorf("tmux terminal process probe: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(string(facts)), "|")
	if len(parts) != 3 || (parts[0] != "0" && parts[0] != "1") {
		return TerminalObservation{}, fmt.Errorf("tmux terminal process probe returned malformed facts")
	}
	pid, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || pid == 0 {
		return TerminalObservation{}, fmt.Errorf("tmux terminal process probe returned invalid PID")
	}
	observation := TerminalObservation{Alive: parts[0] == "0", PID: uint32(pid)}
	if parts[0] == "1" && parts[2] != "" {
		exitCode, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			return TerminalObservation{}, fmt.Errorf("tmux terminal process probe returned invalid exit code")
		}
		value := uint32(exitCode)
		observation.ExitCode = &value
	}
	content, err := b.Capture(ctx, false)
	if err != nil {
		return TerminalObservation{}, err
	}
	b.mu.Lock()
	if content != b.output {
		b.sequence++
		b.output = content
	}
	sequence := b.sequence
	b.mu.Unlock()
	observation.OutputSequence = sequence
	return observation, nil
}

func (b *tmuxTerminalBackend) Capture(ctx context.Context, full bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, terminalOperationTimeout)
	defer cancel()
	if err := b.check(ctx); err != nil {
		return "", err
	}
	args := []string{"capture-pane", "-t", "=" + b.name + ":^", "-p", "-e"}
	if full {
		args = append(args, "-S", "-2000")
	}
	output, err := tmux.ExecContext(ctx, b.socket, args...).Output()
	if err != nil {
		return "", fmt.Errorf("tmux terminal capture: %w", err)
	}
	return string(output), nil
}

func (b *tmuxTerminalBackend) Send(ctx context.Context, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, terminalOperationTimeout)
	defer cancel()
	if err := b.check(ctx); err != nil {
		return err
	}
	if err := terminalbackend.ValidateInput(data); err != nil {
		return err
	}
	// matrix/hardened: raw hex keys preserve embedded NUL, UTF-8 and control
	// sequences without shell quoting. Chunking bounds argv and partial writes
	// remain errors rather than an invented successful message submission.
	for len(data) > 0 {
		count := min(len(data), 1024)
		args := make([]string, 0, count+4)
		args = append(args, "send-keys", "-H", "-t", "="+b.name+":^")
		for _, value := range data[:count] {
			args = append(args, fmt.Sprintf("%02x", value))
		}
		if err := tmux.ExecContext(ctx, b.socket, args...).Run(); err != nil {
			return fmt.Errorf("tmux terminal input: %w", err)
		}
		data = data[count:]
	}
	return nil
}

func (b *tmuxTerminalBackend) Resize(ctx context.Context, cols, rows int) error {
	ctx, cancel := context.WithTimeout(ctx, terminalOperationTimeout)
	defer cancel()
	if err := b.check(ctx); err != nil {
		return err
	}
	if err := terminalbackend.ValidateSize(cols, rows); err != nil {
		return err
	}
	if err := tmux.ExecContext(ctx, b.socket, "resize-window", "-t", "="+b.name+":^", "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)).Run(); err != nil {
		return fmt.Errorf("tmux terminal resize: %w", err)
	}
	return nil
}

func (b *tmuxTerminalBackend) Stop(ctx context.Context) error {
	if err := b.check(ctx); err != nil {
		return err
	}
	// matrix/hardened: retain legacy synchronous owned-process reaping. Its
	// existing bounded teardown finishes once admitted, even if ctx is cancelled.
	return b.session.KillAndWait()
}

var _ terminalbackend.Backend = (*tmuxTerminalBackend)(nil)
