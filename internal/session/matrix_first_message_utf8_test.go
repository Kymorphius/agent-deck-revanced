package session

// matrix/hardened: the first-message sender in a new Claude pane runs every
// tmux call with -u, so a non-ASCII message survives a non-UTF-8 locale.

import (
	"strings"
	"testing"
)

func TestMatrixFirstMessage_TmuxCallsUseUTF8Flag(t *testing.T) {
	startupNameConfig(t, "")
	inst := &Instance{Tool: "claude", Title: "任务", ID: "utf8", ProjectPath: t.TempDir()}

	cmd := inst.buildClaudeCommandWithMessage("claude", "整理 World Store 任务")

	if !strings.Contains(cmd, "'整理 World Store 任务'") {
		t.Fatalf("message not passed through as is: %s", cmd)
	}
	for _, sub := range []string{"display-message", "capture-pane", "send-keys -l"} {
		if !strings.Contains(cmd, "tmux -u "+sub) {
			t.Errorf("tmux %s without -u: %s", sub, cmd)
		}
	}
	if strings.Contains(strings.ReplaceAll(cmd, "tmux -u ", ""), "tmux ") {
		t.Errorf("a tmux call without -u is left: %s", cmd)
	}
}
