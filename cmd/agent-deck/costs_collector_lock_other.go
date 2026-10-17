//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import "fmt"

// matrix/hardened: do not silently launch duplicate collectors on a platform
// lacking the Unix ownership primitive used by the existing daemon paths.
type costCollectorLock struct{}

func tryCostCollectorLock(string) (*costCollectorLock, bool, error) {
	return nil, false, fmt.Errorf("exclusive Codex cost collection is unsupported on this platform")
}
func (*costCollectorLock) valid() error { return fmt.Errorf("cost collector lock unavailable") }
func (*costCollectorLock) Close() error { return nil }
