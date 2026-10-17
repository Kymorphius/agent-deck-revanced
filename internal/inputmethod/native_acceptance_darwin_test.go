//go:build darwin && cgo

package inputmethod

// matrix/hardened: explicit, foreground-terminal-only native acceptance. The
// normal suite skips this test and never switches the desktop's input source.
import (
	"os"
	"testing"
	"time"
)

func TestInputMethodNativeAcceptance(t *testing.T) {
	if os.Getenv("AGENTDECK_IME_ACCEPTANCE") != "1" {
		t.Skip("requires an explicitly opened local foreground terminal")
	}
	b := newBackend()
	if b == nil {
		t.Fatal("no local terminal owner")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !b.foreground() && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if !b.foreground() {
		t.Fatal("test terminal did not gain focus")
	}
	original := b.current()
	defer b.selectSource(original)
	target := os.Getenv("AGENTDECK_IME_ACCEPTANCE_SOURCE")
	if target == "" {
		target = original
	}
	if target == b.ascii() {
		t.Fatal("select a non-ASCII input method for this test")
	}
	if !b.selectSource(target) {
		t.Fatalf("cannot select test source %q", target)
	}
	wait := func(want string) {
		t.Helper()
		until := time.Now().Add(time.Second)
		for b.current() != want && time.Now().Before(until) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := b.current(); got != want {
			t.Fatalf("native source=%q want=%q", got, want)
		}
		t.Log("native source:", want)
	}
	wait(target)
	c := &Controller{backend: b}
	c.Sync(true)
	wait(b.ascii())
	c.Sync(false)
	wait(target)
}
