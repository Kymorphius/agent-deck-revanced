//go:build windows

package main

// matrix/hardened: opt-in paste fixtures own a private native child and GUID
// pipe, with no AI/authentication or desktop clipboard access. This proves
// headless mode propagation and paste bytes, not visible Windows Terminal UX.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/google/uuid"
	"golang.org/x/term"
)

func nativePasteViewerMode(viewer *nativeViewer) (enabled, observed bool) {
	viewer.mu.Lock()
	defer viewer.mu.Unlock()
	enable := bytes.LastIndex(viewer.output, []byte("\x1b[?2004h"))
	disable := bytes.LastIndex(viewer.output, []byte("\x1b[?2004l"))
	return enable > disable, enable >= 0 || disable >= 0
}

func TestWindowsNativeAttachBracketedPaste(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 and AGENTDECK_NATIVE_CLI to a freshly built native CLI")
	}
	cli := os.Getenv("AGENTDECK_NATIVE_CLI")
	if !filepath.IsAbs(cli) {
		t.Fatal("AGENTDECK_NATIVE_CLI must be the absolute path to the freshly built .exe")
	}
	if info, err := os.Stat(cli); err != nil || info.IsDir() {
		t.Fatalf("native CLI does not exist: %v", err)
	}
	fixture, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pipe := winipc.PipePrefix + "paste-test-" + uuid.NewString()
	listener, err := winipc.Listen(pipe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	payload := "粘贴第一行_" + uuid.NewString() + "\r\n第二行 Unicode"
	root, err := conpty.Start(conpty.Options{
		Executable: fixture, Args: []string{"-test.run=^TestWindowsNativePasteChild$", "-test.timeout=90s"},
		Env: append(os.Environ(), "AGENTDECK_NATIVE_PASTE_CHILD=1", "AGENTDECK_NATIVE_PASTE_PAYLOAD="+payload),
		Dir: t.TempDir(), Cols: 100, Rows: 30,
	})
	if err != nil {
		t.Fatalf("start owned paste fixture: %v", err)
	}
	t.Cleanup(func() { nativeClose(t, "paste fixture", root.Close) })
	rootHandle := nativeProcessHandle(t, root.PID())
	server, err := terminalhost.New(root, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		nativeClose(t, "paste fixture host", server.Close)
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("paste fixture Serve cleanup: %v", err)
			}
		case <-time.After(nativeAttachTimeout):
			t.Error("paste fixture listener did not close")
		}
	})
	nativeAwait(t, "child parsed paste mode", func() (bool, string) {
		frame := nativeCall(t, pipe, terminalhost.Request{Op: "capture"})
		return frame.BracketedPaste && strings.Contains(frame.Screen, "PASTE_FIXTURE_READY"), "waiting for owned child mode and readiness marker"
	})
	viewer := nativeStartViewer(t, cli, pipe, 100, 30)
	nativeAwait(t, "initial attach paste mode", func() (bool, string) {
		enabled, observed := nativePasteViewerMode(viewer)
		return enabled && observed, "waiting for parsed DEC 2004 enable at actual attach console"
	})
	nativeWrite(t, viewer, "\x1b[200~"+payload+"\x1b[201~")
	nativeAwait(t, "exact Chinese multiline paste bytes", func() (bool, string) {
		return strings.Contains(viewer.text(), "PASTE_FIXTURE_OK"), "waiting for owned child to validate paste bytes"
	})
	nativeWrite(t, viewer, "\x11")
	nativeWaitExit(t, viewer.process, 0)
	nativeAssertProcess(t, rootHandle, true)
	reattached := nativeStartViewer(t, cli, pipe, 100, 30)
	nativeAwait(t, "reattach restores active paste mode", func() (bool, string) {
		enabled, observed := nativePasteViewerMode(reattached)
		return enabled && observed && strings.Contains(reattached.text(), "PASTE_FIXTURE_OK"), "waiting for same child's paste mode on reconnect"
	})
	before := nativeCall(t, pipe, terminalhost.Request{Op: "capture"})
	if !before.BracketedPaste || before.PID != root.PID() {
		t.Fatal("reattach lost the selected child's identity or paste mode")
	}
	// Child acknowledges this byte only by changing the mode, not its screen.
	nativeWrite(t, reattached, "\x04")
	nativeAwait(t, "mode-only disable reaches attached console", func() (bool, string) {
		frame := nativeCall(t, pipe, terminalhost.Request{Op: "capture"})
		enabled, observed := nativePasteViewerMode(reattached)
		return !frame.BracketedPaste && !enabled && observed, "waiting for mode-only DEC 2004 disable"
	})
	after := nativeCall(t, pipe, terminalhost.Request{Op: "capture"})
	if after.Render != before.Render || after.CursorX != before.CursorX || after.CursorY != before.CursorY {
		t.Fatal("mode-only child fixture unexpectedly changed its screen or cursor")
	}
	nativeWrite(t, reattached, "\x11")
	nativeWaitExit(t, reattached.process, 0)
	nativeAssertProcess(t, rootHandle, true)
	stopped := nativeCall(t, pipe, terminalhost.Request{Op: "stop"})
	if stopped.Running || stopped.PID != root.PID() {
		t.Fatal("stop did not confirm the same owned paste child exited")
	}
	nativeAssertProcess(t, rootHandle, false)
	t.Logf("native headless paste passed: child_pid=%d enable/reconnect/disable=true exact_multiline=true; no desktop clipboard assertion", root.PID())
}

func TestWindowsNativePasteChild(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_PASTE_CHILD") != "1" {
		t.Skip("private re-exec paste fixture")
	}
	payload := os.Getenv("AGENTDECK_NATIVE_PASTE_PAYLOAD")
	if payload == "" {
		t.Fatal("private paste payload is required")
	}
	raw, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatalf("paste child raw input: %v", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), raw)
	restore, err := enableConsoleOutput(int(os.Stdout.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	defer fmt.Fprint(os.Stdout, "\x1b[?2004l")
	if _, err := io.WriteString(os.Stdout, "PASTE_FIXTURE_READY\x1b[?2004h"); err != nil {
		t.Fatal(err)
	}
	want := []byte("\x1b[200~" + payload + "\x1b[201~")
	received := make([]byte, len(want))
	if _, err := io.ReadFull(os.Stdin, received); err != nil {
		t.Fatalf("read native paste: %v", err)
	}
	if !bytes.Equal(received, want) {
		t.Fatal("native attach changed the bracketed multiline paste bytes")
	}
	if _, err := io.WriteString(os.Stdout, "\r\nPASTE_FIXTURE_OK"); err != nil {
		t.Fatal(err)
	}
	var instruction [1]byte
	if _, err := io.ReadFull(os.Stdin, instruction[:]); err != nil || instruction[0] != 0x04 {
		t.Fatalf("read private mode-disable instruction: %v", err)
	}
	if _, err := io.WriteString(os.Stdout, "\x1b[?2004l"); err != nil {
		t.Fatal(err)
	}
	// Remain alive until the parent verifies exact process ownership and stops it.
	_, _ = io.Copy(io.Discard, os.Stdin)
}
