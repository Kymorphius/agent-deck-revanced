package session

import (
	"strings"
	"testing"
)

// matrix/hardened: a session imported from TUI global search (G) used to store
// "exec claude --resume <id> --dangerously-skip-permissions" as its Command.
// buildClaudeResumeCommand treats that as a custom binary and wraps it again,
// so the pane ran "exec exec claude --resume <id> ... --resume <id> ..." and
// died within a second. The import now stores the plain "claude" command; this
// pins that Start's resume path turns it into exactly one resume invocation.
func TestMatrixGlobalSearchImport_ResumeCommandIsNotDoubled(t *testing.T) {
	inst, _, sessionID, _ := remoteBoundaryFixture(t)
	inst.Command = "claude"
	if err := inst.SetClaudeOptions(&ClaudeOptions{SkipPermissions: true}); err != nil {
		t.Fatalf("SetClaudeOptions: %v", err)
	}

	cmd := inst.buildClaudeResumeCommand()

	for _, want := range []string{"--resume " + sessionID, "--dangerously-skip-permissions"} {
		if n := strings.Count(cmd, want); n != 1 {
			t.Errorf("want %q exactly once, got %d:\n%s", want, n, cmd)
		}
	}
	if strings.Contains(cmd, "exec exec") {
		t.Errorf("resume command execs twice:\n%s", cmd)
	}
}
