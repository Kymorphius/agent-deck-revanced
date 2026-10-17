package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: direct editing replaces the former soft-select landing.
func TestNewDialog_PathSelector_UpDownEscapesField_RegressionFor1020(t *testing.T) {
	d := NewNewDialog()
	d.Show()
	d.SetPathSuggestions([]string{"/p/alpha", "/p/beta"})
	d.focusIndex = d.indexOf(focusPath)
	d.updateFocus()
	if !d.pathInput.Focused() || d.pathSoftSelected {
		t.Fatal("Path is not directly editable")
	}
	d.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if d.currentTarget() == focusPath {
		t.Fatal("Shift+Tab must leave the editor")
	}
	d.focusIndex = d.indexOf(focusPath)
	d.updateFocus()
	d.Update(tea.KeyMsg{Type: tea.KeyDown})
	if !d.IsSuggestionsActive() {
		t.Fatal("Down must allow explicit browsing")
	}
	d.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if d.IsSuggestionsActive() || !d.pathInput.Focused() {
		t.Fatal("Esc must return to editing")
	}
}
