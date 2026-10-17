//go:build windows

package main

// matrix/hardened: opt-in native attach tests own private terminals and a GUID
// pipe; this is headless real-console evidence, never visible desktop acceptance.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

const nativeAttachTimeout = 15 * time.Second

type nativeViewer struct {
	process *conpty.Process
	mu      sync.Mutex
	output  []byte
	done    chan error
}

func (viewer *nativeViewer) Write(data []byte) (int, error) {
	viewer.mu.Lock()
	defer viewer.mu.Unlock()
	viewer.output = append(viewer.output, data...)
	if len(viewer.output) > 2<<20 {
		viewer.output = append([]byte(nil), viewer.output[len(viewer.output)-(2<<20):]...)
	}
	return len(data), nil
}

func (viewer *nativeViewer) text() string {
	viewer.mu.Lock()
	defer viewer.mu.Unlock()
	return ansi.Strip(string(viewer.output))
}

func nativeClose(t *testing.T, label string, closer func() error) {
	t.Helper()
	finished := make(chan error, 1)
	go func() { finished <- closer() }()
	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("%s cleanup: %v", label, err)
		}
	case <-time.After(nativeAttachTimeout):
		t.Errorf("%s cleanup exceeded %s", label, nativeAttachTimeout)
	}
}

func nativeAwait(t *testing.T, label string, probe func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(nativeAttachTimeout)
	var detail string
	for time.Now().Before(deadline) {
		var ok bool
		ok, detail = probe()
		if ok {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s timed out: %s", label, detail)
}

func nativeStartViewer(t *testing.T, cli, pipe string, cols, rows int) *nativeViewer {
	t.Helper()
	process, err := conpty.Start(conpty.Options{
		Executable: cli, Args: []string{"attach", "--pipe", pipe}, Cols: cols, Rows: rows,
		Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("start real CLI attach: %v", err)
	}
	viewer := &nativeViewer{process: process, done: make(chan error, 1)}
	go func() {
		_, err := io.Copy(viewer, process.Output())
		viewer.done <- err
	}()
	t.Cleanup(func() {
		nativeClose(t, "attach client", process.Close)
		select {
		case err := <-viewer.done:
			if err != nil {
				t.Errorf("attach output drain: %v", err)
			}
		case <-time.After(nativeAttachTimeout):
			t.Errorf("attach output did not reach EOF after owned process cleanup")
		}
	})
	return viewer
}

func nativeWrite(t *testing.T, viewer *nativeViewer, data string) {
	t.Helper()
	finished := make(chan error, 1)
	go func() {
		n, err := io.WriteString(viewer.process.Input(), data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("attach native keyboard input: %v", err)
		}
	case <-time.After(nativeAttachTimeout):
		t.Fatal("attach native keyboard input blocked")
	}
}

func nativeWaitExit(t *testing.T, process *conpty.Process, expected uint32) {
	t.Helper()
	type result struct {
		code uint32
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		code, err := process.Wait()
		finished <- result{code, err}
	}()
	select {
	case got := <-finished:
		if got.err != nil || got.code != expected {
			t.Fatalf("native client Wait = %d, %v; expected exit %d", got.code, got.err, expected)
		}
	case <-time.After(nativeAttachTimeout):
		t.Fatal("Ctrl+Q did not exit the actual attach process")
	}
}

func nativeProcessHandle(t *testing.T, pid uint32) windows.Handle {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatalf("open owned process %d: %v", pid, err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return handle
}

func nativeAssertProcess(t *testing.T, handle windows.Handle, running bool) {
	t.Helper()
	want := uint32(windows.WAIT_OBJECT_0)
	if running {
		want = uint32(windows.WAIT_TIMEOUT)
	}
	got, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || got != want {
		t.Fatalf("owned process running=%v native wait=%d, %v; wanted %d", running, got, err, want)
	}
}

func nativeEncodedCommand(shell, script string) string {
	units := utf16.Encode([]rune(script))
	data := make([]byte, 2*len(units))
	for index, unit := range units {
		binary.LittleEndian.PutUint16(data[2*index:], unit)
	}
	return fmt.Sprintf(`"%s" -NoLogo -NoProfile -NonInteractive -EncodedCommand %s`, shell, base64.StdEncoding.EncodeToString(data)) + "\r"
}

func nativeCall(t *testing.T, pipe string, request terminalhost.Request) terminalhost.Response {
	t.Helper()
	response, err := callHost(context.Background(), pipe, request)
	if err != nil {
		t.Fatalf("private host %s: %v", request.Op, err)
	}
	return response
}

func TestWindowsNativeAttachDetachReattachResize(t *testing.T) {
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
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		t.Fatal("SystemRoot is required for isolated Windows shell fixtures")
	}
	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	pipe := winipc.PipePrefix + "attach-test-" + uuid.NewString()
	listener, err := winipc.Listen(pipe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	root, err := conpty.Start(conpty.Options{
		Executable: filepath.Join(systemRoot, "System32", "cmd.exe"),
		Args:       []string{"/d", "/q"}, Dir: t.TempDir(), Cols: 100, Rows: 30,
	})
	if err != nil {
		t.Fatalf("start owned native root Shell: %v", err)
	}
	t.Cleanup(func() { nativeClose(t, "root Shell", root.Close) })
	rootHandle := nativeProcessHandle(t, root.PID())
	server, err := terminalhost.New(root, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nativeClose(t, "in-process terminal host", server.Close) })
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	initial := nativeCall(t, pipe, terminalhost.Request{Op: "status"})
	if !initial.Running || initial.PID != root.PID() || initial.HostPID != os.Getpid() {
		t.Fatalf("unexpected owned host identity: %+v", initial)
	}
	viewer := nativeStartViewer(t, cli, pipe, 100, 30)
	nativeAwait(t, "attach initial screen", func() (bool, string) {
		text := viewer.text()
		return strings.Contains(text, ">"), text
	})
	marker := "ATTACH_中文_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	command := nativeEncodedCommand(powershell, "[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); [Console]::WriteLine('"+marker+"')")
	if strings.Contains(command, marker) {
		t.Fatal("fixture accidentally puts the clear marker in keyboard command echo")
	}
	nativeWrite(t, viewer, command)
	nativeAwait(t, "encoded command execution through real CLI attach", func() (bool, string) {
		text := viewer.text()
		return strings.Contains(text, marker), text
	})
	frame := nativeCall(t, pipe, terminalhost.Request{Op: "capture"})
	if !strings.Contains(frame.Screen, marker) || frame.PID != initial.PID {
		t.Fatalf("root screen did not independently confirm actual execution: %+v", frame)
	}
	// matrix/hardened: the helper sends real DA/DSR queries while attach is
	// connected. Duplicate viewer replies would be counted by its raw reader.
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	queryScript := "$env:AGENTDECK_NATIVE_QUERY_CHILD='1'; & '" + strings.ReplaceAll(testExecutable, "'", "''") + "' '-test.run=^TestWindowsNativeQueryChild$'"
	nativeWrite(t, viewer, nativeEncodedCommand(powershell, queryScript))
	queryMarker := "NATIVE_QUERY_OK da=1 dsr=1 cpr=1"
	nativeAwait(t, "real terminal query replies are delivered once while attached", func() (bool, string) {
		text := viewer.text()
		return strings.Contains(text, queryMarker), text
	})
	nativeWrite(t, viewer, "\x11")
	nativeWaitExit(t, viewer.process, 0)
	nativeAssertProcess(t, rootHandle, true)
	afterDetach := nativeCall(t, pipe, terminalhost.Request{Op: "status"})
	if !afterDetach.Running || afterDetach.PID != initial.PID || afterDetach.HostPID != initial.HostPID {
		t.Fatalf("detach changed host/root identity: before=%+v after=%+v", initial, afterDetach)
	}
	reattached := nativeStartViewer(t, cli, pipe, 100, 30)
	nativeAwait(t, "fresh client restores prior screen without resending input", func() (bool, string) {
		text := reattached.text()
		return strings.Contains(text, queryMarker), text
	})
	if err := reattached.process.Resize(123, 35); err != nil {
		t.Fatalf("resize actual client ConPTY: %v", err)
	}
	nativeAwait(t, "client console resize reaches same root", func() (bool, string) {
		response, err := callHost(context.Background(), pipe, terminalhost.Request{Op: "status"})
		return err == nil && response.Running && response.PID == initial.PID && response.Cols == 123 && response.Rows == 35, fmt.Sprintf("%+v, %v", response, err)
	})
	dimensionMarker := "NATIVE_DIMENSIONS_123x35"
	script := "[Console]::WriteLine(('NATIVE_DIMENSIONS_{0}x{1}' -f [Console]::WindowWidth, [Console]::WindowHeight))"
	command = nativeEncodedCommand(powershell, script)
	if strings.Contains(command, dimensionMarker) {
		t.Fatal("dimension fixture marker appears in typed source")
	}
	nativeWrite(t, reattached, command)
	nativeAwait(t, "native root console observes resized dimensions", func() (bool, string) {
		text := reattached.text()
		return strings.Contains(text, dimensionMarker), text
	})
	frame = nativeCall(t, pipe, terminalhost.Request{Op: "capture"})
	if !strings.Contains(frame.Screen, dimensionMarker) || frame.Cols != 123 || frame.Rows != 35 || frame.PID != initial.PID {
		t.Fatalf("native resize screen and host metadata disagree: %+v", frame)
	}
	nativeWrite(t, reattached, "\x11")
	nativeWaitExit(t, reattached.process, 0)
	nativeAssertProcess(t, rootHandle, true)
	stopped := nativeCall(t, pipe, terminalhost.Request{Op: "stop"})
	if stopped.Running || stopped.PID != initial.PID {
		t.Fatalf("stop ACK did not confirm same owned root exit: %+v", stopped)
	}
	nativeAssertProcess(t, rootHandle, false)
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("private host Serve after stop: %v", err)
		}
	case <-time.After(nativeAttachTimeout):
		t.Fatal("private host listener survived stop")
	}
	t.Logf("native headless attach passed: root_pid=%d host_pid=%d first_client=%d second_client=%d resized=123x35; no visible desktop assertion", initial.PID, initial.HostPID, viewer.process.PID(), reattached.process.PID())
}

// TestWindowsNativeQueryChild is only re-executed by the owned Shell fixture.
// Its environment flag enables synthetic terminal queries, never app commands.
func TestWindowsNativeQueryChild(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_QUERY_CHILD") != "1" {
		t.Skip("private re-exec query fixture")
	}
	inputFD, outputFD := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	raw, err := term.MakeRaw(inputFD)
	if err != nil {
		t.Fatalf("query child raw input: %v", err)
	}
	defer term.Restore(inputFD, raw)
	restore, err := enableConsoleOutput(outputFD)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	input := make(chan []byte, 16)
	readErrors := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4096)
		for {
			count, err := os.Stdin.Read(buffer)
			if count > 0 {
				input <- append([]byte(nil), buffer[:count]...)
			}
			if err != nil {
				readErrors <- err
				return
			}
		}
	}()
	if _, err := io.WriteString(os.Stdout, "\x1b[c\x1b[5n\x1b[6n"); err != nil {
		t.Fatal(err)
	}
	da := regexp.MustCompile("\x1b\\[[?>]?[0-9;]*c")
	dsr := regexp.MustCompile("\x1b\\[\\??0n")
	cpr := regexp.MustCompile("\x1b\\[\\??[0-9]+;[0-9]+R")
	var received []byte
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var quiet <-chan time.Time
	var quietTimer *time.Timer
	defer func() {
		if quietTimer != nil {
			quietTimer.Stop()
		}
	}()
	for {
		select {
		case data := <-input:
			received = append(received, data...)
			if len(received) > 16<<10 {
				t.Fatalf("query response exceeded private bound: %q", received)
			}
			if da.Match(received) && dsr.Match(received) && cpr.Match(received) {
				if quietTimer == nil {
					quietTimer = time.NewTimer(300 * time.Millisecond)
				} else {
					if !quietTimer.Stop() {
						select {
						case <-quietTimer.C:
						default:
						}
					}
					quietTimer.Reset(300 * time.Millisecond)
				}
				quiet = quietTimer.C
			}
		case err := <-readErrors:
			t.Fatalf("query native input failed: %v; replies=%q", err, received)
		case <-deadline.C:
			t.Fatalf("query native replies timed out: %q", received)
		case <-quiet:
			counts := []int{len(da.FindAll(received, -1)), len(dsr.FindAll(received, -1)), len(cpr.FindAll(received, -1))}
			if counts[0] != 1 || counts[1] != 1 || counts[2] != 1 {
				t.Fatalf("duplicate or missing real terminal query replies: da=%d dsr=%d cpr=%d bytes=%q", counts[0], counts[1], counts[2], received)
			}
			fmt.Fprint(os.Stdout, "\r\nNATIVE_QUERY_OK da=1 dsr=1 cpr=1\r\n")
			return
		}
	}
}
