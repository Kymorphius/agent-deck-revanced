//go:build windows

package nativeui

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
)

func TestNativeCursorOutputRetainsConsoleFileIdentity(t *testing.T) {
	writer, filtersCursorFrames := nativeUIOutput()
	output, ok := writer.(term.File)
	if !ok || output.Fd() != os.Stdout.Fd() {
		t.Fatal("cursor filtering hid the console file descriptor from Bubble Tea")
	}
	wrapped, ok := output.(*nativeCursorOutput)
	if !ok || !filtersCursorFrames || wrapped.File != os.Stdout || wrapped.frames.output != os.Stdout {
		t.Fatal("cursor output did not retain the original console file")
	}
}

func TestPrivateCursorFramesRequireFilteringOutputOptIn(t *testing.T) {
	m := modelWith(&fakeService{})
	press(m, "n")
	if strings.Contains(m.View(), cursorFramePrefix) {
		t.Fatal("ordinary New/View emitted private metadata without a filtering output")
	}
	_, filtersCursorFrames := nativeUIOutput()
	m.cursorFrames = filtersCursorFrames
	if !strings.HasPrefix(m.View(), cursorFramePrefix) {
		t.Fatal("filtering output did not enable its paired frame protocol")
	}
}
