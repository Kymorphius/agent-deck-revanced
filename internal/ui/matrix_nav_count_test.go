package ui

// matrix/hardened: tests for the vim-style count prefix ("3j", "3k") and the
// "'N" root-group jump that replaced plain digits.

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func matrixPressKeys(t *testing.T, h *Home, keys ...rune) *Home {
	t.Helper()
	for _, r := range keys {
		model, _ := h.handleMainKey(plainKeyMsg(r))
		h = model.(*Home)
	}
	return h
}

func TestMatrixNavCount_CountJ(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '3', 'j')

	if want := idx["a1"] + 3; h.cursor != want {
		t.Fatalf("3j from a1: cursor = %d (%s), want %d", h.cursor, describeCursor(h), want)
	}
}

func TestMatrixNavCount_CountK(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["b2"]

	h := matrixPressKeys(t, home, '2', 'k')

	if want := idx["b2"] - 2; h.cursor != want {
		t.Fatalf("2k from b2: cursor = %d (%s), want %d", h.cursor, describeCursor(h), want)
	}
}

func TestMatrixNavCount_DigitAloneDoesNotMove(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '2')

	if h.cursor != idx["a1"] {
		t.Fatalf("2 alone: cursor = %d (%s), want a1 unchanged", h.cursor, describeCursor(h))
	}
}

func TestMatrixNavCount_ArrowKeysTakeCount(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '2')
	model, _ := h.handleMainKey(tea.KeyMsg{Type: tea.KeyDown})
	h = model.(*Home)

	if want := idx["a1"] + 2; h.cursor != want {
		t.Fatalf("2<Down> from a1: cursor = %d (%s), want %d", h.cursor, describeCursor(h), want)
	}
}

func TestMatrixNavCount_MultiDigitClampsToEnd(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '1', '0', 'j')

	if want := len(h.flatItems) - 1; h.cursor != want {
		t.Fatalf("10j from a1: cursor = %d (%s), want last row %d", h.cursor, describeCursor(h), want)
	}
}

func TestMatrixNavCount_CountIsConsumed(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '2', 'j', 'j')

	if want := idx["a1"] + 3; h.cursor != want {
		t.Fatalf("2jj from a1: cursor = %d (%s), want %d", h.cursor, describeCursor(h), want)
	}
}

func TestMatrixNavCount_QuoteDigitJumpsToGroup(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '\'', '2')

	it := h.flatItems[h.cursor]
	if it.Type != session.ItemTypeGroup || it.RootGroupNum != 2 {
		t.Fatalf("'2: cursor on %s, want root group 2", describeCursor(h))
	}
	// The digit went to the jump, not into a count.
	h = matrixPressKeys(t, h, 'j')
	if it := h.flatItems[h.cursor]; it.Type != session.ItemTypeSession || it.Session.Title != "b1" {
		t.Fatalf("'2 then j: cursor on %s, want b1", describeCursor(h))
	}
}

func TestMatrixNavCount_QuoteThenOtherKeyActsAsUsual(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '\'', 'j')

	if want := idx["a1"] + 1; h.cursor != want {
		t.Fatalf("' then j from a1: cursor = %d (%s), want %d", h.cursor, describeCursor(h), want)
	}
}

// ---------- ":N" line jump and line numbers (matrix_line_numbers.go) ----------

func matrixTypeCmd(t *testing.T, h *Home, text string) *Home {
	t.Helper()
	h = matrixPressKeys(t, h, ':')
	if !h.navCount.cmd {
		t.Fatal(": did not open the line prompt")
	}
	for _, r := range text {
		model, _ := h.Update(plainKeyMsg(r))
		h = model.(*Home)
	}
	model, _ := h.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return model.(*Home)
}

func TestMatrixLineJump_ColonNJumpsToLine(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]
	nums, _ := home.matrixRowNumbers()
	line := nums[idx["b2"]]

	h := matrixTypeCmd(t, home, strconv.Itoa(line))

	if h.cursor != idx["b2"] || h.navCount.cmd {
		t.Fatalf(":%d: cursor = %d (%s), prompt open=%v; want b2 at %d, prompt closed",
			line, h.cursor, describeCursor(h), h.navCount.cmd, idx["b2"])
	}
}

