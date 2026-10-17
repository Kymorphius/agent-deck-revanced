package ui

// matrix/hardened: path suggestions must stay below the editable value, fit
// the dialog, and preserve raw paths while abbreviating only their display.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func matrixDropdownPaths() []string {
	paths := make([]string, 15)
	for i := range paths {
		paths[i] = "/Users/fixture/" + strings.Repeat("很长的父目录👩🏽‍💻/", 10) + fmt.Sprintf("项目-%02d", i)
	}
	return paths
}

func matrixDropdownLine(t *testing.T, lines []string, needle string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, needle) {
			return i
		}
	}
	t.Fatalf("rendered view does not contain %q:\n%s", needle, strings.Join(lines, "\n"))
	return -1
}

// Compare to the same dialog with the menu dismissed: the focused input row
// must remain intact, and the floating border must fit inside the dialog width.
func matrixAssertDropdownPlacement(t *testing.T, base, view, input, selectedSuffix string, width, height int) {
	t.Helper()
	baseLines := strings.Split(ansi.Strip(base), "\n")
	lines := strings.Split(ansi.Strip(view), "\n")
	// matrix/hardened: inline candidates can move a centered form; compare
	// input content at its actual row instead of assuming a fixed screen row.
	baseInputRow := matrixDropdownLine(t, baseLines, input)
	inputRow := matrixDropdownLine(t, lines, input)
	if strings.Trim(baseLines[baseInputRow], " │") != strings.Trim(lines[inputRow], " │") {
		t.Fatalf("candidate list changed the input: before %q, after %q", baseLines[baseInputRow], lines[inputRow])
	}
	selectedRow := inputRow + 1 + matrixDropdownLine(t, lines[inputRow+1:], selectedSuffix)
	if selectedRow <= inputRow {
		t.Fatalf("selected suggestion row %d must be below focused input row %d", selectedRow, inputRow)
	}
	if !strings.Contains(lines[selectedRow], "▶ ") {
		t.Fatalf("selected candidate lost its marker: %q", lines[selectedRow])
	}

	menuTop := -1
	for row := inputRow + 1; row < selectedRow; row++ {
		if strings.Contains(lines[row], "╭") {
			menuTop = row
			break
		}
	}
	if menuTop != inputRow+1 {
		t.Fatalf("menu border row = %d, want immediately below input row %d", menuTop, inputRow)
	}
	menuLeft := strings.Index(lines[menuTop], "╭")
	menuRight := strings.Index(lines[menuTop][menuLeft:], "╮")
	if menuRight < 0 {
		t.Fatalf("dropdown top border is incomplete: %q", lines[menuTop])
	}
	menuRight += menuLeft
	menuRightCell := lipgloss.Width(lines[menuTop][:menuRight])
	foundDialogBorder := false
	for row := baseInputRow; row >= 0; row-- {
		if right := strings.Index(baseLines[row], "╮"); right >= 0 {
			foundDialogBorder = true
			dialogRightCell := lipgloss.Width(baseLines[row][:right])
			if menuRightCell > dialogRightCell {
				t.Fatalf("dropdown right cell %d exceeds dialog right cell %d", menuRightCell, dialogRightCell)
			}
			break
		}
	}
	if !foundDialogBorder {
		t.Fatal("dismissed baseline has no enclosing dialog border")
	}
	if len(lines) > height {
		t.Fatalf("view has %d rows, exceeds terminal height %d", len(lines), height)
	}
	for i, line := range lines {
		if cells := lipgloss.Width(line); cells > width {
			t.Fatalf("row %d has %d cells, exceeds terminal width %d: %q", i, cells, width, line)
		}
		if !utf8.ValidString(line) {
			t.Fatalf("row %d is invalid UTF-8: %q", i, line)
		}
	}
}

func TestMatrixPathDropdown_GroupInputVisible(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 40}} {
		for _, mode := range []string{"root", "context-root", "subgroup", "subgroup-wrapped-parent", "remote", "remote-wrapped-parent"} {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], mode), func(t *testing.T) {
				g := NewGroupDialog()
				g.SetSize(size[0], size[1])
				switch mode {
				case "context-root":
					g.ShowCreateWithContextDefaultRoot("Tools", "Tools")
				case "subgroup":
					g.ShowCreateWithContext("Tools", "Tools")
				case "subgroup-wrapped-parent":
					g.ShowCreateWithContext("Tools", strings.Repeat("父分组🌟", 8))
				case "remote":
					g.ShowCreateRemoteWithContext("Tools", "Tools", "fixture-host")
				case "remote-wrapped-parent":
					g.ShowCreateRemoteWithContext("Tools", strings.Repeat("父分组🌟", 8), "fixture-host")
				default:
					g.Show()
				}
				g.focusPath()
				g.SetPathSuggestions(matrixDropdownPaths())
				g.pathInput.SetValue("PATH_INPUT_SENTINEL")
				g.pathSuggestionCursor = len(g.pathSuggestions) - 1
				g.pathSuggestionsHidden = true
				base := g.View()
				g.pathSuggestionsHidden = false
				matrixAssertDropdownPlacement(t, base, g.View(), "PATH_INPUT_SENTINEL", "项目-14", size[0], size[1])
			})
		}
	}
}

