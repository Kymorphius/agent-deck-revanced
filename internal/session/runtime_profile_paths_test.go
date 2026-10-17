package session

import (
	"path/filepath"
	"testing"
)

// matrix/hardened: one user-level producer can inspect two profile databases
// without borrowing its launching client's native home or mutable global env.
func TestRuntimeProducerNativeHomesUseOwningProfile(t *testing.T) {
	home := withTempAgentDeckHome(t, `
[profiles.first.claude]
config_dir = "~/.claude-first"
[profiles.second.claude]
config_dir = "~/.claude-second"
[profiles.first.codex]
config_dir = "~/.codex-first"
[profiles.second.codex]
config_dir = "~/.codex-second"
`)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude-ambient"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex-ambient"))
	for _, profile := range []string{"first", "second"} {
		s := newTestStorage(t)
		s.profile = profile
		inst := &Instance{ID: "same-id", Tool: "codex", Command: "codex", ProjectPath: t.TempDir(), Status: StatusRunning, GroupPath: DefaultGroupPath}
		if err := s.SaveWithGroups([]*Instance{inst}, NewGroupTree([]*Instance{inst})); err != nil {
			t.Fatal(err)
		}
		inst.SetRuntimeProducer(true)
		inst.SetRuntimeProducerFence(0)
		if got := inst.getCodexHomeDir(); got != filepath.Join(home, ".codex-"+profile) {
			t.Fatalf("%s Codex root=%q", profile, got)
		}
		if got := GetClaudeConfigDirForInstance(inst); got != filepath.Join(home, ".claude-"+profile) {
			t.Fatalf("%s Claude root=%q", profile, got)
		}
		// Named accounts and command-local CODEX_HOME remain more specific.
		inst.Account = "first"
		if got := GetClaudeConfigDirForInstance(inst); got != filepath.Join(home, ".claude-first") {
			t.Fatalf("account override=%q", got)
		}
		if got := inst.codexHomeForCommand("CODEX_HOME=/explicit/session codex"); got != "/explicit/session" {
			t.Fatalf("command native home=%q", got)
		}
		// An ordinary controller still honors its explicit ambient launch home.
		inst.SetRuntimeProducer(false)
		inst.Account = ""
		if got := inst.getCodexHomeDir(); got != filepath.Join(home, ".codex-ambient") {
			t.Fatalf("ordinary Codex env override=%q", got)
		}
		if got := GetClaudeConfigDirForInstance(inst); got != filepath.Join(home, ".claude-ambient") {
			t.Fatalf("ordinary Claude env override=%q", got)
		}
	}
}
