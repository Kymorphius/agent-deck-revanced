package aiupdate

// matrix/hardened: all update tests use disposable homes, fake releases and fake daemon commands.
import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func packageTestFiles(version, target string) map[string][]byte {
	meta, _ := json.Marshal(packageMetadata{LayoutVersion: 1, Version: version, Target: target, Variant: "codex", Entrypoint: "bin/codex", ResourcesDir: "codex-resources", PathDir: "codex-path"})
	return map[string][]byte{"codex-package.json": meta, "bin/codex": []byte("codex " + version), "bin/codex-code-mode-host": []byte("helper"), "codex-path/rg": []byte("rg"), "codex-resources/bwrap": []byte("bwrap")}
}

func writeTestPackage(t *testing.T, directory, version, target string) {
	t.Helper()
	for _, name := range []string{"bin", "codex-resources", "codex-path"} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range packageTestFiles(version, target) {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0755); err != nil {
			t.Fatal(err)
		}
	}
}

func releaseArchive(t *testing.T, version, target string) []byte {
	t.Helper()
	var result bytes.Buffer
	gz := gzip.NewWriter(&result)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"bin/", "codex-resources/", "codex-path/"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range packageTestFiles(version, target) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

type managerFixture struct {
	m              *Manager
	calls          []string
	running        string
	restartChanges bool
	requests       []string
	mu             sync.Mutex
}

func newManagerFixture(t *testing.T, latest string, damage string) *managerFixture {
	t.Helper()
	home := t.TempDir()
	m := New("codex-test", home)
	if m.target == "" {
		t.Skip("native deferred install is unsupported")
	}
	f := &managerFixture{m: m, running: "0.163.0", restartChanges: true}
	root := m.packageRoot()
	old := filepath.Join(root, "releases", "0.163.0-"+m.target)
	writeTestPackage(t, old, "0.163.0", m.target)
	if err := os.Symlink(filepath.Join("releases", filepath.Base(old)), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "app-server-daemon"), 0700); err != nil {
		t.Fatal(err)
	}
	settings := `{"remoteControlEnabled":true,"futureSetting":{"large":12345678901234567890},"updater":{"autoUpdateEnabled":true,"updateIntervalMinutes":17,"future":42}}`
	if err := os.WriteFile(filepath.Join(home, "app-server-daemon", "settings.json"), []byte(settings), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auto-update-version"), []byte(filepath.Base(old)), 0600); err != nil {
		t.Fatal(err)
	}
	m.verifyRuntime = func(context.Context) error { return nil }
	m.run = func(_ context.Context, binary, _ string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, binary+" "+strings.Join(args, " "))
		if len(args) == 1 && args[0] == "--version" {
			version := "0.163.0"
			if binary != m.binary {
				meta, err := readPackageMetadata(filepath.Dir(filepath.Dir(binary)))
				if err != nil {
					return nil, err
				}
				version = meta.Version
			}
			return []byte("codex-cli " + version + "\n"), nil
		}
		if strings.Join(args, " ") == "app-server daemon pid-update-loop --check-package-ownership" {
			return []byte(""), nil
		}
		_, installed, err := m.inspectSelection()
		if err != nil {
			return nil, err
		}
		if strings.Join(args, " ") == "app-server daemon restart" {
			if f.restartChanges {
				f.running = installed
			}
			return []byte(`{"status":"restarted"}`), nil
		}
		if strings.Join(args, " ") == "app-server daemon version" {
			return json.Marshal(lifecycleOutput{Status: "running", ManagedCodexVersion: installed, AppServerVersion: f.running, CLIVersion: "0.163.0"})
		}
		return nil, fmt.Errorf("unexpected command %v", args)
	}
	archive := releaseArchive(t, latest, m.target)
	archiveHash := sha256.Sum256(archive)
	archiveDigest := hex.EncodeToString(archiveHash[:])
	asset := "codex-package-" + m.target + ".tar.gz"
	manifest := archiveDigest + "  " + asset + "\n"
	manifestHash := sha256.Sum256([]byte(manifest))
	meta := releaseMetadata{Tag: "rust-v" + latest, Assets: []releaseAsset{{Name: asset, Digest: "sha256:" + archiveDigest}, {Name: "codex-package_SHA256SUMS", Digest: "sha256:" + hex.EncodeToString(manifestHash[:])}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, req.URL.Path)
		f.mu.Unlock()
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("Proxy-Authorization") != "" {
			t.Error("release request included credentials")
		}
		switch req.URL.Path {
		case "/channels/latest", "/releases/" + latest + "/release.json":
			if damage == "metadata" && strings.HasSuffix(req.URL.Path, "release.json") {
				meta.Tag = "rust-v0.999.0"
			}
			json.NewEncoder(w).Encode(meta)
		case "/releases/" + latest + "/codex-package_SHA256SUMS":
			if damage == "manifest" {
				fmt.Fprint(w, "broken")
				return
			}
			fmt.Fprint(w, manifest)
		case "/releases/" + latest + "/" + asset:
			if damage == "archive" {
				w.Write(append(archive, 'x'))
				return
			}
			if damage == "truncated" {
				w.Header().Set("Content-Length", fmt.Sprint(len(archive)+1))
			}
			w.Write(archive)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	m.baseURL = server.URL
	m.client = server.Client()
	return f
}

func TestInstallDefersActivationAndPreservesSettings(t *testing.T) {
	t.Setenv("GH_TOKEN", "must-not-send")
	t.Setenv("GITHUB_TOKEN", "must-not-send")
	f := newManagerFixture(t, "0.164.0", "")
	status, err := f.m.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.RunningVersion != "0.163.0" || status.InstalledVersion != "0.164.0" || !status.Pending || status.NativeAutoUpdate {
		t.Fatalf("unexpected status %+v", status)
	}
	for _, call := range f.calls {
		if strings.HasSuffix(call, " restart") || strings.HasSuffix(call, " update") || strings.HasSuffix(call, " stop") {
			t.Fatalf("install mutated lifecycle: %s", call)
		}
	}
	old := filepath.Join(f.m.packageRoot(), "releases", "0.163.0-"+f.m.target, "bin", "codex")
	if data, err := os.ReadFile(old); err != nil || string(data) != "codex 0.163.0" {
		t.Fatal("old release changed")
	}
	if markerExists(f.m.packageRoot()) {
		t.Fatal("native updater eligibility marker remains")
	}
	settingsPath := filepath.Join(f.m.home, "app-server-daemon", "settings.json")
	settings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"12345678901234567890", `"updateIntervalMinutes": 17`, `"future": 42`, `"remoteControlEnabled": true`} {
		if !bytes.Contains(settings, []byte(preserved)) {
			t.Fatalf("setting lost: %s", preserved)
		}
	}
	info, _ := os.Stat(settingsPath)
	if info.Mode().Perm() != 0640 {
		t.Fatal("settings mode changed")
	}
	before := len(f.requests)
	if _, err := f.m.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != before {
		t.Fatal("Inspect fetched a release")
	}
	status, err = f.m.Restart(context.Background())
	if err != nil || status.Pending || status.RunningVersion != "0.164.0" {
		t.Fatalf("restart readback %+v %v", status, err)
	}
}

