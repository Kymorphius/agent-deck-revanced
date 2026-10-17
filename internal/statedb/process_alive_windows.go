//go:build windows

// matrix/hardened: signal zero is unsupported on Windows; unknown ownership is never reclaimed as dead.
package statedb

import (
	"errors"
	"golang.org/x/sys/windows"
)

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	return err != nil || status != windows.WAIT_OBJECT_0
}
