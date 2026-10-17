//go:build windows

package main

// matrix/hardened: opt-in UI integration owns private registry, pipe and native
// processes. Current console frames and native handles are headless evidence.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/nativefleet"
	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
)

type nativeFleetFixture struct {
	pipe   string
	store  *nativeregistry.Store
	server *nativefleet.Server
	served chan error
}

func nativeStartFleet(t *testing.T, stateDir string) *nativeFleetFixture {
	t.Helper()
	store, err := nativeregistry.Open(stateDir)
	if err != nil {
		t.Fatalf("open private native registry: %v", err)
	}
	server, err := nativefleet.New(store, func(launch terminalhost.LaunchSpec) (terminalhost.Process, error) {
		process, err := conpty.Start(conpty.Options{
			Executable: launch.Executable, Args: launch.Args, Env: mergedEnv(os.Environ(), launch.Env),
			Dir: launch.Dir, Cols: launch.Cols, Rows: launch.Rows,
		})
		if err != nil {
			return nil, err
		}
		return process, nil
	}, nativefleet.Identity{StateDir: stateDir, Version: version})
	if err != nil {
		_ = store.Close()
		t.Fatalf("create private native manager: %v", err)
	}
	t.Cleanup(func() { nativeClose(t, "private native manager", server.Close) })
	pipe := winipc.PipePrefix + "fleet-ui-test-" + uuid.NewString()
	listener, err := winipc.Listen(pipe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	fixture := &nativeFleetFixture{pipe: pipe, store: store, server: server, served: make(chan error, 1)}
	go func() { fixture.served <- server.Serve(listener) }()
	if response := nativeCall(t, pipe, terminalhost.Request{Op: "fleet-list"}); len(response.Sessions) > nativefleet.MaxSessions {
		t.Fatalf("private manager returned invalid session count: %d", len(response.Sessions))
	}
	return fixture
}

type nativeFleetUI struct {
	viewer *nativeViewer
	frame  *terminalhost.Server
}

func nativeStartFleetUI(t *testing.T, cli, pipe, stateDir, cwd string) *nativeFleetUI {
	t.Helper()
	// matrix/hardened: the native runner's GUID-private TEMP adds a long prefix.
	// Keep full-path assertions observable, with room for named child projects,
	// the input label and cursor; production UI sizing remains unchanged.
	cols := max(160, ansi.StringWidth(cwd)+80)
	process, err := conpty.Start(conpty.Options{
		Executable: cli, Args: []string{"ui", "--pipe", pipe, "--state-dir", stateDir, "--cwd", cwd},
		Dir: cwd, Cols: cols, Rows: 45,
	})
	if err != nil {
		t.Fatalf("start actual native UI: %v", err)
	}
	frame, err := terminalhost.New(process, cols, 45)
	if err != nil {
		_ = process.Close()
		t.Fatalf("decode current UI console screen: %v", err)
	}
	t.Cleanup(func() { nativeClose(t, "native UI console", frame.Close) })
	return &nativeFleetUI{viewer: &nativeViewer{process: process}, frame: frame}
}

func (ui *nativeFleetUI) screen() string {
	return ui.frame.Dispatch(terminalhost.Request{Op: "capture"}).Screen
}

func nativeFleetFrame(t *testing.T, ui *nativeFleetUI, label string, wanted ...string) {
	t.Helper()
	nativeAwait(t, label, func() (bool, string) {
		screen := ui.screen()
		for _, value := range wanted {
			if !strings.Contains(screen, value) {
				return false, screen
			}
		}
		return true, screen
	})
}

func nativeFleetFind(t *testing.T, pipe, name, state string) terminalhost.SessionInfo {
	t.Helper()
	var result terminalhost.SessionInfo
	nativeAwait(t, "independent registry/runtime readback for "+name, func() (bool, string) {
		response, err := callHost(context.Background(), pipe, terminalhost.Request{Op: "fleet-list"})
		if err != nil {
			return false, err.Error()
		}
		for _, info := range response.Sessions {
			if info.Name == name {
				result = info
				return info.State == state, fmt.Sprintf("%+v", info)
			}
		}
		return false, fmt.Sprintf("%+v", response.Sessions)
	})
	return result
}

func nativeFleetCreateFromUI(t *testing.T, ui *nativeFleetUI, fixture *nativeFleetFixture, name, cwd, powershell string) (terminalhost.SessionInfo, string) {
	t.Helper()
	nativeWrite(t, ui.viewer, "n")
	nativeFleetFrame(t, ui, "new-session form", "新建会话", "名称：", "目录：", "工具：")
	// matrix/hardened: Ctrl+A/Ctrl+K replace the prefilled fields through the
	// same native keyboard path, then Tab exercises directory and tool focus.
	nativeWrite(t, ui.viewer, "\x01\x0b"+name+"\t")
	nativeFleetFrame(t, ui, "directory field focus", name, "› 目录：")
	nativeWrite(t, ui.viewer, "\x01\x0b"+cwd+"\t")
	// matrix/hardened: focus and chosen tool must belong to the same rendered row.
	nativeFleetFrame(t, ui, "tool field focus", cwd, "› 工具：[Shell]")
	nativeWrite(t, ui.viewer, "\x1b[C")
	nativeFleetFrame(t, ui, "select Codex without starting it", "› 工具：Shell   [Codex]")
	nativeWrite(t, ui.viewer, "\x1b[D")
	nativeFleetFrame(t, ui, "return tool selection to Shell", "› 工具：[Shell]")
	nativeWrite(t, ui.viewer, "\r")
	info := nativeFleetFind(t, fixture.pipe, name, "running")
	if info.ID == "" || info.RunID == "" || info.PID == 0 || info.HostPID != os.Getpid() || info.Tool != "shell" || !strings.EqualFold(info.Dir, cwd) {
		t.Fatalf("UI launch identity/fields disagree with native manager: %+v", info)
	}
	nativeAwait(t, "UI automatically attaches to the new native Shell", func() (bool, string) {
		screen := ui.screen()
		return strings.Contains(strings.ToLower(screen), strings.ToLower(cwd)+">"), screen
	})
	marker := "FLEET_中文_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	command := nativeEncodedCommand(powershell, "[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); [Console]::WriteLine('"+marker+"')")
	if strings.Contains(command, marker) {
		t.Fatal("clear execution marker appears in typed command")
	}
	nativeWrite(t, ui.viewer, command)
	nativeFleetFrame(t, ui, "actual encoded command execution through UI attach", marker)
	response := nativeCall(t, fixture.pipe, terminalhost.Request{Op: "capture", SessionID: info.ID, RunID: info.RunID})
	if !strings.Contains(response.Screen, marker) || response.PID != info.PID {
		t.Fatalf("independent Shell screen/PID did not confirm UI execution: %+v", response)
	}
	nativeWrite(t, ui.viewer, "\x11")
	nativeFleetFrame(t, ui, "Ctrl+Q returns from Shell to native list", "Windows 原生会话", name, "已返回列表")
	return info, marker
}

func nativeFleetSameRun(t *testing.T, before, after terminalhost.SessionInfo) {
	t.Helper()
	if before.ID != after.ID || before.RunID != after.RunID || before.PID != after.PID || before.HostPID != after.HostPID || after.State != "running" {
		t.Fatalf("window operation changed native session instance: before=%+v after=%+v", before, after)
	}
}

func nativeFleetProjectUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("descriptor removal changed project sentinel: got=%q err=%v want=%q", got, err, want)
	}
}

