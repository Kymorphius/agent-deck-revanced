package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: Ctrl+N/P must leave all owned overlays unchanged, including
// widgets with their own completion bindings. Arrow navigation remains usable.
func TestMatrixModalCtrlNPDisabled(t *testing.T) {
	keys := []tea.KeyMsg{{Type: tea.KeyCtrlN}, {Type: tea.KeyCtrlP}}
	t.Run("agents", func(t *testing.T) {
		p := &AgentsPanel{visible: true, cursor: 1, rows: make([]agentPanelRow, 3)}
		for _, key := range keys {
			p, _ = p.Update(key)
			if p.cursor != 1 {
				t.Fatalf("%s moved cursor to %d", key.String(), p.cursor)
			}
		}
		p, _ = p.Update(tea.KeyMsg{Type: tea.KeyDown})
		if p.cursor != 2 {
			t.Fatal("Down should still navigate the agent list")
		}
	})
	t.Run("dead-letter", func(t *testing.T) {
		p := &DeadLetterPanel{visible: true, cursor: 1, records: make([]session.DeadLetterRecord, 3)}
		for _, key := range keys {
			p, _ = p.Update(key)
			if p.cursor != 1 {
				t.Fatalf("%s moved cursor to %d", key.String(), p.cursor)
			}
		}
		p, _ = p.Update(tea.KeyMsg{Type: tea.KeyUp})
		if p.cursor != 0 {
			t.Fatal("Up should still navigate the dead-letter list")
		}
	})
	t.Run("branch", func(t *testing.T) {
		d := &BranchPickerDialog{visible: true, cursor: 1, branches: []string{"a", "b", "c"}}
		for _, key := range keys {
			selected, handled := d.Update(key)
			if d.cursor != 1 || selected != "" || !handled {
				t.Fatalf("%s changed cursor or escaped the picker: cursor=%d selected=%q handled=%t", key.String(), d.cursor, selected, handled)
			}
		}
		d.Update(tea.KeyMsg{Type: tea.KeyDown})
		if d.cursor != 2 {
			t.Fatal("Down should still navigate branch choices")
		}
	})
	t.Run("zoxide", func(t *testing.T) {
		calls := 0
		z := newZoxidePickerWithQueryFn(func(string) ([]string, error) {
			calls++
			return []string{"/a", "/b", "/c"}, nil
		})
		z.Show()
		z.cursor = 1
		initialCalls := calls
		for _, key := range keys {
			z, _ = z.Update(key)
			if z.cursor != 1 || z.queryInput.Value() != "" || calls != initialCalls {
				t.Fatalf("%s changed selection, query, or queried again", key.String())
			}
		}
		z, _ = z.Update(tea.KeyMsg{Type: tea.KeyDown})
		if z.cursor != 2 {
			t.Fatal("Down should still navigate zoxide results")
		}
		z, _ = z.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
		if z.queryInput.Value() != "x" || calls != initialCalls+1 {
			t.Fatal("ordinary query input should still refresh results")
		}
	})
	t.Run("group-path", func(t *testing.T) {
		g := NewGroupDialog()
		g.Show()
		g.focusPath()
		g.pathInput.SetValue("/path")
		g.pathSuggestions = []string{"/path/a", "/path/b", "/path/c"}
		g.pathSuggestionCursor = 1
		for _, key := range keys {
			g, _ = g.Update(key)
			if g.focusIndex != 1 || g.pathSuggestionCursor != 1 || g.pathInput.Value() != "/path" {
				t.Fatalf("%s changed group path focus, selection, or input", key.String())
			}
		}
		g, _ = g.Update(tea.KeyMsg{Type: tea.KeyUp})
		if g.pathSuggestionCursor != 0 {
			t.Fatal("Up should still navigate group path suggestions")
		}
	})
	t.Run("edit-paths", func(t *testing.T) {
		d := NewEditPathsDialog()
		d.visible = true
		d.editing = true
		d.paths = []string{"/path"}
		d.pathInput.SetValue("/path")
		d.pathInput.Focus()
		d.pathSuggestions = []string{"/path/a", "/path/b", "/path/c"}
		d.pathSuggestionCursor = 1
		d.suggestionNavigated = true
		for _, key := range keys {
			d, _ = d.Update(key)
			if d.pathSuggestionCursor != 1 || !d.suggestionNavigated || d.pathInput.Value() != "/path" {
				t.Fatalf("%s changed path selection, input, or navigation state", key.String())
			}
		}
		d, _ = d.Update(tea.KeyMsg{Type: tea.KeyDown})
		if d.pathSuggestionCursor != 2 {
			t.Fatal("Down should still navigate editable path suggestions")
		}
		d, _ = d.Update(tea.KeyMsg{Type: tea.KeyUp})
		if d.pathSuggestionCursor != 1 {
			t.Fatal("Up should still navigate editable path suggestions")
		}
	})
	t.Run("recent-picker", func(t *testing.T) {
		d := NewNewDialog()
		d.Show()
		d.recentSessions = []*statedb.RecentSessionRow{
			{Title: "alpha", ProjectPath: "/a", Tool: "claude"},
			{Title: "beta", ProjectPath: "/b", Tool: "claude"},
			{Title: "gamma", ProjectPath: "/c", Tool: "claude"},
		}
		d.showRecentPicker = true
		d.recentSessionCursor = 1
		initialPath := d.pathInput.Value()
		for _, key := range keys {
			d, _ = d.Update(key)
			if d.recentSessionCursor != 1 || d.pathInput.Value() != initialPath {
				t.Fatalf("%s changed recent selection or preview", key.String())
			}
		}
		d, _ = d.Update(tea.KeyMsg{Type: tea.KeyDown})
		if d.recentSessionCursor != 2 {
			t.Fatal("Down should still navigate recent sessions")
		}
	})
}
