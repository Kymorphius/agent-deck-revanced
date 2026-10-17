package aiupdate

// matrix/hardened: immutable releases and native locks defer activation without touching live processes.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type packageMetadata struct {
	LayoutVersion int    `json:"layoutVersion"`
	Version       string `json:"version"`
	Target        string `json:"target"`
	Variant       string `json:"variant"`
	Entrypoint    string `json:"entrypoint"`
	ResourcesDir  string `json:"resourcesDir"`
	PathDir       string `json:"pathDir"`
}

func (m *Manager) packageRoot() string { return filepath.Join(m.home, "packages", "app-server-daemon") }

func (m *Manager) inspectSelection() (bool, string, error) {
	root := m.packageRoot()
	current := filepath.Join(root, "current")
	info, err := os.Lstat(current)
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, "", errors.New("daemon current selection is not a symlink")
	}
	release, err := filepath.EvalSymlinks(current)
	if err != nil {
		return false, "", fmt.Errorf("broken daemon package selection: %w", err)
	}
	if filepath.Dir(release) != filepath.Join(root, "releases") {
		return false, "", errors.New("daemon selection points outside its immutable releases")
	}
	meta, err := readPackageMetadata(release)
	if err != nil {
		return false, "", err
	}
	if filepath.Base(release) != meta.Version+"-"+meta.Target {
		return false, "", errors.New("daemon release name disagrees with package metadata")
	}
	if err := validatePackage(release, meta.Version, m.target); err != nil {
		return false, "", err
	}
	return true, meta.Version, nil
}

func readPackageMetadata(directory string) (packageMetadata, error) {
	var meta packageMetadata
	data, err := readRegularFile(filepath.Join(directory, "codex-package.json"), 1<<20)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("invalid Codex package metadata: %w", err)
	}
	return meta, nil
}

func validatePackage(directory, version, target string) error {
	meta, err := readPackageMetadata(directory)
	if err != nil {
		return err
	}
	if meta.LayoutVersion != 1 || meta.Version != version || !stableVersion.MatchString(version) || meta.Target != target || meta.Variant != "codex" || meta.Entrypoint != "bin/codex" || meta.ResourcesDir != "codex-resources" || meta.PathDir != "codex-path" {
		return errors.New("unsupported or mismatched Codex package metadata")
	}
	for _, dir := range []string{"bin", "codex-resources", "codex-path"} {
		info, err := os.Lstat(filepath.Join(directory, dir))
		if err != nil || !info.IsDir() {
			return fmt.Errorf("Codex package lacks regular directory %s", dir)
		}
	}
	files := []string{"bin/codex", "bin/codex-code-mode-host", "codex-path/rg"}
	if strings.Contains(target, "linux") {
		files = append(files, "codex-resources/bwrap")
	}
	for _, file := range files {
		info, err := os.Lstat(filepath.Join(directory, file))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() == 0 {
			return fmt.Errorf("Codex package lacks executable regular file %s", file)
		}
	}
	return nil
}

func readSettings(path string) (map[string]json.RawMessage, error) {
	data, err := readRegularFile(path, 1<<20)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil || settings == nil {
		return nil, errors.New("daemon settings are not a JSON object; refusing to replace them")
	}
	return settings, nil
}

func autoUpdateEnabled(settings map[string]json.RawMessage) bool {
	var updater map[string]json.RawMessage
	if json.Unmarshal(settings["updater"], &updater) != nil {
		return true
	}
	var enabled bool
	if json.Unmarshal(updater["autoUpdateEnabled"], &enabled) != nil {
		return true
	}
	return enabled
}

func disabledNativeSettings(path string) ([]byte, os.FileMode, error) {
	settings, err := readSettings(path)
	if err != nil {
		return nil, 0, err
	}
	updater := map[string]json.RawMessage{}
	if data, present := settings["updater"]; present {
		if err := json.Unmarshal(data, &updater); err != nil || updater == nil {
			return nil, 0, errors.New("daemon updater settings are not a JSON object")
		}
	}
	updater["autoUpdateEnabled"] = json.RawMessage("false")
	settings["updater"], err = json.Marshal(updater)
	if err != nil {
		return nil, 0, err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, 0, err
	}
	mode := os.FileMode(0600)
	if info, err := os.Lstat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return append(data, '\n'), mode, nil
}

