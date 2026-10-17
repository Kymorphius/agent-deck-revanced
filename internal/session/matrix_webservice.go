package session

// matrix/hardened: watch and control the long-running `agent-deck web
// --no-tui` service that serves the phone UI. agent-deck does not install
// that service (it is a LaunchAgent / systemd unit the user wrote); this only
// reads its state from the service manager, probes its /healthz, and asks the
// manager to start, stop or restart it. Nothing here runs unless the user
// fills in [webservice]. The probe is a plain GET to the URL the user
// configured, without credentials, and nothing else goes out (R1/R3).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Service managers [webservice].manager accepts.
const (
	WebServiceLaunchd     = "launchd"      // a LaunchAgent in the user's gui/<uid> domain
	WebServiceSystemd     = "systemd"      // a system unit (control may need sudo = true)
	WebServiceSystemdUser = "systemd-user" // a `systemctl --user` unit
)

// WebServiceSettings is the [webservice] section of config.toml.
type WebServiceSettings struct {
	// Manager is launchd, systemd or systemd-user. Empty disables the feature.
	Manager string `toml:"manager,omitempty"`

	// Service is the launchd label or the systemd unit name.
	Service string `toml:"service,omitempty"`

	// Plist is the LaunchAgent file used to load the job again after a stop
	// (launchd only; default ~/Library/LaunchAgents/<service>.plist).
	Plist string `toml:"plist,omitempty"`

	// Sudo runs systemd start/stop/restart as `sudo -n systemctl …` (systemd
	// only; reading the state never needs it).
	Sudo bool `toml:"sudo,omitempty"`

	// URL is the server's base address, e.g. "http://100.64.0.1:8420". When
	// set, <url>/healthz is probed so "running" also means "answering".
	URL string `toml:"url,omitempty"`
}

// Enabled reports whether a service is configured.
func (s WebServiceSettings) Enabled() bool {
	return strings.TrimSpace(s.Manager) != "" && strings.TrimSpace(s.Service) != ""
}

// Validate rejects settings the commands below cannot act on.
func (s WebServiceSettings) Validate() error {
	switch s.Manager {
	case WebServiceLaunchd, WebServiceSystemd, WebServiceSystemdUser:
	default:
		return fmt.Errorf("[webservice].manager must be %q, %q or %q, got %q", WebServiceLaunchd, WebServiceSystemd, WebServiceSystemdUser, s.Manager)
	}
	if strings.TrimSpace(s.Service) == "" || strings.HasPrefix(s.Service, "-") || strings.ContainsAny(s.Service, "/ \t\n") {
		return fmt.Errorf("[webservice].service %q is not a service name", s.Service)
	}
	if s.URL != "" && !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
		return fmt.Errorf("[webservice].url %q must start with http:// or https://", s.URL)
	}
	return nil
}

// WebServiceStatus is one reading of the service's state.
type WebServiceStatus struct {
	Configured bool   `json:"configured"`
	Manager    string `json:"manager,omitempty"`
	Service    string `json:"service,omitempty"`
	URL        string `json:"url,omitempty"`

	// Loaded: launchd has the job bootstrapped / systemd knows the unit.
	Loaded bool `json:"loaded"`
	// Running: the manager reports a live process.
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
	// State is the manager's own wording ("running", "not loaded",
	// "active (running)", "inactive (dead)", …).
	State        string `json:"state,omitempty"`
	ManagerError string `json:"manager_error,omitempty"`

	// Probed: URL is set and /healthz was requested.
	Probed      bool   `json:"probed"`
	Healthy     bool   `json:"healthy"`
	HealthError string `json:"health_error,omitempty"`
	LatencyMs   int64  `json:"latency_ms,omitempty"`

	CheckedAt time.Time `json:"checked_at"`
}

