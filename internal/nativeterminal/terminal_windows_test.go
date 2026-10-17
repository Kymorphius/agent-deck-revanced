//go:build windows

// matrix/hardened: opt-in acceptance connects real ConPTY sessions through the
// private Windows pipe while the unchanged original StateDB reader sees them.
// Only GUID-private test files and exact retained process handles are owned.
package nativeterminal

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/nativefleet"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

func TestWindowsSharedStateDBConPTYAdapter(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 for GUID-private real Windows adapter acceptance")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(parent, "shared-state-"+uuid.NewString())
	project := filepath.Join(parent, "原生工程")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("project sentinel remains unchanged\n")
	if err := os.WriteFile(filepath.Join(project, "keep.txt"), sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := nativestatestore.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := nativefleet.New(store, func(launch terminalhost.LaunchSpec) (terminalhost.Process, error) {
		// Only the in-memory process launch inherits this test environment;
		// the stored LaunchSpec keeps explicit user overrides alone.
		process, err := conpty.Start(conpty.Options{Executable: launch.Executable, Args: launch.Args, Env: append(os.Environ(), launch.Env...), Dir: launch.Dir, Cols: launch.Cols, Rows: launch.Rows})
		if err != nil {
			return nil, err
		}
		return process, nil
	}, nativefleet.Identity{StateDir: stateDir, Version: "shared-adapter-test"})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	pipe := winipc.PipePrefix + "shared-adapter-" + uuid.NewString()
	listener, err := winipc.Listen(pipe)
	if err != nil {
		_ = manager.Close()
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- manager.Serve(listener) }()
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("exact manager cleanup: %v", err)
		}
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("manager listener cleanup: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("manager listener did not close")
		}
	})
	request := func(r terminalhost.Request) terminalhost.Response {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		response, err := callPipe(ctx, pipe, r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	// The higher layer verifies manager namespace/build/protocol before binding.
	identity := request(terminalhost.Request{Op: "fleet-status"})
	if identity.ManagerStateDir != stateDir || identity.ManagerVersion != "shared-adapter-test" || identity.ProtocolVersion != nativefleet.ProtocolVersion || identity.HostPID != os.Getpid() || identity.RunID == "" {
		t.Fatalf("unverified manager: %+v", identity)
	}
	systemRoot := os.Getenv("SystemRoot")
	if !filepath.IsAbs(systemRoot) {
		t.Fatal("absolute SystemRoot is required")
	}
	cmd := filepath.Join(systemRoot, "System32", "cmd.exe")
	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	var processHandles []windows.Handle
	defer func() {
		for _, handle := range processHandles {
			_ = windows.CloseHandle(handle)
		}
	}()
	openHandle := func(pid uint32) windows.Handle {
		t.Helper()
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			t.Fatal(err)
		}
		processHandles = append(processHandles, handle)
		return handle
	}
	type nativeShell struct {
		info     terminalhost.SessionInfo
		terminal *Terminal
		handle   windows.Handle
		envValue string
		launch   terminalhost.LaunchSpec
	}
	create := func(name, group string) nativeShell {
		t.Helper()
		value := "ENV_中文_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		launch := terminalhost.LaunchSpec{Executable: cmd, Args: []string{"/d", "/q"}, Env: []string{"AD_NATIVE_ACCEPTANCE=" + value}, Dir: project, Cols: 120, Rows: 40}
		response := request(terminalhost.Request{Op: "fleet-create", Name: name, Tool: "shell", Group: group, Launch: &launch, AutoStart: true})
		if len(response.Sessions) != 1 || response.SessionID == "" || response.RunID == "" || !response.Running || response.PID == 0 {
			t.Fatalf("invalid created native shell: %+v", response)
		}
		info := response.Sessions[0]
		terminal, err := New(pipe, info.ID, info.RunID)
		if err != nil {
			t.Fatal(err)
		}
		return nativeShell{info: info, terminal: terminal, handle: openHandle(info.PID), envValue: value, launch: launch}
	}
	a := create("主会话", "开发/Windows")
	b := create("兄弟会话", "运维")
	if a.info.ID == b.info.ID || a.info.RunID == b.info.RunID || a.info.PID == b.info.PID {
		t.Fatal("sibling sessions reused a native identity")
	}
	writer, err := statedb.Open(filepath.Join(stateDir, nativestatestore.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.WriteToolDataExtra(a.info.ID, "acceptance_extra", json.RawMessage(`{"keep":"中文"}`)); err != nil {
		t.Fatal(err)
	}
	reader, err := statedb.OpenReadOnlyLive(filepath.Join(stateDir, nativestatestore.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	checkRow := func(shell nativeShell, name, group string) {
		t.Helper()
		row, err := reader.LoadInstanceByID(shell.info.ID)
		if err != nil || row == nil {
			t.Fatalf("original StateDB readback: %+v %v", row, err)
		}
		if row.Title != name || row.GroupPath != group || row.ProjectPath != project || row.Tool != "shell" || row.TmuxSession != "" || row.TmuxSocketName != "" {
			t.Fatalf("shared native row: %+v", row)
		}
		backend, err := nativestatestore.DecodeBackend(row.ToolData)
		if err != nil || backend == nil || !reflect.DeepEqual(backend.Launch, shell.launch) || len(backend.Launch.Env) != 1 {
			t.Fatalf("explicit launch changed/inherited: %+v %v", backend, err)
		}
		var extras map[string]json.RawMessage
		if err := json.Unmarshal(row.ToolData, &extras); err != nil {
			t.Fatal(err)
		}
		if shell.info.ID == a.info.ID && string(extras["acceptance_extra"]) != `{"keep":"中文"}` {
			t.Fatalf("unknown extra lost: %s", row.ToolData)
		}
		for current := group; current != ""; {
			if group, err := reader.LoadGroup(current); err != nil || group == nil {
				t.Fatalf("original group readback %s: %+v %v", current, group, err)
			}
			if index := strings.LastIndex(current, "/"); index >= 0 {
				current = current[:index]
			} else {
				current = ""
			}
		}
	}
	checkRow(a, "主会话", "开发/Windows")
	checkRow(b, "兄弟会话", "运维")
	runEncoded := func(terminal *Terminal, expected string) {
		t.Helper()
		// The result marker only comes from the child's explicit environment;
		// it is absent from the typed command, so input echo cannot satisfy it.
		script := "[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); [Console]::WriteLine($env:AD_NATIVE_ACCEPTANCE)"
		command := fmt.Sprintf("\"%s\" -NoLogo -NoProfile -EncodedCommand %s\r", powershell, encodePowerShell(script))
		if strings.Contains(command, expected) {
			t.Fatal("execution marker appears in typed input")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := terminal.Send(ctx, []byte(command)); err != nil {
			t.Fatal(err)
		}
		var last string
		for deadline := time.Now().Add(12 * time.Second); time.Now().Before(deadline); {
			screen, err := terminal.Capture(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			last = screen
			if strings.Contains(screen, expected) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("encoded native command was not observed: %q", last)
	}
	runEncoded(a.terminal, a.envValue)
	runEncoded(b.terminal, b.envValue)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := a.terminal.Resize(ctx, 123, 35); err != nil {
		t.Fatal(err)
	}
	resized := request(terminalhost.Request{Op: "capture", SessionID: a.info.ID, RunID: a.info.RunID})
	if resized.Cols != 123 || resized.Rows != 35 || resized.PID != a.info.PID {
		t.Fatalf("resize targeted wrong runtime: %+v", resized)
	}
	request(terminalhost.Request{Op: "fleet-rename", SessionID: a.info.ID, RunID: a.info.RunID, Name: "共享数据库改名"})
	request(terminalhost.Request{Op: "fleet-move", SessionID: a.info.ID, RunID: a.info.RunID, Group: "研发/原生"})
	checkRow(a, "共享数据库改名", "研发/原生")
	// Every operation uses a new client connection; reconnect retains exact run.
	reconnected, err := New(pipe, a.info.ID, a.info.RunID)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := reconnected.Observe(ctx)
	if err != nil || !observation.Alive || observation.PID != a.info.PID || observation.RunID != a.info.RunID {
		t.Fatalf("reconnected identity changed: %+v %v", observation, err)
	}
	if err := a.terminal.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if wait, err := windows.WaitForSingleObject(a.handle, 5000); err != nil || wait != windows.WAIT_OBJECT_0 {
		t.Fatalf("owned shell did not exit: %d %v", wait, err)
	}
	if wait, err := windows.WaitForSingleObject(b.handle, 0); err != nil || wait != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("stopping A killed sibling B: %d %v", wait, err)
	}
	if observed, err := b.terminal.Observe(ctx); err != nil || !observed.Alive || observed.PID != b.info.PID || observed.RunID != b.info.RunID {
		t.Fatalf("sibling changed: %+v %v", observed, err)
	}
	restarted := request(terminalhost.Request{Op: "fleet-start", SessionID: a.info.ID, RunID: a.info.RunID})
	if restarted.RunID == a.info.RunID || restarted.RunID == "" || !restarted.Running {
		t.Fatalf("restart reused generation: %+v", restarted)
	}
	replacementHandle := openHandle(restarted.PID)
	for _, operation := range []string{"status", "capture", "send", "resize", "stop"} {
		if err := exercise(a.terminal, operation, ctx); !errors.Is(err, ErrTargetMismatch) {
			t.Fatalf("stale %s accepted/rebound: %v", operation, err)
		}
	}
	replacement, err := New(pipe, a.info.ID, restarted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if observation, err := replacement.Observe(ctx); err != nil || !observation.Alive || observation.RunID != restarted.RunID {
		t.Fatalf("stale operation affected new run: %+v %v", observation, err)
	}
	runEncoded(replacement, a.envValue)
	runEncoded(b.terminal, b.envValue)
	checkRow(a, "共享数据库改名", "研发/原生")
	checkRow(b, "兄弟会话", "运维")
	if data, err := os.ReadFile(filepath.Join(project, "keep.txt")); err != nil || string(data) != string(sentinel) {
		t.Fatalf("project sentinel changed: %q %v", data, err)
	}
	if err := replacement.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.terminal.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []windows.Handle{replacementHandle, b.handle} {
		if wait, err := windows.WaitForSingleObject(handle, 5000); err != nil || wait != windows.WAIT_OBJECT_0 {
			t.Fatalf("exact final cleanup: %d %v", wait, err)
		}
	}
	t.Logf("shared original StateDB + verified native adapter passed; session IDs %s/%s; stale run %s fenced from replacement %s", a.info.ID, b.info.ID, a.info.RunID, restarted.RunID)
}

func encodePowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	data := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(data[index*2:], unit)
	}
	return base64.StdEncoding.EncodeToString(data)
}
