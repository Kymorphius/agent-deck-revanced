//go:build windows

package main

// matrix/hardened: opt-in group/search acceptance uses current native console
// frames, exact run identities and private test directories; no desktop claim.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

func TestWindowsNativeShortSearchKeepsKeyboardMeaning(t *testing.T) {
	cli := nativeBootstrapCLI(t)
	stateDir, project := filepath.Join(t.TempDir(), "registry"), t.TempDir()
	fixture := nativeStartFleet(t, stateDir)
	process, err := conpty.Start(conpty.Options{Executable: cli, Args: []string{"ui", "--pipe", fixture.pipe, "--state-dir", stateDir, "--cwd", project}, Dir: project, Cols: 80, Rows: 6})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := terminalhost.New(process, 80, 6)
	if err != nil {
		_ = process.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { nativeClose(t, "short native UI", frame.Close) })
	ui := &nativeFleetUI{viewer: &nativeViewer{process: process}, frame: frame}
	nativeGroupFrame(t, ui, "short native list before search", []string{"q 退出", "按 n 新建"}, nil)
	nativeWrite(t, ui.viewer, "/")
	nativeGroupFrame(t, ui, "short native search preserves active key hints", []string{"Enter 保留", "Esc 清除"}, []string{"q 退出", "按 n 新建"})
	nativeWrite(t, ui.viewer, "nq")
	nativeGroupFrame(t, ui, "n and q are query text in actual short console", []string{"搜索：nq"}, []string{"q 退出", "按 n 新建"})
	if response := nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-list"}); len(response.Sessions) != 0 {
		t.Fatal("search input created a native session")
	}
	nativeWrite(t, ui.viewer, "\x1b")
	nativeGroupFrame(t, ui, "short native list restores shortcuts after search", []string{"q 退出", "按 n 新建"}, []string{"Enter 保留"})
	nativeWrite(t, ui.viewer, "q")
	nativeWaitExit(t, process, 0)
}

func nativeGroupFrame(t *testing.T, ui *nativeFleetUI, label string, wanted, absent []string) {
	t.Helper()
	nativeAwait(t, label, func() (bool, string) {
		screen := ui.screen()
		for _, text := range wanted {
			if !strings.Contains(screen, text) {
				return false, screen
			}
		}
		for _, text := range absent {
			if strings.Contains(screen, text) {
				return false, screen
			}
		}
		return true, screen
	})
}

func nativeGroupSearch(t *testing.T, ui *nativeFleetUI, query, selected, excluded string) {
	t.Helper()
	nativeWrite(t, ui.viewer, "/"+query)
	nativeGroupFrame(t, ui, "live search selects exact current row for "+query,
		[]string{"1/2 个", "分组：全部分组", "› " + selected, "Esc 清除"}, []string{excluded})
	nativeWrite(t, ui.viewer, "\x1b")
	nativeGroupFrame(t, ui, "Esc clears live search for "+query,
		[]string{"2/2 个", "分组：全部分组", selected, excluded}, []string{"Enter 保留"})
}

