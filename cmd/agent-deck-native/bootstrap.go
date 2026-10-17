package main

// matrix/hardened: startup only owns an explicitly absent private manager;
// existing endpoints must identify this namespace and build before reuse.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

const nativeManagerProtocol = 2
const nativeManagerStartupTimeout = 15 * time.Second

// matrix/hardened: a caller cannot reuse a same-directory manager backed by
// another store, including when the user supplies an explicit pipe name.
func nativeManagerVersion(config options) string {
	if config.Store == "statedb" {
		return version + "/statedb"
	}
	return version
}

type nativeBootstrapDeps struct {
	probe  func(context.Context, string) (terminalhost.Response, error)
	spawn  func(context.Context, options) (uint32, error)
	absent func(error) bool
}

func ensureNativeManagerWith(ctx context.Context, config options, deps nativeBootstrapDeps) (terminalhost.Response, error) {
	// matrix/hardened: an unrecognized store cannot authorize any bootstrap.
	if config.Store != "" && config.Store != "prototype" && config.Store != "statedb" {
		return terminalhost.Response{}, errors.New("unsupported native session store")
	}
	if err := ctx.Err(); err != nil {
		return terminalhost.Response{}, err
	}
	if err := validatePipe(config.Pipe); err != nil {
		return terminalhost.Response{}, err
	}
	if deps.probe == nil || deps.spawn == nil || deps.absent == nil {
		return terminalhost.Response{}, errors.New("后台启动依赖不完整")
	}
	if !filepath.IsAbs(config.StateDir) || strings.ContainsRune(config.StateDir, '\x00') {
		return terminalhost.Response{}, errors.New("后台管理目录必须是有效的绝对路径")
	}
	stateDir, err := filepath.Abs(config.StateDir)
	if err != nil {
		return terminalhost.Response{}, errors.New("无法确定后台管理目录的绝对路径")
	}
	config.StateDir = filepath.Clean(stateDir)
	ctx, cancel := context.WithTimeout(ctx, nativeManagerStartupTimeout)
	defer cancel()
	response, err := deps.probe(ctx, config.Pipe)
	if err == nil {
		return response, validateNativeManager(config, response)
	}
	// An inaccessible, busy, timed-out or malformed endpoint may belong to an
	// existing manager. None of these conditions authorizes another launch.
	if !deps.absent(err) {
		return terminalhost.Response{}, fmt.Errorf("无法确认现有后台管理进程的身份，未启动新进程：%w", err)
	}
	if err := ctx.Err(); err != nil {
		return terminalhost.Response{}, err
	}
	pid, spawnErr := deps.spawn(ctx, config)
	if spawnErr != nil {
		// Another simultaneous client can win even if this launch failed.
		// Reuse only its complete handshake; never stop it using our PID.
		if err := ctx.Err(); err != nil {
			return terminalhost.Response{}, err
		}
		response, probeErr := deps.probe(ctx, config.Pipe)
		if probeErr == nil {
			return response, validateNativeManager(config, response)
		}
		if !deps.absent(probeErr) {
			return terminalhost.Response{}, fmt.Errorf("后台启动失败，现有端点的身份也未通过检查：%w", probeErr)
		}
		return terminalhost.Response{}, fmt.Errorf("无法创建独立的后台管理进程：%w", spawnErr)
	}
	for {
		if err := ctx.Err(); err != nil {
			return terminalhost.Response{}, fmt.Errorf("后台进程 PID %d 未完成启动握手：%w", pid, err)
		}
		response, err = deps.probe(ctx, config.Pipe)
		if err == nil {
			return response, validateNativeManager(config, response)
		}
		if !deps.absent(err) {
			return terminalhost.Response{}, fmt.Errorf("后台进程 PID %d 的启动握手失败：%w", pid, err)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return terminalhost.Response{}, fmt.Errorf("后台进程 PID %d 未完成启动握手：%w", pid, ctx.Err())
		case <-timer.C:
		}
	}
}

func validateNativeManager(config options, response terminalhost.Response) error {
	if !response.OK || response.HostPID <= 0 {
		return errors.New("现有后台未返回有效的管理进程身份；请检查对应的 serve 进程")
	}
	runID, err := uuid.Parse(response.RunID)
	if err != nil || runID == uuid.Nil || runID.String() != response.RunID {
		return errors.New("现有后台未返回有效的运行标识；请检查对应的 serve 进程")
	}
	if response.ProtocolVersion != nativeManagerProtocol || response.ManagerVersion != nativeManagerVersion(config) {
		return errors.New("现有后台的版本与当前程序不兼容；请先使用原版本显式关闭对应后台，再重新启动")
	}
	if !filepath.IsAbs(response.ManagerStateDir) || strings.ContainsRune(response.ManagerStateDir, '\x00') {
		return errors.New("现有后台未返回有效的注册库目录；未创建或替换后台")
	}
	want, got := filepath.Clean(config.StateDir), filepath.Clean(response.ManagerStateDir)
	if runtime.GOOS == "windows" {
		want, got = strings.ToLower(want), strings.ToLower(got)
	}
	if want != got {
		return errors.New("该管道属于另一个注册库目录；请使用匹配的 --state-dir 和 --pipe")
	}
	return nil
}
