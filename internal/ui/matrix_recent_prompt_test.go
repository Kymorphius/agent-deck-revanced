package ui

import (
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: only confirmed inline operator delivery changes recents;
// failed delivery and results for removed/rebound targets cannot promote rows.
func TestMatrixRecentOperatorPromptCompletion(t *testing.T) {
	for _, outcome := range []string{"accepted", "failed", "replaced"} {
		t.Run(outcome, func(t *testing.T) {
			h, inst := armHomeWithRunningClaudeSession(t, "claude")
			old := time.Now().Add(-time.Hour)
			inst.LastAccessedAt = old
			msg := matrixRecentPromptResultMsg{instance: inst, target: inst.GetTmuxSession()}
			if outcome == "failed" {
				msg.err = errors.New("refused")
			}
			if outcome == "replaced" {
				msg.instance = &session.Instance{ID: inst.ID, LastAccessedAt: old}
			}
			h.updateInner(msg)
			if outcome == "accepted" {
				if !inst.LastAccessedAt.After(old) {
					t.Fatal("confirmed inline prompt did not update the visit time")
				}
			} else if !inst.LastAccessedAt.Equal(old) {
				t.Fatal("failed or obsolete prompt changed the visit time")
			}
		})
	}
}
