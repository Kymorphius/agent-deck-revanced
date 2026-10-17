package ui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

// matrix/hardened: only remote rows must not lock creation to remote storage.
func TestMatrixGroupTargetRemoteOnlyCanCreateLocal(t *testing.T) {
	h := armHomeWithOneRemoteSessionInGroups(t)
	h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	g := h.groupDialog
	if g.RemoteName() != "lab" {
		t.Fatal("cursor target lost")
	}
	g.pathInput.SetValue("/remote/project")
	g.nameInput.SetValue("local-only-group")
	h.handleGroupDialogKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	if g.RemoteName() != "" || g.HasParent() || g.CanToggle() || g.GetDefaultPath() != "" {
		t.Fatal("remote context leaked to Local")
	}
	if !strings.Contains(g.View(), "Local") {
		t.Fatal("Local destination not visible")
	}
	h.handleGroupDialogKey(tea.KeyMsg{Type: tea.KeyEnter})
	found := false
	for _, group := range h.groupTree.Groups {
		if group.Name == "local-only-group" {
			found = true
		}
	}
	if !found {
		t.Fatal("group was not created in local tree")
	}
}

func TestMatrixGroupTargetFocusAndContext(t *testing.T) {
	h := armHomeWithOneRemoteSessionInGroups(t)
	h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	g := h.groupDialog
	g.SetCreationTargets([]string{"other", "lab"})
	g.nameInput.SetValue("keep-name")
	g.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if g.focusIndex != 2 {
		t.Fatal("Target not reachable with Shift+Tab")
	}
	// Enter on the target must change device, without submitting the form.
	h.handleGroupDialogKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !g.IsVisible() || g.RemoteName() != "other" || g.GetValue() != "keep-name" {
		t.Fatal("target Enter submitted or corrupted form")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if g.RemoteName() != "lab" || !g.CanToggle() || g.GetParentPath() != "" {
		t.Fatal("original host context not restored safely at root")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyTab})
	if g.focusIndex != 0 {
		t.Fatal("Target Tab must return to Name")
	}
	g.Hide()
	g.ShowCreateWithContext("", "")
	if len(g.creationTargets) != 0 || g.RemoteName() != "" {
		t.Fatal("stale target state on reopen")
	}
}
