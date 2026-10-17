package nativeui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: one observer sends only valid geometry changes. Its sender
// must use the same cancellation context, as Program.Send does with WithContext;
// this keeps an Exec pause from leaving a blocked sender after the UI exits.
func observeWindowSizes(ctx context.Context, ticks <-chan time.Time, size func() (int, int, error), send func(tea.Msg)) {
	var lastWidth, lastHeight int
	for {
		if ctx.Err() != nil {
			return
		}
		width, height, err := size()
		if err == nil && width > 0 && height > 0 && (width != lastWidth || height != lastHeight) {
			send(tea.WindowSizeMsg{Width: width, Height: height})
			lastWidth, lastHeight = width, height
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
		}
	}
}
