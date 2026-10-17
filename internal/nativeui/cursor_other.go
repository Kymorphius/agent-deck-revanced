//go:build !windows

package nativeui

import (
	"io"
	"os"
)

func nativeUIOutput() (io.Writer, bool) { return os.Stdout, false }

func nativeCursorFrameView(view string, _ cursorFrame) string { return view }
