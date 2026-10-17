package ui

// matrix/hardened: Tab completion on the new-session path field must descend
// through unique matches like a shell instead of re-offering the same
// directory forever.

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMatrixPathCompletion_UniqueMatchDescends(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "beta", "inner"), 0755); err != nil {
		t.Fatal(err)
	}

	d := NewNewDialog()
	d.SetSize(200, 50)
	d.Show()
	d.focusIndex = 2
	d.updateFocus()
	if d.currentTarget() != focusPath {
		t.Fatalf("expected focusPath, got %v", d.currentTarget())
	}

	d.pathInput.SetValue(filepath.Join(tmpDir, "be"))
	d.pathInput.SetCursor(len(d.pathInput.Value()))

	d, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got, want := d.pathInput.Value(), filepath.Join(tmpDir, "beta")+"/"; got != want {
		t.Fatalf("first Tab: got %q, want %q", got, want)
	}
	if d.pathCycler.IsActive() {
		t.Fatal("unique match must not arm the cycler")
	}

	d, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got, want := d.pathInput.Value(), filepath.Join(tmpDir, "beta", "inner")+"/"; got != want {
		t.Fatalf("second Tab should descend: got %q, want %q", got, want)
	}

	// The trailing "/" left by completion is not part of the session path.
	if _, path, _ := d.GetValues(); path != filepath.Join(tmpDir, "beta", "inner") {
		t.Fatalf("GetValues path = %q, want trailing separator dropped", path)
	}

	// matrix/hardened: even a leaf stays in the editor; Enter moves on.
	d, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
	if d.currentTarget() != focusPath {
		t.Fatal("Tab must remain in Path")
	}
}

func TestMatrixPathCompletion_TildeUniqueMatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "proj", "sub"), 0755); err != nil {
		t.Fatal(err)
	}

	d := NewNewDialog()
	d.SetSize(200, 50)
	d.Show()
	d.focusIndex = 2
	d.updateFocus()

	d.pathInput.SetValue("~/pr")
	d.pathInput.SetCursor(len(d.pathInput.Value()))
	d, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := d.pathInput.Value(); got != "~/proj/" {
		t.Fatalf("first Tab: got %q, want %q", got, "~/proj/")
	}
	d, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := d.pathInput.Value(); got != "~/proj/sub/" {
		t.Fatalf("second Tab: got %q, want %q", got, "~/proj/sub/")
	}
}
