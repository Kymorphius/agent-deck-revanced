//go:build windows

package main

// matrix/hardened: native console output must enable VT parsing without changing persistent settings.

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func enableConsoleOutput(fd int) (func(), error) {
	handle := windows.Handle(fd)
	var original uint32
	if err := windows.GetConsoleMode(handle, &original); err != nil {
		return nil, fmt.Errorf("read console output mode: %w", err)
	}
	mode := original | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(handle, mode); err != nil {
		return nil, fmt.Errorf("enable console VT output: %w", err)
	}
	return func() { _ = windows.SetConsoleMode(handle, original) }, nil
}
