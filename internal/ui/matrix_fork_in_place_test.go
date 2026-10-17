package ui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// matrix/hardened: a same-directory fork overrides every structural default,
// including inherit_from_parent, without probing or modifying a repository.
func TestMatrixForkInPlace_SpecOverridesStructuralDefaultsWithoutVCS(t *testing.T) {
	probeDir := t.TempDir()
	probeLog := filepath.Join(probeDir, "vcs-probes")
	t.Setenv("MATRIX_FORK_PROBE_LOG", probeLog)
	t.Setenv("PATH", probeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"git", "jj"} {
		if err := os.WriteFile(filepath.Join(probeDir, name), []byte("#!/bin/sh\nprintf '%s\\n' probe >> \"$MATRIX_FORK_PROBE_LOG\"\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	on, off := true, false
	dockerOn, dockerOff := "on", "off"
	for _, test := range []struct {
		name      string
		fork      session.ForkSettings
		sandboxed bool
	}{
		{name: "defaults"},
		{name: "all structural options on", fork: session.ForkSettings{Worktree: &on, WithState: &on, WithIgnored: &on, BranchPrefix: "custom/"}},
		{name: "inherit overrides options", fork: session.ForkSettings{InheritFromParent: true, Worktree: &on, WithState: &on, WithIgnored: &on, BranchPrefix: "custom/"}, sandboxed: true},
		{name: "sandbox on", fork: session.ForkSettings{Docker: &dockerOn}},
		{name: "sandbox off", fork: session.ForkSettings{Docker: &dockerOff}, sandboxed: true},
		{name: "inherit preserves sandbox resolution", fork: session.ForkSettings{InheritFromParent: true, Worktree: &off, WithState: &off, WithIgnored: &off, Docker: &dockerOff}, sandboxed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &session.Instance{Title: "My Feature", GroupPath: "team/feature", ProjectPath: filepath.Join(probeDir, "missing-project")}
			if test.sandboxed {
				source.Sandbox = &session.SandboxConfig{Enabled: true}
			}
			spec := resolveInPlaceForkSpec(source, test.fork)
			if spec.Title != "My Feature (fork)" || spec.GroupPath != source.GroupPath {
				t.Fatalf("same-directory fork lost source title/group: %+v", spec)
			}
			if spec.Branch != "" || spec.Plan.Worktree || spec.Plan.WithState || spec.Plan.WithIgnored {
				t.Fatalf("same-directory fork retained structural settings: %+v", spec)
			}
			if spec.Plan.Sandbox != test.fork.Resolve(test.sandboxed).Sandbox {
				t.Fatalf("same-directory fork changed resolved sandbox setting: %+v", spec.Plan)
			}
		})
	}
	if _, err := os.Stat(probeLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("same-directory spec invoked VCS detection: stat error %v", err)
	}
}

// matrix/hardened: adding Alt+F must preserve the existing f worktree plan.
func TestMatrixForkInPlace_ExistingQuickForkKeepsWorktreePlan(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	gitMustUI(t, repo, "init", "-q")
	source := &session.Instance{Title: "My Feature", GroupPath: "team/feature", ProjectPath: repo}
	on := true
	fork := session.ForkSettings{Worktree: &on, WithState: &on, WithIgnored: &on, InheritFromParent: true, BranchPrefix: "custom/"}
	regular := resolveQuickForkSpec(source, fork)
	inPlace := resolveInPlaceForkSpec(source, fork)
	if regular.Branch != "custom/my-feature" || !regular.Plan.Worktree || !regular.Plan.WithState || !regular.Plan.WithIgnored {
		t.Fatalf("existing quick fork lost its configured worktree plan: %+v", regular)
	}
	if inPlace.Title != regular.Title || inPlace.GroupPath != regular.GroupPath || inPlace.Plan.Sandbox != regular.Plan.Sandbox {
		t.Fatalf("fork actions disagree on non-structural defaults: regular %+v, in-place %+v", regular, inPlace)
	}
}

// matrix/hardened: use the production Home dispatch without constructor workers
// or executing the returned lifecycle command; each fixture owns its source.
func matrixForkInPlaceHome(t *testing.T) (*Home, *session.Instance) {
	t.Helper()
	withTempAgentDeckHome(t, "[fork]\nworktree = true\nwith_state = true\nwith_ignored = true\ninherit_from_parent = true\n")
	source := &session.Instance{ID: "in-place-source", Title: "Feature", GroupPath: "team/feature", ProjectPath: t.TempDir(), Tool: "claude", ClaudeSessionID: "11111111-2222-3333-4444-555555555555", ClaudeDetectedAt: time.Now()}
	h := NewGateHome(GateHomeOptions{Width: 120, Height: 40, Instances: []*session.Instance{source}}).home
	h.flatItems = []session.Item{{Type: session.ItemTypeSession, Session: source}}
	h.cursor = 0
	h.remoteForking = make(map[string]struct{})
	return h, source
}

// matrix/hardened: both terminal Alt+F and macOS Option+F reach the real entry
// through Home.Update as well as direct main-list dispatch.
func TestMatrixForkInPlace_MainKeysReachCommand(t *testing.T) {
	if got := resolveHotkeys(nil)[hotkeyForkInPlace]; got != "alt+f" {
		t.Fatalf("same-directory fork default key = %q, want alt+f", got)
	}
	for _, throughUpdate := range []bool{false, true} {
		for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: true}, {Type: tea.KeyRunes, Runes: []rune{'ƒ'}}} {
			name := "dispatch/" + key.String()
			if throughUpdate {
				name = "update/" + key.String()
			}
			t.Run(name, func(t *testing.T) {
				h, source := matrixForkInPlaceHome(t)
				var cmd tea.Cmd
				if throughUpdate {
					_, cmd = h.Update(key)
				} else {
					_, cmd = h.handleMainKey(key)
				}
				if cmd == nil || h.err != nil {
					t.Fatalf("%q did not start same-directory fork: cmd nil %v, error %v", key.String(), cmd == nil, h.err)
				}
				if _, animated := h.forkingSessions[source.ID]; !animated {
					t.Fatal("same-directory fork did not mark its source as forking")
				}
				if h.forkDialog.IsVisible() || len(h.instances) != 1 {
					t.Fatal("quick same-directory fork opened a dialog or mutated rows before completion")
				}
			})
		}
	}
}

