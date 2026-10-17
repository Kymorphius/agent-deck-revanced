package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// matrix/hardened: test binaries always resolve the isolated HOME. This hook
// is compiled only into tests; installed binaries retain passwd-home lookup.
func init() { claudeLaunchGuardHome = os.UserHomeDir }

// matrix/hardened: fake host guard and Claude processes stay inside the
// run-isolated.py HOME/tmux socket; no real account or request is touched.
func matrixClaudeGuardFixture(t *testing.T, allowed bool) (string, string) {
	t.Helper()
	home := isolatedHomeDir(t)
	if canonical, err := filepath.EvalSymlinks(home); err == nil {
		home = canonical
		t.Setenv("HOME", home)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	old := claudeLaunchGuardHome
	claudeLaunchGuardHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { claudeLaunchGuardHome = old })
	dir := filepath.Join(home, ".local", "lib", "claude-launch-guard")
	bin := filepath.Join(home, ".local", "bin")
	for _, path := range []string{dir, bin} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "enabled.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(home, "guard-calls")
	response := `{"allowed":false,"reason":"minute_budget","retry_after":42}`
	exit := "75"
	if allowed {
		response = `{"allowed":true,"ticket":"` + strings.Repeat("a", 48) + `"}`
		exit = "0"
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + shellescape.Quote(calls) + "\nprintf '%s\\n' " + shellescape.Quote(response) + "\nexit " + exit + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude-launch-guard"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return home, calls
}

func matrixAssertGuardArgs(t *testing.T, calls, kind, id string) {
	t.Helper()
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	if strings.Count(args, "reserve\n") != 1 || !strings.Contains(args, "--kind\n"+kind+"\n") || !strings.Contains(args, "--operation-id\n") || !strings.Contains(args, "--json\n") {
		t.Fatalf("unexpected admission args: %s", args)
	}
	if id != "" && !strings.Contains(args, "--session-id\n"+id+"\n") {
		t.Fatalf("receipt was not bound to target UUID: %s", args)
	}
}

// matrix/hardened: detached admission planning must preserve a conductor's
// explicit channel opt-out instead of healing a Telegram channel onto it.
func TestMatrixClaudeLaunchGuardPreservesChannelOptOut(t *testing.T) {
	home, _ := matrixClaudeGuardFixture(t, true)
	withConductorEnvFile(t, "guard-opt-out", "export TELEGRAM_STATE_DIR=/tmp/guard-telegram\n")
	i := newGuardInstance(t, home)
	i.Title = "conductor-guard-opt-out"
	i.IsConductor = true
	i.PluginChannelLinkDisabled = true
	plan, err := i.planClaudeLaunchAdmission(false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.cleanup()
	if len(plan.planned.Channels) != 0 || strings.Contains(plan.command, "--channels") {
		t.Fatal("admission planning linked channels despite conductor opt-out")
	}
	i.applyClaudeLaunchAdmission(plan)
	if len(i.Channels) != 0 || !i.PluginChannelLinkDisabled {
		t.Fatal("admitted launch lost explicit channel opt-out")
	}
}

func TestMatrixClaudeLaunchGuardDeniedStartsPreserveBinding(t *testing.T) {
	for _, message := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "start-message"}[message], func(t *testing.T) {
			home, calls := matrixClaudeGuardFixture(t, false)
			i := newGuardInstance(t, home)
			const old = "11111111-2222-4333-8444-555555555555"
			i.ClaudeSessionID = old // missing transcript means actual fresh fallback
			i.ClaudeDetectedAt = time.Now()
			before := i.ClaudeDetectedAt
			var err error
			if message {
				err = i.StartWithMessage("hello")
			} else {
				err = i.Start()
			}
			if err == nil || !strings.Contains(err.Error(), "minute_budget") {
				t.Fatalf("expected denial, got %v", err)
			}
			if i.ClaudeSessionID != old || i.ClaudeDetectedAt != before || i.tmuxSession.Exists() {
				t.Fatal("denied launch changed binding or spawned tmux")
			}
			matrixAssertGuardArgs(t, calls, "new", old)
		})
	}
}

