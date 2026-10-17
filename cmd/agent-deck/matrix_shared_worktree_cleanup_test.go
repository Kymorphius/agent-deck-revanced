package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: exercise real git and registry state at the destructive CLI
// boundaries so an in-place fork survives removal of its original session.
type matrixSharedWorktreeFixture struct {
	repo, path, branch, profile string
	storage                     *session.Storage
	parent, child               *session.Instance
}

func matrixNewSharedWorktreeFixture(t *testing.T) *matrixSharedWorktreeFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("AGENT_DECK_HOME", "")
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	f := &matrixSharedWorktreeFixture{
		repo: filepath.Join(home, "repo"), path: filepath.Join(home, "worktree"),
		branch: "shared-feature", profile: "_test_matrix_shared_cleanup",
	}
	if err := os.MkdirAll(f.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(f.repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.repo, "add", "base.txt")
	f.git(t, f.repo, "commit", "-m", "base")
	f.git(t, f.repo, "worktree", "add", f.path, "-b", f.branch)
	for _, title := range []string{"shared-parent", "shared-child"} {
		inst := session.NewInstanceWithTool(title, f.path, "shell")
		inst.Status = session.StatusStopped
		inst.WorktreePath, inst.WorktreeRepoRoot, inst.WorktreeBranch = f.path, f.repo, f.branch
		if f.parent == nil {
			f.parent = inst
		} else {
			f.child = inst
			f.child.ParentSessionID = f.parent.ID
		}
	}
	var err error
	f.storage, err = session.NewStorageWithProfile(f.profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.storage.Close() })
	f.save(t, []*session.Instance{f.parent, f.child})
	return f
}

func (f *matrixSharedWorktreeFixture) git(t *testing.T, path string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (f *matrixSharedWorktreeFixture) save(t *testing.T, instances []*session.Instance) {
	t.Helper()
	if err := f.storage.SaveWithGroups(instances, session.NewGroupTree(instances)); err != nil {
		t.Fatal(err)
	}
}

func (f *matrixSharedWorktreeFixture) assertRegistry(t *testing.T, want ...string) {
	t.Helper()
	instances, _, err := f.storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != len(want) {
		t.Fatalf("registry has %d sessions, want %d", len(instances), len(want))
	}
	for _, id := range want {
		found := false
		for _, inst := range instances {
			found = found || inst.ID == id
		}
		if !found {
			t.Fatalf("surviving session %s missing", id)
		}
	}
}

func (f *matrixSharedWorktreeFixture) assertWorktree(t *testing.T, present, branchPresent bool) {
	t.Helper()
	_, err := os.Stat(f.path)
	if present && err != nil || !present && !os.IsNotExist(err) {
		t.Fatalf("worktree present=%v, stat error=%v", present, err)
	}
	cmd := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "--quiet", "refs/heads/"+f.branch)
	if got := cmd.Run() == nil; got != branchPresent {
		t.Fatalf("branch present=%v, want %v", got, branchPresent)
	}
	if present {
		// The surviving session can still issue git operations in its directory.
		f.git(t, f.path, "status", "--porcelain")
	}
}

func (f *matrixSharedWorktreeFixture) commitFeature(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.path, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.path, "add", "feature.txt")
	f.git(t, f.path, "commit", "-m", "feature")
}

func TestMatrixSharedWorktreeRemoveCLI(t *testing.T) {
	for _, entry := range []string{"remove", "session-prune"} {
		t.Run(entry, func(t *testing.T) {
			f := matrixNewSharedWorktreeFixture(t)
			remove := func(id string) {
				captureStdout(t, func() {
					if entry == "remove" {
						handleRemove(f.profile, []string{id, "--json"})
					} else {
						handleSessionRemove(f.profile, []string{id, "--prune-worktree", "--json"})
					}
				})
			}
			remove(f.parent.ID)
			f.assertRegistry(t, f.child.ID)
			f.assertWorktree(t, true, true)
			remove(f.child.ID)
			f.assertRegistry(t)
			// Ordinary removal has always kept the branch; finish deletes it.
			f.assertWorktree(t, false, true)
		})
	}
}

func TestMatrixSharedWorktreeBulkPrune(t *testing.T) {
	for _, retainPinnedChild := range []bool{false, true} {
		t.Run(map[bool]string{false: "last-user-in-batch", true: "pinned-child-survives"}[retainPinnedChild], func(t *testing.T) {
			f := matrixNewSharedWorktreeFixture(t)
			f.parent.Status, f.child.Status = session.StatusError, session.StatusError
			if retainPinnedChild {
				f.child.Pin = session.PinTop
			}
			f.save(t, []*session.Instance{f.parent, f.child})
			captureStdout(t, func() {
				handleSessionRemove(f.profile, []string{"--all-errored", "--prune-worktree", "--json"})
			})
			if retainPinnedChild {
				f.assertRegistry(t, f.child.ID)
				f.assertWorktree(t, true, true)
				captureStdout(t, func() {
					handleSessionRemove(f.profile, []string{"--all-errored", "--prune-worktree", "--force", "--json"})
				})
			}
			f.assertRegistry(t)
			f.assertWorktree(t, false, true)
		})
	}
}

func matrixFinishJSON(t *testing.T, profile string, args ...string) map[string]any {
	t.Helper()
	output := captureStdout(t, func() { handleWorktreeFinish(profile, append(args, "--json")) })
	// The existing CLI writes progress before its final JSON object.
	start := strings.IndexByte(output, '{')
	if start < 0 {
		t.Fatalf("no JSON result: %s", output)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(output[start:]), &result); err != nil {
		t.Fatalf("decode finish result: %v\n%s", err, output)
	}
	return result
}

func TestMatrixSharedWorktreeFinishCLI(t *testing.T) {
	f := matrixNewSharedWorktreeFixture(t)
	f.commitFeature(t)
	parentResult := matrixFinishJSON(t, f.profile, f.parent.ID, "--into", "main", "--force")
	if parentResult["branch_deleted"] != false || parentResult["merged"] != true {
		t.Fatalf("shared parent finish result: %v", parentResult)
	}
	f.assertRegistry(t, f.child.ID)
	f.assertWorktree(t, true, true)
	if _, err := os.Stat(filepath.Join(f.repo, "feature.txt")); err != nil {
		t.Fatalf("shared finish did not merge: %v", err)
	}
	childResult := matrixFinishJSON(t, f.profile, f.child.ID, "--no-merge", "--force")
	if childResult["branch_deleted"] != true {
		t.Fatalf("final finish result: %v", childResult)
	}
	f.assertRegistry(t)
	f.assertWorktree(t, false, false)
}

func TestMatrixWorktreeFinishJSONBranchDeleteFailure(t *testing.T) {
	f := matrixNewSharedWorktreeFixture(t)
	f.commitFeature(t)
	if err := f.storage.DeleteInstance(f.child.ID); err != nil {
		t.Fatal(err)
	}
	result := matrixFinishJSON(t, f.profile, f.parent.ID, "--no-merge")
	if result["branch_deleted"] != false {
		t.Fatalf("unmerged branch deletion incorrectly reported as successful: %v", result)
	}
	f.assertRegistry(t)
	f.assertWorktree(t, false, true)
}
