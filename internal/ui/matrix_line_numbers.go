package ui

// matrix/hardened: vim-style line numbers and ":N" jump for the session list.
//
// Every list row except view-mode dividers gets a number (group headers too,
// so ":N" can land on a group). ":" opens a one-line command prompt in place of
// the help bar: ":12<Enter>" or ":12o" jumps to and activates row 12,
// matrix/hardened: ":12l" only selects row 12; ":12h" applies h/left there.
// ":set nonu" / ":set nu" hide or show the numbers for this run. Numbers are drawn only in
// the classic one-line-per-row list; the embedded sidebar's card heights
// depend on width, so it keeps its full width (":N" still works there).

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

const matrixCmdMaxLen = 32

// matrix/hardened: resolve every numeric suffix before applying its list action.
type matrixNumericAction uint8

const (
	matrixNumericOpen matrixNumericAction = iota
	matrixNumericSelect
	matrixNumericCollapse
)

// matrixRowNumbers maps flatItems indices to 1-based line numbers (0 for
// dividers) and returns the highest number.
func (h *Home) matrixRowNumbers() ([]int, int) {
	nums := make([]int, len(h.flatItems))
	n := 0
	for i, it := range h.flatItems {
		if it.Type == session.ItemTypeDivider {
			continue
		}
		n++
		nums[i] = n
	}
	return nums, n
}

// matrixLineNumberWidth is the number column's width including its trailing
// space, or 0 when numbers are not drawn.
func (h *Home) matrixLineNumberWidth(total int) int {
	if h.navCount.noNumbers || h.embeddedLayout || total == 0 {
		return 0
	}
	return len(strconv.Itoa(total)) + 1
}

// matrixNumberRow prefixes a rendered row with its line number (blank for
// num 0 and for continuation lines) and clips each line to width.
func matrixNumberRow(row string, num, numW, width int, current bool) string {
	style := DimStyle
	if current {
		style = lipgloss.NewStyle().Foreground(ColorYellow).Bold(true)
	}
	lines := strings.Split(row, "\n")
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			break // trailing newline
		}
		prefix := strings.Repeat(" ", numW)
		if i == 0 && num > 0 {
			prefix = style.Render(fmt.Sprintf("%*d", numW-1, num)) + " "
		}
		line = prefix + line
		if cellWidth(line) > width {
			line = cellTruncate(line, width, "")
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// matrixJumpToLine moves the cursor to line n; false when n is out of range.
func (h *Home) matrixJumpToLine(n int) bool {
	// matrix/hardened: divider rows carry zero, which is never an address.
	if n <= 0 {
		return false
	}
	nums, _ := h.matrixRowNumbers()
	for i, num := range nums {
		if num == n {
			h.matrixJump(func() { h.jumpToIndex(i) })
			return true
		}
	}
	return false
}

// matrixHandleCmdKey handles keys while the ":" prompt is open.
func (h *Home) matrixHandleCmdKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// matrix/hardened: only a complete numeric command accepts h/l or the
	// configured open key; letters in textual commands remain ordinary text.
	text := strings.TrimSpace(h.navCount.cmdBuf)
	if matrixNumericCommand(text) {
		if msg.String() == "l" {
			return h.matrixSubmitCmd(matrixNumericSelect)
		}
		if msg.String() == "h" {
			return h.matrixSubmitCmd(matrixNumericCollapse)
		}
		if h.normalizeMainKey(msg.String()) == defaultHotkeyBindings[hotkeyOpenSession] {
			return h.matrixSubmitCmd(matrixNumericOpen)
		}
	}
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		h.matrixResetNav()
		return h, nil
	case tea.KeyBackspace:
		if h.navCount.cmdBuf == "" {
			h.matrixResetNav()
			return h, nil
		}
		h.navCount.cmdOverflow = false // matrix/hardened: editing the visible command clears the length error.
		r := []rune(h.navCount.cmdBuf)
		h.navCount.cmdBuf = string(r[:len(r)-1])
		return h, nil
	case tea.KeyEnter:
		return h.matrixSubmitCmd(matrixNumericOpen)
	case tea.KeySpace:
		h.matrixCmdAppend(" ")
		return h, nil
	case tea.KeyRunes:
		h.matrixCmdAppend(string(msg.Runes))
		return h, nil
	}
	return h, nil
}

