package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/asheshgoplani/agent-deck/internal/git"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: every repository, native transcript and aggregation directory
// belongs to this fixture; the production fork builder never invokes Pi here.
type matrixMultiRepoForkFixture struct {
	home, worktreesRoot, transcript string
	source                          *session.Instance
	repos, destructionMarkers       []string
	dirtyFiles                      map[string]string
}

// matrix/hardened: model the real multi-repo layout, where each worktree lives
// inside the source aggregation directory rather than beside it.
func newMatrixMultiRepoForkFixture(t *testing.T, withWorktrees bool) *matrixMultiRepoForkFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := setXDGTestHome(t)
	t.Setenv("AGENT_DECK_HOME", "")
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	socketName := "matrix-multi-fork-" + session.GenerateID()[:8]
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
	fixture := &matrixMultiRepoForkFixture{home: home, dirtyFiles: make(map[string]string)}
	for _, name := range []string{"repo-a", "repo-b"} {
		repo := filepath.Join(home, name)
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		if resolved, err := filepath.EvalSymlinks(repo); err == nil {
			repo = resolved
		}
		gitMustUI(t, repo, "init", "-qb", "main")
		gitMustUI(t, repo, "config", "user.name", "Multi Repo Fork Test")
		gitMustUI(t, repo, "config", "user.email", "multi-fork@example.invalid")
		for name, body := range map[string]string{"tracked.txt": "base\n", ".gitignore": "ignored.txt\n"} {
			if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gitMustUI(t, repo, "add", "tracked.txt", ".gitignore")
		gitMustUI(t, repo, "commit", "-qm", "fixture")
		fixture.repos = append(fixture.repos, repo)
	}
	root, err := multiRepoWorktreesRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.worktreesRoot = root
	source := session.NewInstanceWithTool("Multi repo source", fixture.repos[0], "pi")
	source.GroupPath = "team/multi"
	source.Command = "pi --model fixture-model"
	container := filepath.Join(root, "source-"+source.ID[:8])
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	var result session.MultiRepoWorktreeResult
	if withWorktrees {
		result, err = session.CreateMultiRepoWorktreesStrictWithOptions(fixture.repos, container, "shared-multi-feature", session.WorktreeSettings{})
		if err != nil || len(result.Worktrees) != 2 || len(result.MappedPaths) != 2 {
			t.Fatalf("create two real multi-repo worktrees: result %+v, error %v", result, err)
		}
	} else {
		for _, repo := range fixture.repos {
			link := filepath.Join(container, filepath.Base(repo))
			if err := os.Symlink(repo, link); err != nil {
				t.Fatal(err)
			}
			result.MappedPaths = append(result.MappedPaths, link)
		}
	}
	source.ProjectPath = result.MappedPaths[0]
	source.AdditionalPaths = append([]string(nil), result.MappedPaths[1:]...)
	source.MultiRepoEnabled = true
	source.MultiRepoTempDir = container
	source.MultiRepoWorktrees = result.Worktrees
	source.GetTmuxSession().WorkDir = container
	fixture.source = source
	transcriptDir := filepath.Join(home, ".pi", "agent-deck", source.ID)
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.transcript = filepath.Join(transcriptDir, "session.jsonl")
	if err := os.WriteFile(fixture.transcript, []byte("{\"type\":\"session\",\"version\":3,\"id\":\"fixture-multi-source\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, worktree := range source.MultiRepoWorktrees {
		path := filepath.Join(worktree.WorktreePath, "tracked.txt")
		if err := os.WriteFile(path, []byte("base\nstaged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitMustUI(t, worktree.WorktreePath, "add", "tracked.txt")
		for name, body := range map[string]string{
			"tracked.txt":   "base\nstaged\nunstaged\n",
			"untracked.txt": "untracked\n",
			"ignored.txt":   "ignored\n",
		} {
			path := filepath.Join(worktree.WorktreePath, name)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			fixture.dirtyFiles[path] = body
		}
	}
	return fixture
}

// matrix/hardened: capture Git's own branch/worktree registry and staged state;
// a clone with equal metadata alone does not prove no workspace was created.
func (fixture *matrixMultiRepoForkFixture) snapshot(t *testing.T) map[string]string {
	t.Helper()
	state := make(map[string]string)
	for _, repo := range fixture.repos {
		for _, args := range [][]string{{"for-each-ref", "--format=%(refname)", "refs/heads"}, {"worktree", "list", "--porcelain"}} {
			state[repo+"/"+strings.Join(args, " ")] = gitOutUI(t, repo, args...)
		}
	}
	for _, worktree := range fixture.source.MultiRepoWorktrees {
		for _, args := range [][]string{
			{"status", "--porcelain=v1", "--untracked-files=all", "--ignored"},
			{"diff", "--binary"}, {"diff", "--cached", "--binary"},
		} {
			state[worktree.WorktreePath+"/"+strings.Join(args, " ")] = gitOutUI(t, worktree.WorktreePath, args...)
		}
	}
	entries, err := os.ReadDir(fixture.worktreesRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	state["aggregation directories"] = strings.Join(names, "\n")
	return state
}

// matrix/hardened: keep the genuine instance builder and aggregation creator;
// only start is replaced, so assertions inspect the prepared native launch.
func (fixture *matrixMultiRepoForkFixture) forkWithoutStarting(t *testing.T, inPlace bool) *session.Instance {
	t.Helper()
	deps := defaultForkInstanceDeps()
	createMultiRepoDir := deps.createMultiRepoDir
	created, started := 0, 0
	deps.createMultiRepoDir = func(child, source *session.Instance) error {
		created++
		return createMultiRepoDir(child, source)
	}
	deps.startInstance = func(child *session.Instance) error {
		started++
		if child.GetTmuxSession() == nil || child.GetTmuxSession().WorkDir != child.EffectiveWorkingDir() {
			t.Fatalf("native start directory differs from effective directory: %+v", child)
		}
		return nil
	}
	deps.rollback = func(_, _, _ string) { t.Fatal("fork without a new worktree attempted rollback") }
	var opts *session.ClaudeOptions
	if inPlace {
		// The entry prepares this directory before the native builder runs;
		// completeFork restores the declared primary repository afterward.
		opts = prepareInPlaceForkOptions(fixture.source, nil)
	}
	child, err := completeFork(fixture.source, "Multi repo child", fixture.source.GroupPath, forkToggles{InPlace: inPlace}, opts, "", "", false, deps)
	if err != nil {
		t.Fatalf("complete multi-repo fork: %v", err)
	}
	wantCreated := 1
	if inPlace {
		wantCreated = 0
	}
	if created != wantCreated || started != 1 {
		t.Fatalf("fork lifecycle calls: aggregation creator %d (want %d), start %d (want 1)", created, wantCreated, started)
	}
	if child.ID == fixture.source.ID || child.Tool != fixture.source.Tool || !child.IsMultiRepo() {
		t.Fatalf("fork lost independent instance identity or multi-repo mode: %+v", child)
	}
	return child
}

// matrix/hardened: Alt+F shares the actual aggregation directory and repository
// entries, but each session owns independent metadata slices.
func TestMatrixForkInPlace_MultiRepoCompleteForkSharesDirectory(t *testing.T) {
	fixture := newMatrixMultiRepoForkFixture(t, true)
	before := fixture.snapshot(t)
	child := fixture.forkWithoutStarting(t, true)
	source := fixture.source
	if child.ProjectPath != source.ProjectPath || child.MultiRepoTempDir != source.MultiRepoTempDir ||
		child.EffectiveWorkingDir() != source.EffectiveWorkingDir() || !reflect.DeepEqual(child.AllProjectPaths(), source.AllProjectPaths()) ||
		!reflect.DeepEqual(child.MultiRepoWorktrees, source.MultiRepoWorktrees) {
		t.Fatalf("in-place fork moved a multi-repo path: source %+v, child %+v", source, child)
	}
	if child.WorktreePath != "" || child.WorktreeRepoRoot != "" || child.WorktreeBranch != "" {
		t.Fatal("multi-repo in-place fork acquired single-repository worktree metadata")
	}
	if after := fixture.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("in-place fork changed the Git state or aggregation directories:\nbefore %#v\nafter %#v", before, after)
	}
	for path, want := range fixture.dirtyFiles {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("in-place fork changed dirty file %s: %q, error %v", path, got, err)
		}
	}
	child.AdditionalPaths[0] = "child-only-path"
	child.MultiRepoWorktrees[0].Branch = "child-only-branch"
	if source.AdditionalPaths[0] == child.AdditionalPaths[0] || source.MultiRepoWorktrees[0].Branch == child.MultiRepoWorktrees[0].Branch {
		t.Fatal("editing child metadata changed the source slices")
	}
	source.AdditionalPaths[0] = "source-only-path"
	source.MultiRepoWorktrees[0].Branch = "source-only-branch"
	if child.AdditionalPaths[0] != "child-only-path" || child.MultiRepoWorktrees[0].Branch != "child-only-branch" {
		t.Fatal("editing source metadata changed the child slices")
	}
}

// matrix/hardened: passing a transient container cwd must retain global Claude
// defaults when the source has no saved options, and preserve explicit options.
// Build the native Claude command without starting the host Claude executable.
func TestMatrixForkInPlace_MultiRepoClaudeLaunchOptions(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "global defaults"
		if saved {
			name = "saved options"
		}
		t.Run(name, func(t *testing.T) {
			home := setXDGTestHome(t)
			t.Setenv("AGENT_DECK_HOME", "")
			writeXDGTestConfig(t, home, "[claude]\ndefault_model = \"global-model\"\ndangerous_mode = true\n[launch]\ninject_identity = false\n")
			source := session.NewInstanceWithTool("Claude multi source", t.TempDir(), "claude")
			source.ClaudeSessionID = "11111111-2222-3333-4444-555555555555"
			source.ClaudeDetectedAt = time.Now()
			source.MultiRepoEnabled = true
			source.MultiRepoTempDir = t.TempDir()
			source.AdditionalPaths = []string{t.TempDir()}
			var opts *session.ClaudeOptions
			wantModel, wantSkipPermissions := "global-model", true
			if saved {
				opts = &session.ClaudeOptions{Model: "saved-model", AutoMode: true, WorkDir: source.ProjectPath}
				if err := source.SetClaudeOptions(opts); err != nil {
					t.Fatal(err)
				}
				wantModel, wantSkipPermissions = opts.Model, opts.SkipPermissions
			} else if len(source.ToolOptionsJSON) != 0 {
				t.Fatal("nil-options fixture unexpectedly has persisted tool options")
			}
			prepared := prepareInPlaceForkOptions(source, opts)
			if prepared.WorkDir != source.EffectiveWorkingDir() || prepared.Model != wantModel || prepared.SkipPermissions != wantSkipPermissions {
				t.Fatalf("container cwd preparation changed Claude launch defaults: %+v", prepared)
			}
			if opts != nil && (opts.WorkDir != source.ProjectPath || prepared == opts || !prepared.AutoMode) {
				t.Fatal("container cwd preparation mutated or replaced saved options")
			}
			deps := defaultForkInstanceDeps()
			deps.createMultiRepoDir = func(_, _ *session.Instance) error {
				t.Fatal("in-place Claude fork created a new aggregation directory")
				return nil
			}
			deps.startInstance = func(_ *session.Instance) error { return nil }
			child, err := completeFork(source, "Claude multi child", "", forkToggles{InPlace: true}, prepared, "", "", false, deps)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(child.Command, "cd '"+source.EffectiveWorkingDir()+"' && ") ||
				!strings.Contains(child.Command, "--model "+wantModel) ||
				strings.Contains(child.Command, "--dangerously-skip-permissions") != wantSkipPermissions ||
				!strings.Contains(child.Command, "--resume "+source.ClaudeSessionID+" --fork-session") {
				t.Fatalf("native Claude fork command lost its cwd or launch defaults: %s", child.Command)
			}
			if child.ProjectPath != source.ProjectPath || child.GetTmuxSession().WorkDir != source.EffectiveWorkingDir() || child.ClaudeSessionID == source.ClaudeSessionID {
				t.Fatal("native Claude fork lost source paths or independent conversation identity")
			}
		})
	}
}

// matrix/hardened: drive the real quick-fork entry through native Start and a
// fixture Pi executable; the observed process cwd must be the aggregation dir.
func TestMatrixForkInPlace_MultiRepoAsyncUsesAggregationDirectory(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	fixture := newMatrixMultiRepoForkFixture(t, true)
	before := fixture.snapshot(t)
	binDir := filepath.Join(fixture.home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	argvLog, cwdLog := filepath.Join(fixture.home, "pi-argv"), filepath.Join(fixture.home, "pi-cwd")
	fakePi := "#!/bin/sh\npwd -P > " + shellescape.Quote(cwdLog) + "\nprintf '%s\\n' \"$@\" > " + shellescape.Quote(argvLog) + "\nexec /bin/sleep 60\n"
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(fakePi), 0o700); err != nil {
		t.Fatal(err)
	}
	h := NewGateHome(GateHomeOptions{Instances: []*session.Instance{fixture.source}}).home
	cmd := h.quickForkSessionInPlace(fixture.source)
	if cmd == nil {
		t.Fatalf("in-place multi-repo quick fork failed: %v", h.err)
	}
	msg, ok := cmd().(sessionForkedMsg)
	if !ok || msg.err != nil || msg.instance == nil {
		t.Fatalf("in-place multi-repo command result: %+v", msg)
	}
	child := msg.instance
	t.Cleanup(func() {
		if err := child.KillAndWait(); err != nil {
			t.Errorf("stop fixture fork: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	var argvBytes, cwdBytes []byte
	for time.Now().Before(deadline) {
		argvBytes, _ = os.ReadFile(argvLog)
		cwdBytes, _ = os.ReadFile(cwdLog)
		if len(argvBytes) > 0 && len(cwdBytes) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cwd := strings.TrimSpace(string(cwdBytes)); cwd != fixture.source.EffectiveWorkingDir() {
		t.Fatalf("forked native process cwd = %q, want shared aggregation directory %q", cwd, fixture.source.EffectiveWorkingDir())
	}
	argv := strings.Split(strings.TrimSpace(string(argvBytes)), "\n")
	if !reflect.DeepEqual(argv, []string{"--model", "fixture-model", "--fork", fixture.transcript, "--session-dir", filepath.Join(fixture.home, ".pi", "agent-deck", child.ID)}) {
		t.Fatalf("fake Pi received wrong native fork arguments: %q", argvBytes)
	}
	if child.ProjectPath != fixture.source.ProjectPath || child.MultiRepoTempDir != fixture.source.MultiRepoTempDir || child.GetTmuxSession().WorkDir != fixture.source.EffectiveWorkingDir() {
		t.Fatalf("fork record does not describe its actual shared paths: %+v", child)
	}
	if after := fixture.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("actual native start changed source Git state or aggregation directories:\nbefore %#v\nafter %#v", before, after)
	}
}

// matrix/hardened: trusted fixture hooks prove worktree teardown runs while the
// parent directory still exists, before the final aggregation RemoveAll.
func (fixture *matrixMultiRepoForkFixture) installDestructionMarkers(t *testing.T) {
	t.Helper()
	for i, repo := range fixture.repos {
		scriptDir := filepath.Join(repo, ".agent-deck")
		if err := os.MkdirAll(scriptDir, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(fixture.home, fmt.Sprintf("destruction-%d", i))
		scriptPath := filepath.Join(scriptDir, "worktree-destruction.sh")
		script := "#!/bin/sh\ntest -d " + shellescape.Quote(fixture.source.MultiRepoTempDir) + " || exit 11\nprintf 'removed\\n' > " + shellescape.Quote(marker) + "\n"
		if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := git.TrustScript(repo, "destruction", scriptPath); err != nil {
			t.Fatalf("trust fixture destruction script: %v", err)
		}
		fixture.destructionMarkers = append(fixture.destructionMarkers, marker)
	}
}

// matrix/hardened: execute the production deletion command without creating or
// touching a runtime process; fork startup was separately accepted above.
func matrixDeleteMultiRepoSession(t *testing.T, h *Home, inst *session.Instance) {
	t.Helper()
	inst.SetTmuxSessionForTest(nil)
	msg, ok := h.deleteSession(inst)().(sessionDeletedMsg)
	if !ok || msg.deletedID != inst.ID || msg.killErr != nil {
		t.Fatalf("delete multi-repo session result: %+v", msg)
	}
}

// matrix/hardened: deleting either user must preserve the other user's Git
// registry and dirty files; the last user removes worktrees before their parent.
func TestMatrixForkInPlace_MultiRepoDeletePreservesSharedUser(t *testing.T) {
	fixture := newMatrixMultiRepoForkFixture(t, true)
	child := fixture.forkWithoutStarting(t, true)
	fixture.installDestructionMarkers(t)
	before := fixture.snapshot(t)
	h := NewGateHome(GateHomeOptions{Instances: []*session.Instance{fixture.source, child}}).home
	matrixDeleteMultiRepoSession(t, h, fixture.source)
	if after := fixture.snapshot(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("source deletion changed the surviving fork's Git state:\nbefore %#v\nafter %#v", before, after)
	}
	for path, want := range fixture.dirtyFiles {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("source deletion lost shared dirty file %s: %q, error %v", path, got, err)
		}
	}
	for _, marker := range fixture.destructionMarkers {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("source deletion executed a shared worktree destruction hook: %v", err)
		}
	}
	h.instancesMu.Lock()
	h.instances = []*session.Instance{child}
	h.instancesMu.Unlock()
	matrixDeleteMultiRepoSession(t, h, child)
	if _, err := os.Stat(child.MultiRepoTempDir); !os.IsNotExist(err) {
		t.Fatalf("last user retained shared aggregation directory: %v", err)
	}
	fixture.assertFinalWorktreeCleanup(t)
}

// matrix/hardened: ordinary f still creates a separate aggregation directory,
// whose symlinks must survive deletion of the source containing their worktrees.
func TestMatrixForkInPlace_MultiRepoRegularForkRetainsSharedWorktrees(t *testing.T) {
	fixture := newMatrixMultiRepoForkFixture(t, true)
	child := fixture.forkWithoutStarting(t, false)
	fixture.installDestructionMarkers(t)
	if child.MultiRepoTempDir == fixture.source.MultiRepoTempDir {
		t.Fatal("ordinary multi-repo fork stopped creating its own aggregation directory")
	}
	h := NewGateHome(GateHomeOptions{Instances: []*session.Instance{fixture.source, child}}).home
	matrixDeleteMultiRepoSession(t, h, fixture.source)
	for i, path := range child.AllProjectPaths() {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != fixture.source.MultiRepoWorktrees[i].WorktreePath {
			t.Fatalf("source deletion stranded ordinary fork repository link %q: target %q, error %v", path, resolved, err)
		}
		gitMustUI(t, path, "status", "--porcelain")
	}
	h.instancesMu.Lock()
	h.instances = []*session.Instance{child}
	h.instancesMu.Unlock()
	matrixDeleteMultiRepoSession(t, h, child)
	if _, err := os.Stat(child.MultiRepoTempDir); !os.IsNotExist(err) {
		t.Fatalf("last ordinary fork retained its aggregation directory: %v", err)
	}
	fixture.assertFinalWorktreeCleanup(t)
}

// matrix/hardened: a multi-repo source may aggregate original repositories with
// symlinks only. A regular fork must link the originals directly so its source
// aggregation directory can be deleted without breaking the surviving child.
func TestMatrixForkInPlace_MultiRepoRegularForkLinksOriginalRepositories(t *testing.T) {
	fixture := newMatrixMultiRepoForkFixture(t, false)
	child := fixture.forkWithoutStarting(t, false)
	if len(child.MultiRepoWorktrees) != 0 || child.MultiRepoTempDir == fixture.source.MultiRepoTempDir {
		t.Fatal("symlink-only regular fork unexpectedly created worktrees or reused the source aggregation directory")
	}
	h := NewGateHome(GateHomeOptions{Instances: []*session.Instance{fixture.source, child}}).home
	matrixDeleteMultiRepoSession(t, h, fixture.source)
	if _, err := os.Stat(fixture.source.MultiRepoTempDir); !os.IsNotExist(err) {
		t.Fatalf("direct repository links unnecessarily retained the source aggregation directory: %v", err)
	}
	for i, path := range child.AllProjectPaths() {
		link, err := os.Readlink(path)
		if err != nil || link != fixture.repos[i] {
			t.Fatalf("regular fork repository link %q still depends on the source aggregation directory: target %q, error %v", path, link, err)
		}
		gitMustUI(t, path, "rev-parse", "--verify", "HEAD")
	}
	h.instancesMu.Lock()
	h.instances = []*session.Instance{child}
	h.instancesMu.Unlock()
	matrixDeleteMultiRepoSession(t, h, child)
	if _, err := os.Stat(child.MultiRepoTempDir); !os.IsNotExist(err) {
		t.Fatalf("last symlink-only fork retained its aggregation directory: %v", err)
	}
	for _, repo := range fixture.repos {
		gitMustUI(t, repo, "rev-parse", "--verify", "HEAD")
		if registered := gitOutUI(t, repo, "worktree", "list", "--porcelain"); strings.Count(registered, "worktree ") != 1 {
			t.Fatalf("symlink-only fork altered original repository worktrees: %s", registered)
		}
	}
}

// matrix/hardened: removing bytes without Git's administrative references and
// ordered destruction is incomplete cleanup; original repositories remain valid.
func (fixture *matrixMultiRepoForkFixture) assertFinalWorktreeCleanup(t *testing.T) {
	t.Helper()
	for i, worktree := range fixture.source.MultiRepoWorktrees {
		if _, err := os.Stat(worktree.WorktreePath); !os.IsNotExist(err) {
			t.Fatalf("last user retained worktree %q: %v", worktree.WorktreePath, err)
		}
		registered := gitOutUI(t, worktree.RepoRoot, "worktree", "list", "--porcelain")
		if strings.Contains(registered, worktree.WorktreePath) || strings.Count(registered, "worktree ") != 1 {
			t.Fatalf("last user retained administrative worktree references: %s", registered)
		}
		gitMustUI(t, worktree.RepoRoot, "rev-parse", "--verify", "HEAD")
		if body, err := os.ReadFile(fixture.destructionMarkers[i]); err != nil || string(body) != "removed\n" {
			t.Fatalf("worktree removed before its destruction hook could observe the aggregation directory: marker %q, error %v", body, err)
		}
	}
}
