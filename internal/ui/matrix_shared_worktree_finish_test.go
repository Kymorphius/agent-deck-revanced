package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/web"
)

// matrix/hardened: the headless Web path uses its freshly hydrated registry;
// merging a parent must leave the child's real git directory and branch usable.
func TestMatrixSharedWorktreeFinishWeb(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("AGENT_DECK_HOME", "")
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	repo, wt, branch := filepath.Join(home, "repo"), filepath.Join(home, "worktree"), "shared-feature"
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(path string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "base.txt")
	git(repo, "commit", "-m", "base")
	git(repo, "worktree", "add", wt, "-b", branch)
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(wt, "add", "feature.txt")
	git(wt, "commit", "-m", "feature")

	parent := session.NewInstanceWithTool("web-shared-parent", wt, "shell")
	child := session.NewInstanceWithTool("web-shared-child", wt, "shell")
	child.ParentSessionID = parent.ID
	instances := []*session.Instance{parent, child}
	for _, inst := range instances {
		inst.Status = session.StatusStopped
		inst.WorktreePath, inst.WorktreeRepoRoot, inst.WorktreeBranch = wt, repo, branch
	}
	h, storage := newHeadlessHomeForTest(t, "_test_matrix_shared_finish")
	if err := storage.SaveWithGroups(instances, session.NewGroupTree(instances)); err != nil {
		t.Fatal(err)
	}
	m := NewWebMutator(h)
	result, err := m.FinishWorktree(parent.ID, web.WorktreeFinishOptions{Into: "main", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Merged || result.BranchDeleted {
		t.Fatalf("shared parent finish result: %+v", result)
	}
	remaining, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != child.ID {
		t.Fatalf("parent finish must only drop the parent: %+v", remaining)
	}
	git(wt, "status", "--porcelain")
	git(repo, "show-ref", "--verify", "refs/heads/"+branch)
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("parent finish did not merge the branch: %v", err)
	}

	result, err = m.FinishWorktree(child.ID, web.WorktreeFinishOptions{NoMerge: true, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.BranchDeleted {
		t.Fatalf("final user should delete the branch: %+v", result)
	}
	remaining, _, err = storage.LoadWithGroups()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("last finish must remove the remaining session: len=%d err=%v", len(remaining), err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("last finish retained worktree: %v", err)
	}
	if err := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run(); err == nil {
		t.Fatal("last finish retained branch")
	}
}
