package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/costs"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: every manual or automatic costSyncSessions invocation has a
// new directory snapshot, while Claude, unmanaged and remote sources retain
// their existing selection/error behavior.
func TestMatrixCodexCostSourcesBatchSnapshotPerInvocation(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	const sid = "thread"
	instances := []*session.Instance{
		{ID: "local", Tool: "codex", CodexSessionID: sid},
		{ID: "claude", Tool: "claude", ClaudeSessionID: "claude-thread", ProjectPath: project},
		{ID: "remote", Tool: "codex", CodexSessionID: sid, SSHHost: "remote"},
		{ID: "unbound", Tool: "codex"},
	}
	sources, errs := costSyncSessions(instances)
	if len(sources) != 1 || sources[0].InstanceID != "claude" || len(errs) != 1 {
		t.Fatalf("missing rollout sources=%+v errors=%v", sources, errs)
	}
	dir := filepath.Join(home, "sessions", "2026", "10", "13")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout--"+sid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sources, errs = costSyncSessions(instances)
	if len(sources) != 2 || sources[0].CodexPath != path || sources[1].InstanceID != "claude" || len(errs) != 0 {
		t.Fatalf("new rollout sources=%+v errors=%v", sources, errs)
	}
	duplicate := filepath.Join(dir, "rollout-date-"+sid+".jsonl")
	if err := os.WriteFile(duplicate, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sources, errs = costSyncSessions(instances)
	if len(sources) != 1 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "Codex cost source local: ambiguous") {
		t.Fatalf("duplicate sources=%+v errors=%v", sources, errs)
	}
	for _, file := range []string{path, duplicate} {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	sources, errs = costSyncSessions(instances)
	if len(sources) != 1 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "no exact context artifact") {
		t.Fatalf("deleted rollout sources=%+v errors=%v", sources, errs)
	}
}

// matrix/hardened: resolved Claude sources are local exact paths even before
// their file exists; invalid and remote identities become explicit errors.
func TestMatrixClaudeCostSourcesLocalExactPath(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "claude")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	project := filepath.Join(home, "project")
	working := filepath.Join(home, "effective")
	for _, dir := range []string{project, working} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const sid = "claude-thread"
	sources, errs := costSyncSessions([]*session.Instance{
		{ID: "local", Tool: "claude", ClaudeSessionID: sid, ProjectPath: project, MultiRepoEnabled: true, MultiRepoTempDir: working},
		{ID: "remote", Tool: "claude", ClaudeSessionID: sid, ProjectPath: project, SSHHost: "remote"},
		{ID: "container", Tool: "claude", ClaudeSessionID: sid, ProjectPath: project, Sandbox: &session.SandboxConfig{Enabled: true}},
		{ID: "invalid", Tool: "claude", ClaudeSessionID: "../foreign", ProjectPath: project},
		{ID: "unbound", Tool: "claude", ProjectPath: project},
		{ID: "other-tool", Tool: "gemini", ClaudeSessionID: sid, ProjectPath: project},
	})
	want := filepath.Join(config, "projects", session.ConvertToClaudeDirName(working), sid+".jsonl")
	if len(sources) != 1 || sources[0].InstanceID != "local" || sources[0].ClaudePath != want || sources[0].Tool != "claude" || len(errs) != 3 {
		t.Fatalf("sources=%+v errors=%v, want only exact local %q", sources, errs, want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("fixture must remain a missing transcript: %v", err)
	}
	for _, err := range errs {
		if !strings.HasPrefix(err.Error(), "Claude cost source ") {
			t.Fatalf("lost source-specific error: %v", err)
		}
	}
	// The default-home copy has usable usage but belongs to a different
	// launch cwd/home. A missing explicit source must skip rather than borrow it.
	defaultPath := filepath.Join(home, ".claude", "projects", session.ConvertToClaudeDirName(project), sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(defaultPath), 0700); err != nil {
		t.Fatal(err)
	}
	usage := `{"type":"assistant","uuid":"foreign-usage","sessionId":"claude-thread","message":{"id":"foreign-message","model":"claude-sonnet-4-6","role":"assistant","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"
	if err := os.WriteFile(defaultPath, []byte(usage), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(filepath.Join(home, "costs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	store := costs.NewStore(db.DB())
	pricer := costs.NewPricer(costs.PricerConfig{})
	result := costs.SyncFromTranscripts(store, pricer, sources)
	if result.SessionsScanned != 1 || result.EventsImported != 0 || len(result.Errors) != 0 {
		t.Fatalf("missing explicit Claude source did not skip: %+v", result)
	}
	// Prove the neighbouring fixture can import through the old direct-call
	// default, so the zero-import assertion above detects a path fallback.
	sources[0].ClaudePath = ""
	result = costs.SyncFromTranscripts(store, pricer, sources)
	if result.EventsImported != 1 || len(result.Errors) != 0 {
		t.Fatalf("default-home usage fixture is not readable: %+v", result)
	}
}
