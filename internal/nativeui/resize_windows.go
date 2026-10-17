//go:build windows

package nativeui

import (
	"context"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

// matrix/hardened: pinned Bubble Tea does not observe Windows console resizes.
// Poll only while this UI is open, including the list and input forms.
func observeProgramWindowSize(ctx context.Context, program *tea.Program) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticks := time.NewTicker(250 * time.Millisecond)
		defer ticks.Stop()
		observeWindowSizes(ctx, ticks.C, func() (int, int, error) {
			return term.GetSize(int(os.Stdout.Fd()))
		}, program.Send)
	}()
	return func() { <-done }
}
