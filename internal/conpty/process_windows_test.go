//go:build windows

// matrix/hardened: opt-in integration tests exercise actual Windows APIs using
// private child processes, never existing agents or user configuration.
package conpty

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const childModeVariable = "AGENTDECK_CONPTY_TEST_CHILD"

func integrationEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENTDECK_CONPTY_TEST") != "1" {
		t.Skip("set AGENTDECK_CONPTY_TEST=1 to create isolated real ConPTY processes")
	}
}

type captureBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
	done chan error
}

func (capture *captureBuffer) Write(data []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.data.Write(data)
}

func (capture *captureBuffer) String() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.data.String()
}

func captureProcess(p *Process) *captureBuffer {
	capture := &captureBuffer{done: make(chan error, 1)}
	go func() {
		_, err := io.Copy(capture, p.Output())
		capture.done <- err
	}()
	return capture
}

func awaitContains(t *testing.T, capture *captureBuffer, wanted string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		value := capture.String()
		if strings.Contains(value, wanted) {
			return value
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("terminal did not emit %q; output %q", wanted, capture.String())
	return ""
}

func closeProcess(t *testing.T, p *Process) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close failed to finish while output was drained")
	}
}

func waitProcess(t *testing.T, p *Process, expected uint32) {
	t.Helper()
	type result struct {
		code uint32
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := p.Wait()
		done <- result{code, err}
	}()
	select {
	case got := <-done:
		if got.code != expected || got.err != nil {
			t.Fatalf("Wait = %d, %v; want %d", got.code, got.err, expected)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}
}

func helperOptions(t *testing.T, mode string) Options {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{Executable: executable, Args: []string{"-test.run=^TestConPTYChild$", "--"},
		Env: append(os.Environ(), childModeVariable+"="+mode), Cols: 300, Rows: 40}
}

func TestConPTYArgumentEnvironmentDirectory(t *testing.T) {
	integrationEnabled(t)
	options := helperOptions(t, "arguments")
	options.Dir = filepath.Join(t.TempDir(), "中文 space")
	if err := os.Mkdir(options.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"", "two words", `say "hello"`, `C:\with space\`, `before\"after`, "中文😀"}
	options.Args = append(options.Args, args...)
	options.Env = append(options.Env, "AGENTDECK_CONPTY_PROBE=first", "agentdeck_conpty_probe=中文 值")
	p, err := Start(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeProcess(t, p) })
	capture := captureProcess(p)
	expected, err := json.Marshal(struct {
		Args []string `json:"args"`
		Env  string   `json:"env"`
		Dir  string   `json:"dir"`
	}{args, "中文 值", options.Dir})
	if err != nil {
		t.Fatal(err)
	}
	awaitContains(t, capture, "PROBE:"+string(expected))
	waitProcess(t, p, 0)
	closeProcess(t, p)
}

func TestConPTYInputResizeWaitAndFinalOutput(t *testing.T) {
	integrationEnabled(t)
	p, err := Start(helperOptions(t, "interactive"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeProcess(t, p) })
	capture := captureProcess(p)
	awaitContains(t, capture, "READY")
	if err := p.Resize(123, 35); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p.Input(), "中文输入\r"); err != nil {
		t.Fatal(err)
	}
	awaitContains(t, capture, "RECEIVED:中文输入")
	awaitContains(t, capture, "SIZE:123,35")
	waitProcess(t, p, 7)
	closeProcess(t, p)
	select {
	case err := <-capture.done:
		if err != nil || !strings.Contains(capture.String(), "FINAL_FRAME") {
			t.Fatalf("final output = %q, %v", capture.String(), err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("output did not reach EOF after Close")
	}
	if err := p.Resize(80, 24); err != ErrClosed {
		t.Fatalf("Resize after Close = %v", err)
	}
}

func TestConPTYInteractiveShell(t *testing.T) {
	integrationEnabled(t)
	p, err := Start(Options{Executable: "cmd.exe", Args: []string{"/D", "/Q"}, Cols: 120, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeProcess(t, p) })
	capture := captureProcess(p)
	if _, err := io.WriteString(p.Input(), "chcp 65001\recho SHELL_中文_OK\rexit 9\r"); err != nil {
		t.Fatal(err)
	}
	awaitContains(t, capture, "SHELL_中文_OK")
	waitProcess(t, p, 9)
	closeProcess(t, p)
}

func startOutsideJob(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	options := helperOptions(t, mode)
	command := exec.Command(options.Executable, options.Args...)
	command.Env = options.Env
	return command
}

func openProcessForWait(t *testing.T, pid uint32) windows.Handle {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return handle
}

func assertNativeExit(t *testing.T, handle windows.Handle, timeout uint32, expected uint32) {
	t.Helper()
	result, err := windows.WaitForSingleObject(handle, timeout)
	if err != nil || result != expected {
		t.Fatalf("native wait = %d, %v; want %d", result, err, expected)
	}
}

func TestConPTYCloseOwnsDescendants(t *testing.T) {
	integrationEnabled(t)
	sentinel := startOutsideJob(t, "sleep")
	if err := sentinel.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sentinel.Process.Kill(); _ = sentinel.Wait() })
	sentinelHandle := openProcessForWait(t, uint32(sentinel.Process.Pid))
	p, err := Start(helperOptions(t, "descendant"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeProcess(t, p) })
	capture := captureProcess(p)
	output := awaitContains(t, capture, "CHILD_PID:")
	var descendant uint32
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = fmt.Sscanf(output[strings.Index(output, "CHILD_PID:"):], "CHILD_PID:%d", &descendant)
		if descendant != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
		output = capture.String()
	}
	if descendant == 0 {
		t.Fatalf("missing descendant PID in %q", output)
	}
	descendantHandle := openProcessForWait(t, descendant)
	rootHandle := openProcessForWait(t, p.PID())
	closeProcess(t, p)
	assertNativeExit(t, rootHandle, 5000, windows.WAIT_OBJECT_0)
	assertNativeExit(t, descendantHandle, 5000, windows.WAIT_OBJECT_0)
	assertNativeExit(t, sentinelHandle, 0, uint32(windows.WAIT_TIMEOUT))
}

