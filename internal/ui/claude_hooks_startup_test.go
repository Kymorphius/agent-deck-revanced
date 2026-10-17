package ui

import "testing"

// Review round 3 (finding 4): a user who installed the hooks with
// `agent-deck hooks install` (no TUI answer recorded) must not be asked
// again when an upgrade makes the entries drift; presence is consent and the
// drift is repaired silently.
// matrix/hardened: drift is never repaired silently (watch only), and an
// accepted-then-removed install is treated as revoked consent.
func TestClaudeHooksStartupDecision(t *testing.T) {
	cases := []struct {
		name               string
		present, installed bool
		prompted           string
		want               claudeHooksStartup
	}{
		{"clean install, never asked", true, true, "", claudeHooksStartup{watch: true}},
		{"clean install, accepted", true, true, "accepted", claudeHooksStartup{watch: true}},
		{"CLI install drifted after upgrade, never asked: watch only, no prompt", true, false, "", claudeHooksStartup{watch: true}},
		{"TUI install drifted after upgrade: watch only, no prompt", true, false, "accepted", claudeHooksStartup{watch: true}},
		{"drifted but once declined: still present, watch only", true, false, "declined", claudeHooksStartup{watch: true}},
		{"accepted, then removed: removal revokes consent, nothing", false, false, "accepted", claudeHooksStartup{}},
		{"declined, absent: nothing", false, false, "declined", claudeHooksStartup{}},
		{"absent, never asked: prompt", false, false, "", claudeHooksStartup{prompt: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeHooksStartupDecision(tc.present, tc.installed, tc.prompted); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}
