package ui

// matrix/hardened: model-level focus tests use a fake native controller.
import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type fakeInputMethod struct{ modes []bool }

func (f *fakeInputMethod) Sync(nav bool) { f.modes = append(f.modes, nav) }
func (f *fakeInputMethod) Restore()      {}
func (f *fakeInputMethod) Blur()         {}

func TestInputMethodModes(t *testing.T) {
	h := &Home{}
	check := func(want bool) {
		t.Helper()
		if got := h.inputMethodNavigation(); got != want {
			t.Fatalf("navigation=%v want=%v", got, want)
		}
	}
	check(true)
	h.insertMode = true
	check(false)
	h.sessionSwitcher = NewSessionSwitcher()
	h.sessionSwitcher.visible = true
	check(true)
	h.sessionSwitcher.visible = false
	h.insertMode = false
	h.search = NewSearch()
	h.search.visible = true
	check(false)
	h.search.visible = false
	h.groupDialog = NewGroupDialog()
	h.groupDialog.Show()
	check(false)
	h.groupDialog.mode = GroupDialogMove
	check(true)
	h.groupDialog.visible = false
	h.settingsPanel = NewSettingsPanel()
	h.settingsPanel.visible = true
	check(true)
	h.settingsPanel.editingText = true
	check(false)
	h.settingsPanel.visible = false
	h.notesEditing = true
	check(false)
	h.notesEditing = false
	h.isAttaching.Store(true)
	check(false)
	h.isAttaching.Store(false)
	h.isQuitting = true
	check(false)
}

func TestInputMethodFocusAndRefresh(t *testing.T) {
	f := &fakeInputMethod{}
	h := &Home{inputMethod: f}
	h.syncInputMethod(tea.WindowSizeMsg{})
	if len(f.modes) != 0 {
		t.Fatal("startup without keyboard focus must not acquire the IME")
	}
	h.syncInputMethod(tea.FocusMsg{})
	if len(f.modes) != 1 || !f.modes[0] {
		t.Fatal("focused list did not enter navigation")
	}
	h.syncInputMethod(tea.WindowSizeMsg{})
	if len(f.modes) != 1 {
		t.Fatal("unchanged refresh invoked the native controller")
	}
	h.notesEditing = true
	h.syncInputMethod(tea.KeyMsg{Type: tea.KeyEnter})
	if f.modes[len(f.modes)-1] {
		t.Fatal("text editor did not restore the IME")
	}
	h.syncInputMethod(tea.BlurMsg{})
	h.notesEditing = false
	h.syncInputMethod(tea.WindowSizeMsg{})
	h.syncInputMethod(tea.MouseMsg{Action: tea.MouseActionMotion})
	if len(f.modes) != 2 {
		t.Fatal("blurred background updates changed input mode")
	}
	h.syncInputMethod(tea.FocusMsg{})
	if len(f.modes) != 3 || !f.modes[2] {
		t.Fatal("returning to the list did not reapply navigation")
	}
}

func TestInputMethodHomeUpdateSearch(t *testing.T) {
	h, _ := buildFocusHome(t)
	f := &fakeInputMethod{}
	h.inputMethod = f
	h.Update(tea.FocusMsg{})
	h.Update(plainKeyMsg('/'))
	if !h.search.IsVisible() || len(f.modes) < 2 || f.modes[len(f.modes)-1] {
		t.Fatal("opening search through Home.Update did not restore text input")
	}
	h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文")})
	if got := h.search.input.Value(); got != "中文" {
		t.Fatalf("search text was changed by navigation handling: %q", got)
	}
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.search.IsVisible() || !f.modes[len(f.modes)-1] {
		t.Fatal("closing search did not restore list navigation")
	}
}

func TestInputMethodFormFields(t *testing.T) {
	d := NewNewDialog()
	d.Show()
	h := &Home{newDialog: d}
	d.focusIndex = d.indexOf(focusName)
	if h.inputMethodNavigation() {
		t.Fatal("name input was treated as a shortcut picker")
	}
	d.focusIndex = d.indexOf(focusCreate)
	if !h.inputMethodNavigation() {
		t.Fatal("create button was treated as text input")
	}
	d.Hide()
	h.groupDialog = NewGroupDialog()
	h.groupDialog.Show()
	h.groupDialog.focusIndex = 2
	if !h.inputMethodNavigation() {
		t.Fatal("group target picker was treated as text input")
	}
	h.groupDialog.focusIndex = 0
	if h.inputMethodNavigation() {
		t.Fatal("group name was treated as navigation")
	}
}

func TestInputMethodConfiguredStartupWithoutFocusReport(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	if err := session.SaveUserConfig(&session.UserConfig{UI: session.UISettings{AutoInputMethod: true}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeInputMethod{}
	previous := createInputMethodController
	createInputMethodController = func() inputMethodController { return f }
	t.Cleanup(func() { createInputMethodController = previous })
	h := &Home{}
	if !h.ConfigureInputMethod() {
		t.Fatal("explicitly enabled input method was not configured")
	}
	// Teft sends no initial FocusMsg when the foreground TUI re-execs.
	h.syncInputMethod(tea.WindowSizeMsg{})
	if len(f.modes) != 1 || !f.modes[0] {
		t.Fatal("startup waited for a focus report before bypassing the IME")
	}
	h.syncInputMethod(tea.BlurMsg{})
	h.notesEditing = true
	h.syncInputMethod(tea.WindowSizeMsg{})
	if len(f.modes) != 1 {
		t.Fatal("startup focus assumption overrode an actual blur report")
	}
}
