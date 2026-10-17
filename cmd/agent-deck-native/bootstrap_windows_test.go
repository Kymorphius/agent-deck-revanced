//go:build windows

package main

// matrix/hardened: these opt-in tests launch only GUID-private managers via the
// actual CLI. Exact IPC identities/handles delimit cleanup; no service or task,
// process-name scan, unverified PID kill, or visible-desktop claim is involved.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

func nativeBootstrapCLI(t *testing.T) string {
	t.Helper()
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 and AGENTDECK_NATIVE_CLI to the freshly built native CLI")
	}
	cli := os.Getenv("AGENTDECK_NATIVE_CLI")
	if !filepath.IsAbs(cli) {
		t.Fatal("AGENTDECK_NATIVE_CLI must identify an absolute freshly built .exe")
	}
	if info, err := os.Stat(cli); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("native CLI missing or not a regular file: %v", err)
	}
	return cli
}

// Cleanup binds to the exact first verified daemon instance. Startup failure
// must never grant permission to kill the broker's unverified PID.
func nativeBootstrapCleanup(t *testing.T, config options, identity *terminalhost.Response) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), nativeManagerStartupTimeout)
		defer cancel()
		response, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: "fleet-status"})
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return
		}
		if err != nil || validateNativeManager(config, response) != nil {
			t.Errorf("private manager cleanup could not verify namespace %s: %v", config.Pipe, err)
			return
		}
		if identity.RunID != "" && (response.RunID != identity.RunID || response.HostPID != identity.HostPID) {
			t.Errorf("private manager identity changed before cleanup; refused shutdown for %s", config.Pipe)
			return
		}
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(response.HostPID))
		if err != nil {
			t.Errorf("private manager handle for confirmed shutdown: %v", err)
			return
		}
		defer windows.CloseHandle(handle)
		if _, err := callHost(ctx, config.Pipe, terminalhost.Request{Op: "fleet-shutdown"}); err != nil {
			t.Errorf("exact private manager shutdown: %v", err)
			return
		}
		wait, err := windows.WaitForSingleObject(handle, uint32(nativeAttachTimeout/time.Millisecond))
		if err != nil || wait != windows.WAIT_OBJECT_0 {
			t.Errorf("verified manager did not exit after shutdown: wait=%d err=%v", wait, err)
			return
		}
		// Closing the verified daemon must release the exact registry lease.
		var store interface{ Close() error }
		// matrix/hardened: verify lease release with the same authoritative store.
		if config.Store == "statedb" {
			store, err = nativestatestore.Open(config.StateDir)
		} else {
			store, err = nativeregistry.Open(config.StateDir)
		}
		if err != nil {
			t.Errorf("private manager shutdown did not release its registry owner: %v", err)
			return
		}
		if err := store.Close(); err != nil {
			t.Errorf("private registry cleanup readback: %v", err)
		}
	})
}

func nativeBootstrapIdentity(t *testing.T, config options) terminalhost.Response {
	t.Helper()
	response := nativeCall(t, config.Pipe, terminalhost.Request{Op: "fleet-status"})
	if err := validateNativeManager(config, response); err != nil {
		t.Fatalf("actual CLI manager handshake rejected: %v response=%+v", err, response)
	}
	return response
}