func pinNativeUpdates(settingsPath, root string) error {
	// Prepare and validate all JSON before changing updater eligibility. Removing the marker
	// FIRST ensures a later settings/publish failure cannot leave a queued native restart eligible.
	data, mode, err := disabledNativeSettings(settingsPath)
	if err != nil {
		return err
	}
	if err := validateNativeMarker(root); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(root, "auto-update-version")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not pin native updater; settings and selection unchanged: %w", err)
	}
	if err := atomicWrite(settingsPath, data, mode); err != nil {
		return fmt.Errorf("native updater marker was cleared; saving disabled automatic-update preference failed: %w", err)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".agent-deck-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("refusing nonregular or oversized file %s", filepath.Base(path))
	}
	file, err := openReadNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("file identity changed while opening %s", filepath.Base(path))
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("file grew beyond size limit")
	}
	return data, err
}

// Install downloads and selects a complete package for a future daemon launch.
// It deliberately never runs a lifecycle mutation, sends a signal, or removes an old release.
func (m *Manager) Install(ctx context.Context) (Status, error) {
	status, err := m.Inspect(ctx)
	if err != nil {
		return status, err
	}
	if m.target == "" || !status.Managed || status.RunningVersion != "0.163.0" || status.CLIVersion != "0.163.0" {
		return status, errors.New("deferred install currently requires a dedicated package and a confirmed running Codex daemon 0.163.0; other lifecycle versions are unsupported")
	}
	channel, err := m.latest(ctx)
	if err != nil {
		return status, err
	}
	status.LatestVersion = channel.version()
	if compareVersions(channel.version(), status.InstalledVersion) < 0 || compareVersions(channel.version(), status.RunningVersion) < 0 {
		return status, errors.New("refusing to downgrade the selected or running Codex version")
	}
	if status.InstalledVersion == channel.version() && !status.NativeAutoUpdate && !markerExists(m.packageRoot()) {
		return status, nil
	}
	if status.InstalledVersion == channel.version() {
		return m.pinCurrent(ctx, status)
	}
	// matrix/hardened: fetch immutable metadata separately so a channel change cannot mix release assets.
	release, err := m.metadata(ctx, "/releases/"+channel.version()+"/release.json")
	if err != nil {
		return status, err
	}
	if release.Tag != channel.Tag {
		return status, errors.New("Codex immutable release metadata disagrees with channel version")
	}
	work, err := os.MkdirTemp("", "agent-deck-codex-update-*")
	if err != nil {
		return status, err
	}
	defer os.RemoveAll(work)
	archive := filepath.Join(work, "package.tar.gz")
	if err := m.download(ctx, release, archive); err != nil {
		return status, err
	}
	root := m.packageRoot()
	for _, path := range []string{m.home, filepath.Join(m.home, "app-server-daemon"), filepath.Join(m.home, "packages"), root, filepath.Join(root, "releases")} {
		if err := ensurePlainDirectory(path); err != nil {
			return status, err
		}
	}
	stage, err := os.MkdirTemp(filepath.Join(root, "releases"), ".agent-deck-staging-*")
	if err != nil {
		return status, err
	}
	defer os.RemoveAll(stage)
	if err := extractPackage(archive, stage); err != nil {
		return status, err
	}
	if err := validatePackage(stage, release.version(), m.target); err != nil {
		return status, err
	}
	versionOutput, err := m.run(ctx, filepath.Join(stage, "bin", "codex"), m.home, "--version")
	if err != nil {
		return status, err
	}
	version, err := parseVersion(versionOutput)
	if err != nil || version != release.version() {
		return status, errors.New("downloaded Codex binary did not report the expected release version")
	}
	if _, err := m.run(ctx, filepath.Join(stage, "bin", "codex"), m.home, "app-server", "daemon", "pid-update-loop", "--check-package-ownership"); err != nil {
		return status, errors.New("downloaded Codex does not support dedicated daemon packages")
	}
	// Native installers provide this convenience symlink separately from the archive.
	if err := os.Symlink("bin/codex", filepath.Join(stage, "codex")); err != nil {
		return status, err
	}
	unlock, err := acquireNativeLocks(ctx, filepath.Join(m.home, "app-server-daemon", "daemon.lock"), filepath.Join(root, "install.lock"))
	if err != nil {
		return status, err
	}
	defer unlock()
	// matrix/hardened: re-probe after both locks. Concurrent native lifecycle operations may have changed the safety boundary.
	current, err := m.Inspect(ctx)
	current.LatestVersion = release.version()
	if err != nil || !current.Managed || current.RunningVersion != "0.163.0" || current.CLIVersion != "0.163.0" {
		if err != nil {
			return status, err
		}
		return current, errors.New("daemon changed during download; deferred install was not selected")
	}
	if compareVersions(release.version(), current.InstalledVersion) < 0 {
		return current, errors.New("another installer selected a newer Codex release; refusing downgrade")
	}
	if err := m.verifyRuntime(ctx); err != nil {
		return current, err
	}
	releaseName := release.version() + "-" + m.target
	destination := filepath.Join(root, "releases", releaseName)
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return current, errors.New("refusing to overwrite an existing immutable Codex release")
	}
	if err := pinNativeUpdates(filepath.Join(m.home, "app-server-daemon", "settings.json"), root); err != nil {
		return m.afterMutationError(ctx, current, err)
	}
	if err := os.Rename(stage, destination); err != nil {
		return m.afterMutationError(ctx, current, fmt.Errorf("native automatic updates were disabled; release publication failed: %w", err))
	}
	if err := selectRelease(root, releaseName); err != nil {
		return m.afterMutationError(ctx, current, fmt.Errorf("native automatic updates were disabled and the native updater marker cleared; package selection failed: %w", err))
	}
	selected, err := m.Inspect(ctx)
	selected.LatestVersion = release.version()
	return selected, err
}