func TestInstallRejectsDamagedReleases(t *testing.T) {
	for _, damage := range []string{"metadata", "manifest", "archive", "truncated"} {
		t.Run(damage, func(t *testing.T) {
			f := newManagerFixture(t, "0.164.0", damage)
			before, _ := os.Readlink(filepath.Join(f.m.packageRoot(), "current"))
			if _, err := f.m.Install(context.Background()); err == nil {
				t.Fatal("damaged release accepted")
			}
			after, _ := os.Readlink(filepath.Join(f.m.packageRoot(), "current"))
			if before != after {
				t.Fatal("selection changed after verification failed")
			}
			settings, _ := readSettings(filepath.Join(f.m.home, "app-server-daemon", "settings.json"))
			if !autoUpdateEnabled(settings) {
				t.Fatal("failed download modified settings")
			}
		})
	}
}

func TestInstallPinsSameVersionAndNeverDowngrades(t *testing.T) {
	f := newManagerFixture(t, "0.163.0", "")
	status, err := f.m.Install(context.Background())
	if err != nil || status.NativeAutoUpdate || status.Pending {
		t.Fatalf("same-version pin %+v %v", status, err)
	}
	if markerExists(f.m.packageRoot()) {
		t.Fatal("same-version pin retained native marker")
	}
	if _, err := f.m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, req := range f.requests {
		if strings.Contains(req, "/releases/") {
			t.Fatal("same-version install downloaded a package")
		}
	}
	f = newManagerFixture(t, "0.162.0", "")
	if _, err := f.m.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade result %v", err)
	}
}

