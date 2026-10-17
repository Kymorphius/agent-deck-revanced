//go:build !windows

// matrix/hardened: platform stubs explicitly reject operations instead of using a TCP fallback.
package winipc

import (
	"context"
	"errors"
	"testing"
)

func TestUnsupportedHost(t *testing.T) {
	if listener, err := Listen(PipePrefix + "test"); listener != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Listen = %v, %v", listener, err)
	}
	if conn, err := Dial(context.Background(), PipePrefix+"test"); conn != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Dial = %v, %v", conn, err)
	}
	if err := ProtectPath(`C:\state`); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ProtectPath = %v", err)
	}
	if sid, err := CurrentUserSID(); sid != "" || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("CurrentUserSID = %q, %v", sid, err)
	}
	if _, err := Listen(`\\server\pipe\agent-deck-native-test`); !errors.Is(err, ErrInvalidPipePath) {
		t.Fatalf("remote pipe = %v", err)
	}
	if err := ProtectPath("."); !errors.Is(err, ErrInvalidStatePath) {
		t.Fatalf("ambiguous state path = %v", err)
	}
}
