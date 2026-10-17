package session

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: exercise the common Instance I/O against a real isolated
// legacy terminal. Raw bytes and the configured socket must survive the seam.
func TestInstanceTerminalTmuxAdapterPreservesRawBytesAndExactTarget(t *testing.T) {
	skipIfNoTmuxBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	payload := []byte("中文\x00\x03\x1b[A\r\n")
	outputPath := filepath.Join(dir, "input.bin")
	readyPath := filepath.Join(dir, "ready")
	scriptPath := filepath.Join(dir, "capture.sh")
	script := fmt.Sprintf("#!/bin/sh\nstty raw -echo\n: > %s\ndd bs=1 count=%d of=%s 2>/dev/null\nprintf '\\033[2J\\033[Hterminal byte capture done'\nsleep 30\n", shellescape.Quote(readyPath), len(payload), shellescape.Quote(outputPath))
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	session := tmux.NewSession("common-terminal-byte-test", dir)
	session.SocketName = "terminal-seam-" + strings.TrimPrefix(session.Name, tmux.SessionPrefix)
	inst := &Instance{ID: "terminal-tmux-byte-test", Tool: "shell", Status: StatusIdle, tmuxSession: session}
	for _, socket := range []string{session.SocketName, session.SocketName + "-other"} {
		command := "exec /bin/sleep 30"
		if socket == session.SocketName {
			command = "exec /bin/sh " + shellescape.Quote(scriptPath)
		}
		if err := tmux.ExecContext(ctx, socket, "new-session", "-d", "-s", session.Name, "-x", "120", "-y", "40", command).Run(); err != nil {
			t.Fatal(err)
		}
		// This exact fixture target, on this exact private socket, is all we own.
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cleanupCancel()
			_ = tmux.ExecContext(cleanupCtx, socket, "kill-session", "-t", "="+session.Name).Run()
		})
	}
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("terminal reader did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	observation, err := inst.ObserveTerminal(ctx)
	if err != nil || !observation.Alive || observation.PID == 0 {
		t.Fatalf("process facts = %#v, %v", observation, err)
	}
	if err := inst.SendTerminal(ctx, payload); err != nil {
		t.Fatal(err)
	}
	for {
		got, err := os.ReadFile(outputPath)
		if err == nil && len(got) == len(payload) {
			if !bytes.Equal(got, payload) {
				t.Fatalf("terminal bytes = %x, want %x", got, payload)
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("terminal input was not delivered: %x, %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := inst.ResizeTerminal(ctx, 123, 35); err != nil {
		t.Fatal(err)
	}
	geometry, err := tmux.ExecContext(ctx, session.SocketName, "display-message", "-p", "-t", "="+session.Name+":^", "#{pane_width}|#{pane_height}").Output()
	if err != nil || strings.TrimSpace(string(geometry)) != "123|35" {
		t.Fatalf("terminal geometry = %q, %v", geometry, err)
	}
	for {
		capture, err := inst.CaptureTerminal(ctx, false)
		if err == nil && strings.Contains(capture, "terminal byte capture done") {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("terminal capture = %q, %v", capture, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := inst.KillAndWait(); err != nil {
		t.Fatal(err)
	}
	if got := inst.GetStatusThreadSafe(); got != StatusStopped || inst.Exists() {
		t.Fatalf("legacy stop changed semantics: status=%q exists=%v", got, inst.Exists())
	}
	observation, err = inst.ObserveTerminal(ctx)
	if err != nil || observation.Alive {
		t.Fatalf("stopped process facts = %#v, %v", observation, err)
	}
	if err := tmux.ExecContext(ctx, session.SocketName+"-other", "has-session", "-t", "="+session.Name).Run(); err != nil {
		t.Fatalf("same-named sibling on another socket was touched: %v", err)
	}
}
