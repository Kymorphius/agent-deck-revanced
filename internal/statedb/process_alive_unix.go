//go:build !windows

// matrix/hardened: retain the original Unix primary-owner liveness behavior.
package statedb

import (
	"os"
	"syscall"
)

// pidAlive reports whether pid refers to a live process on this host. It uses
// the kill -0 idiom (signal 0 performs permission/existence checks only and is
// never delivered), mirroring filterAliveOurProcesses in
// internal/tmux/ensure_pids_dead.go. A dead or reaped PID returns false so a
// crashed primary is reclaimed immediately by ElectPrimary instead of lingering
// for the full heartbeat-staleness window.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