func nativeGroupShellFromUI(t *testing.T, ui *nativeFleetUI, fixture *nativeFleetFixture, name, cwd, group, powershell string) terminalhost.SessionInfo {
	t.Helper()
	nativeWrite(t, ui.viewer, "n")
	nativeFleetFrame(t, ui, "grouped creation form", "新建会话", "名称：", "目录：", "工具：", "分组：")
	nativeWrite(t, ui.viewer, "\x01\x0b"+name+"\t")
	nativeFleetFrame(t, ui, "grouped creation directory focus", name, "› 目录：")
	nativeWrite(t, ui.viewer, "\x01\x0b"+cwd+"\t")
	// matrix/hardened: focus and Shell selection must be on the same current row.
	nativeFleetFrame(t, ui, "grouped creation tool focus", cwd, "› 工具：[Shell]")
	nativeWrite(t, ui.viewer, "\t")
	nativeFleetFrame(t, ui, "fourth creation field is group", "› 分组：", "工具：[Shell]")
	nativeWrite(t, ui.viewer, "\x01\x0b"+group+"\r")
	info := nativeFleetFind(t, fixture.pipe, name, "running")
	if info.Group != group || info.Tool != "shell" || info.ID == "" || info.RunID == "" || info.PID == 0 || info.HostPID != os.Getpid() || !strings.EqualFold(info.Dir, cwd) {
		t.Fatalf("UI grouped Shell launch disagrees with independent manager readback: %+v", info)
	}
	nativeAwait(t, "UI enters grouped Shell", func() (bool, string) {
		screen := ui.screen()
		return strings.Contains(strings.ToLower(screen), strings.ToLower(cwd)+">"), screen
	})
	marker := "GROUP_SHELL_中文_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	command := nativeEncodedCommand(powershell, "[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); [Console]::WriteLine('"+marker+"')")
	if strings.Contains(command, marker) {
		t.Fatal("execution proof marker appears in typed command")
	}
	nativeWrite(t, ui.viewer, command)
	nativeFleetFrame(t, ui, "grouped Shell executed encoded native input", marker)
	response := nativeCall(t, fixture.pipe, terminalhost.Request{Op: "capture", SessionID: info.ID, RunID: info.RunID})
	if response.PID != info.PID || !strings.Contains(response.Screen, marker) {
		t.Fatalf("independent grouped Shell capture omitted execution proof: %+v", response)
	}
	nativeWrite(t, ui.viewer, "\x11")
	nativeFleetFrame(t, ui, "Ctrl+Q leaves grouped Shell running", "Windows 原生会话", name, group, "已返回列表")
	return info
}

