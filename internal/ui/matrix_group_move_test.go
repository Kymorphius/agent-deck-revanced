package ui

// matrix/hardened: M on a group row moves the whole group.

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// armGroupMoveHome builds a Home with groups a, a/b, a/b/c, x (and any extra
// root groups), one session in a/b/c, and the cursor on the given group row.
func armGroupMoveHome(t *testing.T, cursorPath string, extraRoots ...string) (*Home, *session.Instance) {
	t.Helper()
	setXDGTestHome(t)
	home := NewHome()
	home.width = 100
	home.height = 30
	home.initialLoading = false

	inst := &session.Instance{ID: "sess-1", Title: "s1", ProjectPath: "/tmp/p", GroupPath: "a/b/c"}
	tree := session.NewGroupTree([]*session.Instance{})
	tree.CreateGroup("a")
	tree.CreateSubgroup("a", "b")
	tree.CreateSubgroup("a/b", "c")
	tree.CreateGroup("x")
	for _, r := range extraRoots {
		tree.CreateGroup(r)
	}
	tree.AddSession(inst)

	home.instancesMu.Lock()
	home.instances = []*session.Instance{inst}
	home.instanceByID[inst.ID] = inst
	home.instancesMu.Unlock()
	home.groupTree = tree
	home.rebuildFlatItems()
	home.cursor = -1
	for i, it := range home.flatItems {
		if it.Type == session.ItemTypeGroup && it.Path == cursorPath {
			home.cursor = i
		}
	}
	if home.cursor < 0 {
		t.Fatalf("group %q not in flatItems", cursorPath)
	}
	return home, inst
}

func groupMovePress(t *testing.T, h *Home, msg tea.KeyMsg) *Home {
	t.Helper()
	model, _ := h.Update(msg)
	next, ok := model.(*Home)
	if !ok {
		t.Fatal("Update should return *Home")
	}
	return next
}

func selectMoveTarget(t *testing.T, h *Home, want string) {
	t.Helper()
	for i, p := range h.groupDialog.groupPaths {
		if p == want {
			h.groupDialog.selected = i
			return
		}
	}
	t.Fatalf("target %q not offered: %q", want, h.groupDialog.groupPaths)
}

func TestMatrixGroupMove_MOnGroupOffersParents(t *testing.T) {
	home, _ := armGroupMoveHome(t, "a/b")
	h := groupMovePress(t, home, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})

	if !h.groupDialog.IsVisible() || h.groupDialog.Mode() != GroupDialogMove {
		t.Fatal("M on a group should open the move dialog")
	}
	if got := h.groupDialog.MoveGroupSource(); got != "a/b" {
		t.Fatalf("MoveGroupSource = %q, want a/b", got)
	}
	// Root first; not itself, its descendant a/b/c, or its current parent a.
	if want := []string{"", "x"}; !reflect.DeepEqual(h.groupDialog.groupPaths, want) {
		t.Fatalf("targets = %q, want %q", h.groupDialog.groupPaths, want)
	}
}