// Summary is a short human description ("running, answering in 3 ms").
func (s WebServiceStatus) Summary() string {
	if !s.Configured {
		return "not configured"
	}
	var b strings.Builder
	switch {
	case s.ManagerError != "":
		b.WriteString("state unknown")
	case s.Running && s.PID > 0:
		fmt.Fprintf(&b, "running (pid %d)", s.PID)
	case s.Running:
		b.WriteString("running")
	case !s.Loaded:
		b.WriteString("stopped (not loaded)")
	default:
		b.WriteString("stopped")
	}
	if s.Probed {
		if s.Healthy {
			fmt.Fprintf(&b, ", answering in %d ms", s.LatencyMs)
		} else {
			b.WriteString(", not answering")
		}
	}
	return b.String()
}

// webServiceExec runs a service-manager command and returns its combined
// output. A var so tests can replace it.
var webServiceExec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed manager binaries, validated service name
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// webServiceHTTP probes /healthz. Proxy is nil on purpose: the server sits on
// loopback or a tailnet address, and a proxy from the environment would only
// get in the way (or see the request).
var webServiceHTTP = &http.Client{
	Timeout:   3 * time.Second,
	Transport: &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// CheckWebService reads the service's state and probes its health.
func CheckWebService(ctx context.Context, s WebServiceSettings) WebServiceStatus {
	st := WebServiceStatus{CheckedAt: time.Now()}
	if !s.Enabled() {
		return st
	}
	st.Configured = true
	st.Manager, st.Service, st.URL = s.Manager, s.Service, s.URL
	if err := s.Validate(); err != nil {
		st.ManagerError = err.Error()
		return st
	}
	switch s.Manager {
	case WebServiceLaunchd:
		readLaunchdState(ctx, s, &st)
	default:
		readSystemdState(ctx, s, &st)
	}
	if s.URL != "" {
		probeWebServiceHealth(ctx, s.URL, &st)
	}
	return st
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func readLaunchdState(ctx context.Context, s WebServiceSettings, st *WebServiceStatus) {
	out, err := webServiceExec(ctx, "launchctl", "print", launchdDomain()+"/"+s.Service)
	if err != nil {
		// launchctl print exits 113 for a label the domain does not have.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 113 {
			st.State = "not loaded"
			return
		}
		st.ManagerError = commandFailure("launchctl print", out, err)
		return
	}
	st.Loaded = true
	// Only the job's own top-level keys (one tab deep), not nested blocks.
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			st.State = val
			st.Running = val == "running"
		case "pid":
			st.PID, _ = strconv.Atoi(val)
		}
	}
}

func systemctlArgs(s WebServiceSettings, args ...string) []string {
	if s.Manager == WebServiceSystemdUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

func readSystemdState(ctx context.Context, s WebServiceSettings, st *WebServiceStatus) {
	out, err := webServiceExec(ctx, "systemctl", systemctlArgs(s, "show", "--property=LoadState,ActiveState,SubState,MainPID", "--", s.Service)...)
	if err != nil {
		st.ManagerError = commandFailure("systemctl show", out, err)
		return
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	st.Loaded = props["LoadState"] == "loaded"
	st.Running = props["ActiveState"] == "active"
	st.PID, _ = strconv.Atoi(props["MainPID"])
	switch {
	case props["LoadState"] != "" && props["LoadState"] != "loaded":
		st.State = props["LoadState"]
	case props["ActiveState"] != "":
		st.State = fmt.Sprintf("%s (%s)", props["ActiveState"], props["SubState"])
	}
}

func probeWebServiceHealth(ctx context.Context, base string, st *WebServiceStatus) {
	st.Probed = true
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil)
	if err != nil {
		st.HealthError = err.Error()
		return
	}
	start := time.Now()
	resp, err := webServiceHTTP.Do(req)
	if err != nil {
		st.HealthError = err.Error()
		return
	}
	_ = resp.Body.Close()
	st.LatencyMs = time.Since(start).Milliseconds()
	if resp.StatusCode != http.StatusOK {
		st.HealthError = resp.Status
		return
	}
	st.Healthy = true
}

func commandFailure(what string, out []byte, err error) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return fmt.Sprintf("%s: %s", what, msg)
	}
	return fmt.Sprintf("%s: %v", what, err)
}

