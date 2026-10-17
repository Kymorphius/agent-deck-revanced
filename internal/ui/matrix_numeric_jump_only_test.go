package ui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// matrix/hardened: l selects the displayed numeric row through the real router,
// including windows and remotes, without entering any session or restarting it.
func TestMatrixNumericJumpOnlyTargets(t *testing.T) {
	for _, row := range []string{"local", "window", "remote"} {
		for _, prefix := range []string{"", ":"} {
			t.Run(row+"/"+prefix+"2l", func(t *testing.T) {
				h, source := matrixNumericTargetHome(t, row)
				h.sessionExists = func(*session.Instance) bool { return false }
				h = matrixNumericType(t, h, prefix+"2l")
				if h.cursor != 2 || h.embeddedMode || h.insertMode || h.navCount.cmd || h.navCount.n != 0 || h.err != nil || !h.resumingSessions[source.ID].IsZero() {
					t.Fatalf("numeric l activated or lost target: cursor=%d embedded=%v insert=%v pending=%+v err=%v", h.cursor, h.embeddedMode, h.insertMode, h.navCount, h.err)
				}
				if !h.navCount.hasMark || h.navCount.markIdx != 0 {
					t.Fatal("cursor-only jump did not preserve the previous row for ''")
				}
			})
		}
	}
}

// matrix/hardened: addressing a group selects it without changing expansion;
// a later bare l must retain its existing group toggle behavior.
func TestMatrixNumericJumpOnlyGroupAndBareL(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		for _, expanded := range []bool{false, true} {
			t.Run(prefix+"/expanded="+strconv.FormatBool(expanded), func(t *testing.T) {
				withTempAgentDeckHome(t, "")
				h, idx := buildTwoGroupHome(t)
				h.groupTree.Groups["beta"].Expanded = expanded
				h.rebuildFlatItems()
				h.cursor = idx["a1"]
				nums, _ := h.matrixRowNumbers()
				for i, item := range h.flatItems {
					if item.Type == session.ItemTypeGroup && item.Path == "beta" {
						h = matrixNumericType(t, h, prefix+strconv.Itoa(nums[i])+"l")
						break
					}
				}
				item := h.flatItems[h.cursor]
				if item.Type != session.ItemTypeGroup || item.Path != "beta" || h.groupTree.Groups["beta"].Expanded != expanded || h.insertMode || h.navCount.cmd || h.navCount.n != 0 {
					t.Fatalf("numeric l toggled or missed group: want expanded=%v cursor=%s", expanded, describeCursor(h))
				}
				h.Update(plainKeyMsg('l'))
				if h.groupTree.Groups["beta"].Expanded == expanded {
					t.Fatal("bare l no longer toggles the selected group")
				}
			})
		}
	}
}

// matrix/hardened: invalid addresses never select a divider, activate the old
// selection, or accept a command whose visible numeric prefix was truncated.
func TestMatrixNumericJumpOnlyInvalid(t *testing.T) {
	for _, text := range []string{":0", ":-1", ":999", "999", ":" + strings.Repeat("9", 22), strings.Repeat("9", 22), ":" + strings.Repeat("1", 40), strings.Repeat("1", 40), ":" + strings.Repeat("0", matrixCmdMaxLen-1) + "19"} {
		t.Run(text+"l", func(t *testing.T) {
			h, _ := matrixOpenSessionHome(t, "local")
			h.flatItems = append(h.flatItems, session.Item{Type: session.ItemTypeDivider})
			h = matrixNumericType(t, h, text+"l")
			if h.cursor != 0 || h.embeddedMode || h.insertMode || h.navCount.cmd || h.navCount.n != 0 || h.err == nil {
				t.Fatalf("invalid l moved/activated old row: cursor=%d embedded=%v pending=%+v err=%v", h.cursor, h.embeddedMode, h.navCount, h.err)
			}
		})
	}
}

