//go:build !windows

package nativeui

import tea "github.com/charmbracelet/bubbletea"

// matrix/hardened: other platforms already receive decoded Unicode input.
func consoleKeyProgramOption() tea.ProgramOption {
	return func(*tea.Program) {}
}
