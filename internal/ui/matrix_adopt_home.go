package ui

// matrix/hardened: Home-side glue for the alt+u adopt panel. Kept out of
// home.go so upstream merges only meet one-line hooks there.

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// openAdoptPanel lists adoptable Claude processes on the remote under the
// cursor, or on this machine otherwise.
func (h *Home) openAdoptPanel() tea.Cmd {
	if h.adoptPanel == nil {
		h.adoptPanel = NewAdoptPanel()
	}
	h.adoptPanel.SetSize(h.width, h.height)

	target := ""
	if h.cursor >= 0 && h.cursor < len(h.flatItems) {
		if item := h.flatItems[h.cursor]; item.Type == session.ItemTypeRemoteGroup || item.Type == session.ItemTypeRemoteSession {
			target = item.RemoteName
		}
	}
	return h.adoptPanel.Open(target, h.adoptRunner(target))
}

// adoptRunner runs agent-deck on target: this binary under the TUI's profile,
// or the remote's agent-deck over its SSH runner.
func (h *Home) adoptRunner(target string) adoptRunFunc {
	if target == "" {
		profile := h.profile
		return func(ctx context.Context, args ...string) ([]byte, error) {
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			if profile != "" {
				args = append([]string{"-p", profile}, args...)
			}
			return exec.CommandContext(ctx, exe, args...).Output() //nolint:gosec // our own binary, fixed verbs
		}
	}
	return func(ctx context.Context, args ...string) ([]byte, error) {
		cfg, err := session.LoadUserConfig()
		if err != nil || cfg == nil {
			return nil, fmt.Errorf("load config: %v", err)
		}
		rc, ok := cfg.Remotes[target]
		if !ok {
			return nil, fmt.Errorf("remote %q is not configured", target)
		}
		return session.NewSSHRunner(target, rc).RunCommand(ctx, args...)
	}
}

// handleAdoptMsg consumes the panel's async results. ok is false for any
// other message.
func (h *Home) handleAdoptMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case adoptLoadedMsg:
		if h.adoptPanel != nil {
			h.adoptPanel.handleLoaded(m)
		}
		return nil, true
	case adoptDoneMsg:
		var cmds []tea.Cmd
		if h.adoptPanel != nil {
			cmds = append(cmds, h.adoptPanel.handleDone(m))
		}
		if m.err == nil {
			if m.target != "" {
				h.noteRemoteActionSettled(m.target)
				cmds = append(cmds, h.fetchRemoteSessions)
			} else {
				state := h.preserveState()
				cmds = append(cmds, h.sessionLoadCmd(&state, false))
			}
		}
		return tea.Batch(cmds...), true
	}
	return nil, false
}
