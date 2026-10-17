package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// CodexCostRolloutResolver is a directory snapshot for one cost-source pass.
// matrix/hardened: share the historical tree scan within a pass, never across
// passes. Every resolution still validates the matching artifact with Lstat.
type CodexCostRolloutResolver struct {
	mu    sync.Mutex
	homes map[string]matrixCodexCostRolloutSnapshot
	glob  func(string) ([]string, error)
}

type matrixCodexCostRolloutSnapshot struct {
	paths []string
	err   error
}

// NewCodexCostRolloutResolver creates one cost enumeration's resolver. Discard
// it after that enumeration so newly created or duplicated rollouts are found.
// matrix/hardened: no global cache, persistence or rollout-reader changes.
func NewCodexCostRolloutResolver() *CodexCostRolloutResolver {
	return &CodexCostRolloutResolver{
		homes: make(map[string]matrixCodexCostRolloutSnapshot),
		glob:  filepath.Glob,
	}
}

// Resolve retains CodexCostRolloutPath's exact identity and local-read gates.
// matrix/hardened: only directory enumeration is shared; deleted, replaced,
// nonregular and symlink artifacts are checked anew for every instance.
func (r *CodexCostRolloutResolver) Resolve(inst *Instance) (string, error) {
	if !inst.CodexRolloutIsResolvableLocally() {
		return "", fmt.Errorf("Codex rollout is not locally readable")
	}
	if err := validateExactSessionID(inst.CodexSessionID); err != nil {
		return "", err
	}
	home := filepath.Clean(ExpandPath(inst.getCodexHomeDir()))
	snapshot := r.snapshot(home)
	if snapshot.err != nil {
		return "", fmt.Errorf("locate Codex rollout: %w", snapshot.err)
	}

	// The old glob is rollout-*-<raw SID>.jsonl. The SID is validated but not
	// rewritten; two literal separators are required even when '*' is empty.
	prefix, suffix := "rollout-", "-"+inst.CodexSessionID+".jsonl"
	var matches []string
	for _, path := range snapshot.paths {
		name := filepath.Base(path)
		if len(name) >= len(prefix)+len(suffix) && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
			matches = append(matches, path)
		}
	}
	return uniqueRegularArtifact(matches, "rollout for "+inst.CodexSessionID)
}

func (r *CodexCostRolloutResolver) snapshot(home string) matrixCodexCostRolloutSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if snapshot, ok := r.homes[home]; ok {
		return snapshot
	}
	if r.homes == nil {
		r.homes = make(map[string]matrixCodexCostRolloutSnapshot)
	}
	glob := r.glob
	if glob == nil {
		glob = filepath.Glob
	}
	paths, err := glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*.jsonl"))
	snapshot := matrixCodexCostRolloutSnapshot{paths: paths, err: err}
	r.homes[home] = snapshot
	return snapshot
}
