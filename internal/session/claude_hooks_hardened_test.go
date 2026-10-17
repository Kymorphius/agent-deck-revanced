package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: RemoveClaudeHooks removes agent-deck commands under every
// event (including ones this version no longer installs), keeps all other
// hooks and settings, and leaves a timestamped backup of the original.
func TestRemoveClaudeHooks_HardenedBackupAndLegacyEvents(t *testing.T) {
	dir := t.TempDir()
	orig := `{
  "model": "opus",
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "agent-deck hook-handler"}, {"type": "command", "command": "say done"}]}],
    "LegacyEvent": [{"matcher": "", "hooks": [{"type": "command", "command": "/opt/homebrew/bin/agent-deck hook-handler"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "my-guard"}]}]
  }
}`
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveClaudeHooks(dir)
	if err != nil || !removed {
		t.Fatalf("RemoveClaudeHooks = %v, %v", removed, err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, "hook-handler") || strings.Contains(s, "LegacyEvent") {
		t.Fatalf("agent-deck entries left behind:\n%s", s)
	}
	for _, keep := range []string{"say done", "my-guard", `"model"`} {
		if !strings.Contains(s, keep) {
			t.Fatalf("lost %q:\n%s", keep, s)
		}
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "settings.json.bak-agentdeck-uninstall-*Z"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b) != orig {
		t.Fatal("backup must be the original bytes")
	}

	// Nothing left to remove: no write, no new backup.
	if removed, err := RemoveClaudeHooks(dir); err != nil || removed {
		t.Fatalf("second RemoveClaudeHooks = %v, %v", removed, err)
	}
}
