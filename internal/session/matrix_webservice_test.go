package session

// matrix/hardened: tests for the [webservice] reader and controller, with the
// service manager replaced by a fake (no launchctl/systemctl runs here).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeManager struct {
	calls []string
	reply func(name string, args []string) ([]byte, error)
}

func (f *fakeManager) install(t *testing.T) {
	t.Helper()
	prevExec, prevSettle := webServiceExec, webServiceSettle
	webServiceExec = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		return f.reply(name, args)
	}
	webServiceSettle = 50 * time.Millisecond
	t.Cleanup(func() { webServiceExec, webServiceSettle = prevExec, prevSettle })
}

// exitCode yields a real *exec.ExitError with the given code.
func exitCode(code int) error {
	return exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
}

const launchctlRunning = `gui/501/my.web = {
	active count = 1
	path = /Users/me/Library/LaunchAgents/my.web.plist
	type = LaunchAgent
	state = running

	program = /usr/local/bin/agent-deck
	arguments = {
		/usr/local/bin/agent-deck
		web
	}

	pid = 4242
	immediate reason = speculative
	endpoints = {
		state = active
	}
}
`

func TestWebServiceSettingsValidate(t *testing.T) {
	ok := WebServiceSettings{Manager: WebServiceLaunchd, Service: "my.web", URL: "http://127.0.0.1:8420"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	for _, bad := range []WebServiceSettings{
		{Manager: "rc.d", Service: "x"},
		{Manager: WebServiceSystemd, Service: "-x"},
		{Manager: WebServiceSystemd, Service: "a b"},
		{Manager: WebServiceLaunchd, Service: "gui/501/x"},
		{Manager: WebServiceLaunchd, Service: "x", URL: "ftp://x"},
	} {
		if bad.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want error", bad)
		}
	}
	if (WebServiceSettings{Manager: WebServiceLaunchd}).Enabled() {
		t.Error("Enabled() without a service name")
	}
}

func TestCheckWebService_NotConfigured(t *testing.T) {
	f := &fakeManager{reply: func(string, []string) ([]byte, error) { t.Fatal("manager called"); return nil, nil }}
	f.install(t)
	st := CheckWebService(context.Background(), WebServiceSettings{})
	if st.Configured || st.Summary() != "not configured" {
		t.Fatalf("got %+v", st)
	}
}

