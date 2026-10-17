package ui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: use the real router but leave asynchronous commands inert.
func matrixNumericType(t *testing.T, h *Home, text string) *Home {
	t.Helper()
	for _, r := range text {
		model, _ := h.Update(plainKeyMsg(r))
		h = model.(*Home)
	}
	return h
}

// matrix/hardened: row 2 follows a divider and differs from the old selection.
func matrixNumericTargetHome(t *testing.T, row string) (*Home, *session.Instance) {
	t.Helper()
	h, source := matrixOpenSessionHome(t, row)
	target := h.flatItems[0]
	old := &session.Instance{ID: "previous", Title: "Previous", Tool: "claude", Status: session.StatusRunning}
	h.flatItems = []session.Item{{Type: session.ItemTypeSession, Session: old}, {Type: session.ItemTypeDivider}, target}
	return h, source
}

// matrix/hardened: numeric activation must select the displayed row before
// entering the same local, window or remote target as the ordinary open key.
func TestMatrixNumericActivateTargets(t *testing.T) {
	for _, row := range []string{"local", "window", "remote"} {
		for _, prefix := range []string{"", ":"} {
			for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, plainKeyMsg('o')} {
				t.Run(row+"/"+prefix+"2/"+key.String(), func(t *testing.T) {
					h, source := matrixNumericTargetHome(t, row)
					h = matrixNumericType(t, h, prefix+"2")
					_, cmd := h.Update(key)
					if cmd == nil || h.cursor != 2 || !h.embeddedMode || !h.insertMode || h.navCount.cmd || h.navCount.n != 0 {
						t.Fatalf("numeric open failed: cursor=%d embedded=%v insert=%v pending=%+v err=%v", h.cursor, h.embeddedMode, h.insertMode, h.navCount, h.err)
					}
					if row == "remote" {
						if h.insertModeRemoteName != "lab" || h.insertModeRemoteID != "remote-1" {
							t.Fatalf("wrong remote target: %q/%q", h.insertModeRemoteName, h.insertModeRemoteID)
						}
					} else if h.insertModeSessionID != source.ID {
						t.Fatalf("opened %q, want %q", h.insertModeSessionID, source.ID)
					}
					if row == "window" {
						target, ok := h.selectedInsertTarget()
						if !ok || !target.hasWindow || target.windowIndex != 2 {
							t.Fatalf("lost window target: %+v", target)
						}
					}
				})
			}
		}
	}
}

// matrix/hardened: numeric group activation toggles once, with row numbering
// recalculated after collapse so the same header can be reopened.
func TestMatrixNumericActivateGroup(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, plainKeyMsg('o')} {
			t.Run(prefix+"N/"+key.String(), func(t *testing.T) {
				withTempAgentDeckHome(t, "")
				h, idx := buildTwoGroupHome(t)
				h.cursor = idx["a1"]
				for _, expanded := range []bool{false, true} {
					nums, _ := h.matrixRowNumbers()
					for i, item := range h.flatItems {
						if item.Type == session.ItemTypeGroup && item.Path == "beta" {
							h = matrixNumericType(t, h, prefix+strconv.Itoa(nums[i]))
							break
						}
					}
					h.Update(key)
					item := h.flatItems[h.cursor]
					if h.groupTree.Groups["beta"].Expanded != expanded || item.Type != session.ItemTypeGroup || item.Path != "beta" || h.insertMode {
						t.Fatalf("group toggle: want expanded=%v, cursor=%s", expanded, describeCursor(h))
					}
				}
			})
		}
	}
}

// matrix/hardened: invalid numbers may report an error, but must never open
// the prior selection or mistake an unnumbered divider for line zero.
func TestMatrixNumericInvalidDoesNotActivate(t *testing.T) {
	for _, text := range []string{":0", ":-1", ":999", "999", ":" + strings.Repeat("9", 22), strings.Repeat("9", 22), ":" + strings.Repeat("1", 40), strings.Repeat("1", 40), ":" + strings.Repeat("0", matrixCmdMaxLen-1) + "19"} {
		for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, plainKeyMsg('o')} {
			t.Run(text+"/"+key.String(), func(t *testing.T) {
				h, _ := matrixOpenSessionHome(t, "local")
				h.flatItems = append(h.flatItems, session.Item{Type: session.ItemTypeDivider})
				h = matrixNumericType(t, h, text)
				h.Update(key)
				if h.cursor != 0 || h.embeddedMode || h.insertMode || h.navCount.cmd || h.err == nil {
					t.Fatalf("invalid %q activated/moved old row: cursor=%d embedded=%v pending=%+v err=%v", text, h.cursor, h.embeddedMode, h.navCount, h.err)
				}
			})
		}
	}
}