func TestMatrixClaudeLaunchGuardDeniedRestartsPreserveOldPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "fresh"}[fresh], func(t *testing.T) {
			home, calls := matrixClaudeGuardFixture(t, false)
			i := newGuardInstance(t, home)
			if err := i.tmuxSession.Start("exec sleep 300"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = i.tmuxSession.Kill() })
			const old = "11111111-2222-4333-8444-555555555555"
			i.ClaudeSessionID = old
			i.ClaudeDetectedAt = time.Now()
			before, generation := i.ClaudeDetectedAt, i.spawnGen.Load()
			paneBefore, err := i.tmuxSession.PanePID()
			if err != nil {
				t.Fatal(err)
			}
			if fresh {
				err = i.RestartFresh()
			} else {
				err = i.Restart()
			}
			if err == nil || !strings.Contains(err.Error(), "minute_budget") {
				t.Fatalf("expected denial, got %v", err)
			}
			paneAfter, err := i.tmuxSession.PanePID()
			if err != nil || paneAfter != paneBefore || !i.tmuxSession.Exists() {
				t.Fatalf("denial stopped/replaced old pane: %v", err)
			}
			if i.ClaudeSessionID != old || i.ClaudeDetectedAt != before || i.spawnGen.Load() != generation {
				t.Fatal("denial mutated old binding/generation")
			}
			matrixAssertGuardArgs(t, calls, "new", "")
		})
	}
}

func TestMatrixClaudeLaunchGuardReceiptAndFinalKind(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "fork"}[fork], func(t *testing.T) {
			home, calls := matrixClaudeGuardFixture(t, true)
			i := newGuardInstance(t, home)
			kind := "new"
			if fork {
				kind = "fork"
				i.ClaudeSessionID = "11111111-2222-4333-8444-555555555555"
				i.IsForkAwaitingStart = true
				i.Command = "exec claude --session-id " + i.ClaudeSessionID + " --resume 22222222-2222-4333-8444-555555555555 --fork-session"
			}
			plan, err := i.planClaudeLaunchAdmission(false, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.cleanup()
			if !fork && i.ClaudeSessionID != "" {
				t.Fatal("planning changed live binding")
			}
			if fork && !i.IsForkAwaitingStart {
				t.Fatal("planning consumed live fork sentinel")
			}
			matrixAssertGuardArgs(t, calls, kind, plan.planned.ClaudeSessionID)
			if strings.Contains(plan.command, strings.Repeat("a", 48)) {
				t.Fatal("ticket leaked into diagnostic command")
			}
			info, err := os.Stat(plan.ticketPath)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("receipt permissions: %v", err)
			}
			// Execute the actual receipt prefix using a local fake Claude process.
			prefix := plan.command[:strings.Index(plan.command, "export "+claudeLaunchTicketEnv+";")+len("export "+claudeLaunchTicketEnv+";")]
			command := prefix + ` printf '%s' "$CLAUDE_LAUNCH_GUARD_TICKET"`
			cmd := exec.Command("/bin/sh", "-c", command)
			cmd.Env = []string{"PATH=/usr/bin:/bin", claudeLaunchTicketEnv + "=ancestor"}
			got, err := cmd.Output()
			if err != nil || string(got) != strings.Repeat("a", 48) {
				t.Fatalf("ticket environment propagation failed: %v", err)
			}
			if _, err := os.Stat(plan.ticketPath); !os.IsNotExist(err) {
				t.Fatal("receipt was not removed after read")
			}
		})
	}
}

