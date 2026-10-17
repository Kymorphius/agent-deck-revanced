package ui

import (
	"strings"
	"testing"
)

// matrix/hardened: recent interactions have a configurable list-only shortcut
// that participates in the same canonical routing and conflict rules as other views.
func TestMatrixRecentHotkeys(t *testing.T) {
	const canonical = "ctrl+e"
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		key       string
		want      string
		blocked   bool
	}{
		{name: "default", key: canonical, want: canonical},
		{name: "remapped", overrides: map[string]string{hotkeyToggleRecentView: "ctrl+g"}, key: "ctrl+g", want: canonical, blocked: true},
		{name: "disabled", overrides: map[string]string{hotkeyToggleRecentView: ""}, key: canonical, blocked: true},
		{name: "conflicting earlier action", overrides: map[string]string{hotkeyNewSession: canonical}, key: canonical, want: defaultHotkeyBindings[hotkeyNewSession]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bindings := resolveHotkeys(tc.overrides)
			lookup, blocked := buildHotkeyLookup(bindings)
			if got := lookup[tc.key]; got != tc.want {
				t.Fatalf("key %q maps to %q, want %q", tc.key, got, tc.want)
			}
			if got := blocked[canonical]; got != tc.blocked {
				t.Fatalf("default recent key blocked = %v, want %v", got, tc.blocked)
			}
			if tc.blocked && lookup[canonical] != "" {
				t.Fatalf("released default recent key still maps to %q", lookup[canonical])
			}
		})
	}
}

// matrix/hardened: help advertises the effective recent shortcut and the full
// view cycle, and never shows a disabled or shadowed direct shortcut.
func TestMatrixRecentHelp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		wantKey   string
	}{
		{name: "default", wantKey: "ctrl+e"},
		{name: "remapped", overrides: map[string]string{hotkeyToggleRecentView: "ctrl+g"}, wantKey: "ctrl+g"},
		{name: "disabled", overrides: map[string]string{hotkeyToggleRecentView: ""}},
		{name: "conflict", overrides: map[string]string{hotkeyNewSession: "ctrl+e"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			help := NewHelpOverlay()
			help.SetHotkeys(resolveHotkeys(tc.overrides))
			help.SetSize(200, 300)
			help.Show()
			view := stripAnsi(help.View())
			if !strings.Contains(view, "Cycle view: normal / active / populated / pinned / recent") {
				t.Fatalf("help is missing the complete view cycle:\n%s", view)
			}
			found := false
			for _, line := range strings.Split(view, "\n") {
				if strings.Contains(line, "Toggle 最近 / Recent view") {
					found = true
					if tc.wantKey == "" || !strings.Contains(line, tc.wantKey) || !strings.Contains(line, "latest first") {
						t.Fatalf("unexpected recent help row: %q", line)
					}
				}
			}
			if found != (tc.wantKey != "") {
				t.Fatalf("recent help visibility = %v, want key %q", found, tc.wantKey)
			}
		})
	}
}