// matrix/hardened: custom bindings own the new action, and disabling it blocks
// both default representations rather than allowing a hardcoded fallback.
func TestMatrixForkInPlace_HotkeyCanBeReboundAndDisabled(t *testing.T) {
	h, source := matrixForkInPlaceHome(t)
	h.setHotkeys(resolveHotkeys(map[string]string{hotkeyForkInPlace: "ctrl+f"}))
	defaults := []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: true}, {Type: tea.KeyRunes, Runes: []rune{'ƒ'}}}
	for _, key := range defaults {
		if _, cmd := h.Update(key); cmd != nil || len(h.forkingSessions) != 0 {
			t.Fatalf("rebound action still responds to %q", key.String())
		}
	}
	if _, cmd := h.Update(tea.KeyMsg{Type: tea.KeyCtrlF}); cmd == nil {
		t.Fatal("configured ctrl+f did not start same-directory fork")
	}
	if _, forking := h.forkingSessions[source.ID]; !forking {
		t.Fatal("configured ctrl+f returned a command without starting a fork")
	}
	delete(h.forkingSessions, source.ID)
	h.setHotkeys(resolveHotkeys(map[string]string{hotkeyForkInPlace: ""}))
	for _, key := range append(defaults, tea.KeyMsg{Type: tea.KeyCtrlF}) {
		// matrix/hardened: Ctrl+F regains its existing half-page navigation
		// after unbinding, which can legitimately return a preview command.
		h.Update(key)
		if len(h.forkingSessions) != 0 {
			t.Fatalf("disabled action still responds to %q", key.String())
		}
	}
}

