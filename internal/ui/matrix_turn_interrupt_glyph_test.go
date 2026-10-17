package ui

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: a turn that ended on an API error shows ↯ instead of the
// finished-turn glyph, only on waiting/idle rows.
func TestRowStatusGlyph_MatrixAPIInterrupted(t *testing.T) {
	sub := session.SubstateTurnInterrupted
	tests := []struct {
		name     string
		status   session.Status
		archived bool
		wantIcon string
	}{
		{"waiting", session.StatusWaiting, false, "↯"},
		{"idle (acknowledged)", session.StatusIdle, false, "↯"},
		{"running: a new turn may be under way", session.StatusRunning, false, "●"},
		{"error with interrupted turn keeps interruption glyph", session.StatusError, false, "↯"},
		{"stopped", session.StatusStopped, false, "■"},
		{"archived", session.StatusWaiting, true, "■"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			icon, style := rowStatusGlyph(tt.status, sub, tt.archived)
			if icon != tt.wantIcon {
				t.Errorf("icon = %q, want %q", icon, tt.wantIcon)
			}
			if icon == "↯" && style.GetForeground() != SessionStatusError.GetForeground() {
				t.Errorf("↯ not in the error color")
			}
		})
	}
}