func TestWindowsNativeFleetUIOwnsOnlyWindows(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 and AGENTDECK_NATIVE_CLI to a fresh managed native CLI")
	}
	cli := os.Getenv("AGENTDECK_NATIVE_CLI")
	if !filepath.IsAbs(cli) {
		t.Fatal("AGENTDECK_NATIVE_CLI must be an absolute freshly built .exe path")
	}
	if info, err := os.Stat(cli); err != nil || info.IsDir() {
		t.Fatalf("native CLI missing: %v", err)
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		t.Fatal("SystemRoot is required for native Shell test fixtures")
	}
	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	stateDir := filepath.Join(t.TempDir(), "manager")
	projectRoot := t.TempDir()
	firstDir, secondDir := filepath.Join(projectRoot, "first"), filepath.Join(projectRoot, "second")
	for _, dir := range []string{firstDir, secondDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sentinelData := []byte("private native test project content\n")
	firstFile, secondFile := filepath.Join(firstDir, "keep.txt"), filepath.Join(secondDir, "keep.txt")
	for _, path := range []string{firstFile, secondFile} {
		if err := os.WriteFile(path, sentinelData, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := nativeStartFleet(t, stateDir)
	ui := nativeStartFleetUI(t, cli, fixture.pipe, stateDir, projectRoot)
	nativeFleetFrame(t, ui, "fresh real UI shows empty private registry", "Windows 原生会话", "0 个", "没有会话")
	firstName, secondName := "第一 Shell", "第二 Shell"
	first, firstMarker := nativeFleetCreateFromUI(t, ui, fixture, firstName, firstDir, powershell)
	firstHandle := nativeProcessHandle(t, first.PID)
	second, _ := nativeFleetCreateFromUI(t, ui, fixture, secondName, secondDir, powershell)
	secondHandle := nativeProcessHandle(t, second.PID)
	if first.ID == second.ID || first.PID == second.PID || first.RunID == second.RunID {
		t.Fatalf("two UI-created native sessions share identity: first=%+v second=%+v", first, second)
	}
	nativeFleetFrame(t, ui, "two sessions visible after detaching", "2 个", firstName, secondName)
	nativeWrite(t, ui.viewer, "q")
	nativeWaitExit(t, ui.viewer.process, 0)
	nativeAssertProcess(t, firstHandle, true)
	nativeAssertProcess(t, secondHandle, true)
	nativeFleetSameRun(t, first, nativeFleetFind(t, fixture.pipe, firstName, "running"))
	nativeFleetSameRun(t, second, nativeFleetFind(t, fixture.pipe, secondName, "running"))
	for _, info := range []terminalhost.SessionInfo{first, second} {
		record, err := fixture.store.Get(info.ID)
		if err != nil || record.Name != info.Name || record.Tool != "shell" || !strings.EqualFold(record.Launch.Dir, info.Dir) {
			t.Fatalf("UI records were not durably saved: record=%+v err=%v", record, err)
		}
	}
	reopened := nativeStartFleetUI(t, cli, fixture.pipe, stateDir, projectRoot)
	nativeFleetFrame(t, reopened, "fresh UI reads both durable session records", "2 个", firstName, secondName)
	nativeWrite(t, reopened.viewer, "k\r")
	nativeFleetFrame(t, reopened, "fresh UI reattaches to prior screen", firstMarker)
	nativeFleetSameRun(t, first, nativeFleetFind(t, fixture.pipe, firstName, "running"))
	nativeWrite(t, reopened.viewer, "\x11")
	nativeFleetFrame(t, reopened, "reattach detach returns list", "Windows 原生会话", "已返回列表", firstName)
	nativeWrite(t, reopened.viewer, "s")
	nativeFleetFrame(t, reopened, "stop confirmation targets first session", "停止此会话？", firstName)
	nativeWrite(t, reopened.viewer, "\r")
	nativeFleetFrame(t, reopened, "UI stop returns completed list", "会话已停止。", "已停止")
	stopped := nativeFleetFind(t, fixture.pipe, firstName, "stopped")
	if stopped.ID != first.ID || stopped.RunID != first.RunID {
		t.Fatalf("UI stopped a replacement instance: before=%+v after=%+v", first, stopped)
	}
	nativeAssertProcess(t, firstHandle, false)
	nativeAssertProcess(t, secondHandle, true)
	nativeFleetSameRun(t, second, nativeFleetFind(t, fixture.pipe, secondName, "running"))
	nativeWrite(t, reopened.viewer, "q")
	nativeWaitExit(t, reopened.viewer.process, 0)
	started := nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-start", SessionID: first.ID, RunID: first.RunID})
	if len(started.Sessions) != 1 {
		t.Fatalf("restart did not report exact session: %+v", started)
	}
	replacement := started.Sessions[0]
	if replacement.State != "running" || replacement.PID == 0 || replacement.PID == first.PID || replacement.RunID == "" || replacement.RunID == first.RunID {
		t.Fatalf("restart did not establish a new native instance: before=%+v after=%+v", first, replacement)
	}
	replacementHandle := nativeProcessHandle(t, replacement.PID)
	stale, err := callHost(context.Background(), fixture.pipe, terminalhost.Request{Op: "fleet-stop", SessionID: first.ID, RunID: first.RunID})
	if err == nil || stale.OK || !strings.Contains(stale.Error, "run changed") {
		t.Fatalf("stale stop accepted or returned wrong boundary: %+v %v", stale, err)
	}
	nativeAssertProcess(t, replacementHandle, true)
	nativeAssertProcess(t, secondHandle, true)
	renamed := firstName + " 已改名"
	nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-rename", SessionID: first.ID, RunID: replacement.RunID, Name: renamed})
	record, err := fixture.store.Get(first.ID)
	if err != nil || record.Name != renamed {
		t.Fatalf("renamed descriptor was not saved: %+v %v", record, err)
	}
	nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-remove", SessionID: first.ID, RunID: replacement.RunID})
	nativeAssertProcess(t, replacementHandle, false)
	if _, err := fixture.store.Get(first.ID); !errors.Is(err, nativeregistry.ErrNotFound) {
		t.Fatalf("removed descriptor remains or registry error changed: %v", err)
	}
	nativeFleetProjectUnchanged(t, firstFile, sentinelData)
	nativeFleetProjectUnchanged(t, secondFile, sentinelData)
	nativeAssertProcess(t, secondHandle, true)
	nativeFleetSameRun(t, second, nativeFleetFind(t, fixture.pipe, secondName, "running"))
	nativeClose(t, "explicit native manager shutdown", fixture.server.Close)
	nativeAssertProcess(t, secondHandle, false)
	select {
	case err := <-fixture.served:
		if err != nil {
			t.Fatalf("native manager Serve after exact cleanup: %v", err)
		}
	case <-time.After(nativeAttachTimeout):
		t.Fatal("private manager listener did not close")
	}
	recovered := nativeStartFleet(t, stateDir)
	response := nativeCall(t, recovered.pipe, terminalhost.Request{Op: "fleet-list"})
	if len(response.Sessions) != 1 || response.Sessions[0].ID != second.ID || response.Sessions[0].State != "stopped" || response.Sessions[0].PID != 0 || response.Sessions[0].RunID != "" {
		t.Fatalf("manager restart invented or automatically launched runtime state: %+v", response)
	}
	nativeCall(t, recovered.pipe, terminalhost.Request{Op: "fleet-remove", SessionID: second.ID})
	if response := nativeCall(t, recovered.pipe, terminalhost.Request{Op: "fleet-list"}); len(response.Sessions) != 0 {
		t.Fatalf("descriptor-only removal did not clear recovered record: %+v", response)
	}
	nativeFleetProjectUnchanged(t, firstFile, sentinelData)
	nativeFleetProjectUnchanged(t, secondFile, sentinelData)
	t.Logf("headless native UI workflow verified two Shells, persistent IDs, detach/reattach, independent stop, RunID fence, descriptor-only removal; first_pid=%d second_pid=%d replacement_pid=%d; no visible desktop assertion", first.PID, second.PID, replacement.PID)
}

func TestWindowsNativeFleetPartialCreateReportsDescriptor(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 to test isolated native partial create failures")
	}
	stateDir := filepath.Join(t.TempDir(), "manager")
	projectDir := t.TempDir()
	fixture := nativeStartFleet(t, stateDir)
	missing := filepath.Join(projectDir, "missing-owned-native-fixture.exe")
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"create", "--state-dir", stateDir, "--pipe", fixture.pipe,
		"--tool", "custom", "--name", "CLI partial failure", "--cwd", projectDir,
		"--json", "--", missing,
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("creating a nonexistent executable unexpectedly succeeded")
	}
	var response terminalhost.Response
	if decodeErr := json.Unmarshal(stdout.Bytes(), &response); decodeErr != nil {
		t.Fatalf("CLI failed create omitted structured descriptor proof: stdout=%q stderr=%q decode=%v create=%v", stdout.String(), stderr.String(), decodeErr, err)
	}
	if response.OK || response.Error == "" || len(response.Sessions) != 1 || response.Sessions[0].ID == "" || response.Sessions[0].Name != "CLI partial failure" {
		t.Fatalf("CLI partial-create response lost exact descriptor identity: %+v create=%v", response, err)
	}
	cliID := response.Sessions[0].ID
	records, listErr := fixture.store.List()
	if listErr != nil || len(records) != 1 || records[0].ID != cliID || records[0].Launch.Executable != missing {
		t.Fatalf("CLI failure did not preserve exactly one matching descriptor: records=%+v err=%v", records, listErr)
	}
	// matrix/hardened: mutate only this test process's shell selection and
	// restore it immediately, including the unset case, before any later test.
	previous, existed := os.LookupEnv("COMSPEC")
	if err := os.Setenv("COMSPEC", missing); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if existed {
			_ = os.Setenv("COMSPEC", previous)
		} else {
			_ = os.Unsetenv("COMSPEC")
		}
	}
	defer restore()
	service := &nativeService{pipe: fixture.pipe}
	info, createErr := service.Create(context.Background(), "UI partial failure", "shell", projectDir)
	restore()
	if createErr == nil || info.ID == "" || info.Name != "UI partial failure" || info.ID == cliID {
		t.Fatalf("service partial create lost new descriptor identity: info=%+v err=%v", info, createErr)
	}
	records, listErr = fixture.store.List()
	if listErr != nil || len(records) != 2 {
		t.Fatalf("service partial create retried or failed to persist once: records=%+v err=%v", records, listErr)
	}
	var matched int
	for _, record := range records {
		if record.ID == info.ID && record.Name == info.Name && record.Launch.Executable == missing && record.Tool == "shell" {
			matched++
		}
	}
	if matched != 1 {
		t.Fatalf("service error identity did not map to exactly one saved descriptor: info=%+v records=%+v", info, records)
	}
	response = nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-list"})
	if len(response.Sessions) != 2 {
		t.Fatalf("independent fleet readback omitted partial descriptors: %+v", response)
	}
	for _, saved := range response.Sessions {
		if saved.PID != 0 || saved.RunID != "" || saved.State == "running" {
			t.Fatalf("failed executable created invented native runtime identity: %+v", saved)
		}
	}
	t.Logf("failed auto-start preserved exact descriptor once and returned identity: cli_id=%s service_id=%s", cliID, info.ID)
}
