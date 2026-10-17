package session

import "fmt"

// ClaudeCostTranscriptPath returns this instance's account-bound exact source.
// matrix/hardened: manual costs use the same literal identity, effective cwd
// and symlink guards as context export, without cross-account/latest fallback.
// A missing transcript retains its expected path for the cost reader to skip.
func (i *Instance) ClaudeCostTranscriptPath() (string, error) {
	if !i.TranscriptIsResolvableLocally() || i.IsSandboxed() {
		return "", fmt.Errorf("Claude transcript is not locally readable")
	}
	return canonicalClaudeExactTranscriptPath(i)
}