// matrix/hardened: the additional fork path retains all lifecycle guards and
// shares the in-flight animation guard with the existing f path.
func TestMatrixForkInPlace_LocalGuards(t *testing.T) {
	t.Run("creating placeholder", func(t *testing.T) {
		h, _ := matrixForkInPlaceHome(t)
		h.flatItems = []session.Item{{Type: session.ItemTypeSession, CreatingID: "pending", CreatingTitle: "creating"}}
		_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: true})
		if cmd != nil || !errors.Is(h.err, errSessionStillCreating) {
			t.Fatalf("placeholder fork was not refused: cmd nil %v, error %v", cmd == nil, h.err)
		}
	})
	for _, animation := range []string{"launching", "resuming", "mcp loading", "forking"} {
		t.Run(animation, func(t *testing.T) {
			h, source := matrixForkInPlaceHome(t)
			maps := map[string]map[string]time.Time{"launching": h.launchingSessions, "resuming": h.resumingSessions, "mcp loading": h.mcpLoadingSessions, "forking": h.forkingSessions}
			maps[animation][source.ID] = time.Now()
			_, cmd := h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: true})
			if cmd != nil || h.err == nil || !strings.Contains(h.err.Error(), "please wait") {
				t.Fatalf("%s animation did not guard fork: cmd nil %v, error %v", animation, cmd == nil, h.err)
			}
		})
	}
	for _, firstAlt := range []bool{false, true} {
		name := "f then alt+f"
		if firstAlt {
			name = "alt+f then f"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := matrixForkInPlaceHome(t)
			if _, cmd := h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: firstAlt}); cmd == nil {
				t.Fatal("first quick fork did not start")
			}
			if _, cmd := h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: !firstAlt}); cmd != nil || h.err == nil {
				t.Fatal("other quick fork path bypassed the shared in-flight guard")
			}
		})
	}
}

// matrix/hardened: remote Alt+F shares the actual SSH fork command and durable
// in-flight guard with f, including after animation expiry.
func TestMatrixForkInPlace_RemoteGuardOutlivesAnimation(t *testing.T) {
	h, _ := matrixForkInPlaceHome(t)
	remote := &session.RemoteSessionInfo{ID: "remote-source", Title: "remote feature", Tool: "claude", Group: "team/feature"}
	h.flatItems = []session.Item{{Type: session.ItemTypeRemoteSession, RemoteName: "lab", RemoteSession: remote}}
	h.remoteSessions = map[string][]session.RemoteSessionInfo{"lab": {*remote}}
	altF := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: true}
	if _, cmd := h.Update(altF); cmd == nil {
		t.Fatal("remote Alt+F did not return a fork command")
	}
	forkID := remoteRestartAnimationID("lab", remote.ID)
	if _, inFlight := h.remoteForking[forkID]; !inFlight {
		t.Fatal("remote Alt+F did not acquire the durable in-flight guard")
	}
	h.forkingSessions[forkID] = time.Now().Add(-time.Minute)
	h.cleanupExpiredAnimations(h.forkingSessions, 20*time.Second, 5*time.Second)
	for _, key := range []tea.KeyMsg{altF, {Type: tea.KeyRunes, Runes: []rune{'f'}}} {
		if _, cmd := h.handleMainKey(key); cmd != nil || h.err == nil || !strings.Contains(h.err.Error(), "remote session is forking") {
			t.Fatalf("%q bypassed remote fork guard after animation expiry", key.String())
		}
	}
	h.Update(remoteSessionForkedMsg{remoteName: "lab", sessionID: remote.ID, title: remote.Title, err: errors.New("fixture refusal")})
	if _, inFlight := h.remoteForking[forkID]; inFlight {
		t.Fatal("remote fork result did not release the guard")
	}
	if _, cmd := h.handleMainKey(altF); cmd == nil {
		t.Fatal("remote Alt+F remained blocked after its result")
	}
}