// matrix/hardened: an absolute line number uses every digit, independently of
// the legacy 999 cap on the relative j/k movement count.
func TestMatrixNumericJumpOnlyAboveCountLimit(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		t.Run(prefix+"1000l", func(t *testing.T) {
			h, _ := matrixNumericTargetHome(t, "local")
			old, target := h.flatItems[0], h.flatItems[2]
			h.flatItems = make([]session.Item, 1000)
			for i := range h.flatItems {
				h.flatItems[i] = old
			}
			h.flatItems[999] = target
			h = matrixNumericType(t, h, prefix+"1000l")
			if h.cursor != 999 || h.embeddedMode || h.insertMode || h.err != nil {
				t.Fatalf("1000l lost full target: cursor=%d err=%v", h.cursor, h.err)
			}
		})
	}
}

// matrix/hardened: Esc discards either numeric prefix, so its later l is a
// normal group action rather than a delayed jump to the former address.
func TestMatrixNumericJumpOnlyEscCancels(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		t.Run(prefix+"2Esc", func(t *testing.T) {
			withTempAgentDeckHome(t, "")
			h, _ := buildTwoGroupHome(t)
			h = matrixNumericType(t, h, prefix+"2")
			h.Update(tea.KeyMsg{Type: tea.KeyEsc})
			if h.cursor != 0 || h.matrixPendingNav() || h.navCount.cmd {
				t.Fatal("Esc failed to cancel the numeric prefix")
			}
			h.Update(plainKeyMsg('l'))
			if h.cursor != 0 || h.groupTree.Groups["alpha"].Expanded {
				t.Fatal("l after Esc did not use the original group selection")
			}
		})
	}
}

// matrix/hardened: l remains literal text in colon commands and modal inputs,
// while the adjacent j/k suffixes keep their relative movement semantics.
func TestMatrixNumericJumpOnlyPreservesTextAndCounts(t *testing.T) {
	h, source := matrixOpenSessionHome(t, "local")
	h = matrixNumericType(t, h, ":list")
	if !h.navCount.cmd || h.navCount.cmdBuf != "list" || h.embeddedMode {
		t.Fatal("l prematurely submitted a text command")
	}
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	h = matrixNumericType(t, h, ":set nonu")
	h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !h.navCount.noNumbers || h.navCount.cmd || h.err != nil {
		t.Fatal(":set nonu no longer works as a text command")
	}
	h.promptInputDialog.Show(source.ID, source.Title)
	h = matrixNumericType(t, h, "2l")
	if h.promptInputDialog.input.Value() != "2l" || h.embeddedMode || h.navCount.n != 0 {
		t.Fatal("modal 2l became a numeric jump")
	}
	for _, suffix := range []string{"3j", "3k"} {
		t.Run(suffix, func(t *testing.T) {
			h, idx := buildTwoGroupHome(t)
			from, want := idx["a1"], idx["a1"]+3
			if suffix == "3k" {
				from, want = idx["b2"], idx["b2"]-3
			}
			h.cursor = from
			h = matrixNumericType(t, h, suffix)
			if h.cursor != want || h.matrixPendingNav() || h.navCount.cmd {
				t.Fatalf("%s movement changed: cursor=%d want=%d", suffix, h.cursor, want)
			}
		})
	}
}

// matrix/hardened: both prefix forms advertise their cursor-only l suffix.
func TestMatrixNumericJumpOnlyHints(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		t.Run(prefix+"2", func(t *testing.T) {
			h, _ := matrixNumericTargetHome(t, "local")
			h = matrixNumericType(t, h, prefix+"2")
			bar := h.matrixRenderPendingBar()
			if prefix == ":" {
				bar = h.matrixRenderCmdBar()
			}
			if got := ansi.Strip(bar); !strings.Contains(got, "l goes to line") && !strings.Contains(got, "N+l goes to line") {
				t.Fatalf("numeric suffix hint missing: %s", got)
			}
		})
	}
}
