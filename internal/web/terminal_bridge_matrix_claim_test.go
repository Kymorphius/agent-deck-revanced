package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// matrix/hardened: Resize must signal the attach client even when the size is
// unchanged, so a viewer coming back to the page takes tmux's `latest` slot.
// A shell that counts SIGWINCH stands in for `tmux attach`; no tmux needed.
func TestTmuxPTYBridgeResizeSignalsAttachClientWithoutSizeChange(t *testing.T) {
	marks := filepath.Join(t.TempDir(), "winch")
	cmd := exec.Command("sh", "-c", `trap 'echo w >> "$1"' WINCH; echo ready >> "$1"; while :; do sleep 0.05; done`, "sh", marks)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = ptmx.Close()
	})
	waitForWinch := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(marks)
			if strings.Count(string(data), "w\n") >= want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		data, _ := os.ReadFile(marks)
		t.Fatalf("attach client saw %d SIGWINCH, want >= %d", strings.Count(string(data), "w\n"), want)
	}
	waitForReady := func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if data, _ := os.ReadFile(marks); strings.Contains(string(data), "ready") {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("stand-in attach client never started")
	}
	waitForReady()

	b := &tmuxPTYBridge{cmd: cmd, ptmx: ptmx}
	// Same size as the PTY already has: the kernel raises nothing by itself.
	if err := b.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	waitForWinch(1)
	if err := b.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	waitForWinch(2)
}
