package session

import (
	"path/filepath"
	"strings"
)

// matrix/hardened: a fork that stays inside its parent's worktree shares that
// directory. Preserve the reference on the new record so dismissing the parent
// cannot remove files that the child still uses. Explicit new-worktree metadata
// wins, and this never sets launch options that would create another worktree.
func inheritForkWorktreeLocation(parent, forked *Instance) {
	if parent == nil || forked == nil || forked.WorktreePath != "" ||
		strings.TrimSpace(parent.WorktreePath) == "" || strings.TrimSpace(forked.ProjectPath) == "" {
		return
	}

	worktreePath, err := filepath.Abs(parent.WorktreePath)
	if err != nil {
		return
	}
	projectPath, err := filepath.Abs(forked.ProjectPath)
	if err != nil {
		return
	}
	// matrix/hardened: compare resolved paths before checking containment, so a
	// symlink inside the worktree pointing elsewhere does not inherit ownership.
	rel, err := filepath.Rel(canonicalPath(worktreePath), canonicalPath(projectPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}

	forked.WorktreePath = parent.WorktreePath
	forked.WorktreeRepoRoot = parent.WorktreeRepoRoot
	forked.WorktreeBranch = parent.WorktreeBranch
	forked.WorktreeType = parent.WorktreeType
}