func TestMatrixPathDropdown_NewHistoryInputVisible(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			d := NewNewDialog()
			d.SetSize(size[0], size[1])
			d.Show()
			d.focusIndex = 2
			d.updateFocus()
			d.SetPathSuggestions(matrixDropdownPaths())
			d.pathInput.SetValue("PATH_INPUT_SENTINEL")
			d.pathSuggestionCursor = len(d.pathSuggestions)
			d.suggestionsHidden = true
			base := d.View()
			d.suggestionsHidden = false
			matrixAssertDropdownPlacement(t, base, d.View(), "PATH_INPUT_SENTINEL", "项目-14", size[0], size[1])
		})
	}
}

func TestMatrixPathDropdown_NewFilesystemCompletionInputVisible(t *testing.T) {
	parent := filepath.Join(t.TempDir(), strings.Repeat("long-parent-", 5), "nested")
	for i := 0; i < 15; i++ {
		if err := os.MkdirAll(filepath.Join(parent, fmt.Sprintf("candidate-%02d-项目👩🏽‍💻", i)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			d := NewNewDialog()
			d.SetSize(size[0], size[1])
			d.Show()
			d.focusIndex = 2
			d.updateFocus()
			d.pathInput.SetValue(filepath.Join(parent, "candidate-"))
			d.pathInput.SetCursor(len(d.pathInput.Value()))
			for i := 0; i < 15; i++ {
				d, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
			}
			if got := d.pathCycler.Index(); got != 14 {
				t.Fatalf("Tab did not reach final candidate: index = %d", got)
			}
			if got, want := d.pathInput.Value(), filepath.Join(parent, "candidate-14-项目👩🏽‍💻"); got != want {
				t.Fatalf("completion changed raw path: got %q, want %q", got, want)
			}
			d.suggestionsHidden = true
			base := d.View()
			d.suggestionsHidden = false
			// This suffix is unique to the input in the dismissed baseline.
			matrixAssertDropdownPlacement(t, base, d.View(), "candidate-14-项目👩🏽‍💻", "candidate-14-项目👩🏽‍💻", size[0], size[1])
		})
	}
}

func TestMatrixPathDropdown_MultiRepoEditingInputVisible(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			d := NewNewDialog()
			d.SetSize(size[0], size[1])
			d.Show()
			d.ToggleMultiRepo()
			d.multiRepoPaths = []string{"/fixture/repo-a", "/fixture/repo-b", "/fixture/repo-c", "/fixture/repo-d", "/fixture/repo-e"}
			d.multiRepoPathCursor = 4
			d.multiRepoEditing = true
			d.focusIndex = d.indexOf(focusMultiRepo)
			d.updateFocus()
			d.SetPathSuggestions(matrixDropdownPaths())
			d.pathInput.SetValue("PATH_INPUT_SENTINEL")
			d.pathSuggestionCursor = len(d.pathSuggestions)
			d.suggestionsHidden = true
			base := d.View()
			d.suggestionsHidden = false
			matrixAssertDropdownPlacement(t, base, d.View(), "PATH_INPUT_SENTINEL", "项目-14", size[0], size[1])
		})
	}
}

func TestMatrixPathDropdown_SelectedTailFitsLimitedRowsAndRawAccept(t *testing.T) {
	paths := matrixDropdownPaths()
	for _, height := range []int{3, 4, 6} {
		for _, cursor := range []int{0, 7, 14} {
			t.Run(fmt.Sprintf("height-%d/cursor-%d", height, cursor), func(t *testing.T) {
				g := NewGroupDialog()
				g.Show()
				g.focusPath()
				g.SetPathSuggestions(paths)
				g.pathSuggestionCursor = cursor
				groupMenu := g.renderPathDropdown(32, height)
				g.ApplyHighlightedPathSuggestion()
				if got := g.GetDefaultPath(); got != paths[cursor] {
					t.Fatalf("group accept changed raw path: %q", got)
				}

				d := NewNewDialog()
				d.Show()
				d.focusIndex = 2
				d.updateFocus()
				d.SetPathSuggestions(paths)
				d.pathSuggestionCursor = cursor + 1
				d.suggestionsActive = true
				historyMenu := d.renderSuggestionsDropdown(32, height)
				d.ApplyHighlightedSuggestion()
				if _, got, _ := d.GetValues(); got != paths[cursor] {
					t.Fatalf("new-session accept changed raw path: %q", got)
				}

				d.pathCycler.SetMatches(paths)
				for range cursor + 1 {
					d.pathCycler.Next()
				}
				completionMenu := d.renderCompletionDropdown(32, height)
				for name, menu := range map[string]string{"group": groupMenu, "history": historyMenu, "completion": completionMenu} {
					plain := ansi.Strip(menu)
					if !strings.Contains(plain, fmt.Sprintf("项目-%02d", cursor)) || !strings.Contains(plain, "▶ ") {
						t.Errorf("%s menu lost selected tail:\n%s", name, plain)
					}
					if !strings.Contains(plain, "…") {
						t.Errorf("%s long path did not indicate abbreviation:\n%s", name, plain)
					}
					if cells, rows := lipgloss.Width(menu), lipgloss.Height(menu); cells > 32 || rows > height {
						t.Errorf("%s menu size = %dx%d, exceeds 32x%d:\n%s", name, cells, rows, height, plain)
					}
					if !utf8.ValidString(plain) {
						t.Errorf("%s menu invalid UTF-8: %q", name, plain)
					}
				}
			})
		}
	}
}
