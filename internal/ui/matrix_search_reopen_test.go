package ui

import (
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: exercise reopening the / overlay after typing a query,
// including the opt-out and a live settings change in the same instance.
func TestMatrixLocalSearchReopen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clear *bool
	}{
		{name: "default"},
		{name: "clear", clear: boolPtr(true)},
		{name: "remember", clear: boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
			session.ClearUserConfigCache()
			t.Cleanup(session.ClearUserConfigCache)
			if err := session.SaveUserConfig(&session.UserConfig{UI: session.UISettings{ClearSearchOnOpen: tc.clear}}); err != nil {
				t.Fatal(err)
			}

			s := NewSearch()
			s.SetItems([]*session.Instance{{Title: "alpha"}, {Title: "beta"}, {Title: "beta-second"}})
			s.Show()
			s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("beta")})
			s.Update(tea.KeyMsg{Type: tea.KeyDown})
			if s.input.Value() != "beta" || len(s.results) != 2 || s.cursor != 1 {
				t.Fatal("precondition: query must narrow results and move selection")
			}
			s.Update(tea.KeyMsg{Type: tea.KeyEsc})
			s.Show()
			if tc.clear == nil || *tc.clear {
				if s.input.Value() != "" || len(s.results) != 3 || s.cursor != 0 {
					t.Fatalf("reopened search: query=%q results=%d cursor=%d; want empty, all 3 results, first selected", s.input.Value(), len(s.results), s.cursor)
				}
			} else if s.input.Value() != "beta" || len(s.results) != 2 || s.cursor != 1 {
				t.Fatalf("remembered search: query=%q results=%d cursor=%d; want beta, 2 results, second selected", s.input.Value(), len(s.results), s.cursor)
			}

			// Saving a changed preference must affect this existing overlay.
			s.Hide()
			s.input.SetValue("beta")
			clear := true
			if err := session.SaveUserConfig(&session.UserConfig{UI: session.UISettings{ClearSearchOnOpen: &clear}}); err != nil {
				t.Fatal(err)
			}
			s.Show()
			if s.input.Value() != "" || len(s.results) != 3 {
				t.Fatal("changing the setting must clear the same search instance on next open")
			}
		})
	}
}
