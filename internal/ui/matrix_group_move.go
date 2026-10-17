package ui

// matrix/hardened: M on a group row moves the whole group (subgroups and
// sessions included) under another parent, or to the root level. The engine is
// GroupTree.MoveGroupTo, the same one `agent-deck group change` uses.

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// groupParentPath returns the parent path of a group path ("" for root level).
func groupParentPath(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[:idx]
	}
	return ""
}

// groupMoveTargets lists the parents a group can move under: "" (root) first,
// then every group in tree order except the group itself, its descendants and
// its current parent. Root is offered only without an active group scope (a
// root-level group would leave the scope). Destinations that already hold a
// group of the same name are dropped too: MoveGroupTo would refuse them.
func (h *Home) groupMoveTargets(sourcePath string) []string {
	base := sourcePath[strings.LastIndex(sourcePath, "/")+1:]
	current := groupParentPath(sourcePath)
	var targets []string
	if h.groupScope == "" && current != "" {
		if _, taken := h.groupTree.Groups[base]; !taken {
			targets = append(targets, "")
		}
	}
	for _, p := range h.scopedGroupPaths() {
		if p == sourcePath || strings.HasPrefix(p, sourcePath+"/") || p == current {
			continue
		}
		if _, taken := h.groupTree.Groups[p+"/"+base]; taken {
			continue
		}
		targets = append(targets, p)
	}
	return targets
}

// openGroupMoveDialog handles M on a group row.
func (h *Home) openGroupMoveDialog(item session.Item) {
	if item.Path == session.DefaultGroupPath {
		h.setError(fmt.Errorf("the default group cannot be moved"))
		return
	}
	targets := h.groupMoveTargets(item.Path)
	if len(targets) == 0 {
		h.setError(fmt.Errorf("no other place to move group %q", item.Path))
		return
	}
	h.groupDialog.ShowMoveGroup(item.Path, targets)
}

// moveGroupTo applies a confirmed group move, records it for reload-race
// reapply, persists, and keeps the cursor on the moved group.
func (h *Home) moveGroupTo(sourcePath, destParent string) {
	if err := h.groupTree.MoveGroupTo(sourcePath, destParent); err != nil {
		h.setError(err)
		return
	}
	newPath := sourcePath[strings.LastIndex(sourcePath, "/")+1:]
	if destParent != "" {
		newPath = destParent + "/" + newPath
	}
	h.pendingGroupOps = append(h.pendingGroupOps, pendingGroupOp{
		kind: groupOpMoveGroup, oldPath: sourcePath, targetPath: destParent,
	})
	h.instancesMu.Lock()
	h.instances = h.groupTree.GetAllInstances()
	h.instancesMu.Unlock()
	// The moved group may land inside a collapsed parent; open the way to it
	// so the cursor can follow.
	for p := groupParentPath(newPath); p != ""; p = groupParentPath(p) {
		h.groupTree.ExpandGroup(p)
	}
	h.rebuildFlatItems()
	h.moveCursorToGroup(newPath)
	h.saveInstances()
}

// reapplyPendingGroupMove re-runs a group move a reload race discarded. It
// reports whether the tree changed; it is a no-op when the source is gone (the
// move already landed, or the group was renamed/deleted elsewhere) or already
// sits under the destination.
func (h *Home) reapplyPendingGroupMove(op pendingGroupOp) bool {
	if _, ok := h.groupTree.Groups[op.oldPath]; !ok || groupParentPath(op.oldPath) == op.targetPath {
		return false
	}
	if err := h.groupTree.MoveGroupTo(op.oldPath, op.targetPath); err != nil {
		uiLog.Warn("pending_group_move_failed",
			slog.String("source", op.oldPath), slog.String("dest_parent", op.targetPath), slog.String("err", err.Error()))
		return false
	}
	h.instancesMu.Lock()
	h.instances = h.groupTree.GetAllInstances()
	h.instancesMu.Unlock()
	uiLog.Info("pending_group_reapplied", slog.String("kind", "move_group"), slog.String("source", op.oldPath), slog.String("dest_parent", op.targetPath))
	return true
}