func TestMatrixClaudeLaunchGuardResumeNeverCallsHelper(t *testing.T) {
	home, calls := matrixClaudeGuardFixture(t, false)
	i := newGuardInstance(t, home)
	i.ClaudeSessionID = "11111111-2222-4333-8444-555555555555"
	stageConversation(t, home, i.ProjectPath, i.ClaudeSessionID)
	i.markClaudeSessionIDVerified()
	plan, err := i.planClaudeLaunchAdmission(false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || !strings.Contains(plan.command, "--resume "+i.ClaudeSessionID) || !strings.Contains(plan.command, "unset "+claudeLaunchTicketEnv) {
		t.Fatal("resume lost exact binding or inherited ticket strip")
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("ordinary resume called reservation helper")
	}
}

func TestMatrixClaudeLaunchGuardFreshRestartReservesExactlyOnce(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	home, calls := matrixClaudeGuardFixture(t, true)
	i := newGuardInstance(t, home)
	if err := i.tmuxSession.Start("exec sleep 300"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = i.Kill() })
	i.ClaudeSessionID = "11111111-2222-4333-8444-555555555555"
	i.ClaudeDetectedAt = time.Now()
	sink := filepath.Join(home, "native-launch")
	fake := filepath.Join(home, "fake-claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$CLAUDE_LAUNCH_GUARD_TICKET\" \"$@\" > " + shellescape.Quote(sink) + "\nexec sleep 300\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadUserConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Claude.Command = fake
	if err := SaveUserConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ClearUserConfigCache()
	if err := i.RestartFresh(); err != nil {
		t.Fatal(err)
	}
	matrixAssertGuardArgs(t, calls, "new", i.ClaudeSessionID)
	deadline := time.Now().Add(3 * time.Second)
	var native []byte
	for time.Now().Before(deadline) {
		native, err = os.ReadFile(sink)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || !strings.HasPrefix(string(native), strings.Repeat("a", 48)+"\n") || !strings.Contains(string(native), "--session-id\n"+i.ClaudeSessionID+"\n") {
		t.Fatalf("native fake Claude did not receive matching admitted UUID/ticket: %v", err)
	}
}

func TestMatrixClaudeLaunchGuardConfigIndependentAndFailClosed(t *testing.T) {
	home, _ := matrixClaudeGuardFixture(t, true)
	i := newGuardInstance(t, home)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	plan, err := i.planClaudeLaunchAdmission(false, false, nil)
	if err != nil || plan == nil {
		t.Fatalf("guard was redirected by account HOME/config: %v", err)
	}
	plan.cleanup()
	if err := os.Remove(filepath.Join(home, ".local", "bin", "claude-launch-guard")); err != nil {
		t.Fatal(err)
	}
	if _, err := i.planClaudeLaunchAdmission(false, false, nil); err == nil {
		t.Fatal("enabled missing helper failed open")
	}
	marker := filepath.Join(home, ".local", "lib", "claude-launch-guard", "enabled.json")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if plan, err := i.planClaudeLaunchAdmission(false, false, nil); err != nil || plan != nil {
		t.Fatalf("portable disabled behavior changed: %v", err)
	}
	if err := os.Symlink(filepath.Join(home, "guard-calls"), marker); err != nil {
		t.Fatal(err)
	}
	if _, err := i.planClaudeLaunchAdmission(false, false, nil); err == nil {
		t.Fatal("symlinked enabled marker failed open")
	}
}

func TestMatrixClaudeLaunchGuardInvalidReceiptsNeverAuthorize(t *testing.T) {
	for _, test := range []struct{ name, body, code string }{
		{"malformed", "not json", "0"},
		{"missing-ticket", `{"allowed":true}`, "0"},
		{"error-despite-ticket", `{"allowed":true,"ticket":"` + strings.Repeat("a", 48) + `"}`, "78"},
		{"denied-with-success", `{"allowed":false,"reason":"no"}`, "0"},
		{"oversized", strings.Repeat("x", 17*1024), "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, _ := matrixClaudeGuardFixture(t, true)
			script := "#!/bin/sh\nprintf '%s' " + shellescape.Quote(test.body) + "\nexit " + test.code + "\n"
			if err := os.WriteFile(filepath.Join(home, ".local", "bin", "claude-launch-guard"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			i := newGuardInstance(t, home)
			if _, err := i.planClaudeLaunchAdmission(false, false, nil); err == nil {
				t.Fatal("invalid helper receipt authorized a launch")
			}
			if i.ClaudeSessionID != "" {
				t.Fatal("failed receipt changed live binding")
			}
		})
	}
}

func TestMatrixClaudeLaunchGuardUnsupportedPlansPreservePane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	for _, test := range []struct {
		name, wrapper string
		args          []string
	}{
		{"resume-selector", "", []string{"--resume", "11111111-2222-4333-8444-555555555555"}},
		{"fork-selector", "", []string{"--fork-session"}},
		{"background", "", []string{"--background"}},
		{"cloud", "", []string{"--cloud"}},
		{"altering-wrapper", "{command} --fork-session", nil},
		{"replacing-wrapper", "exec claude", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, calls := matrixClaudeGuardFixture(t, true)
			i := newGuardInstance(t, home)
			if err := i.tmuxSession.Start("exec sleep 300"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = i.tmuxSession.Kill() })
			i.ClaudeSessionID = "11111111-2222-4333-8444-555555555555"
			i.ExtraArgs, i.Wrapper = test.args, test.wrapper
			before, _ := i.tmuxSession.PanePID()
			if err := i.RestartFresh(); err == nil {
				t.Fatal("unsupported plan authorized")
			}
			after, err := i.tmuxSession.PanePID()
			if err != nil || before != after || i.ClaudeSessionID != "11111111-2222-4333-8444-555555555555" {
				t.Fatalf("rejection damaged old binding/pane: %v", err)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatal("unsupported plan called admission helper")
			}
			marker := filepath.Join(home, ".local", "lib", "claude-launch-guard", "enabled.json")
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			if p, err := i.planClaudeLaunchAdmission(true, false, nil); err != nil || p != nil {
				t.Fatal("disabled machine changed unsupported-plan behavior")
			}
		})
	}
}

