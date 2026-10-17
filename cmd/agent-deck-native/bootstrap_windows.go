//go:build windows

package main

// matrix/hardened: the local WMI provider owns the detached manager, outside the
// caller's console/job. No persistent service, task, hook or timer is installed.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"golang.org/x/sys/windows"
)

func ensureNativeManager(ctx context.Context, config options) (terminalhost.Response, error) {
	return ensureNativeManagerWith(ctx, config, nativeBootstrapDeps{
		probe: func(ctx context.Context, pipe string) (terminalhost.Response, error) {
			return callHost(ctx, pipe, terminalhost.Request{Op: "fleet-status"})
		},
		spawn:  spawnNativeManager,
		absent: func(err error) bool { return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) },
	})
}

type nativeBrokerRequest struct {
	CommandLine      string   `json:"command_line"`
	WorkingDirectory string   `json:"working_directory"`
	Environment      []string `json:"environment"`
}

type nativeBrokerResponse struct {
	ReturnCode uint32 `json:"return_code"`
	ProcessID  uint32 `json:"process_id"`
}

// The encoded helper is fixed code. The inherited environment is supplied only
// through stdin, never through argv, logs or PowerShell interpolation.
const nativeBrokerScript = `$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
try {
  $encoded = [Console]::In.ReadToEnd()
  $data = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) | ConvertFrom-Json
  $startup = New-CimInstance -Namespace root/cimv2 -ClassName Win32_ProcessStartup -ClientOnly -Property @{
    CreateFlags = [uint32]0x01000408
    ShowWindow = [uint16]0
    EnvironmentVariables = [string[]]$data.environment
  }
  $result = Invoke-CimMethod -Namespace root/cimv2 -ClassName Win32_Process -MethodName Create -Arguments @{
    CommandLine = [string]$data.command_line
    CurrentDirectory = [string]$data.working_directory
    ProcessStartupInformation = $startup
  }
  [Console]::Out.WriteLine((@{return_code = [uint32]$result.ReturnValue; process_id = [uint32]$result.ProcessId} | ConvertTo-Json -Compress))
  exit 0
} catch {
  [Console]::Error.WriteLine('Unable to create the private native manager.')
  exit 1
}`

func spawnNativeManager(ctx context.Context, config options) (uint32, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, errors.New("无法读取当前原生程序路径")
	}
	executable, err = filepath.Abs(executable)
	if err != nil || !strings.EqualFold(filepath.Ext(executable), ".exe") {
		return 0, errors.New("后台启动要求有效的原生 .exe 路径")
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return 0, errors.New("当前原生程序不是可读取的普通文件")
	}
	args := []string{executable, "serve", "--background", "--state-dir", config.StateDir, "--pipe", config.Pipe}
	// matrix/hardened: the detached child must open the exact selected store.
	if config.Store == "statedb" {
		args = append(args, "--store", "statedb")
	}
	escaped := make([]string, len(args))
	for index, arg := range args {
		if strings.ContainsRune(arg, '\x00') {
			return 0, errors.New("后台启动参数含有无效字符")
		}
		escaped[index] = syscall.EscapeArg(arg)
	}
	commandLine := strings.Join(escaped, " ")
	if len(utf16.Encode([]rune(commandLine))) >= 32767 {
		return 0, errors.New("后台启动命令超过 Windows 长度限制")
	}
	payload, err := json.Marshal(nativeBrokerRequest{CommandLine: commandLine, WorkingDirectory: filepath.Dir(executable), Environment: os.Environ()})
	if err != nil {
		return 0, errors.New("无法准备后台启动参数")
	}
	systemRoot := os.Getenv("SystemRoot")
	if !filepath.IsAbs(systemRoot) {
		return 0, errors.New("找不到 Windows 系统 PowerShell")
	}
	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	info, err = os.Stat(powershell)
	if err != nil || !info.Mode().IsRegular() {
		return 0, errors.New("找不到 Windows 系统 PowerShell")
	}
	cmd := exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", nativeUTF16Base64(nativeBrokerScript))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(payload))
	var output nativeBrokerOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("Windows 本地进程代理未完成启动请求")
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	var result nativeBrokerResponse
	if err := decoder.Decode(&result); err != nil {
		return 0, errors.New("Windows 本地进程代理返回了无效的启动回执")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return 0, errors.New("Windows 本地进程代理返回了多余的数据")
	}
	if result.ReturnCode != 0 {
		return 0, fmt.Errorf("Windows 本地进程代理拒绝创建后台，返回码 %d", result.ReturnCode)
	}
	if result.ProcessID == 0 {
		return 0, errors.New("Windows 本地进程代理未返回后台 PID")
	}
	return result.ProcessID, nil
}

type nativeBrokerOutput struct{ bytes.Buffer }

func (output *nativeBrokerOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > 4096 {
		return 0, errors.New("native broker response exceeds limit")
	}
	return output.Buffer.Write(data)
}

func nativeUTF16Base64(text string) string {
	units := utf16.Encode([]rune(text))
	data := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(data[index*2:], unit)
	}
	return base64.StdEncoding.EncodeToString(data)
}
