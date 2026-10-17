// matrix/hardened: an explicit native Codex selection applies only to new sessions.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func validatedCodexExecutable(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("Codex 程序必须使用绝对路径")
	}
	name := filepath.Base(path)
	if !strings.EqualFold(name, "codex.exe") && (runtime.GOOS == "windows" || name != "codex") {
		return "", errors.New("请选择 Codex 原生程序 codex.exe")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("Codex 原生程序不存在或无法读取")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("Codex 原生程序必须是常规文件")
	}
	return filepath.Clean(path), nil
}