func TestMatrixLineJump_GroupHeaderHasALine(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]
	nums, _ := home.matrixRowNumbers()
	group := idx["b1"] - 1 // beta header sits right above b1

	h := matrixTypeCmd(t, home, strconv.Itoa(nums[group]))

	if it := h.flatItems[h.cursor]; it.Type != session.ItemTypeGroup || it.Path != "beta" {
		t.Fatalf(":%d: cursor on %s, want beta group header", nums[group], describeCursor(h))
	}
}

func TestMatrixLineJump_OutOfRangeKeepsCursor(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a2"]

	h := matrixTypeCmd(t, home, "999")

	if h.cursor != idx["a2"] || h.err == nil {
		t.Fatalf(":999: cursor = %d (%s), err = %v; want a2 unchanged and an error", h.cursor, describeCursor(h), h.err)
	}
}

func TestMatrixLineJump_EscCancels(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, ':')
	model, _ := h.Update(plainKeyMsg('5'))
	model, _ = model.(*Home).Update(tea.KeyMsg{Type: tea.KeyEsc})
	h = model.(*Home)

	if h.navCount.cmd || h.cursor != idx["a1"] {
		t.Fatalf(":5<Esc>: prompt open=%v cursor=%d (%s); want closed, a1", h.navCount.cmd, h.cursor, describeCursor(h))
	}
}

func TestMatrixLineNumbers_RenderedAndToggle(t *testing.T) {
	home, _ := buildTwoGroupHome(t)

	list := ansi.Strip(home.renderSessionList(80, 20))
	first := strings.Split(list, "\n")[0]
	if !strings.HasPrefix(first, "1 ") {
		t.Fatalf("first row %q: want line number 1 in front", first)
	}
	for i, line := range strings.Split(list, "\n") {
		if w := cellWidth(line); w > 80 {
			t.Fatalf("row %d is %d cells wide, over the 80-cell list: %q", i, w, line)
		}
	}

	h := matrixTypeCmd(t, home, "set nonu")
	list = ansi.Strip(h.renderSessionList(80, 20))
	if strings.HasPrefix(strings.Split(list, "\n")[0], "1 ") {
		t.Fatalf(":set nonu: numbers still drawn: %q", strings.Split(list, "\n")[0])
	}
	h = matrixPressKeys(t, h, '3', 'j') // a count reset must not bring numbers back
	if !h.navCount.noNumbers {
		t.Fatal(":set nonu was lost after 3j")
	}
}

// ---------- "''" jump back ----------

func TestMatrixJumpBack_AfterGroupJumpToggles(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a2"]

	h := matrixPressKeys(t, home, '\'', '2')
	group := h.cursor
	h = matrixPressKeys(t, h, '\'', '\'')
	if h.cursor != idx["a2"] {
		t.Fatalf("'2 then '': cursor = %d (%s), want back on a2", h.cursor, describeCursor(h))
	}
	h = matrixPressKeys(t, h, '\'', '\'')
	if h.cursor != group {
		t.Fatalf("second '': cursor = %d (%s), want group 2 at %d again", h.cursor, describeCursor(h), group)
	}
}

func TestMatrixJumpBack_AfterLineJump(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]
	nums, _ := home.matrixRowNumbers()

	h := matrixTypeCmd(t, home, strconv.Itoa(nums[idx["b2"]]))
	h = matrixPressKeys(t, h, 'k') // plain moves don't set the mark
	h = matrixPressKeys(t, h, '\'', '\'')

	if h.cursor != idx["a1"] {
		t.Fatalf(":N, k, '': cursor = %d (%s), want a1", h.cursor, describeCursor(h))
	}
}

func TestMatrixJumpBack_FollowsTheRowNotTheIndex(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["b1"]

	h := matrixPressKeys(t, home, '\'', '1')
	// Collapse alpha: b1 moves up in flatItems, the mark must follow it.
	h.groupTree.ToggleGroup("alpha")
	h.rebuildFlatItems()
	h = matrixPressKeys(t, h, '\'', '\'')

	if it := h.flatItems[h.cursor]; it.Type != session.ItemTypeSession || it.Session.Title != "b1" {
		t.Fatalf("'' after collapsing alpha: cursor on %s, want b1", describeCursor(h))
	}
}

