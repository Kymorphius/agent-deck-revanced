package aiupdate

// matrix/hardened: reject unknown CLI/updater lifecycle implementations before selecting a package.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type updaterRecord struct {
	PID   int    `json:"pid"`
	Start string `json:"processStartTime"`
}

func (m *Manager) verifyNativeRuntime(ctx context.Context) error {
	cli, err := exec.LookPath(m.binary)
	if err != nil {
		return err
	}
	cli, err = filepath.EvalSymlinks(cli)
	if err != nil {
		return err
	}
	cliPackage := filepath.Dir(filepath.Dir(cli))
	if filepath.Base(cli) != "codex" || filepath.Base(filepath.Dir(cli)) != "bin" || validatePackage(cliPackage, "0.163.0", m.target) != nil {
		return errors.New("deferred install requires a verified complete Codex 0.163.0 CLI package; custom wrappers and unknown lifecycle versions are unsupported")
	}
	path := filepath.Join(m.home, "app-server-daemon", "daemon-updater.pid")
	before, err := readRegularFile(path, 1<<20)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var record updaterRecord
	if json.Unmarshal(before, &record) != nil || record.PID <= 0 || record.Start == "" {
		return errors.New("native updater identity is unknown; refusing daemon package selection")
	}
	first, executable, err := inspectUpdaterProcess(ctx, record.PID)
	if err != nil {
		return fmt.Errorf("cannot verify native updater: %w", err)
	}
	if first != strings.Join(strings.Fields(record.Start), " ") {
		return errors.New("native updater process identity differs from its record")
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil || canonical != executable || filepath.Dir(filepath.Dir(filepath.Dir(executable))) != filepath.Join(m.packageRoot(), "releases") {
		return errors.New("native updater executable is outside its immutable daemon release")
	}
	packageDir := filepath.Dir(filepath.Dir(executable))
	if filepath.Base(executable) != "codex" || filepath.Base(filepath.Dir(executable)) != "bin" || validatePackage(packageDir, "0.163.0", m.target) != nil {
		return errors.New("native updater is not the audited Codex 0.163.0 package")
	}
	output, err := m.run(ctx, executable, m.home, "--version")
	if err != nil {
		return err
	}
	version, err := parseVersion(output)
	if err != nil || version != "0.163.0" {
		return errors.New("native updater executable version is unsupported")
	}
	second, secondExecutable, err := inspectUpdaterProcess(ctx, record.PID)
	if err != nil || second != first || secondExecutable != executable {
		return errors.New("native updater process changed during verification")
	}
	after, err := readRegularFile(path, 1<<20)
	if err != nil || !bytes.Equal(before, after) {
		return errors.New("native updater record changed during verification")
	}
	return nil
}

func inspectUpdaterProcess(ctx context.Context, pid int) (string, string, error) {
	pidText := strconv.Itoa(pid)
	// ps uses the same legacy start timestamp recorded by the audited native daemon.
	output, err := exec.CommandContext(ctx, "/bin/ps", "-p", pidText, "-o", "stat=", "-o", "lstart=").Output()
	if err != nil {
		return "", "", errors.New("recorded updater is unavailable or inaccessible")
	}
	fields := strings.Fields(string(output))
	if len(fields) != 6 || strings.ContainsAny(fields[0], "ZX") {
		return "", "", errors.New("recorded updater is not a live identifiable process")
	}
	var executable string
	if runtime.GOOS == "linux" {
		executable, err = os.Readlink(filepath.Join("/proc", pidText, "exe"))
	} else if runtime.GOOS == "darwin" {
		output, err = exec.CommandContext(ctx, "/bin/ps", "-p", pidText, "-o", "comm=").Output()
		executable = strings.TrimSpace(string(output))
	} else {
		err = errors.New("native updater identity verification is unsupported on this platform")
	}
	if err != nil || !filepath.IsAbs(executable) {
		return "", "", errors.New("native updater executable path is unknown")
	}
	return strings.Join(fields[1:], " "), executable, nil
}
