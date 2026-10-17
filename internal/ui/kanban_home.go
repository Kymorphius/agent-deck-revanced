package ui

// matrix/hardened: Home-side glue for the World Store task board. Kept out of
// home.go so upstream merges only meet a handful of one-line hooks there.

import (
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/worldstore"
)

// openKanbanPanel shows the board for the session under the cursor.
func (h *Home) openKanbanPanel() tea.Cmd {
	if h.kanbanPanel == nil {
		h.kanbanPanel = NewKanbanPanel()
	}
	h.kanbanPanel.SetSize(h.width, h.height)

	var env *kanbanEnv
	setupErr := ""
	cfg, err := session.LoadUserConfig()
	if err != nil {
		setupErr = err.Error()
	} else if client, cerr := worldstore.New(worldstore.Config{Bridge: cfg.WorldStore.Bridge, Node: cfg.WorldStore.Node}); cerr != nil {
		setupErr = cerr.Error() + "\nExample:\n  [worldstore]\n  bridge = \"/path/to/personal-panel/scripts/worldstore-ai.cjs\""
	} else {
		exe, _ := os.Executable()
		env = &kanbanEnv{client: client, device: worldstore.DeviceID(cfg.WorldStore.DeviceID), exe: exe, profile: h.profile, execCategory: cfg.WorldStore.GetExecutionCategory()}
	}

	var ctx *kanbanSession
	if inst := h.getSelectedSession(); inst != nil {
		s := kanbanSessionOf(inst)
		ctx = &s
	}
	return h.kanbanPanel.Open(env, setupErr, ctx, h.kanbanLookup)
}

func kanbanSessionOf(inst *session.Instance) kanbanSession {
	return kanbanSession{ID: inst.ID, Title: inst.Title, Path: inst.ProjectPath, Group: inst.GroupPath, Status: inst.GetStatusThreadSafe()}
}

// kanbanLookup resolves a linked session against the live deck. Called from
// View/Update on the UI goroutine, like every other instanceByID read there.
func (h *Home) kanbanLookup(id string) (kanbanSession, bool) {
	inst := h.getInstanceByID(id)
	if inst == nil {
		return kanbanSession{}, false
	}
	return kanbanSessionOf(inst), true
}

// handleKanbanMsg consumes the board's async results. ok is false for any
// other message.
func (h *Home) handleKanbanMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case kanbanLoadedMsg:
		if h.kanbanPanel != nil {
			h.kanbanPanel.handleLoaded(m)
		}
		return nil, true
	case kanbanWrittenMsg:
		if h.kanbanPanel != nil {
			return h.kanbanPanel.handleWritten(m), true
		}
		return nil, true
	case kanbanJumpMsg:
		if inst := h.getInstanceByID(m.sessionID); inst != nil {
			h.kanbanPanel.Hide()
			h.jumpToSession(inst)
		}
		return nil, true
	}
	return nil, false
}
