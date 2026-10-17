package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: no latest-file fallback and no remote-to-local confusion.
func TestMatrixCodexCostSourcesExactIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	const sid = "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(home, "sessions", "2026", "10", "10")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-10T10-00-00-"+sid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sources, errs := costSyncSessions([]*session.Instance{
		{ID: "local", Tool: "codex", CodexSessionID: sid},
		{ID: "remote", Tool: "codex", CodexSessionID: sid, SSHHost: "remote"},
		{ID: "unbound", Tool: "codex"},
		{ID: "missing", Tool: "codex", CodexSessionID: "22222222-2222-4222-8222-222222222222"},
	})
	if len(sources) != 1 || sources[0].InstanceID != "local" || sources[0].CodexPath != path || len(errs) != 1 {
		t.Fatalf("sources: %+v errors: %v", sources, errs)
	}
}
