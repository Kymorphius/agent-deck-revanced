//go:build windows

package nativeui

import (
	"io"
	"os"

	"github.com/charmbracelet/x/term"
)

// matrix/hardened: keep the real stdout file and Fd for Bubble Tea's Windows
// terminal setup, while filtering private per-frame IME cursor metadata.
type nativeCursorOutput struct {
	*os.File
	frames cursorFrameWriter
}

var _ term.File = (*nativeCursorOutput)(nil)

func (output *nativeCursorOutput) Write(p []byte) (int, error) {
	return output.frames.Write(p)
}

func nativeUIOutput() (io.Writer, bool) {
	return &nativeCursorOutput{File: os.Stdout, frames: cursorFrameWriter{output: os.Stdout}}, true
}

func nativeCursorFrameView(view string, frame cursorFrame) string {
	return encodeCursorFrame(view, frame)
}