func (h *Home) matrixCmdAppend(s string) {
	if len(h.navCount.cmdBuf)+len(s) <= matrixCmdMaxLen {
		h.navCount.cmdBuf += s
	} else {
		h.navCount.cmdOverflow = true // matrix/hardened: prevent truncated commands from opening the wrong row.
	}
}

// matrix/hardened: clear the prefix before selecting, collapsing, or opening.
func (h *Home) matrixSubmitCmd(action matrixNumericAction) (tea.Model, tea.Cmd) {
	text, overflow := strings.TrimSpace(h.navCount.cmdBuf), h.navCount.cmdOverflow
	h.matrixResetNav()
	if overflow {
		h.setError(fmt.Errorf("command is too long (maximum %d bytes)", matrixCmdMaxLen))
		return h, nil
	}
	return h, h.matrixRunCmd(text, action)
}

func matrixNumericCommand(text string) bool {
	// matrix/hardened: signed numeric input also submits so nonpositive rows
	// are rejected immediately rather than turning the open key into text.
	if len(text) > 1 && (text[0] == '+' || text[0] == '-') {
		text = text[1:]
	}
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// matrix/hardened: numeric l only selects, while h uses ordinary collapse/parent
// navigation; neither enters a session or restarts a stopped session.
func (h *Home) matrixRunCmd(text string, action matrixNumericAction) tea.Cmd {
	if text == "" {
		return nil
	}
	if n, err := strconv.Atoi(text); err == nil {
		if !h.matrixJumpToLine(n) {
			_, total := h.matrixRowNumbers()
			h.setError(fmt.Errorf("no line %d (list has %d)", n, total))
			return nil
		}
		switch action {
		case matrixNumericSelect:
			return h.fetchSelectedPreview()
		case matrixNumericCollapse:
			h.collapseOrNavUp()
			h.syncViewport() // matrix/hardened: keep the resulting parent row visible after folding.
			return h.fetchSelectedPreview()
		}
		// matrix/hardened: reuse the ordinary opening action after resolving the row.
		_, cmd := h.activateSelectedItem()
		return cmd
	}
	switch text {
	case "set nu", "set number":
		h.navCount.noNumbers = false
	case "set nonu", "set nonumber":
		h.navCount.noNumbers = true
	default:
		h.setError(fmt.Errorf("unknown command :%s (use :N, :set nu, :set nonu)", text))
	}
	return nil
}

// matrix/hardened: Enter confirms a numeric address independently of its
// ordinary list binding; avoid advertising Enter twice when o is disabled.
func (h *Home) matrixNumericConfirmKeys() string {
	key := h.sessionOpenKeyLabel()
	if strings.EqualFold(key, "enter") {
		return "Enter"
	}
	return "Enter/" + key
}

// matrixRenderCmdBar replaces the help bar while the ":" prompt is open.
func (h *Home) matrixRenderCmdBar() string {
	border := lipgloss.NewStyle().Foreground(ColorBorder).Render(strings.Repeat("─", max(0, h.width)))
	prompt := lipgloss.NewStyle().Foreground(ColorText).Render(":" + h.navCount.cmdBuf + "█")
	// matrix/hardened: advertise selecting and collapsing without opening.
	hint := lipgloss.NewStyle().Foreground(ColorComment).Render("   N+l goes to line · N+h collapses · N+" + h.matrixNumericConfirmKeys() + " opens · set nu / set nonu · Esc cancel")
	return lipgloss.JoinVertical(lipgloss.Left, border, lipgloss.NewStyle().MaxWidth(h.width).Render(prompt+hint))
}
