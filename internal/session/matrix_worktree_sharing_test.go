package session

import (
	"os"
	"path/filepath"
	"testing"
)

// matrix/hardened: each multi-repo worktree reference protects the same
// directory as the single-repo reference, even when the latter is absent.
func TestMatrixOtherSessionsShareWorktreeIncludesMultiRepo(t *testing.T) {
	first, second, different := t.TempDir(), t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	multi := &Instance{ID: "multi", MultiRepoWorktrees: []MultiRepoWorktree{{WorktreePath: first}, {WorktreePath: second}}}
	for _, tc := range []struct {
		name   string
		target *Instance
		others []*Instance
		want   bool
	}{
		{"nil-target", nil, []*Instance{multi}, false},
		{"empty-target", &Instance{ID: "target"}, []*Instance{multi}, false},
		{"single-same", &Instance{ID: "target", WorktreePath: second}, []*Instance{{ID: "single", WorktreePath: second}}, true},
		{"single-different", &Instance{ID: "target", WorktreePath: second}, []*Instance{{ID: "single", WorktreePath: different}}, false},
		{"project-without-worktree-metadata", &Instance{ID: "target", WorktreePath: second}, []*Instance{{ID: "direct", ProjectPath: second}}, true},
		{"nested-project-without-worktree-metadata", &Instance{ID: "target", WorktreePath: second}, []*Instance{{ID: "direct", ProjectPath: filepath.Join(second, "nested")}}, true},
		{"project-symlink-without-worktree-metadata", &Instance{ID: "target", WorktreePath: second}, []*Instance{{ID: "direct", ProjectPath: alias}}, true},
		{"additional-path-without-worktree-metadata", &Instance{ID: "target", WorktreePath: second}, []*Instance{{ID: "direct", ProjectPath: different, AdditionalPaths: []string{alias}}}, true},
		{"first-per-repo", &Instance{ID: "target", WorktreePath: first}, []*Instance{multi}, true},
		{"later-per-repo", &Instance{ID: "target", WorktreePath: second}, []*Instance{multi}, true},
		{"different-per-repo", &Instance{ID: "target", WorktreePath: different}, []*Instance{multi}, false},
		{"single-different-per-repo-same", &Instance{ID: "target", WorktreePath: second}, []*Instance{{ID: "multi", WorktreePath: different, MultiRepoWorktrees: []MultiRepoWorktree{{WorktreePath: second}}}}, true},
		{"per-repo-symlink", &Instance{ID: "target", WorktreePath: second}, []*Instance{{ID: "multi", MultiRepoWorktrees: []MultiRepoWorktree{{WorktreePath: alias}}}}, true},
		{"target-symlink", &Instance{ID: "target", WorktreePath: alias}, []*Instance{multi}, true},
		{"ignore-own-id", &Instance{ID: "multi", WorktreePath: second}, []*Instance{multi}, false},
		{"ignore-nil-and-empty", &Instance{ID: "target", WorktreePath: second}, []*Instance{nil, {ID: "empty", MultiRepoWorktrees: []MultiRepoWorktree{{}, {WorktreePath: "  "}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OtherSessionsShareWorktree(tc.target, tc.others); got != tc.want {
				t.Fatalf("OtherSessionsShareWorktree = %v, want %v", got, tc.want)
			}
		})
	}
}

// matrix/hardened: a shared multi-repo temporary directory survives deletion of
// either record until the final registered reference has gone.
func TestMatrixOtherSessionsShareMultiRepoTempDir(t *testing.T) {
	dir, different := t.TempDir(), t.TempDir()
	project := filepath.Join(dir, "repo", "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	projectAlias := filepath.Join(different, "repo-alias")
	if err := os.Symlink(project, projectAlias); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(dir, "escape")
	if err := os.Symlink(different, escape); err != nil {
		t.Fatal(err)
	}
	prefixSibling := dir + "-other"
	if err := os.MkdirAll(prefixSibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prefixSibling) })
	target := &Instance{ID: "target", MultiRepoTempDir: dir}
	for _, tc := range []struct {
		name   string
		target *Instance
		others []*Instance
		want   bool
	}{
		{"nil-target", nil, []*Instance{{ID: "other", MultiRepoTempDir: dir}}, false},
		{"empty-target", &Instance{ID: "target"}, []*Instance{{ID: "other", MultiRepoTempDir: dir}}, false},
		{"no-others", target, nil, false},
		{"same", target, []*Instance{{ID: "other", MultiRepoTempDir: dir}}, true},
		{"symlink", target, []*Instance{{ID: "other", MultiRepoTempDir: alias}}, true},
		{"target-symlink", &Instance{ID: "target", MultiRepoTempDir: alias}, []*Instance{{ID: "other", MultiRepoTempDir: dir}}, true},
		{"different", target, []*Instance{{ID: "other", MultiRepoTempDir: different}}, false},
		{"project-subdir", target, []*Instance{{ID: "other", ProjectPath: project}}, true},
		{"project-symlink-different-aggregate", target, []*Instance{{ID: "other", MultiRepoTempDir: different, ProjectPath: projectAlias}}, true},
		{"additional-path", target, []*Instance{{ID: "other", MultiRepoTempDir: different, ProjectPath: different, AdditionalPaths: []string{"", projectAlias}}}, true},
		{"per-repo-worktree", target, []*Instance{{ID: "other", MultiRepoTempDir: different, MultiRepoWorktrees: []MultiRepoWorktree{{WorktreePath: different}, {WorktreePath: projectAlias}}}}, true},
		{"single-worktree", target, []*Instance{{ID: "other", WorktreePath: project}}, true},
		{"prefix-sibling", target, []*Instance{{ID: "other", ProjectPath: prefixSibling, AdditionalPaths: []string{prefixSibling}, MultiRepoWorktrees: []MultiRepoWorktree{{WorktreePath: prefixSibling}}}}, false},
		// matrix/hardened: removing the container would break this registered
		// alias, even though its checkout lives outside the container.
		{"symlink-out-still-needs-container", target, []*Instance{{ID: "other", ProjectPath: escape, AdditionalPaths: []string{escape}, MultiRepoWorktrees: []MultiRepoWorktree{{WorktreePath: escape}}}}, true},
		{"ignore-own-project-reference", target, []*Instance{{ID: "target", ProjectPath: projectAlias, AdditionalPaths: []string{project}, MultiRepoWorktrees: []MultiRepoWorktree{{WorktreePath: project}}}}, false},
		{"ignore-own-id", target, []*Instance{{ID: "target", MultiRepoTempDir: dir}}, false},
		{"ignore-nil-and-empty", target, []*Instance{nil, {ID: "empty", MultiRepoTempDir: "  "}}, false},
		{"mixed", target, []*Instance{nil, {ID: "different", MultiRepoTempDir: different}, {ID: "other", MultiRepoTempDir: dir}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OtherSessionsShareMultiRepoTempDir(tc.target, tc.others); got != tc.want {
				t.Fatalf("OtherSessionsShareMultiRepoTempDir = %v, want %v", got, tc.want)
			}
		})
	}
}