// Actions ControlWebService accepts.
const (
	WebServiceStart   = "start"
	WebServiceStop    = "stop"
	WebServiceRestart = "restart"
)

// webServiceSettle bounds how long ControlWebService waits for the new state.
var webServiceSettle = 10 * time.Second

// ControlWebService asks the service manager to start, stop or restart the
// service, then waits (up to webServiceSettle) for the state to show it and
// returns the last reading. Stopping a LaunchAgent unloads it (bootout) so
// KeepAlive cannot bring it straight back; it loads again on the next login
// or on start.
func ControlWebService(ctx context.Context, s WebServiceSettings, action string) (WebServiceStatus, error) {
	if !s.Enabled() {
		return WebServiceStatus{CheckedAt: time.Now()}, errors.New("no [webservice] configured on this host")
	}
	if err := s.Validate(); err != nil {
		return WebServiceStatus{}, err
	}
	before := CheckWebService(ctx, s)
	var prevPID int
	if before.Running {
		prevPID = before.PID
	}

	var err error
	switch s.Manager {
	case WebServiceLaunchd:
		err = controlLaunchd(ctx, s, action, before)
	default:
		err = controlSystemd(ctx, s, action)
	}
	if err != nil {
		return before, err
	}

	deadline := time.Now().Add(webServiceSettle)
	for {
		st := CheckWebService(ctx, s)
		if webServiceSettled(action, st, prevPID) || time.Now().After(deadline) || ctx.Err() != nil {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func webServiceSettled(action string, st WebServiceStatus, prevPID int) bool {
	if st.ManagerError != "" {
		return true
	}
	switch action {
	case WebServiceStop:
		return !st.Running
	case WebServiceRestart:
		if prevPID > 0 && st.PID == prevPID {
			return false
		}
		fallthrough
	default:
		return st.Running && (!st.Probed || st.Healthy)
	}
}

func controlLaunchd(ctx context.Context, s WebServiceSettings, action string, before WebServiceStatus) error {
	target := launchdDomain() + "/" + s.Service
	run := func(args ...string) error {
		if out, err := webServiceExec(ctx, "launchctl", args...); err != nil {
			return errors.New(commandFailure("launchctl "+args[0], out, err))
		}
		return nil
	}
	bootstrap := func() error {
		plist := s.Plist
		if plist == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			plist = filepath.Join(home, "Library", "LaunchAgents", s.Service+".plist")
		}
		if _, err := os.Stat(plist); err != nil {
			return fmt.Errorf("cannot load %s: %w", s.Service, err)
		}
		return run("bootstrap", launchdDomain(), plist)
	}
	switch action {
	case WebServiceStart:
		if !before.Loaded {
			return bootstrap()
		}
		if before.Running {
			return nil
		}
		return run("kickstart", target)
	case WebServiceStop:
		if !before.Loaded {
			return nil
		}
		return run("bootout", target)
	case WebServiceRestart:
		if !before.Loaded {
			return bootstrap()
		}
		return run("kickstart", "-k", target)
	}
	return fmt.Errorf("unknown action %q (want start, stop or restart)", action)
}

func controlSystemd(ctx context.Context, s WebServiceSettings, action string) error {
	switch action {
	case WebServiceStart, WebServiceStop, WebServiceRestart:
	default:
		return fmt.Errorf("unknown action %q (want start, stop or restart)", action)
	}
	name, args := "systemctl", systemctlArgs(s, action, "--", s.Service)
	if s.Sudo && s.Manager == WebServiceSystemd {
		name, args = "sudo", append([]string{"-n", "systemctl"}, args...)
	}
	if out, err := webServiceExec(ctx, name, args...); err != nil {
		return errors.New(commandFailure(name+" "+action, out, err))
	}
	return nil
}
