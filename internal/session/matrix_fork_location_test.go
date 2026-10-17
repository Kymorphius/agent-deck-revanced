package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// matrix/hardened: test the direct tool builders, since CLI, TUI and Web do not
// all reach forks through the same dispatcher.
var matrixForkLocationTools = []string{"claude", "opencode", "pi", "codex", "omp"}

func matrixForkLocationSetup(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
}

func matrixForkLocationParent(t *testing.T, tool, projectPath string) *Instance {
	t.Helper()
	parent := NewInstanceWithTool("parent", projectPath, tool)
	parent.Command = tool
	switch tool {
	case "claude":
		parent.ClaudeSessionID = "11111111-2222-3333-4444-555555555555"
		parent.ClaudeDetectedAt = time.Now()
	case "opencode":
		parent.OpenCodeSessionID = "ses_parent_123"
		parent.OpenCodeDetectedAt = time.Now()
	case "codex":
		parent.CodexSessionID = "11111111-2222-3333-4444-555555555555"
		parent.CodexDetectedAt = time.Now()
		seedCodexRollout(t, os.Getenv("CODEX_HOME"), parent.CodexSessionID)
	case "pi", "omp":
		dir := filepath.Join(os.Getenv("HOME"), "."+tool, "agent-deck", parent.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "parent.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return parent
}

func matrixCreateLocationFork(t *testing.T, parent *Instance, opts *ClaudeOptions) *Instance {
	t.Helper()
	var child *Instance
	var err error
	switch parent.Tool {
	case "claude":
		child, _, err = parent.CreateForkedInstanceWithOptions("child", "", opts)
	case "opencode":
		var openCodeOpts *OpenCodeOptions
		if opts != nil {
			openCodeOpts = &OpenCodeOptions{Model: opts.Model}
		}
		if opts != nil && opts.WorkDir != "" {
			child, _, err = parent.CreateForkedOpenCodeInstanceWithOptionsAndWorkDir("child", "", openCodeOpts, opts.WorkDir, opts.WorktreeRepoRoot, opts.WorktreeBranch)
		} else {
			child, _, err = parent.CreateForkedOpenCodeInstanceWithOptions("child", "", openCodeOpts)
		}
	case "pi":
		child, _, err = parent.CreateForkedPiInstanceWithOptions("child", "", opts)
	case "codex":
		child, _, err = parent.CreateForkedCodexInstanceWithOptions("child", "", opts)
	case "omp":
		child, _, err = parent.CreateForkedOMPInstanceWithOptions("child", "", opts)
	}
	if err != nil {
		t.Fatalf("direct %s fork: %v", parent.Tool, err)
	}
	return child
}

func matrixAssertForkLocation(t *testing.T, got, want *Instance, checkType bool) {
	t.Helper()
	if got.WorktreePath != want.WorktreePath || got.WorktreeRepoRoot != want.WorktreeRepoRoot ||
		got.WorktreeBranch != want.WorktreeBranch || (checkType && got.WorktreeType != want.WorktreeType) {
		t.Fatalf("worktree metadata = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
			got.WorktreePath, got.WorktreeRepoRoot, got.WorktreeBranch, got.WorktreeType,
			want.WorktreePath, want.WorktreeRepoRoot, want.WorktreeBranch, want.WorktreeType)
	}
}

// matrix/hardened: nil and regular launch options must both retain the shared
// reference, including when the parent's project is below the worktree root.
func TestMatrixForkLocationDirectBuildersShareAndPersist(t *testing.T) {
	matrixForkLocationSetup(t)
	worktree := t.TempDir()
	subdir := filepath.Join(worktree, "nested", "project")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(worktree, alias); err != nil {
		t.Fatal(err)
	}
	for _, tool := range matrixForkLocationTools {
		for _, location := range []struct{ name, project string }{
			{"root", worktree}, {"subdir", subdir}, {"symlink", filepath.Join(alias, "nested", "project")},
		} {
			for _, regularOptions := range []bool{false, true} {
				name := tool + "/" + location.name + "/nil"
				if regularOptions {
					name = tool + "/" + location.name + "/options"
				}
				t.Run(name, func(t *testing.T) {
					parent := matrixForkLocationParent(t, tool, location.project)
					parent.WorktreePath, parent.WorktreeRepoRoot = worktree, filepath.Dir(worktree)
					parent.WorktreeBranch, parent.WorktreeType = "parent-branch", "jujutsu"
					var opts *ClaudeOptions
					if regularOptions {
						opts = &ClaudeOptions{Model: "test-model"}
					}
					child := matrixCreateLocationFork(t, parent, opts)
					matrixAssertForkLocation(t, child, parent, true)
					if child.ProjectPath != location.project || parent.ProjectPath != location.project || child.ID == parent.ID {
						t.Fatal("shared fork must keep the requested project path and create a new instance")
					}
					if opts != nil && (opts.WorktreePath != "" || opts.WorktreeRepoRoot != "" || opts.WorktreeBranch != "") {
						t.Fatal("inheriting a shared directory must not alter launch options")
					}
					if !OtherSessionsShareWorktree(parent, []*Instance{child}) {
						t.Fatal("parent cleanup does not recognize the shared fork")
					}
					storage := newTestStorage(t)
					if err := storage.Save([]*Instance{child}); err != nil {
						t.Fatalf("persist child: %v", err)
					}
					loaded, err := storage.Load()
					if err != nil || len(loaded) != 1 {
						t.Fatalf("read persisted fork: instances=%d error=%v", len(loaded), err)
					}
					// WorktreeType has no existing storage representation; the three
					// cleanup references must survive the current storage contract.
					matrixAssertForkLocation(t, loaded[0], parent, false)
					if !OtherSessionsShareWorktree(parent, loaded) {
						t.Fatal("persisted fork lost shared cleanup protection")
					}
				})
			}
		}
	}
}

// matrix/hardened: ownership follows the actual destination, including symlinks
// into and out of the parent's worktree, rather than a textual path prefix.
func TestMatrixForkLocationDestinationAndExplicitWorktree(t *testing.T) {
	matrixForkLocationSetup(t)
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	subdir := filepath.Join(worktree, "subdir")
	outside := filepath.Join(root, "elsewhere")
	prefixSibling := filepath.Join(root, "worktree-other")
	for _, dir := range []string{subdir, outside, prefixSibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	escape := filepath.Join(worktree, "escape")
	if err := os.Symlink(subdir, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	for _, tool := range matrixForkLocationTools {
		for _, location := range []struct {
			name, dir string
			share     bool
			explicit  bool
		}{
			{"root", worktree, true, false}, {"subdir", subdir, true, false},
			{"symlink-in", alias, true, false}, {"other", outside, false, false},
			{"prefix-sibling", prefixSibling, false, false}, {"symlink-out", escape, false, false},
			{"explicit-new-worktree", subdir, false, true},
		} {
			t.Run(tool+"/"+location.name, func(t *testing.T) {
				parent := matrixForkLocationParent(t, tool, worktree)
				parent.WorktreePath, parent.WorktreeRepoRoot = worktree, root
				parent.WorktreeBranch, parent.WorktreeType = "parent-branch", "jujutsu"
				opts := &ClaudeOptions{WorkDir: location.dir}
				want := &Instance{}
				if location.share {
					want = parent
				}
				if location.explicit {
					opts.WorktreePath, opts.WorktreeRepoRoot, opts.WorktreeBranch = location.dir, "new-repo", "new-branch"
					want = &Instance{WorktreePath: location.dir, WorktreeRepoRoot: "new-repo", WorktreeBranch: "new-branch"}
				}
				child := matrixCreateLocationFork(t, parent, opts)
				matrixAssertForkLocation(t, child, want, true)
				if child.ProjectPath != location.dir || parent.ProjectPath != worktree {
					t.Fatal("fork location inheritance changed source or destination project path")
				}
			})
		}
	}
}

// matrix/hardened: prove the destructive boundary using a real linked worktree
// and a persisted fork; the parent's dismissal keeps the directory and branch,
// while dismissal of the final child can still clean up its dedicated worktree.
func TestMatrixForkLocationParentDismissKeepsChildWorktree(t *testing.T) {
	matrixForkLocationSetup(t)
	for _, tool := range matrixForkLocationTools {
		t.Run(tool, func(t *testing.T) {
			repo := issue1200InitRepo(t)
			branch := "shared-fork-" + tool
			worktree := issue1200AddWorktree(t, repo, branch)
			project := filepath.Join(worktree, "project")
			if err := os.MkdirAll(project, 0o755); err != nil {
				t.Fatal(err)
			}
			parent := matrixForkLocationParent(t, tool, project)
			parent.WorktreePath, parent.WorktreeRepoRoot, parent.WorktreeBranch, parent.WorktreeType = worktree, repo, branch, "git"
			before, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
			if err != nil {
				t.Fatal(err)
			}
			child := matrixCreateLocationFork(t, parent, nil)
			after, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
			if err != nil || string(before) != string(after) {
				t.Fatalf("shared fork changed Git worktree registration: error=%v\nbefore=%s\nafter=%s", err, before, after)
			}
			storage := newTestStorage(t)
			if err := storage.Save([]*Instance{child}); err != nil {
				t.Fatal(err)
			}
			loaded, err := storage.Load()
			if err != nil || len(loaded) != 1 {
				t.Fatalf("read persisted fork: instances=%d error=%v", len(loaded), err)
			}
			removed, err := RemoveSessionWorktreeUnlessShared(parent, loaded)
			if err != nil || removed {
				t.Fatalf("parent dismissal removed shared directory: removed=%v error=%v", removed, err)
			}
			if _, err := os.Stat(project); err != nil || !sharedWtBranchExists(t, repo, branch) {
				t.Fatalf("parent dismissal damaged child's directory or branch: %v", err)
			}
			removed, err = RemoveSessionWorktreeUnlessShared(loaded[0], nil)
			if err != nil || !removed {
				t.Fatalf("final child dismissal did not clean up: removed=%v error=%v", removed, err)
			}
			if _, err := os.Stat(worktree); !os.IsNotExist(err) {
				t.Fatalf("final child worktree still exists: %v", err)
			}
			remaining, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
			if err != nil || strings.Contains(string(remaining), "refs/heads/"+branch) {
				t.Fatalf("final child left a linked worktree registration: error=%v output=%s", err, remaining)
			}
			if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
				t.Fatalf("cleanup damaged the original repository: %v", err)
			}
		})
	}
}