func TestMatrixClaudeLaunchGuardOrphanBackupResumesWithoutReservation(t *testing.T) {
	home, calls := matrixClaudeGuardFixture(t, false)
	i := newGuardInstance(t, home)
	i.ClaudeSessionID = "11111111-2222-4333-8444-555555555555"
	i.markClaudeSessionIDVerified()
	live := stageConversation(t, home, i.ProjectPath, i.ClaudeSessionID)
	if err := os.Rename(live, live+".bak-123456"); err != nil {
		t.Fatal(err)
	}
	plan, err := i.planClaudeLaunchAdmission(false, true, nil)
	if err != nil || plan == nil || !strings.Contains(plan.command, "--resume "+i.ClaudeSessionID) {
		t.Fatalf("owned orphan incorrectly planned as fresh: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("owned orphan was not restored")
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("orphan resume consumed new-session budget")
	}
}

func TestMatrixClaudeLaunchGuardFreshNormalizesPersistentSelectors(t *testing.T) {
	for _, selector := range []string{"--session-id", "--resume", "-r", "-c"} {
		t.Run(selector, func(t *testing.T) {
			home, _ := matrixClaudeGuardFixture(t, true)
			i := newGuardInstance(t, home)
			old := "11111111-2222-4333-8444-555555555555"
			i.Command = "claude " + selector + " " + old
			i.ClaudeSessionID = old
			stageConversation(t, home, i.ProjectPath, old)
			plan, err := i.planClaudeLaunchAdmission(true, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.cleanup()
			if plan.planned.ClaudeSessionID == old || plan.planned.claudeLaunchKind != "new" {
				t.Fatal("fresh plan retained old conversation")
			}
			i.applyClaudeLaunchAdmission(plan)
			if i.Command != "claude" {
				t.Fatal("normalized selector not persisted")
			}
			next, err := i.planClaudeLaunchAdmission(false, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer next.cleanup()
			if next.planned.ClaudeSessionID != i.ClaudeSessionID || next.planned.ClaudeSessionID == old {
				t.Fatal("next restart re-adopted stale command UUID")
			}
		})
	}
}

func TestMatrixClaudeLaunchGuardFinalEnvironmentOwnsTicket(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "resume"}[resume], func(t *testing.T) {
			home, _ := matrixClaudeGuardFixture(t, true)
			i := newGuardInstance(t, home)
			cfg, err := LoadUserConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Shell.InitScript = "export " + claudeLaunchTicketEnv + "=startup_override"
			if err := SaveUserConfig(cfg); err != nil {
				t.Fatal(err)
			}
			ClearUserConfigCache()
			if resume {
				i.ClaudeSessionID = "11111111-2222-4333-8444-555555555555"
				stageConversation(t, home, i.ProjectPath, i.ClaudeSessionID)
			}
			plan, err := i.planClaudeLaunchAdmission(false, true, map[string]string{claudeLaunchTicketEnv: "restart_override"})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.cleanup()
			cut := strings.Index(plan.command, "exec ")
			if cut < 0 {
				t.Fatal("native invocation not found")
			}
			command := plan.command[:cut] + `printf '%s' "${CLAUDE_LAUNCH_GUARD_TICKET-unset}"`
			cmd := exec.Command("/bin/sh", "-c", command)
			cmd.Env = []string{"PATH=/usr/bin:/bin", claudeLaunchTicketEnv + "=ancestor"}
			got, err := cmd.Output()
			want := strings.Repeat("a", 48)
			if resume {
				want = "unset"
			}
			if err != nil || string(got) != want {
				t.Fatalf("configured/one-shot environment overrode admission: %v", err)
			}
		})
	}
}

func TestMatrixClaudeLaunchGuardCrossHarnessPreservesStagedPrompt(t *testing.T) {
	home, calls := matrixClaudeGuardFixture(t, true)
	export := syntheticTransferExport("codex")
	native, err := BuildFreshTargetLaunchPlan(export, FreshTargetLaunchOptions{TargetHarness: "claude", TargetProjectPath: home, TargetAccountHome: filepath.Join(home, ".claude")})
	if err != nil {
		t.Fatal(err)
	}
	literal := "中文首条消息\nquotes ' \" and literal $(never-run) `never-run`"
	native.Prompt = []byte(literal)
	if err := stageCrossHarnessPayload(native); err != nil {
		t.Fatal(err)
	}
	target := newCrossHarnessTarget(native)
	target.crossHarnessLaunch = native
	plan, err := target.planClaudeLaunchAdmission(false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.cleanup()
	if !plan.embeddedPrompt || !strings.Contains(plan.command, native.PayloadPath) || strings.Contains(plan.command, literal) {
		t.Fatal("staged prompt lost or exposed in command")
	}
	matrixAssertGuardArgs(t, calls, "new", native.Target.SessionID)
	sink := filepath.Join(home, "cross-native-prompt")
	bin := filepath.Join(home, "fake-bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nfor last do :; done\nprintf '%s' \"$last\" > " + shellescape.Quote(sink) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", plan.command)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sink)
	if err != nil || string(got) != literal {
		t.Fatalf("native staged prompt bytes changed: %v", err)
	}
}
