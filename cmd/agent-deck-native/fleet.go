// matrix/hardened: native registry/runtime integration stays separate from tmux.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/nativefleet"
	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/nativeui"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"golang.org/x/term"
)

func runFleet(config options, stdout, stderr io.Writer) error {
	if config.Command == "serve" {
		if config.Background {
			stdout = io.Discard
		}
		return runFleetServer(config, stdout)
	}
	if config.Command == "ui" || config.Command == "manager-start" {
		if config.Command == "ui" && (!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))) {
			return errors.New("请在交互式 Windows 终端中打开列表；后台启动可使用 manager-start")
		}
		manager, err := ensureNativeManager(context.Background(), config)
		if err != nil {
			return err
		}
		if config.Command == "manager-start" {
			if config.JSON {
				return json.NewEncoder(stdout).Encode(manager)
			}
			_, err := fmt.Fprintf(stdout, "管理后台已就绪：PID %d，注册库 %s\n", manager.HostPID, manager.ManagerStateDir)
			return err
		}
		return nativeui.RunWithOptions(&nativeService{pipe: config.Pipe, store: config.Store, codexExecutable: config.Executable}, config.Cwd, nativeui.Options{DefaultTool: config.Tool})
	}
	// matrix/hardened: every managed command verifies its selected store and
	// namespace before a read or mutation, including an explicitly named pipe.
	manager, err := callHost(context.Background(), config.Pipe, terminalhost.Request{Op: "fleet-status"})
	if err != nil {
		return err
	}
	if err := validateNativeManager(config, manager); err != nil {
		return err
	}
	request := terminalhost.Request{SessionID: config.SessionID, RunID: config.RunID, Name: config.Name, Tool: config.Tool, Group: config.Group}
	switch config.Command {
	case "list":
		request.Op = "fleet-list"
	case "create":
		launch, err := launchSpec(config.Tool, config.Cwd, config.Executable, config.Args, config.Env, config.Cols, config.Rows)
		if err != nil {
			return err
		}
		request.Op, request.Launch, request.AutoStart = "fleet-create", &launch, !config.NoStart
	case "start", "resume", "rename", "move", "remove":
		request.Op = "fleet-" + config.Command
		if request.RunID == "" {
			info, err := lookupSession(context.Background(), config.Pipe, config.SessionID)
			if err != nil {
				return err
			}
			request.RunID = info.RunID
		}
	case "shutdown":
		request.Op = "fleet-shutdown"
	default:
		return errors.New("unsupported managed command")
	}
	response, err := callHost(context.Background(), config.Pipe, request)
	if err != nil {
		// matrix/hardened: a saved descriptor survives failed auto-start; expose
		// its identity so a retry can start it instead of creating a duplicate.
		if config.Command == "create" && len(response.Sessions) == 1 {
			if config.JSON {
				if writeErr := json.NewEncoder(stdout).Encode(response); writeErr != nil {
					return errors.Join(err, writeErr)
				}
			} else {
				info := response.Sessions[0]
				fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", info.ID, info.Name, info.Tool, info.State, info.Dir)
			}
			return fmt.Errorf("会话已创建，启动失败；可用 start --session %s 重试：%w", response.Sessions[0].ID, err)
		}
		return err
	}
	if config.JSON {
		return json.NewEncoder(stdout).Encode(response)
	}
	if len(response.Sessions) > 0 {
		for _, info := range response.Sessions {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\n", info.ID, info.Name, info.Tool, info.State, info.Group, info.Dir)
		}
		return nil
	}
	_, err = fmt.Fprintln(stdout, config.Command+": completed")
	return err
}

func runFleetServer(config options, stdout io.Writer) (resultErr error) {
	listener, err := winipc.Listen(config.Pipe)
	if err != nil {
		return err
	}
	defer listener.Close()
	var store nativefleet.Registry
	// matrix/hardened: one selected store is authoritative; never synchronize
	// production and prototype databases or implicitly migrate either one.
	if config.Store == "statedb" {
		store, err = nativestatestore.Open(config.StateDir)
	} else if config.Store == "" || config.Store == "prototype" {
		store, err = nativeregistry.Open(config.StateDir)
	} else {
		return errors.New("unsupported native session store")
	}
	if err != nil {
		return err
	}
	server, err := nativefleet.New(store, func(launch terminalhost.LaunchSpec) (terminalhost.Process, error) {
		process, err := conpty.Start(conpty.Options{Executable: launch.Executable, Args: launch.Args, Env: mergedEnv(os.Environ(), launch.Env), Dir: launch.Dir, Cols: launch.Cols, Rows: launch.Rows})
		if err != nil {
			return nil, err
		}
		return process, nil
	}, nativefleet.Identity{StateDir: config.StateDir, Version: nativeManagerVersion(config)})
	if err != nil {
		_ = store.Close()
		return err
	}
	// matrix/hardened: only the manager may release its consumed store;
	// uncertain process cleanup keeps registry ownership fenced until exit.
	defer func() { resultErr = errors.Join(resultErr, server.Close()) }()
	if err := json.NewEncoder(stdout).Encode(struct {
		Ready   bool   `json:"ready"`
		Pipe    string `json:"pipe"`
		HostPID int    `json:"host_pid"`
	}{true, config.Pipe, os.Getpid()}); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	return server.Serve(listener)
}

