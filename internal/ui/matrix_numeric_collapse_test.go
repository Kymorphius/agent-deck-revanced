package ui

// matrix/hardened: Nh/:Nh address a visible row before applying existing h/left
// navigation, so an invalid address can never fold the old selection.

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func matrixNumericCollapseAddress(t *testing.T, h *Home, prefix string, matches func(session.Item) bool) *Home {
	t.Helper()
	nums, _ := h.matrixRowNumbers()
	for i, item := range h.flatItems {
		if matches(item) {
			return matrixNumericType(t, h, prefix+strconv.Itoa(nums[i])+"h")
		}
	}
	t.Fatal("numeric collapse target is missing")
	return h
}

func TestMatrixNumericCollapseLocalGroupAndSession(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		for _, target := range []string{"group", "session"} {
			t.Run(prefix+target, func(t *testing.T) {
				withTempAgentDeckHome(t, "")
				h, idx := buildTwoGroupHome(t)
				t.Cleanup(h.cancel)
				h.cursor = idx["a1"]
				// The address must ignore divider rows, even before a rebuild.
				h.flatItems = append([]session.Item{{Type: session.ItemTypeDivider}}, h.flatItems...)
				h.cursor++
				h = matrixNumericCollapseAddress(t, h, prefix, func(item session.Item) bool {
					if target == "group" {
						return item.Type == session.ItemTypeGroup && item.Path == "beta"
					}
					return item.Type == session.ItemTypeSession && item.Session.Title == "b2"
				})
				item := h.flatItems[h.cursor]
				if h.groupTree.Groups["beta"].Expanded || !h.groupTree.Groups["alpha"].Expanded || item.Type != session.ItemTypeGroup || item.Path != "beta" || h.insertMode || h.embeddedMode || h.matrixPendingNav() || h.navCount.cmd || h.err != nil {
					t.Fatalf("wrong collapse: cursor=%s pending=%+v err=%v", describeCursor(h), h.navCount, h.err)
				}
				// Addressing the same closed header never toggles it open.
				h = matrixNumericCollapseAddress(t, h, prefix, func(item session.Item) bool {
					return item.Type == session.ItemTypeGroup && item.Path == "beta"
				})
				if h.groupTree.Groups["beta"].Expanded {
					t.Fatal("numeric h reopened a closed group")
				}
			})
		}
	}
}

func TestMatrixNumericCollapseRemoteGroupAndSession(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		for _, target := range []session.ItemType{session.ItemTypeRemoteGroup, session.ItemTypeRemoteSession} {
			t.Run(prefix+strconv.Itoa(int(target)), func(t *testing.T) {
				h := matrixPinnedHome(t)
				h.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {{ID: "numeric-remote", Group: "work/deep", Status: "idle"}}}
				h.rebuildFlatItems()
				h.cursor = sessionIndexByTitle(h, "a1")
				const path = "remotes/dev/work/deep"
				h = matrixNumericCollapseAddress(t, h, prefix, func(item session.Item) bool {
					return item.Type == target && item.Path == path
				})
				item := h.flatItems[h.cursor]
				if !h.remoteGroupsCollapsed[path] || item.Type != session.ItemTypeRemoteGroup || item.Path != path || !h.groupTree.Groups["alpha"].Expanded || h.insertMode || h.embeddedMode || h.err != nil {
					t.Fatalf("remote collapse missed owner: cursor=%s folds=%v err=%v", describeCursor(h), h.remoteGroupsCollapsed, h.err)
				}
			})
		}
	}
}

func TestMatrixNumericCollapseWindow(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		t.Run(prefix+"2h", func(t *testing.T) {
			h, source := matrixNumericTargetHome(t, "window")
			h = matrixNumericType(t, h, prefix+"2h")
			if !h.windowsCollapsed[source.ID] || h.flatItems[h.cursor].Session != source || h.insertMode || h.embeddedMode || h.err != nil {
				t.Fatalf("window h lost session target: cursor=%s err=%v", describeCursor(h), h.err)
			}
		})
	}
}

func TestMatrixNumericCollapseInvalid(t *testing.T) {
	for _, text := range []string{":0", ":-1", ":999", "999", ":" + strings.Repeat("9", 22), strings.Repeat("9", 22), ":" + strings.Repeat("1", 40), strings.Repeat("1", 40), ":" + strings.Repeat("0", matrixCmdMaxLen-1) + "19"} {
		t.Run(text+"h", func(t *testing.T) {
			withTempAgentDeckHome(t, "")
			h, _ := buildTwoGroupHome(t)
			t.Cleanup(h.cancel)
			h = matrixNumericType(t, h, text+"h")
			if h.cursor != 0 || !h.groupTree.Groups["alpha"].Expanded || !h.groupTree.Groups["beta"].Expanded || h.matrixPendingNav() || h.navCount.cmd || h.err == nil || h.insertMode || h.embeddedMode {
				t.Fatalf("invalid address folded/moved old row: cursor=%s pending=%+v err=%v", describeCursor(h), h.navCount, h.err)
			}
		})
	}
}

