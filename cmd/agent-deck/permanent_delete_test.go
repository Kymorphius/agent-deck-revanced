package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: CLI preview cannot mutate; --yes only targets the fixture.
func TestPermanentDeleteCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	st, err := session.NewStorageWithProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	inst := session.NewInstanceWithTool("purge fixture", home, "codex")
	inst.CodexSessionID = "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(home, ".codex", "sessions", "2026", "10", "12", "rollout-2026-10-12T10-00-00-"+inst.CodexSessionID+".jsonl")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	_ = os.WriteFile(path, []byte("fixture\n"), 0600)
	if err := st.Save([]*session.Instance{inst}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { err = runPermanentDelete(st, inst, true, false, true) })
	if err != nil || !strings.Contains(out, inst.CodexSessionID) {
		t.Fatalf("preview %q: %v", out, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("preview deleted the transcript")
	}
	if err := runPermanentDelete(st, inst, false, false, true); err == nil {
		t.Fatal("unconfirmed JSON deletion accepted")
	}
	out = captureStdout(t, func() { err = runPermanentDelete(st, inst, false, true, true) })
	if err != nil || !strings.Contains(out, `"permanently_deleted":true`) {
		t.Fatalf("execute %q: %v", out, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("confirmed deletion did not remove fixture")
	}
}
