package ui

import (
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: child editors must not retain Bubbles' Ctrl+N/P movement
// after those navigation aliases are removed from Agent Deck's own UI.
func TestMatrixOwnedEditorsNoControlNavigation(t *testing.T) {
	for name, editor := range map[string]textarea.Model{
		"notes":    newNotesEditor(),
		"feedback": NewFeedbackDialog().commentInput,
	} {
		t.Run(name, func(t *testing.T) {
			editor.SetValue("first\nsecond\nthird")
			editor.Focus()
			for editor.Line() > 0 {
				editor.CursorUp()
			}
			editor, _ = editor.Update(tea.KeyMsg{Type: tea.KeyDown})
			if editor.Line() != 1 {
				t.Fatal("down arrow must still move to the second line")
			}
			for _, key := range []tea.KeyType{tea.KeyCtrlN, tea.KeyCtrlP} {
				editor, _ = editor.Update(tea.KeyMsg{Type: key})
				if editor.Line() != 1 {
					t.Fatalf("%s moved editor to line %d", (tea.KeyMsg{Type: key}).String(), editor.Line())
				}
			}
			editor, _ = editor.Update(tea.KeyMsg{Type: tea.KeyUp})
			if editor.Line() != 0 {
				t.Fatal("up arrow must still move to the first line")
			}
		})
	}
}
