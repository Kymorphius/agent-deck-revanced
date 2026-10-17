package ui

// matrix/hardened: use explicit UI focus instead of trying to infer modes from
// terminal output. Text keeps the operator's IME; navigation uses raw keys.
import (
	"github.com/asheshgoplani/agent-deck/internal/inputmethod"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type inputMethodController interface {
	Sync(navigation bool)
	Restore()
	Blur()
}

// matrix/hardened: injectable without touching the desktop in startup tests.
var createInputMethodController = func() inputMethodController {
	c := inputmethod.New()
	if c == nil {
		return nil
	}
	return c
}

// ConfigureInputMethod is called only on the interactive CLI startup path.
// Terminals report focus changes, not necessarily their initial focus state.
// Start with keyboard focus; the native backend still requires the owning app
// to be foreground, and subsequent terminal blur reports revoke this focus.
func (h *Home) ConfigureInputMethod() bool {
	config, err := session.LoadUserConfig()
	if err != nil || !config.UI.AutoInputMethod {
		return false
	}
	controller := createInputMethodController()
	if controller == nil {
		return false
	}
	h.inputMethod = controller
	h.inputMethodFocused = true
	return true
}

// RestoreInputMethod is safe from both normal exit and the signal cleanup path.
func (h *Home) RestoreInputMethod() {
	if h.inputMethod != nil {
		h.inputMethod.Restore()
	}
}

func (h *Home) syncInputMethod(msg tea.Msg) {
	if h.inputMethod == nil {
		return
	}
	force := false
	switch msg.(type) {
	case tea.FocusMsg, tea.KeyMsg:
		h.inputMethodFocused = true
		force = true
	case tea.MouseMsg:
		// Hover reports do not prove keyboard focus (iTerm reports them even
		// for an inactive pane); a click does.
		if mouse := msg.(tea.MouseMsg); mouse.Action == tea.MouseActionPress && mouse.Button == tea.MouseButtonLeft {
			h.inputMethodFocused = true
			force = true
		}
	case tea.BlurMsg:
		h.inputMethodFocused = false
		h.inputMethod.Blur()
	}
	if h.inputMethodFocused {
		navigation := h.inputMethodNavigation()
		// Output/refresh ticks can be frequent. Native work only accompanies
		// real input, focus, or a mode transition, never each output frame.
		if force || !h.inputMethodKnown || navigation != h.inputMethodNav {
			h.inputMethod.Sync(navigation)
			h.inputMethodKnown, h.inputMethodNav = true, navigation
		}
	}
}

func (h *Home) inputMethodNavigation() bool {
	if h.isQuitting || h.isAttaching.Load() {
		return false
	}
	// Modal focus has priority over the session's insert mode underneath it.
	if h.search != nil && h.search.IsVisible() ||
		h.groupSearch != nil && h.groupSearch.IsVisible() || // matrix/hardened: group names are text input.
		h.globalSearch != nil && h.globalSearch.IsVisible() ||
		h.promptInputDialog != nil && h.promptInputDialog.IsVisible() ||
		h.zoxidePicker != nil && h.zoxidePicker.IsVisible() {
		return false
	}
	if d := h.newDialog; d != nil && d.IsVisible() {
		return !d.isTextInputFocused()
	}
	if d := h.groupDialog; d != nil && d.IsVisible() {
		return d.mode == GroupDialogMove || d.mode == GroupDialogCreate && d.focusIndex == 2
	}
	if d := h.editSessionDialog; d != nil && d.IsVisible() {
		return d.focusIndex < 0 || d.focusIndex >= len(d.fields) || d.fields[d.focusIndex].kind != editFieldText
	}
	if d := h.forkDialog; d != nil && d.IsVisible() {
		switch d.currentFocus() {
		case forkFocusName, forkFocusGroup, forkFocusBranch:
			return false
		default:
			return true
		}
	}
	if d := h.editPathsDialog; d != nil && d.IsVisible() {
		return !d.IsEditing()
	}
	if p := h.settingsPanel; p != nil && p.IsVisible() {
		return !p.editingText
	}
	if p := h.setupWizard; p != nil && p.IsVisible() {
		return !p.configDirInput.Focused()
	}
	if p := h.kanbanPanel; p != nil && p.IsVisible() {
		return !p.filterEditing
	}
	if h.notesEditing {
		return false
	}
	// These overlays consume shortcuts even if an embedded session is active.
	if h.helpOverlay != nil && h.helpOverlay.IsVisible() ||
		h.confirmDialog != nil && h.confirmDialog.IsVisible() ||
		h.hookTrustDialog != nil && h.hookTrustDialog.IsVisible() ||
		h.sessionSwitcher != nil && h.sessionSwitcher.IsVisible() ||
		h.scrollbackPager != nil && h.scrollbackPager.IsVisible() ||
		h.contextPager != nil && h.contextPager.IsVisible() ||
		h.sessionPickerDialog != nil && h.sessionPickerDialog.IsVisible() ||
		h.codeBlockDialog != nil && h.codeBlockDialog.IsVisible() ||
		h.mcpDialog != nil && h.mcpDialog.IsVisible() ||
		h.pluginDialog != nil && h.pluginDialog.IsVisible() ||
		h.skillDialog != nil && h.skillDialog.IsVisible() ||
		h.geminiModelDialog != nil && h.geminiModelDialog.IsVisible() ||
		h.worktreeFinishDialog != nil && h.worktreeFinishDialog.IsVisible() ||
		h.watcherPanel != nil && h.watcherPanel.IsVisible() ||
		h.agentsPanel != nil && h.agentsPanel.IsVisible() ||
		h.adoptPanel != nil && h.adoptPanel.IsVisible() ||
		h.webServicePanel != nil && h.webServicePanel.IsVisible() ||
		h.deadLetterPanel != nil && h.deadLetterPanel.IsVisible() ||
		h.toolVisibilityPanel != nil && h.toolVisibilityPanel.IsVisible() {
		return true
	}
	return !h.insertMode
}
