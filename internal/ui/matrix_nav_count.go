package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// matrix/hardened: vim-style count prefix for the session list ("3j", "12k").
//
// matrix/hardened: digits followed by l only select the numbered row;
// h applies collapse/parent navigation there, and Enter/o activate it. With j/k
// they remain a movement count, as in vim: "3j" moves three rows down, "12k"
// twelve up (also with up/down). Ctrl+N/P are not navigation. "0" extends a pending count
// ("10j"); with no count pending it still clears the status filter. The old
// plain-digit "jump to root group N" moved to "'" followed by the digit
// ("'3"). Any other key drops a pending count or "'". ":" opens the line
// prompt in matrix_line_numbers.go. "''" jumps back to the row the cursor was
// on before the last "'N" or ":N" jump, and again to undo that, like vim.

const matrixNavCountMax = 999

// matrixGroupJumpKey is the prefix for jumping to root group N ("'3").
const matrixGroupJumpKey = "'"

// matrixCmdKey opens the ":" prompt (":12" jumps to line 12).
const matrixCmdKey = ":"

type matrixNavCount struct {
	n           int
	digits      string // matrix/hardened: retain the full row number separately from the capped movement count.
	groupJump   bool   // "'" pressed; the next digit picks the root group
	cmd         bool   // ":" prompt open (matrix_line_numbers.go)
	cmdBuf      string // text typed after ":"
	cmdOverflow bool   // matrix/hardened: never activate a truncated numeric command.
	noNumbers   bool   // ":set nonu"; survives resets

	// Jump mark for "''", survives resets: the row before the last jump.
	hasMark bool
	markID  string // clickedItemID of that row ("" for window/remote rows)
	markIdx int    // its flatItems index, used when markID is "" or gone
}

// matrixResetNav drops any pending count, "'" or ":" prompt.
func (h *Home) matrixResetNav() {
	c := h.navCount
	h.navCount = matrixNavCount{noNumbers: c.noNumbers, hasMark: c.hasMark, markID: c.markID, markIdx: c.markIdx}
}

// matrixJump runs a cursor jump and, when the cursor moved, remembers the row
// it left so "”" can return to it.
func (h *Home) matrixJump(jump func()) {
	fromIdx, fromID := h.cursor, h.clickedItemID(h.cursor)
	jump()
	if h.cursor != fromIdx {
		h.navCount.hasMark, h.navCount.markID, h.navCount.markIdx = true, fromID, fromIdx
	}
}

// matrixJumpBack handles "”".
func (h *Home) matrixJumpBack() {
	if !h.navCount.hasMark || len(h.flatItems) == 0 {
		return
	}
	target := min(max(h.navCount.markIdx, 0), len(h.flatItems)-1)
	if id := h.navCount.markID; id != "" {
		for i := range h.flatItems {
			if h.clickedItemID(i) == id {
				target = i
				break
			}
		}
	}
	h.matrixJump(func() { h.jumpToIndex(target); h.skipDivider(1) })
}

// matrixNavCountKey reports whether key reads or extends a pending count.
func matrixNavCountKey(key string) bool {
	switch key {
	case "up", "k", "down", "j",
		"0", "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return true
	}
	return false
}

// matrixHandleNavPrefix handles "'", the digit after it, and count digits.
// It returns true when the key was consumed.
func (h *Home) matrixHandleNavPrefix(key string) bool {
	if h.navCount.groupJump {
		h.matrixResetNav()
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			h.matrixJump(func() { h.jumpToRootGroup(int(key[0] - '0')) })
			return true
		}
		if key == matrixGroupJumpKey {
			h.matrixJumpBack()
			return true
		}
		// Not a group digit: the "'" is dropped and the key acts as usual.
		return false
	}
	if key == matrixGroupJumpKey {
		h.matrixResetNav()
		h.navCount.groupJump = true
		return true
	}
	if key == matrixCmdKey {
		h.matrixResetNav()
		h.navCount.cmd = true
		return true
	}
	if !matrixNavCountKey(key) {
		h.matrixResetNav()
		return false
	}
	if len(key) != 1 || key[0] < '0' || key[0] > '9' {
		return false // j/k: matrixTakeNavCount reads the count
	}
	if key == "0" && h.navCount.n == 0 {
		return false // plain "0" keeps clearing the status filter
	}
	// matrix/hardened: a row can exceed the legacy 999 movement-count limit.
	// A positive 32-digit prefix cannot fit an int, so truncation remains invalid.
	if len(h.navCount.digits) < matrixCmdMaxLen {
		h.navCount.digits += key
	}
	if n := h.navCount.n*10 + int(key[0]-'0'); n <= matrixNavCountMax {
		h.navCount.n = n
	}
	return true
}

// matrixTakeNavCount returns the step count for a j/k press (1 without a
// count) and clears the pending count.
func (h *Home) matrixTakeNavCount() int {
	n := h.navCount.n
	h.matrixResetNav()
	if n < 1 {
		return 1
	}
	return n
}

// matrixIMEKeys maps what a Chinese input method in Chinese mode types for
// list shortcuts back to those keys, for the session list only. Rime's
// default punctuation turns "'" into 「/」 and ":" into "："; other IMEs type
// curly quotes. Without this "'2" and ":12" silently did nothing there.
var matrixIMEKeys = map[string]string{
	"「": "'", "」": "'", "‘": "'", "’": "'",
	"：": ":",
	// matrix/hardened: Shift+1 can arrive as ！ in Chinese punctuation mode;
	// recognize the other shifted filter/dashboard keys in the same way.
	"！": "!", "＠": "@", "＃": "#", "＄": "$", "￥": "$",
	"％": "%", "＾": "^", "＆": "&", "＊": "*", "～": "~",
}

// matrixIMEKey returns the list shortcut an IME character stands for, or
// pressed unchanged.
func matrixIMEKey(pressed string) string {
	if k, ok := matrixIMEKeys[pressed]; ok {
		return k
	}
	return pressed
}

// matrixPendingNav reports whether "'" or a count is waiting for its next
// key, so the help bar can say so (a bare "'" used to look like no response).
func (h *Home) matrixPendingNav() bool {
	return h.navCount.groupJump || h.navCount.n > 0
}

// matrixRenderPendingBar replaces the help bar while "'" or a count waits.
func (h *Home) matrixRenderPendingBar() string {
	border := lipgloss.NewStyle().Foreground(ColorBorder).Render(strings.Repeat("─", max(0, h.width)))
	keys, hint := "'", "   press 1-9 to jump to that root group ('' jumps back) · any other key cancels"
	if !h.navCount.groupJump {
		keys = h.navCount.digits
		if keys == "" {
			keys = strconv.Itoa(h.navCount.n)
		}
		// matrix/hardened: distinguish selecting a line from opening its target.
		hint = fmt.Sprintf("   l goes to line %s · h collapses · %s opens · j/k (or ↓/↑) moves %d rows · Esc cancels", keys, h.matrixNumericConfirmKeys(), h.navCount.n)
	}
	line := lipgloss.NewStyle().Foreground(ColorYellow).Bold(true).Render(keys) +
		lipgloss.NewStyle().Foreground(ColorComment).Render(hint)
	return lipgloss.JoinVertical(lipgloss.Left, border, lipgloss.NewStyle().MaxWidth(h.width).Render(line))
}
