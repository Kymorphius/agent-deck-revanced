package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// CheckPermanentDeleteTerminal prevents deletion from killing the terminal
// hosting its own confirmation or execution. Previewing a plan does not call it.
// matrix/hardened: every deletion entry point shares the same bounded, pane-specific guard.
func CheckPermanentDeleteTerminal(inst *Instance) error {
	var target *tmux.Session
	if inst != nil {
		target = inst.GetTmuxSession()
	}
	return checkPermanentDeleteTerminal(target, os.Getenv("TMUX"), os.Getenv("TMUX_PANE"), tmux.OutputBounded)
}

// matrix/hardened: inject probes per call so tests do not replace shared globals.
func checkPermanentDeleteTerminal(target *tmux.Session, callerEnv, callerPane string, probe func(string, ...string) ([]byte, error)) error {
	if callerEnv == "" || target == nil {
		return nil
	}
	callerSocket, _, hasComma := strings.Cut(callerEnv, ",")
	if !hasComma || !filepath.IsAbs(callerSocket) || strings.ContainsAny(callerSocket, "\r\n\x00|") {
		return errors.New("cannot verify caller terminal: invalid TMUX socket path")
	}
	if len(callerPane) < 2 || callerPane[0] != '%' || strings.IndexFunc(callerPane[1:], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("cannot verify caller terminal: missing or invalid TMUX_PANE")
	}
	if target.Name == "" || strings.ContainsAny(target.Name, "\r\n\x00|") {
		return errors.New("cannot verify deletion target terminal: invalid session name")
	}
	const format = "#{socket_path}|#{session_name}"
	out, err := probe("", "-S", callerSocket, "display-message", "-p", "-t", callerPane, format)
	if err != nil {
		return fmt.Errorf("cannot verify caller terminal: %w", err)
	}
	actualCallerSocket, callerName, err := permanentDeleteTerminalIdentity(out)
	if err != nil {
		return fmt.Errorf("cannot verify caller terminal: %w", err)
	}
	expectedCallerSocket, err := permanentDeleteSocketPath(callerSocket)
	if err != nil || actualCallerSocket != expectedCallerSocket {
		return errors.New("cannot verify caller terminal: socket identity changed")
	}
	if callerName != target.Name {
		return nil
	}
	// A same-named session on another server is safe. Query the target's actual
	// socket rather than comparing the caller's -S path to a target's -L name.
	out, err = probe(target.SocketName, "display-message", "-p", "-t", "="+target.Name, format)
	if err != nil {
		return fmt.Errorf("cannot verify deletion target terminal: %w", err)
	}
	actualTargetSocket, targetName, err := permanentDeleteTerminalIdentity(out)
	if err != nil {
		return fmt.Errorf("cannot verify deletion target terminal: %w", err)
	}
	if targetName != target.Name {
		return errors.New("cannot verify deletion target terminal: session identity changed")
	}
	if actualCallerSocket == actualTargetSocket {
		return errors.New("run permanent deletion from the agent-deck main menu or a terminal outside the target session")
	}
	return nil
}

// matrix/hardened: malformed or multi-row identities cannot authorize teardown.
func permanentDeleteTerminalIdentity(out []byte) (string, string, error) {
	line := strings.TrimSuffix(string(out), "\n")
	fields := strings.Split(line, "|")
	if len(fields) != 2 || strings.TrimSpace(fields[1]) == "" || strings.ContainsAny(line, "\r\n\x00") {
		return "", "", errors.New("invalid tmux terminal identity")
	}
	socket, err := permanentDeleteSocketPath(fields[0])
	if err != nil {
		return "", "", err
	}
	return socket, fields[1], nil
}

// matrix/hardened: /var and /private/var (or another symlink) denote one server.
func permanentDeleteSocketPath(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00|") {
		return "", errors.New("invalid tmux socket path")
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve tmux socket path: %w", err)
	}
	return path, nil
}