func TestMatrixNumericCollapseAboveCountLimit(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		t.Run(prefix+"1000h", func(t *testing.T) {
			withTempAgentDeckHome(t, "")
			h, idx := buildTwoGroupHome(t)
			t.Cleanup(h.cancel)
			old, target := h.flatItems[0], h.flatItems[idx["b1"]-1]
			h.flatItems = make([]session.Item, 1000)
			for i := range h.flatItems {
				h.flatItems[i] = old
			}
			h.flatItems[999] = target
			h = matrixNumericType(t, h, prefix+"1000h")
			if h.groupTree.Groups["beta"].Expanded || !h.groupTree.Groups["alpha"].Expanded || h.err != nil {
				t.Fatalf("1000h used capped movement count: cursor=%s err=%v", describeCursor(h), h.err)
			}
		})
	}
}

func TestMatrixNumericCollapseKeepsParentInViewport(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		t.Run(prefix+"Nh", func(t *testing.T) {
			withTempAgentDeckHome(t, "")
			h, _ := buildTwoGroupHome(t)
			t.Cleanup(h.cancel)
			for i := range 15 {
				inst := session.NewInstanceWithTool("tail-"+strconv.Itoa(i), "/tmp/numeric-tail", "claude")
				inst.GroupPath = "gamma"
				h.instances = append(h.instances, inst)
			}
			h.groupTree = session.NewGroupTree(h.instances)
			h.rebuildFlatItems()
			h.height = 10
			h = matrixNumericCollapseAddress(t, h, prefix, func(item session.Item) bool {
				return item.Type == session.ItemTypeSession && item.Session.Title == "b2"
			})
			if item := h.flatItems[h.cursor]; item.Type != session.ItemTypeGroup || item.Path != "beta" || h.viewOffset > h.cursor {
				t.Fatalf("collapsed parent is outside viewport: cursor=%s offset=%d", describeCursor(h), h.viewOffset)
			}
		})
	}
}

func TestMatrixNumericCollapsePinnedStandalonePreservesFolds(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		for _, remote := range []bool{false, true} {
			t.Run(prefix+strconv.FormatBool(remote), func(t *testing.T) {
				h := matrixPinnedHome(t)
				matrixPinnedInstance(t, h, "a1").Pin = session.PinTop
				h.remoteSessions = map[string][]session.RemoteSessionInfo{"dev": {{ID: "pin", Group: "work", Pin: matrixRemotePinPointer(session.PinTop)}}}
				h.remoteGroupsCollapsed = map[string]bool{"remotes/dev": true}
				h.groupViewMode = session.GroupViewPinnedOnly
				h.rebuildFlatItems()
				h = matrixNumericCollapseAddress(t, h, prefix, func(item session.Item) bool {
					return remote && item.Type == session.ItemTypeRemoteSession || !remote && item.Type == session.ItemTypeSession
				})
				if !h.groupTree.Groups["alpha"].Expanded || !h.groupTree.Groups["beta"].Expanded || !reflect.DeepEqual(h.remoteGroupsCollapsed, map[string]bool{"remotes/dev": true}) || len(h.flatItems) != 2 || h.insertMode || h.embeddedMode || h.err != nil {
					t.Fatalf("standalone numeric h changed hidden folds: cursor=%s folds=%v err=%v", describeCursor(h), h.remoteGroupsCollapsed, h.err)
				}
			})
		}
	}
}

func TestMatrixNumericCollapsePreservesTextInput(t *testing.T) {
	h, source := matrixOpenSessionHome(t, "local")
	h = matrixNumericType(t, h, ":help")
	if !h.navCount.cmd || h.navCount.cmdBuf != "help" {
		t.Fatal("h submitted a textual command")
	}
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	h.promptInputDialog.Show(source.ID, source.Title)
	h = matrixNumericType(t, h, "2h")
	if h.promptInputDialog.input.Value() != "2h" || h.matrixPendingNav() || h.embeddedMode {
		t.Fatal("modal numeric input became collapse navigation")
	}
	for _, embedded := range []bool{false, true} {
		t.Run("embedded="+strconv.FormatBool(embedded), func(t *testing.T) {
			h, source := matrixOpenSessionHome(t, "local")
			var forwarded strings.Builder
			h.insertKeySink = func(_ *session.Instance, text string, _ bool) error {
				forwarded.WriteString(text)
				return nil
			}
			h.insertBatchDuration = -1
			h.insertMode, h.embeddedMode, h.insertModeSessionID = true, embedded, source.ID
			h = matrixNumericType(t, h, ":2h")
			if forwarded.String() != ":2h" || !h.insertMode || h.embeddedMode != embedded || h.navCount.cmd || h.matrixPendingNav() {
				t.Fatalf("session input became list navigation: forwarded=%q nav=%+v", forwarded.String(), h.navCount)
			}
		})
	}
}

func TestMatrixNumericCollapseHints(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		h, _ := matrixNumericTargetHome(t, "local")
		h = matrixNumericType(t, h, prefix+"2")
		bar := h.matrixRenderPendingBar()
		if prefix == ":" {
			bar = h.matrixRenderCmdBar()
		}
		if !strings.Contains(ansi.Strip(bar), "h collapses") {
			t.Fatalf("numeric collapse hint missing: %s", ansi.Strip(bar))
		}
	}
}
