//go:build !windows

// matrix/hardened: unsupported hosts cannot silently create a Unix session.
package conpty

import (
	"errors"
	"testing"
)

func TestStartUnsupported(t *testing.T) {
	p, err := Start(Options{Executable: "cmd.exe"})
	if p != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Start = %v, %v", p, err)
	}
}
