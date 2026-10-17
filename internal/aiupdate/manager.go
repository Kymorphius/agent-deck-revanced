// Package aiupdate provides explicit, deferred Codex daemon package updates.
// matrix/hardened: no background checks, credentials, installer scripts, or implicit restarts.
package aiupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Status distinguishes the selected package from the shared process serving clients.
type Status struct {
	CLIVersion       string `json:"cli_version"`
	InstalledVersion string `json:"installed_version"`
	RunningVersion   string `json:"running_version"`
	LatestVersion    string `json:"latest_version,omitempty"`
	DaemonStatus     string `json:"daemon_status"`
	Pending          bool   `json:"pending"`
	Managed          bool   `json:"managed"`
	NativeAutoUpdate bool   `json:"native_auto_update"`
	Home             string `json:"home"`
}

type commandRunner func(context.Context, string, string, ...string) ([]byte, error)

// Manager is intentionally inert until an explicit method is called.
type Manager struct {
	binary        string
	home          string
	client        *http.Client
	baseURL       string
	run           commandRunner
	target        string
	verifyRuntime func(context.Context) error
}

// New uses CODEX_HOME when home is empty, matching the native Codex home scope.
func New(binary, home string) *Manager {
	if binary == "" {
		binary = "codex"
	}
	if home == "" {
		home = os.Getenv("CODEX_HOME")
		if home == "" {
			userHome, _ := os.UserHomeDir()
			home = filepath.Join(userHome, ".codex")
		}
	}
	if absolute, err := filepath.Abs(home); err == nil {
		home = absolute
	}
	if canonical, err := filepath.EvalSymlinks(home); err == nil {
		home = canonical
	}
	m := &Manager{binary: binary, home: home, client: releaseClient(), baseURL: "https://releases.openai.com/codex", run: runCommand, target: platformTarget()}
	m.verifyRuntime = m.verifyNativeRuntime
	return m
}

var stableVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func platformTarget() string {
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	if arch == "" {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return arch + "-apple-darwin"
	case "linux":
		return arch + "-unknown-linux-musl"
	default:
		return ""
	}
}

func runCommand(ctx context.Context, binary, home string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	// matrix/hardened: no shell evaluation; explicitly select the inspected Codex home.
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Codex %s failed: %w", strings.Join(args, " "), err)
	}
	if len(output) > 1<<20 {
		return nil, errors.New("Codex returned excessive command output")
	}
	return output, nil
}

func parseVersion(output []byte) (string, error) {
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[0] != "codex-cli" || !stableVersion.MatchString(fields[1]) {
		return "", errors.New("unsupported Codex version output")
	}
	return fields[1], nil
}

type lifecycleOutput struct {
	Status              string `json:"status"`
	ManagedCodexPath    string `json:"managedCodexPath"`
	ManagedCodexVersion string `json:"managedCodexVersion"`
	CLIVersion          string `json:"cliVersion"`
	AppServerVersion    string `json:"appServerVersion"`
}

// Inspect only reads local state and invokes the native read-only version probe.
// Unreachable daemons remain unknown; absence of a response is never treated as stopped.
func (m *Manager) Inspect(ctx context.Context) (Status, error) {
	status := Status{Home: m.home, DaemonStatus: "unknown"}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cli, err := m.run(ctx, m.binary, m.home, "--version")
	if err != nil {
		return status, err
	}
	status.CLIVersion, err = parseVersion(cli)
	if err != nil {
		return status, err
	}
	status.Managed, status.InstalledVersion, err = m.inspectSelection()
	if err != nil {
		return status, err
	}
	settings, err := readSettings(filepath.Join(m.home, "app-server-daemon", "settings.json"))
	if err != nil {
		return status, err
	}
	status.NativeAutoUpdate = autoUpdateEnabled(settings)
	output, err := m.run(ctx, m.binary, m.home, "app-server", "daemon", "version")
	if err != nil {
		return status, fmt.Errorf("shared daemon version is unavailable; running state is unknown: %w", err)
	}
	var daemon lifecycleOutput
	if err := json.Unmarshal(output, &daemon); err != nil {
		return status, fmt.Errorf("invalid native daemon version response: %w", err)
	}
	if daemon.Status != "running" || !stableVersion.MatchString(daemon.AppServerVersion) {
		return status, errors.New("native daemon did not confirm a running version")
	}
	status.DaemonStatus = daemon.Status
	status.RunningVersion = daemon.AppServerVersion
	// matrix/hardened: selection metadata and native readback must agree before claiming an installed version.
	if status.Managed && daemon.ManagedCodexVersion != status.InstalledVersion {
		return status, errors.New("selected package metadata and native installed version disagree")
	}
	status.Pending = status.Managed && status.InstalledVersion != status.RunningVersion
	return status, nil
}

// Check manually fetches the stable release channel; construction and Inspect never fetch it.
func (m *Manager) Check(ctx context.Context) (Status, error) {
	status, inspectErr := m.Inspect(ctx)
	release, err := m.latest(ctx)
	if err != nil {
		return status, err
	}
	status.LatestVersion = release.version()
	return status, inspectErr
}

// Restart is a separate interrupting action. Callers must obtain explicit confirmation.
// It never installs or starts a download, and success requires fresh running-version readback.
func (m *Manager) Restart(ctx context.Context) (Status, error) {
	status, err := m.Inspect(ctx)
	if err != nil {
		return status, err
	}
	if !status.Managed {
		return status, errors.New("Codex does not have a supported dedicated daemon package")
	}
	if _, err := m.run(ctx, m.binary, m.home, "app-server", "daemon", "restart"); err != nil {
		return status, err
	}
	status, err = m.Inspect(ctx)
	if err == nil && status.Pending {
		err = errors.New("daemon restart returned but the selected version is not running")
	}
	return status, err
}