func launchSpec(tool, cwd, executable string, args, env []string, cols, rows int) (terminalhost.LaunchSpec, error) {
	if executable == "" {
		switch tool {
		case "shell":
			if runtime.GOOS == "windows" {
				executable = os.Getenv("COMSPEC")
				if executable == "" {
					executable = "cmd.exe"
				}
				args = []string{"/d", "/q"}
			} else {
				executable = "/bin/sh"
			}
		case "powershell":
			if runtime.GOOS != "windows" {
				return terminalhost.LaunchSpec{}, errors.New("PowerShell 工具需要 Windows 原生环境")
			}
			executable = filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
			args = []string{"-NoLogo", "-NoProfile"}
		case "codex":
			if path, err := exec.LookPath("codex.exe"); err == nil {
				executable = path
			} else if runtime.GOOS == "windows" {
				candidates := []string{filepath.Join(os.Getenv("APPDATA"), "npm", "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe")}
				for _, candidate := range candidates {
					if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
						executable = candidate
						break
					}
				}
			} else {
				executable = "codex"
			}
			if executable == "" {
				return terminalhost.LaunchSpec{}, errors.New("找不到 Codex 原生程序；请安装 Codex 或通过 create 指定程序路径")
			}
		default:
			return terminalhost.LaunchSpec{}, errors.New("custom tool requires an explicit executable")
		}
	}
	launch := terminalhost.LaunchSpec{Executable: executable, Args: args, Env: env, Dir: cwd, Cols: cols, Rows: rows}
	return launch, terminalhost.ValidateLaunchSpec(launch)
}

func lookupSession(ctx context.Context, pipe, id string) (terminalhost.SessionInfo, error) {
	response, err := callHost(ctx, pipe, terminalhost.Request{Op: "fleet-list"})
	if err != nil {
		return terminalhost.SessionInfo{}, err
	}
	for _, info := range response.Sessions {
		if info.ID == id {
			return info, nil
		}
	}
	return terminalhost.SessionInfo{}, errors.New("找不到所选会话")
}

// matrix/hardened: executable preferences are per window and affect new Codex
// records only; reconnect/start/resume keep each existing launch description.
type nativeService struct{ pipe, store, codexExecutable string }

// matrix/hardened: translate the original schema's default group at the UI
// boundary only; persisted records and CLI responses retain their original ID.
func (s *nativeService) uiSession(info terminalhost.SessionInfo) terminalhost.SessionInfo {
	if s.store == "statedb" && info.Group == nativestatestore.DefaultGroup {
		info.Group = ""
	}
	return info
}

func (s *nativeService) List(ctx context.Context) ([]terminalhost.SessionInfo, error) {
	r, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-list"})
	for i := range r.Sessions {
		r.Sessions[i] = s.uiSession(r.Sessions[i])
	}
	return r.Sessions, err
}
func (s *nativeService) Create(ctx context.Context, name, tool, cwd string) (terminalhost.SessionInfo, error) {
	return s.CreateInGroup(ctx, name, tool, cwd, "")
}
func (s *nativeService) CreateInGroup(ctx context.Context, name, tool, cwd, group string) (terminalhost.SessionInfo, error) {
	executable := ""
	if tool == "codex" {
		executable = s.codexExecutable
	}
	launch, err := launchSpec(tool, cwd, executable, nil, nil, 120, 30)
	if err != nil {
		return terminalhost.SessionInfo{}, err
	}
	r, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-create", Name: name, Tool: tool, Group: group, Launch: &launch, AutoStart: true})
	if err != nil {
		if len(r.Sessions) == 1 {
			return s.uiSession(r.Sessions[0]), err
		}
		return terminalhost.SessionInfo{}, err
	}
	info, err := oneSession(r)
	return s.uiSession(info), err
}
func (s *nativeService) Start(ctx context.Context, id string) (terminalhost.SessionInfo, error) {
	info, err := lookupSession(ctx, s.pipe, id)
	if err != nil {
		return info, err
	}
	r, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-start", SessionID: id, RunID: info.RunID})
	if err != nil {
		return terminalhost.SessionInfo{}, err
	}
	started, err := oneSession(r)
	return s.uiSession(started), err
}

// matrix/hardened: never replace a stale viewer's selected generation with a
// fresh lookup; fleet-resume must reject a run started by another window.
func (s *nativeService) Resume(ctx context.Context, id, runID string) (terminalhost.SessionInfo, error) {
	r, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-resume", SessionID: id, RunID: runID})
	if err != nil {
		return terminalhost.SessionInfo{}, err
	}
	resumed, err := oneSession(r)
	return s.uiSession(resumed), err
}
func (s *nativeService) Stop(ctx context.Context, id, runID string) error {
	_, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-stop", SessionID: id, RunID: runID})
	return err
}
func (s *nativeService) Rename(ctx context.Context, id, name string) error {
	info, err := lookupSession(ctx, s.pipe, id)
	if err != nil {
		return err
	}
	_, err = callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-rename", SessionID: id, RunID: info.RunID, Name: name})
	return err
}
func (s *nativeService) Remove(ctx context.Context, id, runID string) error {
	_, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-remove", SessionID: id, RunID: runID})
	return err
}
func (s *nativeService) Move(ctx context.Context, id, runID, group string) error {
	_, err := callHost(ctx, s.pipe, terminalhost.Request{Op: "fleet-move", SessionID: id, RunID: runID, Group: group})
	return err
}
func (s *nativeService) AttachCommand(info terminalhost.SessionInfo) (*exec.Cmd, error) {
	if info.ID == "" || info.RunID == "" {
		return nil, errors.New("会话尚未启动")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(exe, "attach", "--pipe", s.pipe, "--session", info.ID, "--run-id", info.RunID)
	// matrix/hardened: Bubble Tea's cursor-aware output is a writer wrapper.
	// The attach child must inherit real console files rather than exec pipes,
	// so terminal detection and raw input remain available after UI release.
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command, nil
}
func oneSession(response terminalhost.Response) (terminalhost.SessionInfo, error) {
	if len(response.Sessions) != 1 {
		return terminalhost.SessionInfo{}, errors.New("管理服务返回了不完整的会话身份")
	}
	return response.Sessions[0], nil
}
