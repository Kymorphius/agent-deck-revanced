//go:build windows

package main

// matrix/hardened: opt-in acceptance exercises the real PowerShell launcher,
// native executable and detached manager using only GUID-private fixtures. It
// opens the Codex create dialog without creating an AI process or reading auth.

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

func nativeLauncherFixture(t *testing.T, source, destination string) string {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatalf("open explicitly supplied launcher fixture: %v", err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("create owned launcher fixture: %v", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy owned launcher fixture: copy=%v close=%v", copyErr, closeErr)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestWindowsNativeLauncherChain(t *testing.T) {
	cli := nativeBootstrapCLI(t)
	launcher := os.Getenv("AGENTDECK_NATIVE_LAUNCHER")
	if launcher == "" {
		t.Skip("set AGENTDECK_NATIVE_LAUNCHER to the explicitly uploaded windows-native-launch.ps1")
	}
	canonical, err := filepath.EvalSymlinks(launcher)
	if !filepath.IsAbs(launcher) || err != nil || !strings.EqualFold(filepath.Clean(launcher), canonical) {
		t.Fatal("AGENTDECK_NATIVE_LAUNCHER must be an existing absolute canonical path without links")
	}
	if info, err := os.Lstat(launcher); err != nil || !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(launcher), ".ps1") {
		t.Fatal("AGENTDECK_NATIVE_LAUNCHER must identify a regular PowerShell script")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// All these characters are allowed by Windows and StateDB paths. The store
	// deliberately rejects ?, # and %, so those are excluded from this fixture.
	root := filepath.Join(parent, "启动 中文 空格;单引号'"+uuid.NewString()[:8])
	project := filepath.Join(root, "项目 中文 空格;单引号'")
	stateDir := filepath.Join(root, "状态 中文 空格;单引号'")
	for _, directory := range []string{root, project, stateDir} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixtureCLI := filepath.Join(root, "程序 中文 空格;单引号'.exe")
	fixtureLauncher := filepath.Join(root, "入口 中文 空格;单引号'.ps1")
	expectedHash := nativeLauncherFixture(t, cli, fixtureCLI)
	expectedLauncherHash := nativeLauncherFixture(t, launcher, fixtureLauncher)
	config, err := parseOptions([]string{"ui", "--store", "statedb", "--tool", "codex", "--state-dir", stateDir, "--cwd", project}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var identity terminalhost.Response
	nativeBootstrapCleanup(t, config, &identity)
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	start := func() *nativeFleetUI {
		t.Helper()
		process, err := conpty.Start(conpty.Options{
			Executable: powershell,
			Args: []string{"-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", fixtureLauncher,
				"-BinaryPath", fixtureCLI, "-ExpectedSHA256", expectedHash,
				"-ExpectedLauncherSHA256", expectedLauncherHash,
				"-StateDirectory", stateDir, "-WorkingDirectory", project, "-Console"},
			Env: os.Environ(), Dir: project, Cols: 240, Rows: 45,
		})
		if err != nil {
			t.Fatalf("start actual Windows PowerShell launcher: %v", err)
		}
		frame, err := terminalhost.New(process, 240, 45)
		if err != nil {
			_ = process.Close()
			t.Fatalf("decode actual launcher console: %v", err)
		}
		t.Cleanup(func() { nativeClose(t, "PowerShell launcher console", frame.Close) })
		return &nativeFleetUI{viewer: &nativeViewer{process: process}, frame: frame}
	}
	ui := start()
	nativeFleetFrame(t, ui, "PowerShell launcher reaches native empty list with Codex default", "Windows 原生会话", "没有会话", "Enter 直接进入 Codex", project, "Ctrl+Q 返回列表")
	identity = nativeBootstrapIdentity(t, config)
	if identity.HostPID == os.Getpid() || uint32(identity.HostPID) == ui.viewer.process.PID() || len(identity.Sessions) != 0 {
		t.Fatalf("launcher did not create a separate empty background manager: %+v", identity)
	}
	managerHandle := nativeProcessHandle(t, uint32(identity.HostPID))
	fixturePath, err := windows.UTF16PtrFromString(fixtureCLI)
	if err != nil {
		t.Fatal(err)
	}
	longPath := make([]uint16, 32768)
	longSize, err := windows.GetLongPathName(fixturePath, &longPath[0], uint32(len(longPath)))
	if err != nil || longSize == 0 || longSize >= uint32(len(longPath)) {
		t.Fatalf("resolve fixture binary's Windows long path: size=%d error=%v", longSize, err)
	}
	fixtureImage := windows.UTF16ToString(longPath[:longSize])
	image := make([]uint16, 32768)
	size := uint32(len(image))
	if err := windows.QueryFullProcessImageName(managerHandle, 0, &image[0], &size); err != nil || !strings.EqualFold(windows.UTF16ToString(image[:size]), fixtureImage) {
		t.Fatalf("background manager did not load the SHA-verified special-path binary: %v", err)
	}
	nativeWrite(t, ui.viewer, "n")
	nativeFleetFrame(t, ui, "launcher pre-fills native Codex creation without starting a model", "新建会话", filepath.Base(project), project, "[Codex]", "按 Enter 创建并进入 Codex", "留空为默认分组")
	nativeWrite(t, ui.viewer, "\x1b")
	nativeFleetFrame(t, ui, "Esc cancels launcher-default Codex form", "Windows 原生会话", "没有会话", project)
	nativeWrite(t, ui.viewer, "q")
	nativeWaitExit(t, ui.viewer.process, 0)
	nativeAssertProcess(t, managerHandle, true)
	after := nativeBootstrapIdentity(t, config)
	if after.HostPID != identity.HostPID || after.RunID != identity.RunID || len(after.Sessions) != 0 {
		t.Fatalf("quitting PowerShell launcher changed manager or created a model: before=%+v after=%+v", identity, after)
	}
	db, err := statedb.OpenReadOnlyLive(filepath.Join(stateDir, nativestatestore.DatabaseFilename))
	if err != nil {
		t.Fatalf("launcher did not persist the selected original StateDB: %v", err)
	}
	rows, loadErr := db.LoadInstances()
	closeErr := db.Close()
	if loadErr != nil || closeErr != nil || len(rows) != 0 {
		t.Fatalf("launcher create-dialog cancellation persisted an unexpected instance: rows=%d load=%v close=%v", len(rows), loadErr, closeErr)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "registry.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("launcher also wrote the prototype registry: %v", err)
	}
	second := start()
	nativeFleetFrame(t, second, "second PowerShell launcher reuses persisted native namespace", "Windows 原生会话", "Enter 直接进入 Codex", project)
	reconnected := nativeBootstrapIdentity(t, config)
	if reconnected.HostPID != identity.HostPID || reconnected.RunID != identity.RunID {
		t.Fatalf("fresh PowerShell launcher replaced the existing manager: before=%+v after=%+v", identity, reconnected)
	}
	nativeWrite(t, second.viewer, "q")
	nativeWaitExit(t, second.viewer.process, 0)
	nativeAssertProcess(t, managerHandle, true)
	t.Log("real PowerShell → verified native EXE → Codex-default dialog → clean UI exit → same detached StateDB manager verified with Chinese, spaces, semicolon and apostrophe paths; no AI process or desktop shortcut was created")
}
