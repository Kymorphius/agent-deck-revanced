package ui

import (
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: both fixed pin positions belong to the user's pinned list.
func matrixSessionIsPinned(inst *session.Instance) bool {
	return inst != nil && (inst.Pin == session.PinTop || inst.Pin == session.PinBottom)
}

// matrix/hardened: use full membership rather than visible tree rows. A pinned
// child stands on its own when its parent is absent, without an orphan connector.
func matrixPinnedItemsFromTree(tree *session.GroupTree) []session.Item {
	if tree == nil {
		return nil
	}
	// matrix/hardened: allocate rows only for pins, rather than two full-size
	// arrays for all sessions. Keep the tree's group/session order unchanged.
	var pinned []session.Item
	for _, group := range tree.GroupList {
		if group == nil {
			continue
		}
		for _, inst := range group.Sessions {
			if matrixSessionIsPinned(inst) {
				pinned = append(pinned, session.Item{
					Type:    session.ItemTypeSession,
					Session: inst,
					Path:    group.Path,
				})
			}
		}
	}
	return pinned
}

// matrix/hardened: an empty pinned view stays empty and explains how to leave
// it; composing status/time/archive filters must not look like a new workspace.
func (h *Home) matrixPinnedEmptyState() EmptyStateConfig {
	parts := []string{"Pin a session with ,"}
	if h.statusFilter != "" || h.timeFilter != session.TimeFilterAll || h.groupScope != "" {
		parts = []string{"Pinned sessions matching"}
		if h.statusFilter != "" {
			parts = append(parts, h.statusFilterLabel())
		}
		if h.timeFilter != session.TimeFilterAll {
			parts = append(parts, h.timeFilter.Label())
		}
		if h.groupScope != "" {
			parts = append(parts, h.groupScope)
		}
	}
	key := h.actionKey(hotkeyTogglePinnedView)
	if key == "" {
		key = h.actionKey(hotkeyCycleGroupView)
	}
	hints := []string{}
	if key != "" {
		hints = append(hints, key+" back to normal view")
	}
	if h.statusFilter != "" {
		hints = append(hints, "0 clear status filter")
	}
	return EmptyStateConfig{
		Icon:                "📌",
		Title:               "No pinned sessions",
		Subtitle:            strings.Join(parts, " · "),
		ShowSubtitleMinimal: true,
		ShowAllHintsMinimal: true,
		Hints:               hints,
	}
}