func TestWindowsNativeBootstrapSurvivesOuterConsoleJob(t *testing.T) {
	cli := nativeBootstrapCLI(t)
	config := options{StateDir: filepath.Join(t.TempDir(), "manager-"+uuid.NewString()), Pipe: winipc.PipePrefix + "bootstrap-job-" + uuid.NewString()}
	var identity terminalhost.Response
	project := t.TempDir()
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	// The manager must carry the launcher's environment without embedding it in
	// PowerShell argv or persisting the whole environment in its registry.
	const envName = "AGENTDECK_NATIVE_BOOTSTRAP_ENV_TEST"
	envValue := "中文 \"quoted\" $literal & " + uuid.NewString()
	t.Setenv(envName, envValue)
	// Shut the exact daemon/Shell down before restoring environment or deleting
	// its working directory; later UI cleanup still runs before daemon cleanup.
	nativeBootstrapCleanup(t, config, &identity)
	ui := nativeStartFleetUI(t, cli, config.Pipe, config.StateDir, project)
	nativeFleetFrame(t, ui, "automatic native manager launch exposes empty UI", "Windows 原生会话", "没有会话")
	identity = nativeBootstrapIdentity(t, config)
	if identity.HostPID == os.Getpid() || uint32(identity.HostPID) == ui.viewer.process.PID() {
		t.Fatalf("background manager is still the test/client process: %+v", identity)
	}
	managerHandle := nativeProcessHandle(t, uint32(identity.HostPID))
	name := "后台保活 " + uuid.NewString()[:8]
	nativeWrite(t, ui.viewer, "n")
	nativeFleetFrame(t, ui, "create form on automatically started daemon", "新建会话", "名称：", "目录：", "工具：")
	nativeWrite(t, ui.viewer, "\x01\x0b"+name+"\t")
	nativeFleetFrame(t, ui, "project field on native create form", name, "› 目录：")
	nativeWrite(t, ui.viewer, "\x01\x0b"+project+"\t")
	// matrix/hardened: the exact focused row proves Shell remains selected.
	nativeFleetFrame(t, ui, "native Shell selection", project, "› 工具：[Shell]")
	nativeWrite(t, ui.viewer, "\r")
	shell := nativeFleetFind(t, config.Pipe, name, "running")
	// matrix/hardened: verify the complete project path independently of form width.
	if shell.HostPID != identity.HostPID || shell.PID == 0 || shell.RunID == "" || shell.Tool != "shell" || !strings.EqualFold(shell.Dir, project) {
		t.Fatalf("UI-created Shell does not belong to verified background manager: %+v", shell)
	}
	shellHandle := nativeProcessHandle(t, shell.PID)
	nativeAwait(t, "actual UI attaches to its newly created Shell", func() (bool, string) {
		screen := ui.screen()
		return strings.Contains(strings.ToLower(screen), strings.ToLower(project)+">"), screen
	})
	marker := "BOOTSTRAP_中文_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	script := "[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); if ($env:" + envName + " -ceq '" + strings.ReplaceAll(envValue, "'", "''") + "') {[Console]::WriteLine('" + marker + "')} else {exit 17}"
	command := nativeEncodedCommand(powershell, script)
	if strings.Contains(command, marker) || strings.Contains(command, envValue) {
		t.Fatal("clear fixture marker or inherited environment appeared in typed source")
	}
	nativeWrite(t, ui.viewer, command)
	nativeFleetFrame(t, ui, "WMI manager preserves inherited Unicode environment and real Shell execution", marker)
	capture := nativeCall(t, config.Pipe, terminalhost.Request{Op: "capture", SessionID: shell.ID, RunID: shell.RunID})
	if !strings.Contains(capture.Screen, marker) || capture.PID != shell.PID {
		t.Fatalf("independent manager screen did not prove execution on same Shell: %+v", capture)
	}
	// This closes the outer ConPTY's kill-on-close Job while attach is active.
	// A directly inherited background child would die here with the client.
	nativeClose(t, "kill outer UI console and its owned client Job", ui.frame.Close)
	nativeAssertProcess(t, managerHandle, true)
	nativeAssertProcess(t, shellHandle, true)
	after := nativeBootstrapIdentity(t, config)
	if after.HostPID != identity.HostPID || after.RunID != identity.RunID {
		t.Fatalf("closing outer console replaced the manager: before=%+v after=%+v", identity, after)
	}
	nativeFleetSameRun(t, shell, nativeFleetFind(t, config.Pipe, name, "running"))
	second := nativeStartFleetUI(t, cli, config.Pipe, config.StateDir, project)
	nativeFleetFrame(t, second, "new UI reuses the same background manager", "Windows 原生会话", name)
	nativeWrite(t, second.viewer, "\r")
	nativeFleetFrame(t, second, "fresh UI restores original Shell screen", marker)
	nativeFleetSameRun(t, shell, nativeFleetFind(t, config.Pipe, name, "running"))
	nativeWrite(t, second.viewer, "\x11")
	nativeFleetFrame(t, second, "Ctrl+Q returns to list on persistent daemon", "Windows 原生会话", "已返回列表")
	nativeWrite(t, second.viewer, "q")
	nativeWaitExit(t, second.viewer.process, 0)
	nativeAssertProcess(t, managerHandle, true)
	nativeAssertProcess(t, shellHandle, true)
	t.Logf("headless actual CLI automatic startup/job survival verified manager_pid=%d shell_pid=%d; no persistent task/service and no visible desktop assertion", identity.HostPID, shell.PID)
}

func nativeBootstrapRunCLI(ctx context.Context, cli string, config options) (terminalhost.Response, error) {
	cmd := exec.CommandContext(ctx, cli, "manager-start", "--state-dir", config.StateDir, "--pipe", config.Pipe, "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return terminalhost.Response{}, fmt.Errorf("manager-start: %w stderr=%s", err, stderr.String())
	}
	var response terminalhost.Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return terminalhost.Response{}, fmt.Errorf("decode manager-start response: %w", err)
	}
	return response, nil
}

