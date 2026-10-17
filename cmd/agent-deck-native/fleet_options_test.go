package main

// matrix/hardened: managed launch descriptions retain argv and overrides only.

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

// matrix/hardened: explicit launcher defaults do not affect other commands.
func TestUIAcceptsOnlySupportedDefaultTools(t *testing.T) {
	for _, tool := range []string{"shell", "codex", "powershell"} {
		config, err := parseOptions([]string{"ui", "--state-dir", t.TempDir(), "--tool", tool}, io.Discard)
		if err != nil || config.Tool != tool {
			t.Fatalf("UI tool preference lost: tool=%s config=%+v err=%v", tool, config, err)
		}
	}
	for _, tool := range []string{"", "custom", "unknown"} {
		if _, err := parseOptions([]string{"ui", "--tool", tool}, io.Discard); err == nil {
			t.Fatalf("accepted unsupported UI tool %q", tool)
		}
	}
	config, err := parseOptions([]string{"ui", "--state-dir", t.TempDir()}, io.Discard)
	if err != nil || config.Tool != "shell" {
		t.Fatalf("legacy UI default changed: config=%+v err=%v", config, err)
	}
}

// matrix/hardened: native UI filtering must use the same default-group value
// for both stores while preserving literal prototype groups and run identity.
func TestNativeUITranslatesOnlyStateDBDefaultGroup(t *testing.T) {
	for _, store := range []string{"", "prototype", "statedb"} {
		service := &nativeService{store: store}
		for _, group := range []string{"", nativestatestore.DefaultGroup, "研发"} {
			original := terminalhost.SessionInfo{ID: uuid.NewString(), RunID: uuid.NewString(), Name: "中文项目", Group: group, PID: 123}
			got := service.uiSession(original)
			want := original
			if store == "statedb" && group == nativestatestore.DefaultGroup {
				want.Group = ""
			}
			if !reflect.DeepEqual(got, want) || original.Group != group {
				t.Fatalf("group presentation changed other fields or input: store=%s got=%+v want=%+v", store, got, want)
			}
		}
	}
}