func TestMatrixGroupMove_MoveUnderOtherGroup(t *testing.T) {
	home, inst := armGroupMoveHome(t, "a/b")
	h := groupMovePress(t, home, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	selectMoveTarget(t, h, "x")
	h = groupMovePress(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	if h.groupDialog.IsVisible() {
		t.Fatal("dialog should close after Enter")
	}
	for _, p := range []string{"x/b", "x/b/c"} {
		if _, ok := h.groupTree.Groups[p]; !ok {
			t.Fatalf("group %q missing after move; groups: %v", p, h.groupTree.GetGroupPaths())
		}
	}
	for _, p := range []string{"a/b", "a/b/c"} {
		if _, ok := h.groupTree.Groups[p]; ok {
			t.Fatalf("old group %q still present", p)
		}
	}
	if inst.GroupPath != "x/b/c" {
		t.Fatalf("session GroupPath = %q, want x/b/c", inst.GroupPath)
	}
	if it := h.flatItems[h.cursor]; it.Type != session.ItemTypeGroup || it.Path != "x/b" {
		t.Fatalf("cursor on %v %q, want group x/b", it.Type, it.Path)
	}
}

func TestMatrixGroupMove_MoveToRoot(t *testing.T) {
	home, inst := armGroupMoveHome(t, "a/b")
	h := groupMovePress(t, home, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	selectMoveTarget(t, h, "")
	h = groupMovePress(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	if _, ok := h.groupTree.Groups["b"]; !ok {
		t.Fatalf("group b missing at root; groups: %v", h.groupTree.GetGroupPaths())
	}
	if inst.GroupPath != "b/c" {
		t.Fatalf("session GroupPath = %q, want b/c", inst.GroupPath)
	}
}

func TestMatrixGroupMove_RootGroupNotOfferedRootAndCollisionsDropped(t *testing.T) {
	// a/b with a root group "b" already present: root would collide.
	home, _ := armGroupMoveHome(t, "a/b", "b")
	h := groupMovePress(t, home, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	for _, p := range h.groupDialog.groupPaths {
		if p == "" {
			t.Fatalf("root offered despite collision: %q", h.groupDialog.groupPaths)
		}
	}

	// A root-level group is not offered root (it is already there).
	home2, _ := armGroupMoveHome(t, "x")
	h2 := groupMovePress(t, home2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	if want := []string{"a", "a/b", "a/b/c"}; !reflect.DeepEqual(h2.groupDialog.groupPaths, want) {
		t.Fatalf("targets = %q, want %q", h2.groupDialog.groupPaths, want)
	}
}

func TestMatrixGroupMove_DefaultGroupRefused(t *testing.T) {
	home, _ := armGroupMoveHome(t, "x")
	home.groupTree.AddSession(&session.Instance{ID: "sess-def", Title: "d", ProjectPath: "/tmp/p", GroupPath: session.DefaultGroupPath})
	home.rebuildFlatItems()
	for i, it := range home.flatItems {
		if it.Type == session.ItemTypeGroup && it.Path == session.DefaultGroupPath {
			home.cursor = i
		}
	}
	h := groupMovePress(t, home, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	if h.groupDialog.IsVisible() {
		t.Fatal("move dialog opened on the default group")
	}
	if h.err == nil {
		t.Fatal("expected an error for moving the default group")
	}
}

func TestMatrixGroupMove_SessionMoveStillWorks(t *testing.T) {
	// Opening a group move, cancelling, then M on a session must be a plain
	// session move again (moveGroupSource reset).
	home, inst := armGroupMoveHome(t, "a/b")
	h := groupMovePress(t, home, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	h = groupMovePress(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	for i, it := range h.flatItems {
		if it.Type == session.ItemTypeSession && it.Session == inst {
			h.cursor = i
		}
	}
	h = groupMovePress(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	if h.groupDialog.MoveGroupSource() != "" {
		t.Fatal("session move dialog still carries the group source")
	}
	selectMoveTarget(t, h, "x")
	h = groupMovePress(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	if inst.GroupPath != "x" {
		t.Fatalf("session GroupPath = %q, want x", inst.GroupPath)
	}
	if _, ok := h.groupTree.Groups["a/b"]; !ok {
		t.Fatal("group a/b should not have moved")
	}
}

func TestMatrixGroupMove_ReapplyAfterReload(t *testing.T) {
	home, _ := armGroupMoveHome(t, "a/b")
	// The op a move records; a successful save clears it, a reload race keeps it.
	home.pendingGroupOps = []pendingGroupOp{{kind: groupOpMoveGroup, oldPath: "a/b", targetPath: "x"}}

	// Simulate a reload that discarded the move: a fresh tree in the old shape.
	inst := &session.Instance{ID: "sess-1", Title: "s1", ProjectPath: "/tmp/p", GroupPath: "a/b/c"}
	tree := session.NewGroupTree([]*session.Instance{})
	tree.CreateGroup("a")
	tree.CreateSubgroup("a", "b")
	tree.CreateSubgroup("a/b", "c")
	tree.CreateGroup("x")
	tree.AddSession(inst)
	home.groupTree = tree

	if !home.reapplyPendingGroupOps() {
		t.Fatal("reapply should report a change")
	}
	if inst.GroupPath != "x/b/c" {
		t.Fatalf("session GroupPath = %q after reapply, want x/b/c", inst.GroupPath)
	}
	if home.reapplyPendingGroupOps() {
		t.Fatal("second reapply should be a no-op")
	}
}