// matrix/hardened: numeric Enter confirms the address like a colon command,
// even when ordinary Enter has been assigned to another list action.
func TestMatrixNumericEnterConfirmsBeforeCustomMapping(t *testing.T) {
	for _, prefix := range []string{"", ":"} {
		h, source := matrixNumericTargetHome(t, "local")
		h.setHotkeys(resolveHotkeys(map[string]string{hotkeyPromptSession: "enter"}))
		h = matrixNumericType(t, h, prefix+"2")
		h.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if h.cursor != 2 || !h.embeddedMode || h.insertModeSessionID != source.ID || h.promptInputDialog.IsVisible() {
			t.Fatalf("%q Enter did not confirm its numeric target", prefix+"2")
		}
	}
}

// matrix/hardened: the movement count cap must not shorten a line target.
func TestMatrixNumericAboveCountLimit(t *testing.T) {
	h, source := matrixNumericTargetHome(t, "local")
	old, target := h.flatItems[0], h.flatItems[2]
	h.flatItems = make([]session.Item, 1000)
	for i := range h.flatItems {
		h.flatItems[i] = old
	}
	h.flatItems[999] = target
	h = matrixNumericType(t, h, "1000")
	h.Update(plainKeyMsg('o'))
	if h.cursor != 999 || !h.embeddedMode || h.insertModeSessionID != source.ID {
		t.Fatalf("1000o lost full target: cursor=%d session=%q err=%v", h.cursor, h.insertModeSessionID, h.err)
	}
}

// matrix/hardened: pending digits respect the configured open key and retain
// Enter when open_session is disabled; unbound o must not activate a row.
func TestMatrixNumericConfiguredOpenKey(t *testing.T) {
	for _, binding := range []string{"ctrl+o", ""} {
		for _, prefix := range []string{"", ":"} {
			t.Run(binding+"/"+prefix, func(t *testing.T) {
				h, source := matrixNumericTargetHome(t, "local")
				h.setHotkeys(resolveHotkeys(map[string]string{hotkeyOpenSession: binding}))
				h = matrixNumericType(t, h, prefix+"2")
				h.Update(plainKeyMsg('o'))
				if h.embeddedMode || h.insertMode {
					t.Fatal("unbound o activated numeric target")
				}
				h.Update(tea.KeyMsg{Type: tea.KeyEsc})
				h = matrixNumericType(t, h, prefix+"2")
				key := tea.KeyMsg{Type: tea.KeyEnter}
				if binding != "" {
					key.Type = tea.KeyCtrlO
				}
				h.Update(key)
				if h.cursor != 2 || !h.embeddedMode || h.insertModeSessionID != source.ID {
					t.Fatalf("binding %q did not activate target: cursor=%d err=%v", binding, h.cursor, h.err)
				}
			})
		}
	}
}

// matrix/hardened: o inside a text command or modal remains literal input.
func TestMatrixNumericPreservesTextInput(t *testing.T) {
	h, source := matrixOpenSessionHome(t, "local")
	h = matrixNumericType(t, h, ":set no")
	if !h.navCount.cmd || h.navCount.cmdBuf != "set no" || h.embeddedMode {
		t.Fatal("o prematurely submitted a textual colon command")
	}
	h = matrixNumericType(t, h, "nu")
	h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !h.navCount.noNumbers || h.navCount.cmd || h.embeddedMode {
		t.Fatal(":set nonu did not complete as a text command")
	}
	h.promptInputDialog.Show(source.ID, source.Title)
	h = matrixNumericType(t, h, "2o")
	if h.promptInputDialog.input.Value() != "2o" || h.embeddedMode || h.navCount.n != 0 {
		t.Fatal("modal input became a numeric list activation")
	}
}
