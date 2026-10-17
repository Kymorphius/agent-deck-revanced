package costs_test

import (
	"context"
	"github.com/asheshgoplani/agent-deck/internal/costs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: cancelling a manual request before a scan must not read or
// import the supplied source, even when its path would otherwise fail.
func TestMatrixSyncContextCancelledBeforeSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := testStore(t)
	result := costs.SyncFromTranscriptsContext(ctx, store, costs.NewPricer(costs.PricerConfig{}), []costs.SyncSession{{InstanceID: "cancelled", CodexPath: "missing.jsonl", CodexSessionID: "sid"}})
	if result.SessionsScanned != 0 || result.EventsImported != 0 || len(result.Errors) != 1 || result.Errors[0] != context.Canceled.Error() {
		t.Fatalf("cancelled sync: %+v", result)
	}
	sum, err := store.TotalBySession("cancelled")
	if err != nil || sum.EventCount != 0 {
		t.Fatalf("cancelled request wrote usage: %+v %v", sum, err)
	}
}

// matrix/hardened: a managed account's explicit transcript cannot be replaced
// by the same native identity in the default Claude home.
func TestMatrixSyncContextExplicitClaudeAccountPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const sid = "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(home, ".claude-work", "projects", "-Users-x-proj", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(syncTranscript), 0600); err != nil {
		t.Fatal(err)
	}
	defaultPath := filepath.Join(home, ".claude", "projects", "-Users-x-proj", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(defaultPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultPath, []byte(strings.ReplaceAll(syncTranscript, `"input_tokens":100`, `"input_tokens":900`)), 0600); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	result := costs.SyncFromTranscriptsContext(context.Background(), store, costs.NewPricer(costs.PricerConfig{}), []costs.SyncSession{{InstanceID: "work-account", Tool: "claude", ClaudeSessionID: sid, ProjectPath: "/Users/x/proj", ClaudePath: path}})
	if result.EventsImported != 2 || len(result.Errors) != 0 {
		t.Fatalf("account sync: %+v", result)
	}
	sum, err := store.TotalBySession("work-account")
	if err != nil || sum.EventCount != 2 || sum.TotalInputTokens != 107 {
		t.Fatalf("account source missing/wrong: %+v %v", sum, err)
	}
}