func TestWindowsDefaultRegistryWorksOnFirstRun(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows default local application data path")
	}
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	config, err := parseOptions([]string{"serve"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(config.StateDir) != root {
		t.Fatalf("default registry needs a missing intermediate application directory: %s", config.StateDir)
	}
	store, err := nativeregistry.Open(config.StateDir)
	if err != nil {
		t.Fatalf("clean first-run default cannot be opened: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedLaunchPreservesArgvAndOnlyExplicitEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AD_PRIVATE_INHERITED", "must-not-be-persisted")
	args := []string{"create", "--state-dir", filepath.Join(root, "registry"), "--name", "中文会话", "--tool", "custom", "--cwd", root, "--env", "AD_MARK=中文值", "--no-start", "--", filepath.Join(root, "agent.exe"), "--model", "two words", "literal$(value);&", ""}
	config, err := parseOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := launchSpec(config.Tool, config.Cwd, config.Executable, config.Args, config.Env, config.Cols, config.Rows)
	if err != nil {
		t.Fatal(err)
	}
	if !config.NoStart || !reflect.DeepEqual(launch.Args, args[len(args)-4:]) || !reflect.DeepEqual(launch.Env, []string{"AD_MARK=中文值"}) || launch.Dir != root {
		t.Fatalf("managed description changed command boundary or saved inherited secrets: config=%+v launch=%+v", config, launch)
	}
}

func TestManagerEndpointStableAndIndependentByStateRoot(t *testing.T) {
	root := t.TempDir()
	first, err := parseOptions([]string{"serve", "--state-dir", filepath.Join(root, "one")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	again, err := parseOptions([]string{"ui", "--state-dir", filepath.Join(root, "one", "."), "--cwd", root}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	other, err := parseOptions([]string{"list", "--state-dir", filepath.Join(root, "two")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if first.Pipe != again.Pipe || first.Pipe == other.Pipe || !filepath.IsAbs(first.StateDir) {
		t.Fatalf("state roots did not select independent stable endpoints: %q %q %q", first.Pipe, again.Pipe, other.Pipe)
	}
}

func TestManagedCommandsRequireIdentityAndUnambiguousLaunch(t *testing.T) {
	for _, args := range [][]string{
		{"create", "--name", "   "},
		{"create", "--name", "test", "--tool", "unknown"},
		{"create", "--name", "test", "--tool", "custom"},
		{"create", "--name", "test", "--cols", "501"},
		{"create", "--name", "test", "--env", "NO_EQUALS"},
		{"start"}, {"resume"}, {"rename", "--name", "new"}, {"remove"},
		{"start", "--session", "not-an-id"},
		{"resume", "--session", "not-an-id"},
		{"resume", "--session", uuid.NewString(), "--run-id", "not-a-run"},
		{"remove", "--session", uuid.NewString(), "--run-id", "not-a-run"},
		{"serve", "--", "extra"},
	} {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Fatalf("accepted incomplete or ambiguous command %q", args)
		}
	}
}

// matrix/hardened: CLI history selection is explicit and accepts the caller's
// old generation; omitting it is the CLI's separate current-generation lookup.
func TestManagedResumeParserKeepsExactIdentity(t *testing.T) {
	id, runID := uuid.NewString(), uuid.NewString()
	for _, run := range []string{"", runID} {
		args := []string{"resume", "--state-dir", t.TempDir(), "--store", "statedb", "--session", id, "--json"}
		if run != "" {
			args = append(args, "--run-id", run)
		}
		config, err := parseOptions(args, io.Discard)
		if err != nil || config.Command != "resume" || config.SessionID != id || config.RunID != run || config.Store != "statedb" || !config.JSON || !isFleetCommand(config.Command) {
			t.Fatalf("history parser lost identity or store: %+v %v", config, err)
		}
	}
}

func TestUIExplicitCodexExecutableUsesValidatedFixture(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "codex.exe")
	if err := os.WriteFile(executable, []byte("fixture native program"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := parseOptions([]string{"ui", "--state-dir", root, "--tool", "codex", "--codex-executable", executable}, io.Discard)
	if err != nil || config.Executable != executable || config.Tool != "codex" || len(config.Args)+len(config.Env) != 0 {
		t.Fatalf("explicit native program was lost or added flags: %+v %v", config, err)
	}
	for _, invalid := range []string{"relative/codex.exe", filepath.Join(root, "missing", "codex.exe"), root} {
		if _, err := parseOptions([]string{"ui", "--codex-executable", invalid}, io.Discard); err == nil {
			t.Fatalf("UI accepted invalid explicit executable: %q", invalid)
		}
	}
	var help strings.Builder
	printHelp(&help)
	if !strings.Contains(help.String(), "resume --session UUID [--run-id UUID]") || !strings.Contains(help.String(), "--codex-executable PATH") || !strings.Contains(help.String(), "Enter starts a new dialog") {
		t.Fatalf("native help omitted history or explicit executable: %s", help.String())
	}
}

func TestUIAttachCommandBindsExactSessionAndRun(t *testing.T) {
	service := &nativeService{pipe: pipePrefix + "test"}
	info := terminalhost.SessionInfo{ID: uuid.NewString(), RunID: uuid.NewString()}
	command, err := service.AttachCommand(info)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"attach", "--pipe", service.pipe, "--session", info.ID, "--run-id", info.RunID}
	if !reflect.DeepEqual(command.Args[1:], want) {
		t.Fatalf("attach lost selected runtime identity: %q", command.Args)
	}
	if command.Stdin != os.Stdin || command.Stdout != os.Stdout || command.Stderr != os.Stderr {
		t.Fatal("attach must inherit console files even when the UI output is wrapped")
	}
	if _, err := service.AttachCommand(terminalhost.SessionInfo{ID: info.ID}); err == nil {
		t.Fatal("attach accepted an unstarted descriptor")
	}
}

func TestManagedGroupMoveRequiresExplicitDestinationAndRun(t *testing.T) {
	id, runID := uuid.NewString(), uuid.NewString()
	config, err := parseOptions([]string{"move", "--state-dir", t.TempDir(), "--session", id, "--run-id", runID, "--group", ""}, io.Discard)
	if err != nil || config.Group != "" || config.SessionID != id || config.RunID != runID {
		t.Fatalf("move to default group lost exact run: config=%+v err=%v", config, err)
	}
	for _, args := range [][]string{
		{"move", "--session", id},
		{"move", "--session", id, "--group", "研发\n运维"},
		{"create", "--name", "test", "--group", "\x1b[31mhidden"},
	} {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Fatalf("accepted ambiguous or controlled group mutation: %q", args)
		}
	}
}

func TestPowerShellLaunchUsesNativeProgramAndExplicitArgv(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows tool")
	}
	root := t.TempDir()
	t.Setenv("SystemRoot", root)
	launch, err := launchSpec("powershell", root, "", nil, nil, 120, 30)
	if err != nil || launch.Executable != filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe") || !reflect.DeepEqual(launch.Args, []string{"-NoLogo", "-NoProfile"}) || len(launch.Env) != 0 {
		t.Fatalf("PowerShell launch changed native argv or saved inherited env: launch=%+v err=%v", launch, err)
	}
}
