//go:build windows

package nativeui

import tea "github.com/charmbracelet/bubbletea"

// matrix/hardened: decoder state belongs to a single native list window.
func consoleKeyProgramOption() tea.ProgramOption {
	return func(program *tea.Program) {
		decoder := &consoleKeyDecoder{}
		tea.WithFilter(decoder.filter)(program)
	}
}
