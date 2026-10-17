//go:build !windows

package main

// matrix/hardened: keep parser tests buildable off Windows; host IPC itself rejects unsupported systems.

func enableConsoleOutput(_ int) (func(), error) { return func() {}, nil }