func TestMatrixJumpBack_NoMarkIsANoop(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a2"]

	h := matrixPressKeys(t, home, '\'', '9') // no group 9: no move, no mark
	h = matrixPressKeys(t, h, '\'', '\'')

	if h.cursor != idx["a2"] {
		t.Fatalf("'' with no mark: cursor = %d (%s), want a2", h.cursor, describeCursor(h))
	}
}

// ---------- group rows show the real jump key ("'N") ----------

func TestMatrixGroupHint_RowShowsQuoteKey(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a2"]

	lines := strings.Split(ansi.Strip(home.renderSessionList(60, 12)), "\n")
	var beta string
	for _, l := range lines {
		if strings.Contains(l, "beta (2)") {
			beta = l
		}
	}
	if !strings.HasSuffix(strings.TrimRight(beta, " "), "beta (2) '2") {
		t.Fatalf("beta row %q: want the jump key \"'2\" after the count", beta)
	}
	if strings.Contains(beta, "2·") {
		t.Fatalf("beta row %q still shows the old \"2·\" (plain 2 is a count now)", beta)
	}
	// Line number, blank gutter, then the arrow: nothing runs together.
	if f := strings.Fields(beta); len(f) < 2 || f[1] != "▾" {
		t.Fatalf("beta row %q: want \"<line> ▾ beta ...\"", beta)
	}
}

func TestMatrixGroupHint_ShownKeyJumpsThroughUpdate(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["b2"]

	for _, tc := range []struct {
		digit rune
		path  string
	}{{'1', "alpha"}, {'2', "beta"}} {
		for _, r := range []rune{'\'', tc.digit} {
			model, _ := home.Update(plainKeyMsg(r))
			home = model.(*Home)
		}
		if it := home.flatItems[home.cursor]; it.Type != session.ItemTypeGroup || it.Path != tc.path {
			t.Fatalf("'%c via Update: cursor on %s, want group %s", tc.digit, describeCursor(home), tc.path)
		}
		rendered := ansi.Strip(home.renderSessionList(60, 12))
		if !strings.Contains(rendered, tc.path+" ("+map[string]string{"alpha": "3", "beta": "2"}[tc.path]+") '"+string(tc.digit)) {
			t.Fatalf("row for %s does not advertise '%c:\n%s", tc.path, tc.digit, rendered)
		}
	}
}

// ---------- pending-key bar and Chinese IME punctuation ----------

func TestMatrixPendingBar_QuoteAndCountAreVisible(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '\'')
	if v := ansi.Strip(h.View()); !strings.Contains(v, "press 1-9 to jump to that root group") {
		t.Fatalf("after ': no waiting hint in the frame:\n%s", v)
	}
	h = matrixPressKeys(t, h, '2')
	if v := ansi.Strip(h.View()); strings.Contains(v, "press 1-9") {
		t.Fatal("hint still shown after the group jump")
	}

	h = matrixPressKeys(t, h, '3')
	if v := ansi.Strip(h.View()); !strings.Contains(v, "j/k (or ↓/↑) moves 3 rows") {
		t.Fatalf("after 3: no count hint in the frame:\n%s", v)
	}
	h = matrixPressKeys(t, h, 'k')
	if v := ansi.Strip(h.View()); strings.Contains(v, "moves 3 rows") {
		t.Fatal("count hint still shown after 3k")
	}
}

func TestMatrixIMEKeys_ChinesePunctuationActsAsVimPrefix(t *testing.T) {
	for _, quote := range []rune{'「', '」', '‘', '’'} {
		home, idx := buildTwoGroupHome(t)
		home.cursor = idx["a1"]
		h := matrixPressKeys(t, home, quote, '2')
		if it := h.flatItems[h.cursor]; it.Type != session.ItemTypeGroup || it.Path != "beta" {
			t.Errorf("%c2: cursor on %s, want group beta", quote, describeCursor(h))
		}
	}

	home, _ := buildTwoGroupHome(t)
	h := matrixPressKeys(t, home, '：')
	if !h.navCount.cmd {
		t.Fatal("full-width colon did not open the : prompt")
	}
}