func (m *Manager) afterMutationError(ctx context.Context, previous Status, mutationErr error) (Status, error) {
	readback, err := m.Inspect(ctx)
	readback.LatestVersion = previous.LatestVersion
	if err != nil {
		return readback, errors.Join(mutationErr, fmt.Errorf("current state could not be fully read back: %w", err))
	}
	return readback, mutationErr
}

func validateNativeMarker(root string) error {
	info, err := os.Lstat(filepath.Join(root, "auto-update-version"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("native updater marker is not a regular file; refusing settings or selection changes")
	}
	return nil
}

func markerExists(root string) bool {
	_, err := os.Lstat(filepath.Join(root, "auto-update-version"))
	return !os.IsNotExist(err)
}

func compareVersions(a, b string) int {
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, _ := strconv.ParseUint(aa[i], 10, 64)
		bv, _ := strconv.ParseUint(bb[i], 10, 64)
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func (m *Manager) pinCurrent(ctx context.Context, status Status) (Status, error) {
	unlock, err := acquireNativeLocks(ctx, filepath.Join(m.home, "app-server-daemon", "daemon.lock"), filepath.Join(m.packageRoot(), "install.lock"))
	if err != nil {
		return status, err
	}
	defer unlock()
	current, err := m.Inspect(ctx)
	current.LatestVersion = status.LatestVersion
	if err != nil {
		return current, err
	}
	if current.CLIVersion != "0.163.0" || current.RunningVersion != "0.163.0" || current.InstalledVersion != status.InstalledVersion {
		return current, errors.New("daemon selection changed; retry deferred install")
	}
	if err := m.verifyRuntime(ctx); err != nil {
		return current, err
	}
	if err := pinNativeUpdates(filepath.Join(m.home, "app-server-daemon", "settings.json"), m.packageRoot()); err != nil {
		return m.afterMutationError(ctx, current, err)
	}
	current, err = m.Inspect(ctx)
	current.LatestVersion = status.LatestVersion
	return current, err
}

func ensurePlainDirectory(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return os.Mkdir(path, 0700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing non-directory or symlink %s", path)
	}
	return nil
}

func selectRelease(root, name string) error {
	current := filepath.Join(root, "current")
	if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return errors.New("refusing to replace a non-symlink current selection")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(root, ".agent-deck-current-*")
	if err != nil {
		return err
	}
	namePath := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(namePath)
		return err
	}
	if err := os.Remove(namePath); err != nil {
		return err
	}
	defer os.Remove(namePath)
	if err := os.Symlink(filepath.Join("releases", name), namePath); err != nil {
		return err
	}
	return os.Rename(namePath, current)
}
