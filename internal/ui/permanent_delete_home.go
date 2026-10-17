package ui

import (
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: all I/O runs in commands; the renderer and input reader
// remain active throughout preview, cancellation and permanent deletion.
type permanentDeletePreviewMsg struct {
	requestID uint64
	sessionID string
	plan      session.PermanentDeletePlan
	err       error
}

type permanentDeleteResultMsg struct {
	requestID uint64
	sessionID string
	err       error
}

func (h *Home) beginPermanentDelete(id, title string) tea.Cmd {
	if h.permanentDeleteDialog == nil {
		h.permanentDeleteDialog = NewPermanentDeleteDialog()
	}
	h.permanentDeleteRequest++
	requestID := h.permanentDeleteRequest
	h.permanentDeleteDialog.SetSize(h.width, h.height)
	h.permanentDeleteDialog.ShowLoading(requestID, id, title)
	profile := h.storage.Profile()
	return func() tea.Msg {
		msg := permanentDeletePreviewMsg{requestID: requestID, sessionID: id}
		storage, err := session.NewLiveReadOnlyStorageWithProfile(profile)
		if err != nil {
			msg.err = err
			return msg
		}
		defer storage.Close()
		inst, err := permanentDeleteInstance(storage, id)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.plan, msg.err = storage.PlanPermanentDelete(inst)
		return msg
	}
}

func permanentDeleteInstance(storage *session.Storage, id string) (*session.Instance, error) {
	instances, err := storage.Load()
	if err != nil {
		return nil, err
	}
	for _, inst := range instances {
		if inst.ID == id {
			return inst, nil
		}
	}
	return nil, fmt.Errorf("session no longer exists; close this preview and refresh the list")
}

func (h *Home) handlePermanentDeleteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	dialog := h.permanentDeleteDialog
	if !dialog.HandleKey(msg) {
		return h, nil
	}
	requestID, id, plan := dialog.RequestID(), dialog.SessionID(), dialog.Plan()
	profile := h.storage.Profile()
	dialog.StartDeleting()
	return h, func() tea.Msg {
		msg := permanentDeleteResultMsg{requestID: requestID, sessionID: id}
		storage, err := session.NewStorageWithProfile(profile)
		if err != nil {
			msg.err = err
			return msg
		}
		defer storage.Close()
		inst, err := permanentDeleteInstance(storage, id)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.err = storage.PermanentlyDelete(inst, plan)
		return msg
	}
}

func (h *Home) handlePermanentDeleteMsg(msg tea.Msg) (tea.Cmd, bool) {
	dialog := h.permanentDeleteDialog
	switch msg := msg.(type) {
	case permanentDeletePreviewMsg:
		// matrix/hardened: cancelled/replaced previews cannot reopen the modal
		// or approve another target when their background read finishes late.
		if dialog != nil && dialog.SessionID() == msg.sessionID {
			dialog.SetPreview(msg.requestID, msg.plan, msg.err)
		}
		return nil, true
	case permanentDeleteResultMsg:
		if dialog == nil || !dialog.IsVisible() || dialog.RequestID() != msg.requestID || dialog.SessionID() != msg.sessionID {
			return nil, true
		}
		if msg.err != nil {
			dialog.SetDeleteError(msg.requestID, fmt.Errorf("permanent deletion incomplete: %w; close, review and retry", msg.err))
			return nil, true
		}
		dialog.Hide()
		// Permanent deletion never enters the ordinary undo/recent path.
		return h.sessionLoadCmd(nil, false), true
	}
	return nil, false
}
