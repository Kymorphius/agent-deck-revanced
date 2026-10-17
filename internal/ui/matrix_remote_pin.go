package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: remote pins remain authoritative on their owning host.
type matrixRemotePinRunner interface {
	RunCommand(context.Context, ...string) ([]byte, error)
}

var newMatrixRemotePinRunner = func(name string, rc session.RemoteConfig) (matrixRemotePinRunner, error) {
	return session.NewSSHRunner(name, rc), nil
}

// Names and IDs are separate fields so equal IDs on different hosts cannot
// share an in-flight action. No background worker or retry timer is added.
type matrixRemotePinTarget struct {
	remoteName string
	sessionID  string
}

type matrixRemotePinResultMsg struct {
	target matrixRemotePinTarget
	pin    session.PinMode
	config *session.RemoteConfig
	err    error
}

func matrixNextPin(pin session.PinMode) session.PinMode {
	switch pin {
	case session.PinNone:
		return session.PinTop
	case session.PinTop:
		return session.PinBottom
	default:
		return session.PinNone
	}
}

func matrixRemoteSessionIsPinned(info session.RemoteSessionInfo) bool {
	return info.PinKnown() && (info.PinPosition() == session.PinTop || info.PinPosition() == session.PinBottom)
}

func (h *Home) startMatrixRemotePin(name, id string, pin session.PinMode) tea.Cmd {
	// matrix/hardened: read the row and its owning configuration together;
	// asynchronous poll setup may clear rows while repointing a host name.
	h.remoteSessionsMu.RLock()
	rc, configured := h.remotePollConfig[name]
	var info session.RemoteSessionInfo
	ok := false
	for _, row := range h.remoteSessions[name] {
		if row.ID == id {
			info, ok = row, true
			break
		}
	}
	h.remoteSessionsMu.RUnlock()
	if !ok || id == "" {
		h.setError(fmt.Errorf("session no longer listed on %s", name))
		return nil
	}
	if !info.PinKnown() {
		h.setError(fmt.Errorf("update Agent Deck on %s to use remote pins", name))
		return nil
	}
	if pin != session.PinNone && pin != session.PinTop && pin != session.PinBottom {
		h.setError(fmt.Errorf("invalid remote pin position %q", pin))
		return nil
	}
	target := matrixRemotePinTarget{name, id}
	if h.matrixRemotePinPending == nil {
		h.matrixRemotePinPending = make(map[matrixRemotePinTarget]session.PinMode)
	}
	if _, busy := h.matrixRemotePinPending[target]; busy {
		return nil
	}
	if !configured {
		h.setError(fmt.Errorf("remote configuration for %s is not ready; refresh first", name))
		return nil
	}
	// matrix/hardened: bind the write to the host that owns the displayed row,
	// before returning a deferred command; a name may be repointed meanwhile.
	runner, err := newMatrixRemotePinRunner(name, rc)
	if err != nil {
		h.setError(err)
		return nil
	}
	h.matrixRemotePinPending[target] = pin
	h.markRemoteAction(name, "pin", id)
	return func() tea.Msg {
		msg := matrixRemotePinResultMsg{target: target, pin: pin, config: &rc}
		ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
		defer cancel()
		if _, err := runner.RunCommand(ctx, "session", "set", "--json", id, "pin", string(pin)); err != nil {
			msg.err = fmt.Errorf("remote pin failed on %s: %w", name, err)
			return msg
		}
		data, err := runner.RunCommand(ctx, "session", "show", "--json", id)
		var confirmed struct {
			ID  string           `json:"id"`
			Pin *session.PinMode `json:"pin"`
		}
		if err == nil {
			err = json.Unmarshal(data, &confirmed)
		}
		if err != nil || confirmed.ID != id || confirmed.Pin == nil || *confirmed.Pin != pin {
			msg.err = fmt.Errorf("remote pin may have been saved on %s; readback did not confirm it, refreshing", name)
		}
		return msg
	}
}

func (h *Home) applyMatrixRemotePin(msg matrixRemotePinResultMsg) tea.Cmd {
	if pin, pending := h.matrixRemotePinPending[msg.target]; !pending || pin != msg.pin {
		return nil
	}
	delete(h.matrixRemotePinPending, msg.target)
	h.remoteActionTook(msg.target.remoteName, "pin", msg.target.sessionID)
	if msg.config != nil && !h.remotePollConfigMatches(msg.target.remoteName, *msg.config) {
		return nil // matrix/hardened: a repointed remote cannot adopt the old host's answer.
	}
	if msg.err != nil {
		h.setError(msg.err)
	} else {
		// matrix/hardened: older polls and queued pre-write pushes must not
		// undo the confirmed pin before the ordinary fresh listing arrives.
		gen := atomic.AddUint64(&h.remoteFetchSeq, 1)
		h.remoteSessionsMu.Lock()
		if h.remoteFetchApplied == nil {
			h.remoteFetchApplied = make(map[string]uint64)
		}
		h.remoteFetchApplied[msg.target.remoteName] = gen
		h.remoteSessionsMu.Unlock()
		h.noteRemoteActionSettled(msg.target.remoteName)
		pin := msg.pin
		h.noteRemoteAction(msg.target.remoteName, msg.target.sessionID, remoteActionRecord{at: time.Now(), pin: &pin})
		selected := h.captureSelectedItemIdentity()
		h.patchRemoteSession(msg.target.remoteName, msg.target.sessionID, func(info *session.RemoteSessionInfo) {
			pin := msg.pin
			info.Pin = &pin
		})
		h.restoreSelectedItemIdentity(selected)
		h.syncViewport()
	}
	return h.fetchRemoteSessions
}

// matrix/hardened: pin bands override the local manual order, just as local
// pins do. Never mutate the indices or session slice supplied by a caller.
func matrixOrderRemotePins(sessions []session.RemoteSessionInfo, indices []int) []int {
	hasPin := false
	for _, i := range indices {
		hasPin = hasPin || matrixRemoteSessionIsPinned(sessions[i])
	}
	if !hasPin || len(indices) < 2 {
		return indices
	}
	zone := func(i int) int {
		switch sessions[i].PinPosition() {
		case session.PinTop:
			return 0
		case session.PinBottom:
			return 2
		default:
			return 1
		}
	}
	ordered := append([]int(nil), indices...)
	sort.SliceStable(ordered, func(i, j int) bool { return zone(ordered[i]) < zone(ordered[j]) })
	return ordered
}

func matrixPinnedRemoteRows(rows []session.Item) []session.Item {
	pinned := make([]session.Item, 0, len(rows))
	for _, item := range rows {
		if item.Type == session.ItemTypeRemoteSession && item.RemoteSession != nil {
			item.Level = 0
			item.IsSubSession = false
			item.ParentIsLastInGroup = false
			pinned = append(pinned, item)
		}
	}
	return pinned
}

func matrixRemotePinTitle(info session.RemoteSessionInfo, name string, pinnedOnly bool) string {
	title := info.Title
	if matrixRemoteSessionIsPinned(info) {
		if pinnedOnly {
			title = "[" + name + "] " + title
		}
		title = "📌 " + title
	}
	return title
}
