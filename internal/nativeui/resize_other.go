//go:build !windows

package nativeui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: other platforms keep Bubble Tea's native resize listener.
func observeProgramWindowSize(context.Context, *tea.Program) func() {
	return func() {}
}
