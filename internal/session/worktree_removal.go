package session

import (
	"path/filepath"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/git"
)

// IsRemovableWorktree reports whether agent-deck may safely delete the
// session's worktree directory on dismiss. It is deliberately conservative: a
// path is removable ONLY when every check passes, because a false positive
// means os.RemoveAll on a real repository (issue #1200 — silent data loss).
//
// All of the following must hold:
//  1. agent-deck recorded a worktree at all (non-empty WorktreePath and
//     WorktreeRepoRoot). A worktree_reuse session that pointed at the original
//     repo can leave these empty depending on the creation path.
//  2. the worktree path is NOT the original repo root (worktree_reuse points
//     WorktreePath at the user's primary working tree).
//  3. git itself confirms the path is a LINKED worktree (created via
//     `git worktree add`), never the main working tree or a non-worktree dir.
//     This is the location-independent proof that agent-deck created it:
//     worktree placement is user-configurable, so a fixed managed-directory
//     prefix check is unreliable.
func IsRemovableWorktree(inst *Instance) bool {
	if inst == nil {
		return false
	}
	wt := strings.TrimSpace(inst.WorktreePath)
	root := strings.TrimSpace(inst.WorktreeRepoRoot)
	if wt == "" || root == "" {
		return false
	}
	if canonicalPath(wt) == canonicalPath(root) {
		return false
	}
	return git.IsLinkedWorktree(wt)
}

// OtherSessionsShareWorktree reports whether any instance in others (other than
// target itself) still references target's worktree directory. Paths are
// compared canonically (EvalSymlinks), so a symlinked alias to the same
// directory counts as a sharer. nil entries and target's own ID are skipped.
// matrix/hardened: project paths and per-repo references also retain the directory.
// matrix/hardened: a multi-repo instance may also reference the directory in
// MultiRepoWorktrees while its single WorktreePath is empty.
//
// This is the missing guard behind issue #1449: finishing one of several
// sessions that share a single worktree must not remove the shared directory or
// delete the branch while a sibling session still uses it.
func OtherSessionsShareWorktree(target *Instance, others []*Instance) bool {
	if target == nil {
		return false
	}
	wt := strings.TrimSpace(target.WorktreePath)
	if wt == "" {
		return false
	}
	targetCanon := canonicalPath(wt)
	for _, o := range others {
		if o == nil || o.ID == target.ID {
			continue
		}
		owt := strings.TrimSpace(o.WorktreePath)
		if owt != "" && canonicalPath(owt) == targetCanon {
			return true
		}
		// matrix/hardened: older or direct forks can have only a project-path
		// reference. They still depend on this checkout and must protect it.
		if canonicalDirectoryContains(wt, o.ProjectPath) {
			return true
		}
		for _, path := range o.AdditionalPaths {
			if canonicalDirectoryContains(wt, path) {
				return true
			}
		}
		// matrix/hardened: include each per-repository reference before
		// allowing cleanup of a directory used by a shared multi-repo fork.
		for _, worktree := range o.MultiRepoWorktrees {
			path := strings.TrimSpace(worktree.WorktreePath)
			if path != "" && canonicalPath(path) == targetCanon {
				return true
			}
		}
	}
	return false
}

// matrix/hardened: shared multi-repo forks retain the same temporary working
// directory and its repository links. Only the last registered user may remove
// that directory, including when another record uses a symlinked alias or a
// different aggregate directory whose project links resolve into this one.
func OtherSessionsShareMultiRepoTempDir(target *Instance, others []*Instance) bool {
	if target == nil || strings.TrimSpace(target.MultiRepoTempDir) == "" {
		return false
	}
	targetCanon := canonicalPath(strings.TrimSpace(target.MultiRepoTempDir))
	for _, other := range others {
		if other == nil || other.ID == target.ID {
			continue
		}
		path := strings.TrimSpace(other.MultiRepoTempDir)
		if path != "" && canonicalPath(path) == targetCanon {
			return true
		}
		// matrix/hardened: a distinct aggregate directory can still depend on
		// worktrees below this directory through its project symlinks.
		if canonicalDirectoryContains(targetCanon, other.ProjectPath) ||
			canonicalDirectoryContains(targetCanon, other.WorktreePath) {
			return true
		}
		for _, path := range other.AdditionalPaths {
			if canonicalDirectoryContains(targetCanon, path) {
				return true
			}
		}
		for _, worktree := range other.MultiRepoWorktrees {
			if canonicalDirectoryContains(targetCanon, worktree.WorktreePath) {
				return true
			}
		}
	}
	return false
}

// matrix/hardened: a registered path depends on its parent directories even
// when its final symlink points outside the container. Check its ancestors as
// well as its resolved destination; sibling prefixes remain unrelated.
func canonicalDirectoryContains(directory, path string) bool {
	if strings.TrimSpace(directory) == "" || strings.TrimSpace(path) == "" {
		return false
	}
	root, err := filepath.Abs(strings.TrimSpace(directory))
	if err != nil {
		return false
	}
	candidate, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	root = canonicalPath(root)
	for {
		if isPathWithin(root, canonicalPath(candidate)) {
			return true
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return false
		}
		candidate = parent
	}
}

// RemoveSessionWorktree removes the session's worktree directory if and only if
// IsRemovableWorktree permits it. It returns whether a removal was performed.
// A reused original repo (or any non-linked-worktree path) is left untouched —
// the caller should simply drop the session from the registry (#1200).
func RemoveSessionWorktree(inst *Instance) (removed bool, err error) {
	if !IsRemovableWorktree(inst) {
		return false, nil
	}
	if err := git.RemoveWorktree(inst.WorktreeRepoRoot, inst.WorktreePath, true); err != nil {
		return false, err
	}
	// Best-effort: drop the now-stale worktree administrative reference.
	_ = git.PruneWorktrees(inst.WorktreeRepoRoot)
	return true, nil
}

// RemoveSessionWorktreeUnlessShared is the shared-worktree-safe entry point
// (issue #1449). It removes the session's worktree directory only when no OTHER
// live session still references that worktree; otherwise it skips the
// destructive git steps and reports removed=false so the caller merely detaches
// this session from the registry. When this is the last sharer, behaviour is
// identical to RemoveSessionWorktree (including the #1200 reuse guard).
func RemoveSessionWorktreeUnlessShared(inst *Instance, others []*Instance) (removed bool, err error) {
	if OtherSessionsShareWorktree(inst, others) {
		return false, nil
	}
	return RemoveSessionWorktree(inst)
}

// canonicalPath resolves symlinks and cleans a path for equality comparison so
// that e.g. /var vs /private/var (macOS) or other symlinked roots do not let a
// reused repo slip past the path == root check. Falls back to a lexical clean
// when the path cannot be resolved (e.g. it no longer exists).
func canonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	// EvalSymlinks failed (e.g. the path no longer exists, or a broken/
	// inaccessible symlink alias). Fall back to an absolute lexical clean so a
	// relative vs absolute spelling of the same path still compares equal. For
	// the shared-worktree check this matters: a false negative here would let a
	// still-shared worktree reach destructive cleanup, so normalize before the
	// lexical compare rather than trusting the raw input.
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}