func TestWindowsNativeBootstrapConcurrentClientsHaveOneOwner(t *testing.T) {
	cli := nativeBootstrapCLI(t)
	config := options{StateDir: filepath.Join(t.TempDir(), "manager-"+uuid.NewString()), Pipe: winipc.PipePrefix + "bootstrap-race-" + uuid.NewString()}
	var identity terminalhost.Response
	nativeBootstrapCleanup(t, config, &identity)
	ctx, cancel := context.WithTimeout(context.Background(), nativeManagerStartupTimeout+5*time.Second)
	defer cancel()
	var responses [2]terminalhost.Response
	var failures [2]error
	var workers sync.WaitGroup
	start := make(chan struct{})
	for index := range responses {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			responses[index], failures[index] = nativeBootstrapRunCLI(ctx, cli, config)
		}(index)
	}
	close(start)
	workers.Wait()
	identity = nativeBootstrapIdentity(t, config)
	for index, response := range responses {
		if failures[index] != nil || validateNativeManager(config, response) != nil || response.HostPID != identity.HostPID || response.RunID != identity.RunID {
			t.Fatalf("concurrent client %d did not reuse sole verified owner: response=%+v err=%v owner=%+v", index, response, failures[index], identity)
		}
	}
	if second, err := nativeregistry.Open(config.StateDir); !errors.Is(err, nativeregistry.ErrOwned) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("verified background daemon does not hold sole registry lease: %v", err)
	}
	nativeAssertProcess(t, nativeProcessHandle(t, uint32(identity.HostPID)), true)
	t.Logf("two simultaneous CLI clients agreed on one manager/registry owner pid=%d run_id=%s", identity.HostPID, identity.RunID)
}

func TestWindowsNativeBootstrapNamespaceCollisionPreservesOwner(t *testing.T) {
	cli := nativeBootstrapCLI(t)
	foreignRoot := filepath.Join(t.TempDir(), "foreign-manager-"+uuid.NewString())
	fixture := nativeStartFleet(t, foreignRoot)
	before := nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-status"})
	config := options{StateDir: filepath.Join(t.TempDir(), "requested-manager-"+uuid.NewString()), Pipe: fixture.pipe}
	ctx, cancel := context.WithTimeout(context.Background(), nativeManagerStartupTimeout+5*time.Second)
	defer cancel()
	_, err := nativeBootstrapRunCLI(ctx, cli, config)
	if err == nil {
		t.Fatal("manager-start reused another registry's endpoint")
	}
	after := nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-status"})
	if before.HostPID != after.HostPID || before.RunID != after.RunID || after.ManagerStateDir != foreignRoot {
		t.Fatalf("collision check replaced or mutated existing manager: before=%+v after=%+v", before, after)
	}
	if _, err := os.Stat(config.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign endpoint check created requested registry before rejection: %v", err)
	}
	nativeAssertProcess(t, nativeProcessHandle(t, uint32(before.HostPID)), true)
}

func TestWindowsNativeBootstrapMissingParentReturnsBoundedError(t *testing.T) {
	cli := nativeBootstrapCLI(t)
	root := t.TempDir()
	config := options{StateDir: filepath.Join(root, "missing-parent-"+uuid.NewString(), "manager"), Pipe: winipc.PipePrefix + "bootstrap-missing-" + uuid.NewString()}
	ctx, cancel := context.WithTimeout(context.Background(), nativeManagerStartupTimeout+5*time.Second)
	defer cancel()
	started := time.Now()
	_, err := nativeBootstrapRunCLI(ctx, cli, config)
	if err == nil || time.Since(started) > nativeManagerStartupTimeout+4*time.Second {
		t.Fatalf("unavailable registry parent did not return bounded startup error: err=%v elapsed=%s", err, time.Since(started))
	}
	if _, err := os.Stat(config.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup unexpectedly created ancestors: %v", err)
	}
	if response, probeErr := callHost(context.Background(), config.Pipe, terminalhost.Request{Op: "fleet-status"}); !errors.Is(probeErr, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("failed startup left a live endpoint: response=%+v err=%v", response, probeErr)
	}
}