func TestInstallRefusesUnknownNativeMechanism(t *testing.T) {
	f := newManagerFixture(t, "0.164.0", "")
	f.m.verifyRuntime = func(context.Context) error { return errors.New("unknown updater") }
	if _, err := f.m.Install(context.Background()); err == nil {
		t.Fatal("unknown updater accepted")
	}
	_, selected, _ := f.m.inspectSelection()
	if selected != "0.163.0" || !markerExists(f.m.packageRoot()) {
		t.Fatal("unknown updater changed selection")
	}
	settings, _ := readSettings(filepath.Join(f.m.home, "app-server-daemon", "settings.json"))
	if !autoUpdateEnabled(settings) {
		t.Fatal("unknown updater changed settings")
	}
}

func TestInstallExistingReleaseFailsBeforeSettingsMutation(t *testing.T) {
	f := newManagerFixture(t, "0.164.0", "")
	destination := filepath.Join(f.m.packageRoot(), "releases", "0.164.0-"+f.m.target)
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Install(context.Background()); err == nil {
		t.Fatal("existing release was replaced")
	}
	settings, _ := readSettings(filepath.Join(f.m.home, "app-server-daemon", "settings.json"))
	if !autoUpdateEnabled(settings) || !markerExists(f.m.packageRoot()) {
		t.Fatal("release collision changed updater preferences or marker")
	}
}

func TestPinRejectsMalformedSettingsBeforeClearingMarker(t *testing.T) {
	f := newManagerFixture(t, "0.163.0", "")
	path := filepath.Join(f.m.home, "app-server-daemon", "settings.json")
	if err := os.WriteFile(path, []byte(`{"updater":"future-unsupported-value"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Install(context.Background()); err == nil {
		t.Fatal("invalid updater settings were replaced")
	}
	if !markerExists(f.m.packageRoot()) {
		t.Fatal("invalid settings unexpectedly cleared eligibility")
	}
	data, _ := os.ReadFile(path)
	if string(data) != `{"updater":"future-unsupported-value"}` {
		t.Fatal("invalid settings were overwritten")
	}
}

func TestRestartRequiresRunningVersionReadback(t *testing.T) {
	f := newManagerFixture(t, "0.164.0", "")
	if _, err := f.m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.restartChanges = false
	status, err := f.m.Restart(context.Background())
	if err == nil || status.RunningVersion != "0.163.0" || !status.Pending {
		t.Fatalf("restart overstated success: %+v %v", status, err)
	}
}

func TestInspectUnreachableDaemonRemainsUnknown(t *testing.T) {
	f := newManagerFixture(t, "0.164.0", "")
	original := f.m.run
	f.m.run = func(ctx context.Context, binary, home string, args ...string) ([]byte, error) {
		if len(args) > 1 {
			return nil, errors.New("no response")
		}
		return original(ctx, binary, home, args...)
	}
	status, err := f.m.Inspect(context.Background())
	if err == nil || status.DaemonStatus != "unknown" || status.RunningVersion != "" {
		t.Fatalf("unknown daemon overstated: %+v %v", status, err)
	}
}

func TestSelectionReplacementIsAtomic(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"old", "new"} {
		if err := os.MkdirAll(filepath.Join(root, "releases", name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "releases", name, "id"), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/old", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			content, err := os.ReadFile(filepath.Join(root, "current", "id"))
			// macOS/APFS can return EINVAL from a lookup of a symlink being renamed,
			// even when replacement is a single rename syscall (also reproduced in Python).
			// This is not a missing-selection window; eventual readback below must succeed.
			if runtime.GOOS == "darwin" && errors.Is(err, syscall.EINVAL) {
				continue
			}
			if err != nil || (string(content) != "old" && string(content) != "new") {
				failed <- fmt.Errorf("observed broken selected package %q: %v", content, err)
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		name := "old"
		if i%2 == 0 {
			name = "new"
		}
		if err := selectRelease(root, name); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	<-done
	if _, err := os.ReadFile(filepath.Join(root, "current", "id")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		t.Fatal(err)
	default:
	}
}