func TestWindowsNativeGroupsSearchPowerShell(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 and AGENTDECK_NATIVE_CLI to a fresh native group/search CLI")
	}
	cli := os.Getenv("AGENTDECK_NATIVE_CLI")
	if !filepath.IsAbs(cli) {
		t.Fatal("AGENTDECK_NATIVE_CLI must identify an absolute freshly built .exe")
	}
	if info, err := os.Stat(cli); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("native CLI missing or not a regular file: %v", err)
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		t.Fatal("SystemRoot is required for rooted native PowerShell launch")
	}
	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	stateDir := filepath.Join(t.TempDir(), "manager")
	projectRoot := t.TempDir()
	shellDir := filepath.Join(projectRoot, "研发工程-dir-match")
	powerDir := filepath.Join(projectRoot, "系统控制")
	sentinel := []byte("private grouped native project content\n")
	for _, dir := range []string{shellDir, powerDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), sentinel, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := nativeStartFleet(t, stateDir)
	ui := nativeStartFleetUI(t, cli, fixture.pipe, stateDir, projectRoot)
	nativeFleetFrame(t, ui, "fresh current native group screen", "Windows 原生会话", "0/0 个", "分组：全部分组")
	const shellName, powerName = "中文研发", "中文维护"
	shell := nativeGroupShellFromUI(t, ui, fixture, shellName, shellDir, "研发", powershell)
	shellHandle := nativeProcessHandle(t, shell.PID)
	launch := terminalhost.LaunchSpec{
		Executable: powershell, Args: []string{"-NoLogo", "-NoProfile"},
		Dir: powerDir, Cols: 160, Rows: 45,
	}
	created := nativeCall(t, fixture.pipe, terminalhost.Request{
		Op: "fleet-create", Name: powerName, Tool: "powershell", Group: "运维", Launch: &launch, AutoStart: true,
	})
	if len(created.Sessions) != 1 {
		t.Fatalf("PowerShell creation omitted exact session identity: %+v", created)
	}
	power := created.Sessions[0]
	if power.ID == shell.ID || power.PID == 0 || power.PID == shell.PID || power.RunID == "" || power.RunID == shell.RunID || power.State != "running" || power.Tool != "powershell" || power.Group != "运维" || !strings.EqualFold(power.Dir, powerDir) {
		t.Fatalf("separate grouped PowerShell identity invalid: shell=%+v powershell=%+v", shell, power)
	}
	powerHandle := nativeProcessHandle(t, power.PID)
	marker := "GROUP_POWERSHELL_中文_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	command := "& " + nativeEncodedCommand(powershell, "[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false); [Console]::WriteLine('"+marker+"')")
	if strings.Contains(command, marker) {
		t.Fatal("PowerShell execution proof marker appears in submitted input")
	}
	nativeCall(t, fixture.pipe, terminalhost.Request{Op: "send", SessionID: power.ID, RunID: power.RunID, Data: []byte(command)})
	nativeAwait(t, "real grouped PowerShell executed input", func() (bool, string) {
		response := nativeCall(t, fixture.pipe, terminalhost.Request{Op: "capture", SessionID: power.ID, RunID: power.RunID})
		return response.PID == power.PID && strings.Contains(response.Screen, marker), response.Screen
	})
	for _, info := range []terminalhost.SessionInfo{shell, power} {
		record, err := fixture.store.Get(info.ID)
		if err != nil || record.Group != info.Group || record.Tool != info.Tool || !strings.EqualFold(record.Launch.Dir, info.Dir) {
			t.Fatalf("group/tool metadata disagrees with durable descriptor: info=%+v record=%+v err=%v", info, record, err)
		}
	}
	nativeGroupFrame(t, ui, "both real grouped sessions appear in current list",
		[]string{"2/2 个", "分组：全部分组", shellName, powerName, "PowerShell"}, nil)
	// matrix/hardened: default is an explicit filter even when no descriptor
	// uses it. Assertions reject excluded names in the current emulator frame.
	nativeWrite(t, ui.viewer, "\t")
	nativeGroupFrame(t, ui, "Tab filters empty default group",
		[]string{"0/2 个", "分组：默认分组", "没有匹配的会话"}, []string{shellName, powerName})
	nativeWrite(t, ui.viewer, "\t")
	nativeGroupFrame(t, ui, "Tab filters research group",
		[]string{"1/2 个", "分组：研发", "› " + shellName}, []string{powerName})
	nativeWrite(t, ui.viewer, "\t")
	nativeGroupFrame(t, ui, "Tab filters operations group",
		[]string{"1/2 个", "分组：运维", "› " + powerName}, []string{shellName})
	nativeWrite(t, ui.viewer, "\t")
	nativeGroupFrame(t, ui, "Tab returns to all groups",
		[]string{"2/2 个", "分组：全部分组", shellName, powerName}, nil)
	nativeGroupSearch(t, ui, powerName, powerName, shellName)
	nativeGroupSearch(t, ui, "dir-match", shellName, powerName)
	nativeGroupSearch(t, ui, "运维", powerName, shellName)
	// Enter retains a live query, while g edits the selected original run.
	nativeWrite(t, ui.viewer, "/"+shellName+"\r")
	nativeGroupFrame(t, ui, "retained name search selects original Shell",
		[]string{"1/2 个", "› " + shellName, "搜索：" + shellName}, []string{powerName, "Enter 保留"})
	nativeWrite(t, ui.viewer, "g")
	nativeFleetFrame(t, ui, "move group form targets searched Shell", "移动会话分组", shellName, "› 分组：", "研发")
	nativeWrite(t, ui.viewer, "\x01\x0b另组\r")
	nativeGroupFrame(t, ui, "saving group clears search and returns selected original session",
		[]string{"2/2 个", "分组：全部分组", "› " + shellName, "另组", "会话分组已保存；当前进程继续运行。"}, nil)
	moved := nativeFleetFind(t, fixture.pipe, shellName, "running")
	nativeFleetSameRun(t, shell, moved)
	if moved.Group != "另组" {
		t.Fatalf("UI group change missing from independent readback: %+v", moved)
	}
	record, err := fixture.store.Get(shell.ID)
	if err != nil || record.Group != "另组" {
		t.Fatalf("UI group change not durably saved: record=%+v err=%v", record, err)
	}
	nativeFleetSameRun(t, power, nativeFleetFind(t, fixture.pipe, powerName, "running"))
	nativeAssertProcess(t, shellHandle, true)
	nativeAssertProcess(t, powerHandle, true)
	nativeWrite(t, ui.viewer, "q")
	nativeWaitExit(t, ui.viewer.process, 0)
	nativeAssertProcess(t, shellHandle, true)
	nativeAssertProcess(t, powerHandle, true)
	reopened := nativeStartFleetUI(t, cli, fixture.pipe, stateDir, projectRoot)
	nativeFleetFrame(t, reopened, "new UI reads durable group changes", "2/2 个", "另组", "运维", shellName, powerName)
	nativeWrite(t, reopened.viewer, "/"+shellName+"\r")
	nativeGroupFrame(t, reopened, "search selection binds subsequent stop",
		[]string{"1/2 个", "› " + shellName}, []string{powerName})
	nativeWrite(t, reopened.viewer, "s")
	nativeFleetFrame(t, reopened, "stop confirmation targets searched grouped run", "停止此会话？", shellName)
	nativeWrite(t, reopened.viewer, "\r")
	nativeFleetFrame(t, reopened, "UI stop completes exact Shell run", "会话已停止。", "已停止")
	stopped := nativeFleetFind(t, fixture.pipe, shellName, "stopped")
	if stopped.ID != shell.ID || stopped.RunID != shell.RunID || stopped.Group != "另组" {
		t.Fatalf("grouped stop targeted wrong descriptor or run: %+v", stopped)
	}
	nativeAssertProcess(t, shellHandle, false)
	nativeAssertProcess(t, powerHandle, true)
	nativeFleetSameRun(t, power, nativeFleetFind(t, fixture.pipe, powerName, "running"))
	nativeWrite(t, reopened.viewer, "d")
	nativeFleetFrame(t, reopened, "remove confirmation keeps exact searched Shell target", "移除此会话？", shellName)
	nativeWrite(t, reopened.viewer, "\r")
	nativeFleetFrame(t, reopened, "descriptor-only Shell removal completed", "会话已从列表移除；项目文件保留。")
	if _, err := fixture.store.Get(shell.ID); !errors.Is(err, nativeregistry.ErrNotFound) {
		t.Fatalf("removed grouped Shell descriptor still exists: %v", err)
	}
	nativeAssertProcess(t, powerHandle, true)
	nativeWrite(t, reopened.viewer, "\x1b")
	nativeGroupFrame(t, reopened, "clear retained search after Shell removal",
		[]string{"1/1 个", "分组：全部分组", powerName}, []string{shellName})
	nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-remove", SessionID: power.ID, RunID: power.RunID})
	nativeAssertProcess(t, powerHandle, false)
	nativeAwait(t, "current UI reads both exact removals", func() (bool, string) {
		screen := reopened.screen()
		return strings.Contains(screen, "0/0 个") && !strings.Contains(screen, shellName) && !strings.Contains(screen, powerName), screen
	})
	if response := nativeCall(t, fixture.pipe, terminalhost.Request{Op: "fleet-list"}); len(response.Sessions) != 0 {
		t.Fatalf("grouped exact cleanup left descriptors: %+v", response)
	}
	for _, dir := range []string{shellDir, powerDir} {
		nativeFleetProjectUnchanged(t, filepath.Join(dir, "keep.txt"), sentinel)
	}
	nativeWrite(t, reopened.viewer, "q")
	nativeWaitExit(t, reopened.viewer.process, 0)
	t.Log(fmt.Sprintf("headless real Windows group/search workflow verified Shell PID %d and PowerShell PID %d; current frames, UUID/RunID identity, group move without restart, separate cleanup and preserved project files; no visible desktop assertion", shell.PID, power.PID))
}