// matrix/hardened: help and the session preview advertise the configured
// same-directory shortcut, and omit an action that the user disables.
func TestMatrixForkInPlace_HelpAndPreviewFollowBinding(t *testing.T) {
	for _, binding := range []string{"alt+f", "ctrl+f", ""} {
		t.Run(binding, func(t *testing.T) {
			h, _ := matrixForkInPlaceHome(t)
			h.setHotkeys(resolveHotkeys(map[string]string{hotkeyForkInPlace: binding}))
			h.helpOverlay.SetSize(200, 300)
			h.helpOverlay.Show()
			help := ansi.Strip(h.helpOverlay.View())
			var preview strings.Builder
			h.renderForkHintLine(&preview)
			for _, frame := range []string{help, ansi.Strip(preview.String())} {
				found := false
				for _, line := range strings.Split(frame, "\n") {
					if strings.Contains(strings.ToLower(line), "same directory") {
						found = true
						if binding == "" || !strings.Contains(line, binding) {
							t.Fatalf("same-directory hint disagrees with binding %q: %s", binding, line)
						}
					}
				}
				if found != (binding != "") {
					t.Fatalf("same-directory hint visibility = %v for binding %q:\n%s", found, binding, frame)
				}
			}
		})
	}
}

// matrix/hardened: execute the real async command in TestMain's private tmux
// socket. A fixture-owned Pi executable records the launch and stays alive;
// the actual checkout, branch list, index and dirty bytes must remain unchanged.
func TestMatrixForkInPlace_AsyncCommandSharesDirtyCheckout(t *testing.T) {
	for _, binary := range []string{"git", "tmux"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s not on PATH", binary)
		}
	}
	home := setXDGTestHome(t)
	// matrix/hardened: use Pi's local transcript fork to keep the integration
	// independent of the host Claude guard, whose home intentionally ignores
	// process HOME. A fresh named socket also binds target-side HOME to this
	// fixture even if other tests have already started their tmux server.
	socketName := "matrix-fork-" + session.GenerateID()[:8]
	writeXDGTestConfig(t, home, fmt.Sprintf(`[fork]
worktree = true
with_state = true
with_ignored = true
inherit_from_parent = true
[launch]
inject_identity = false
[tmux]
launch_as = "direct"
inject_status_line = false
socket_name = %q
`, socketName))
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitMustUI(t, repo, "init", "-q")
	gitMustUI(t, repo, "config", "user.name", "Fork Test")
	gitMustUI(t, repo, "config", "user.email", "fork-test@example.invalid")
	for name, content := range map[string]string{"tracked.txt": "base\n", ".gitignore": "ignored.txt\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitMustUI(t, repo, "add", "tracked.txt", ".gitignore")
	gitMustUI(t, repo, "commit", "-qm", "fixture")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\nstaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitMustUI(t, repo, "add", "tracked.txt")
	dirtyFiles := map[string]string{"tracked.txt": "base\nstaged\nunstaged\n", "untracked.txt": "untracked\n", "ignored.txt": "ignored\n"}
	for name, content := range dirtyFiles {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	queries := [][]string{
		{"status", "--porcelain=v1", "--untracked-files=all", "--ignored"},
		{"diff", "--binary"},
		{"diff", "--cached", "--binary"},
		{"rev-parse", "HEAD"},
		{"symbolic-ref", "HEAD"},
		{"for-each-ref", "--format=%(refname)", "refs/heads"},
		{"worktree", "list", "--porcelain"},
	}
	before := make([]string, len(queries))
	for i, args := range queries {
		before[i] = gitOutUI(t, repo, args...)
	}
	argvLog, cwdLog := filepath.Join(home, "fake-pi-argv"), filepath.Join(home, "fake-pi-cwd")
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Keep this directory off the test process PATH: the production spawn
	// prelude embeds it absolutely, even when a private tmux server retains
	// another test's PATH and environment.
	fakePi := "#!/bin/sh\npwd -P > " + shellescape.Quote(cwdLog) + "\nprintf '%s\\n' \"$@\" > " + shellescape.Quote(argvLog) + "\nexec /bin/sleep 60\n"
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(fakePi), 0o700); err != nil {
		t.Fatal(err)
	}
	source := session.NewInstanceWithTool("Dirty feature", repo, "pi")
	source.GroupPath = "team/feature"
	source.Command = "pi --model fixture-model"
	sourceDir := filepath.Join(home, ".pi", "agent-deck", source.ID)
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceTranscript := filepath.Join(sourceDir, "session.jsonl")
	transcript := []byte("{\"type\":\"session\",\"version\":3,\"id\":\"fixture-native-source\"}\n")
	if err := os.WriteFile(sourceTranscript, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Home{forkingSessions: make(map[string]time.Time)}
	cmd := h.quickForkSessionInPlace(source)
	if cmd == nil {
		t.Fatalf("same-directory async command was not built: %v", h.err)
	}
	msg, ok := cmd().(sessionForkedMsg)
	if !ok {
		t.Fatal("async fork did not return sessionForkedMsg")
	}
	if msg.instance != nil {
		// matrix/hardened: stop through Instance so teardown supersedes and
		// barriers its fast-death watcher, reaps owned processes synchronously,
		// and clears the scratch generation before restoring fixture HOME.
		t.Cleanup(func() {
			if err := msg.instance.KillAndWait(); err != nil {
				t.Errorf("cleanup fork instance: %v", err)
			}
		})
	}
	if msg.err != nil || msg.instance == nil {
		t.Fatalf("async fork failed: %v", msg.err)
	}
	forked := msg.instance
	if msg.sourceID != source.ID || forked.ProjectPath != source.ProjectPath || forked.GroupPath != source.GroupPath || forked.Title != source.Title+" (fork)" {
		t.Fatalf("fork did not retain the source checkout/title/group: source %q %q %q, fork %q %q %q", source.ProjectPath, source.Title, source.GroupPath, forked.ProjectPath, forked.Title, forked.GroupPath)
	}
	if forked.WorktreePath != "" || forked.WorktreeRepoRoot != "" || forked.WorktreeBranch != "" {
		t.Fatalf("same-directory fork retained new worktree metadata: path %q, root %q, branch %q", forked.WorktreePath, forked.WorktreeRepoRoot, forked.WorktreeBranch)
	}
	if forked.ID == source.ID || forked.Tool != "pi" {
		t.Fatal("same-directory fork did not receive a distinct Pi instance")
	}
	if forked.Command != source.Command {
		t.Fatalf("same-directory fork did not inherit launch options: %q, want %q", forked.Command, source.Command)
	}
	deadline := time.Now().Add(5 * time.Second)
	var argvBytes, cwdBytes []byte
	for time.Now().Before(deadline) {
		argvBytes, err = os.ReadFile(argvLog)
		cwdBytes, _ = os.ReadFile(cwdLog)
		if err == nil && len(argvBytes) > 0 && len(cwdBytes) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	argv := "\n" + strings.TrimSpace(string(argvBytes)) + "\n"
	forkDir := filepath.Join(home, ".pi", "agent-deck", forked.ID)
	for _, sequence := range []string{"\n--fork\n" + sourceTranscript + "\n", "\n--session-dir\n" + forkDir + "\n", "\n--model\nfixture-model\n"} {
		if !strings.Contains(argv, sequence) {
			t.Fatalf("fake Pi did not receive actual native fork arguments %q; argv %q", sequence, argv)
		}
	}
	if cwd := strings.TrimSpace(string(cwdBytes)); cwd != repo {
		t.Fatalf("fork process cwd = %q, want shared checkout %q", cwd, repo)
	}
	for i, args := range queries {
		if after := gitOutUI(t, repo, args...); after != before[i] {
			t.Errorf("fork changed git %s:\nbefore %q\nafter %q", strings.Join(args, " "), before[i], after)
		}
	}
	for name, want := range dirtyFiles {
		got, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil || string(got) != want {
			t.Errorf("fork changed source file %s: %q, error %v", name, got, err)
		}
	}
	if got, err := os.ReadFile(sourceTranscript); err != nil || string(got) != string(transcript) {
		t.Errorf("fork changed source transcript: %q, error %v", got, err)
	}
}