func TestConPTYHostCrashOwnsTree(t *testing.T) {
	integrationEnabled(t)
	host := startOutsideJob(t, "host")
	stdout, err := host.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Process.Kill(); _ = host.Wait() })
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		lines <- line
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(10 * time.Second):
		t.Fatal("helper host did not publish its child")
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "OWNED_PID:")), 10, 32)
	if err != nil {
		t.Fatalf("helper host response %q: %v", line, err)
	}
	childHandle := openProcessForWait(t, uint32(pid))
	if err := host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	assertNativeExit(t, childHandle, 5000, windows.WAIT_OBJECT_0)
}

func TestConPTYRedirectedHostStandardHandles(t *testing.T) {
	integrationEnabled(t)
	host := startOutsideJob(t, "redirected-host")
	var output bytes.Buffer
	host.Stdout = &output
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		_ = host.Process.Kill()
		t.Fatal("redirected helper host did not finish")
	}
	line := strings.TrimSpace(output.String())
	if !strings.HasPrefix(line, "HOST_CAPTURE:") || strings.Contains(line, "\n") {
		t.Fatalf("child output escaped its ConPTY into host stdout: %q", line)
	}
	captured, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "HOST_CAPTURE:"))
	if err != nil || !bytes.Contains(captured, []byte("PROBE:")) || !bytes.Contains(captured, []byte("redirected stdio 中文")) {
		t.Fatalf("redirected host's ConPTY output = %q, %v", captured, err)
	}
}

func nativeHandleCount(t *testing.T) uint32 {
	t.Helper()
	process, err := windows.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	var count uint32
	result, _, nativeErr := kernel32.NewProc("GetProcessHandleCount").Call(uintptr(process), uintptr(unsafe.Pointer(&count)))
	if result == 0 {
		t.Fatalf("GetProcessHandleCount: %v", nativeErr)
	}
	return count
}

// Go's Windows runtime allocates Thread/Event/IRTimer/WaitCompletionPacket
// handles lazily per OS thread. Native handle diagnostics showed six handles
// added for each new runtime thread even when ConPTY released all resources.
// Prewarm the bounded syscall thread pool before requiring exact handle counts;
// assertions below still allow no growth or extra direct child processes.
func prepareHandleCountRuntime(t *testing.T) {
	t.Helper()
	previous := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	var ready, finished sync.WaitGroup
	const workers = 12
	ready.Add(workers)
	finished.Add(workers)
	release := make(chan struct{})
	for range workers {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			defer finished.Done()
			ready.Done()
			<-release
		}()
	}
	ready.Wait()
	close(release)
	finished.Wait()
}

