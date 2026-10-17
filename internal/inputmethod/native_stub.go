//go:build !darwin || !cgo

// matrix/hardened: other platforms and CGO-free builds preserve existing input.
package inputmethod

func newBackend() backend { return nil }
