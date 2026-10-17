package ui

import (
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: a project history entry must never outrank one path component.
func TestMatrixPathDirectoryOneLevel(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "333.dev", "project")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	d := NewNewDialog()
	d.Show()
	d.SetPathSuggestions([]string{project})
	d.pathInput.SetValue(filepath.Join(root, "333."))
	d.focusIndex = d.indexOf(focusPath)
	d.updateFocus()
	// A previously browsed history suggestion must not change Tab semantics.
	d.suggestionsActive = true
	d.suggestionNavigated = true
	d.pathSuggestionCursor = 1
	d.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got, want := d.pathInput.Value(), filepath.Join(root, "333.dev")+"/"; got != want {
		t.Fatalf("Tab=%q want %q", got, want)
	}
	if d.currentTarget() != focusPath || !d.pathInput.Focused() {
		t.Fatal("Tab left the path editor")
	}
	d.Update(tea.KeyMsg{Type: tea.KeyTab})
	if d.pathInput.Value() != project+"/" {
		t.Fatal("a second deliberate Tab must complete the next directory")
	}
}

func TestMatrixPathDirectoryRemoteAndStaleResults(t *testing.T) {
	for _, scenario := range []string{"success", "edited", "reopened", "other-host", "left-field", "failure"} {
		t.Run(scenario, func(t *testing.T) {
			d := NewNewDialog()
			d.Show()
			d.remoteTarget = true
			d.remoteName = "fixture"
			d.pathInput.SetValue("/remote/333.")
			d.focusIndex = d.indexOf(focusPath)
			d.updateFocus()
			_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyTab})
			if cmd == nil || d.currentTarget() != focusPath || d.pathInput.Value() != "/remote/333." {
				t.Fatal("remote Tab must request completion and retain editing")
			}
			r := pathDirectoryResult{dialog: d, seq: d.pathCompletionSeq, remote: "fixture", query: "/remote/333.", matches: []string{"/remote/333.dev"}}
			switch scenario {
			case "edited":
				d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
			case "reopened":
				d.Hide()
				d.Show()
				d.pathInput.SetValue(r.query)
			case "other-host":
				d.remoteName = "other"
			case "left-field":
				d.moveFocus(1)
			case "failure":
				r.err = errors.New("offline")
			}
			before := d.pathInput.Value()
			h := &Home{newDialog: d}
			h.Update(r)
			if scenario == "success" {
				if d.pathInput.Value() != "/remote/333.dev/" {
					t.Fatal("remote completion was not applied")
				}
			} else if d.pathInput.Value() != before {
				t.Fatal("stale or failed remote response overwrote input")
			}
			if scenario == "failure" && !strings.Contains(d.pathCompletionStatus, "unavailable") {
				t.Fatal("failure lacks visible feedback")
			}
		})
	}
}

// matrix/hardened: preserve a useful reason rather than a generic failure.
func TestMatrixPathDirectoryErrorReason(t *testing.T) {
	d := NewNewDialog()
	d.Show()
	d.applyDirectoryMatches(nil, errors.New("verb not allowed over the channel"))
	if !strings.Contains(d.pathCompletionStatus, "verb not allowed") {
		t.Fatal(d.pathCompletionStatus)
	}
}