// Only count direct children of this private test process. No unrelated host
// process is classified by executable name or targeted for cleanup.
func nativeChildren(t *testing.T) map[uint32]bool {
	t.Helper()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	err = windows.Process32First(snapshot, &entry)
	children := make(map[uint32]bool)
	for err == nil {
		if entry.ParentProcessID == uint32(os.Getpid()) {
			children[entry.ProcessID] = true
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		t.Fatal(err)
	}
	return children
}

func nativeThreadCount(t *testing.T) uint32 {
	t.Helper()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		if entry.ProcessID == uint32(os.Getpid()) {
			return entry.Threads
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	t.Fatalf("own process missing from native snapshot: %v", err)
	return 0
}

func assertStartupResourcesStable(t *testing.T, handles uint32, children map[uint32]bool, threads uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var gotHandles uint32
	var gotChildren map[uint32]bool
	for time.Now().Before(deadline) {
		gotChildren = nativeChildren(t)
		gotHandles = nativeHandleCount(t)
		if gotHandles == handles && reflect.DeepEqual(gotChildren, children) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("startup resources changed: handles %d -> %d, threads %d -> %d, children %v -> %v", handles, gotHandles, threads, nativeThreadCount(t), children, gotChildren)
}

func assertStartupFailure(t *testing.T, options Options, stage *launchStage) {
	t.Helper()
	injected := errors.New("private test startup failure")
	var childHandle windows.Handle
	var observedChild uint32
	var checkpoint launchCheckpoint
	if stage != nil {
		checkpoint = func(current launchStage, pid uint32) error {
			if current != *stage {
				return nil
			}
			observedChild = pid
			if pid != 0 {
				var err error
				childHandle, err = windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
				if err != nil {
					return err
				}
			}
			return injected
		}
	}
	type result struct {
		process *Process
		err     error
	}
	done := make(chan result, 1)
	go func() {
		p, err := start(options, checkpoint)
		done <- result{p, err}
	}()
	select {
	case got := <-done:
		if childHandle != 0 {
			defer windows.CloseHandle(childHandle)
			assertNativeExit(t, childHandle, 5000, windows.WAIT_OBJECT_0)
		}
		if got.process != nil {
			closeProcess(t, got.process)
			t.Fatal("failed startup returned a live process")
		}
		if got.err == nil || (stage != nil && !errors.Is(got.err, injected)) {
			t.Fatalf("startup failure = %v", got.err)
		}
		if stage != nil && *stage != launchConsoleCreated && observedChild == 0 {
			t.Fatal("startup injection did not reach a real suspended child")
		}
		if stage == nil && !errors.Is(got.err, windows.ERROR_BAD_EXE_FORMAT) && !errors.Is(got.err, windows.ERROR_EXE_MACHINE_TYPE_MISMATCH) {
			t.Fatalf("invalid native image returned %v, want a native invalid-image error", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("native startup failure did not clean up promptly")
	}
}

func TestConPTYStartupFailureCleanup(t *testing.T) {
	integrationEnabled(t)
	prepareHandleCountRuntime(t)
	for name, stage := range map[string]launchStage{
		"after HPCON creation":           launchConsoleCreated,
		"before job assignment":          launchChildCreated,
		"after job assignment":           launchJobAssigned,
		"before suspended thread resume": launchResuming,
	} {
		t.Run(name, func(t *testing.T) {
			options := helperOptions(t, "sleep")
			// Warm Go's native syscall threads and runtime handles before
			// requiring exact steady-state handle counts for this path.
			for range 3 {
				assertStartupFailure(t, options, &stage)
			}
			children := nativeChildren(t)
			handles := nativeHandleCount(t)
			threads := nativeThreadCount(t)
			for range 5 {
				assertStartupFailure(t, options, &stage)
				assertStartupResourcesStable(t, handles, children, threads)
			}
			t.Logf("native startup resources stable: %d handles, %d direct children", handles, len(children))
		})
	}
}

func TestConPTYInvalidImageCleanup(t *testing.T) {
	integrationEnabled(t)
	prepareHandleCountRuntime(t)
	path := filepath.Join(t.TempDir(), "invalid.exe")
	// A recognizable x64 PE header with a deliberately truncated optional
	// header reaches the native image loader without Windows treating random
	// text as a legacy DOS application and initializing its compatibility path.
	image := make([]byte, 256)
	copy(image, "MZ")
	binary.LittleEndian.PutUint32(image[0x3c:], 0x80)
	copy(image[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(image[0x84:], 0x8664)
	binary.LittleEndian.PutUint16(image[0x94:], 0xf0)
	if err := os.WriteFile(path, image, 0600); err != nil {
		t.Fatal(err)
	}
	options := Options{Executable: path}
	// LookPath accepts this existing .exe, so failure comes from the actual
	// CreateProcess call after a real HPCON and Job Object were created.
	for range 3 {
		assertStartupFailure(t, options, nil)
	}
	children := nativeChildren(t)
	handles := nativeHandleCount(t)
	threads := nativeThreadCount(t)
	for range 5 {
		assertStartupFailure(t, options, nil)
		assertStartupResourcesStable(t, handles, children, threads)
	}
	t.Logf("invalid-image cleanup stable: %d handles, %d direct children", handles, len(children))
}

// TestConPTYChild is a re-exec helper. It prints only synthetic test values,
// while its launch and cleanup use the same real API as production sessions.
func TestConPTYChild(t *testing.T) {
	mode := os.Getenv(childModeVariable)
	if mode == "" {
		t.Skip("isolated integration helper")
	}
	switch mode {
	case "arguments":
		args := []string{}
		for index, arg := range os.Args {
			if arg == "--" {
				args = os.Args[index+1:]
				break
			}
		}
		dir, _ := os.Getwd()
		data, _ := json.Marshal(struct {
			Args []string `json:"args"`
			Env  string   `json:"env"`
			Dir  string   `json:"dir"`
		}{args, os.Getenv("AGENTDECK_CONPTY_PROBE"), dir})
		fmt.Println("PROBE:" + string(data))
		os.Exit(0)
	case "interactive":
		fmt.Println("READY")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			os.Exit(2)
		}
		fmt.Println("RECEIVED:" + strings.TrimRight(line, "\r\n"))
		handle, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
		var info windows.ConsoleScreenBufferInfo
		if err := windows.GetConsoleScreenBufferInfo(handle, &info); err != nil {
			os.Exit(3)
		}
		fmt.Printf("SIZE:%d,%d\n", info.Size.X, info.Size.Y)
		fmt.Println("FINAL_FRAME")
		os.Exit(7)
	case "descendant":
		executable, _ := os.Executable()
		child := exec.Command(executable, "-test.run=^TestConPTYChild$")
		child.Env = append(os.Environ(), childModeVariable+"=sleep")
		if child.Start() != nil {
			os.Exit(4)
		}
		fmt.Printf("CHILD_PID:%d\n", child.Process.Pid)
		time.Sleep(10 * time.Minute)
	case "host":
		executable, _ := os.Executable()
		p, err := Start(Options{Executable: executable, Args: []string{"-test.run=^TestConPTYChild$"},
			Env: append(os.Environ(), childModeVariable+"=sleep")})
		if err != nil {
			fmt.Println("START_ERROR:" + err.Error())
			os.Exit(5)
		}
		fmt.Printf("OWNED_PID:%d\n", p.PID())
		time.Sleep(10 * time.Minute)
	case "redirected-host":
		executable, _ := os.Executable()
		p, err := Start(Options{Executable: executable, Args: []string{"-test.run=^TestConPTYChild$", "--"},
			Env: append(os.Environ(), childModeVariable+"=arguments", "AGENTDECK_CONPTY_PROBE=redirected stdio 中文"), Cols: 300})
		if err != nil {
			os.Exit(8)
		}
		capture := captureProcess(p)
		_, waitErr := p.Wait()
		closeErr := p.Close()
		if waitErr != nil || closeErr != nil || <-capture.done != nil {
			os.Exit(9)
		}
		fmt.Println("HOST_CAPTURE:" + base64.StdEncoding.EncodeToString([]byte(capture.String())))
	case "sleep":
		time.Sleep(10 * time.Minute)
	default:
		os.Exit(6)
	}
	os.Exit(0)
}