func TestCheckWebService_LaunchdRunningAndHealthy(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	f := &fakeManager{reply: func(string, []string) ([]byte, error) { return []byte(launchctlRunning), nil }}
	f.install(t)
	st := CheckWebService(context.Background(), WebServiceSettings{Manager: WebServiceLaunchd, Service: "my.web", URL: srv.URL + "/"})
	if !st.Loaded || !st.Running || st.PID != 4242 || st.State != "running" {
		t.Fatalf("launchd state not parsed (nested state/pid must not leak): %+v", st)
	}
	if !st.Probed || !st.Healthy || st.HealthError != "" {
		t.Fatalf("health: %+v", st)
	}
	if gotPath != "/healthz" || gotAuth != "" {
		t.Fatalf("probe hit %q with Authorization %q", gotPath, gotAuth)
	}
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "launchctl print gui/") || !strings.HasSuffix(f.calls[0], "/my.web") {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestCheckWebService_LaunchdNotLoaded(t *testing.T) {
	f := &fakeManager{reply: func(string, []string) ([]byte, error) {
		return []byte("Could not find service \"my.web\" in domain for port\n"), exitCode(113)
	}}
	f.install(t)
	st := CheckWebService(context.Background(), WebServiceSettings{Manager: WebServiceLaunchd, Service: "my.web"})
	if st.Loaded || st.Running || st.ManagerError != "" || st.State != "not loaded" {
		t.Fatalf("got %+v", st)
	}
	if st.Summary() != "stopped (not loaded)" {
		t.Fatalf("Summary() = %q", st.Summary())
	}
}

func TestCheckWebService_SystemdAndUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	f := &fakeManager{reply: func(string, []string) ([]byte, error) {
		return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=77\n"), nil
	}}
	f.install(t)
	st := CheckWebService(context.Background(), WebServiceSettings{Manager: WebServiceSystemdUser, Service: "web.service", URL: srv.URL})
	if !st.Running || st.PID != 77 || st.State != "active (running)" {
		t.Fatalf("got %+v", st)
	}
	if st.Healthy || !strings.Contains(st.HealthError, "503") {
		t.Fatalf("health: %+v", st)
	}
	if f.calls[0] != "systemctl --user show --property=LoadState,ActiveState,SubState,MainPID -- web.service" {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestCheckWebService_SystemdUnitMissing(t *testing.T) {
	f := &fakeManager{reply: func(string, []string) ([]byte, error) {
		return []byte("LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\n"), nil
	}}
	f.install(t)
	st := CheckWebService(context.Background(), WebServiceSettings{Manager: WebServiceSystemd, Service: "nope"})
	if st.Loaded || st.Running || st.State != "not-found" {
		t.Fatalf("got %+v", st)
	}
}

// launchdSim is a tiny launchd: print reflects bootstrap/bootout/kickstart.
type launchdSim struct {
	loaded, running bool
	pid             int
}

func (s *launchdSim) reply(_ string, args []string) ([]byte, error) {
	switch args[0] {
	case "print":
		if !s.loaded {
			return []byte("Could not find service"), exitCode(113)
		}
		state := "not running"
		if s.running {
			state = "running"
		}
		out := "x = {\n\tstate = " + state + "\n"
		if s.running {
			out += "\tpid = " + strconv.Itoa(s.pid) + "\n"
		}
		return []byte(out + "}\n"), nil
	case "bootstrap":
		s.loaded, s.running, s.pid = true, true, s.pid+1
	case "bootout":
		s.loaded, s.running = false, false
	case "kickstart":
		s.running, s.pid = true, s.pid+1
	}
	return nil, nil
}

func TestControlWebService_Launchd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(plistDir, "my.web.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := WebServiceSettings{Manager: WebServiceLaunchd, Service: "my.web"}
	sim := &launchdSim{loaded: true, running: true, pid: 10}
	f := &fakeManager{reply: sim.reply}
	f.install(t)
	ctx := context.Background()

	st, err := ControlWebService(ctx, settings, WebServiceRestart)
	if err != nil || !st.Running || st.PID != 11 {
		t.Fatalf("restart: %+v, %v", st, err)
	}
	if !containsCall(f.calls, "launchctl kickstart -k gui/") {
		t.Fatalf("restart did not kickstart -k: %q", f.calls)
	}

	st, err = ControlWebService(ctx, settings, WebServiceStop)
	if err != nil || st.Running || st.Loaded {
		t.Fatalf("stop: %+v, %v", st, err)
	}
	if !containsCall(f.calls, "launchctl bootout gui/") {
		t.Fatalf("stop did not bootout: %q", f.calls)
	}

	f.calls = nil
	st, err = ControlWebService(ctx, settings, WebServiceStop)
	if err != nil || containsCall(f.calls, "launchctl bootout") {
		t.Fatalf("stopping a stopped service should be a no-op: %q, %v", f.calls, err)
	}

	st, err = ControlWebService(ctx, settings, WebServiceStart)
	if err != nil || !st.Running {
		t.Fatalf("start: %+v, %v", st, err)
	}
	if !containsCall(f.calls, "launchctl bootstrap gui/") || !containsCall(f.calls, plist) {
		t.Fatalf("start from unloaded did not bootstrap the plist: %q", f.calls)
	}

	if _, err := ControlWebService(ctx, settings, "reload"); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestControlWebService_LaunchdMissingPlist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sim := &launchdSim{}
	f := &fakeManager{reply: sim.reply}
	f.install(t)
	_, err := ControlWebService(context.Background(), WebServiceSettings{Manager: WebServiceLaunchd, Service: "my.web"}, WebServiceStart)
	if err == nil || !strings.Contains(err.Error(), "cannot load my.web") {
		t.Fatalf("err = %v", err)
	}
}

func TestControlWebService_SystemdSudo(t *testing.T) {
	active := "active"
	f := &fakeManager{reply: func(name string, args []string) ([]byte, error) {
		if name == "systemctl" && args[0] == "show" {
			return []byte("LoadState=loaded\nActiveState=" + active + "\nSubState=x\nMainPID=0\n"), nil
		}
		if strings.Contains(strings.Join(args, " "), "stop") {
			active = "inactive"
		}
		return nil, nil
	}}
	f.install(t)
	_, err := ControlWebService(context.Background(), WebServiceSettings{Manager: WebServiceSystemd, Service: "web", Sudo: true}, WebServiceStop)
	if err != nil {
		t.Fatal(err)
	}
	if !containsCall(f.calls, "sudo -n systemctl stop -- web") {
		t.Fatalf("calls = %q", f.calls)
	}

	// sudo never applies to --user units.
	f.calls = nil
	active = "active"
	if _, err := ControlWebService(context.Background(), WebServiceSettings{Manager: WebServiceSystemdUser, Service: "web", Sudo: true}, WebServiceStop); err != nil {
		t.Fatal(err)
	}
	if !containsCall(f.calls, "systemctl --user stop -- web") || containsCall(f.calls, "sudo") {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestControlWebService_ReportsManagerFailure(t *testing.T) {
	f := &fakeManager{reply: func(name string, args []string) ([]byte, error) {
		if args[0] == "show" {
			return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=5\n"), nil
		}
		return []byte("Failed to restart web.service: Interactive authentication required.\n"), exitCode(1)
	}}
	f.install(t)
	st, err := ControlWebService(context.Background(), WebServiceSettings{Manager: WebServiceSystemd, Service: "web"}, WebServiceRestart)
	if err == nil || !strings.Contains(err.Error(), "Interactive authentication required") {
		t.Fatalf("err = %v", err)
	}
	if !st.Running || st.PID != 5 {
		t.Fatalf("failure should return the reading from before: %+v", st)
	}
}

func containsCall(calls []string, sub string) bool {
	for _, c := range calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}
