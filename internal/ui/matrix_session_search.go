package ui

// matrix/hardened: search scope controls candidates independently from the
// presentation that must reveal the selected session.
import (
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func (h *Home) setSessionSearchItems() {
	h.instancesMu.RLock()
	items := append([]*session.Instance(nil), h.instances...)
	h.instancesMu.RUnlock()
	h.search.SetItems(items)
}

func (h *Home) openSessionSearch() {
	h.search.SetScopedGroup("")
	h.search.SetSize(h.width, h.height)
	h.setSessionSearchItems()
	h.search.Show()
}

func sessionSearchGroupPath(inst *session.Instance) string {
	if inst.GroupPath == "" {
		return session.DefaultGroupPath
	}
	return inst.GroupPath
}

func (h *Home) openViewSearch() {
	// Capture the chosen filters, not the rows currently exposed by folds.
	status, window, mode, scope := h.statusFilter, h.timeFilter, h.groupViewMode, h.groupScope
	labels := []string{h.groupViewMode.Label(), h.statusFilterLabel()}
	if window != session.TimeFilterAll {
		labels = append(labels, window.Label())
	}
	if scope != "" {
		labels = append(labels, scope)
	}
	h.search.SetViewScope(strings.Join(labels, " · "), func(inst *session.Instance) bool {
		if inst == nil || !pathInScope(sessionSearchGroupPath(inst), scope) || inst.IsArchived() != (status == FilterModeArchived) {
			return false
		}
		if status != "" && status != FilterModeArchived && !h.matchesSessionFilter(status, inst) {
			return false
		}
		if mode == session.GroupViewPinnedOnly && !matrixSessionIsPinned(inst) {
			return false
		}
		activity := inst.DisplayLastActivityTime()
		if mode == session.GroupViewRecent {
			activity = inst.LastAccessedAt
			if activity.IsZero() {
				return false
			}
		}
		return window.Matches(activity, time.Now())
	})
	h.search.SetSize(h.width, h.height)
	h.setSessionSearchItems()
	h.search.Show()
}

func (h *Home) locateSearchSession(selected *session.Instance, preserveView bool) bool {
	if selected == nil || selected.ID == "" || h.groupTree == nil {
		return false
	}
	// A reload can replace or remove the selected object while search is open.
	var target *session.Instance
	h.instancesMu.RLock()
	for _, inst := range h.instances {
		if inst != nil && inst.ID == selected.ID {
			target = inst
			break
		}
	}
	h.instancesMu.RUnlock()
	if target == nil {
		return false
	}
	if preserveView {
		if h.search.viewFilter == nil || !h.search.viewFilter(target) {
			return false
		}
	} else {
		if h.search.IsVisible() && h.search.scopedGroup != "" && sessionSearchGroupPath(target) != h.search.scopedGroup {
			return false
		}
		h.statusFilter = ""
		h.keepEmptyFilter = false
		if target.IsArchived() {
			h.statusFilter = FilterModeArchived
		}
		h.timeFilter = session.TimeFilterAll
		h.groupViewMode = session.GroupViewNormal
		h.groupScopeMu.Lock()
		h.groupScope = ""
		h.groupScopeMu.Unlock()
	}
	h.groupTree.ExpandGroupWithParents(sessionSearchGroupPath(target))
	h.rebuildFlatItems()
	if index := h.flatItemIndexByID(target.ID); index >= 0 {
		h.jumpToIndex(index)
		h.saveGroupState()
		h.saveUIState()
		return true
	}
	return false
}
