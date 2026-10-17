// matrix/hardened: cell-width clipping and process-only labels for native lists.
package nativeui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// matrix/hardened: rendering and input sizing share one cell-width-aware hint.
const createGroupHint = "（留空为默认分组）"

func plain(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(value))
}

func clipped(value string, width int) string {
	return ansi.Truncate(value, max(1, width), "…")
}

func stateLabel(state string) string {
	switch state {
	case "running":
		return "正在运行"
	case "exited":
		return "已退出"
	case "stopped":
		return "已停止"
	case "error":
		return "异常"
	default:
		return "未知"
	}
}

func toolLabel(tool string) string {
	if tool == "codex" {
		return "Codex"
	}
	if tool == "shell" {
		return "Shell"
	}
	if tool == "powershell" {
		return "PowerShell"
	}
	return plain(tool)
}

func groupLabel(group string) string {
	if group == "" {
		return "默认分组"
	}
	return plain(group)
}

func (m *Model) View() string {
	view, frame := m.viewFrame()
	if !m.cursorFrames {
		return view
	}
	return nativeCursorFrameView(view, frame)
}

func (m *Model) viewFrame() (string, cursorFrame) {
	if m.quitting {
		return "", cursorFrame{width: m.width, height: m.height, rows: 1}
	}
	var lines []string
	switch m.mode {
	case createMode, renameMode, moveMode:
		lines = m.formLines()
	case confirmStopMode, confirmRemoveMode:
		lines = m.confirmationLines()
	case workingMode, attachMode:
		lines = []string{"Agent Deck · Windows 原生会话", "", m.busyLabel, "", "Ctrl+C 退出界面（会话继续运行）"}
	default:
		lines = m.listLines()
	}
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	lines, anchor := clipCursorLines(lines, m.width, m.height)
	return strings.Join(lines, "\n"), cursorFrame{anchor: anchor, width: m.width, height: m.height, rows: len(lines)}
}

func (m *Model) searchPrefix() string {
	group := "全部分组"
	if !m.allGroups {
		group = groupLabel(m.filterGroup)
	}
	// Keep some editable space when a long group name fills the filter line.
	group = clipped(group, max(1, m.width-ansi.StringWidth("分组： · 搜索：")-2))
	return "分组：" + group + " · 搜索："
}

func (m *Model) listLines() []string {
	header := fmt.Sprintf("Agent Deck · Windows 原生会话 · %d/%d 个", len(m.items), len(m.allItems))
	toolbar := "n 新建 · Enter/o 打开 · h 历史 · s 停止 · r 改名 · g 分组 · d 移除"
	// matrix/hardened: distinguish leaving a view from stopping its process.
	footer := "↑↓/j/k 选择 · / 搜索 · Tab 分组 · q/Ctrl+C 退出列表"
	lifetime := "会话内 Ctrl+Q 返回列表 · q 退出列表 · 会话继续运行"
	// matrix/hardened: stopped Codex has two explicit, different launch actions.
	if selected, ok := m.selected(); ok && selected.Tool == "codex" && (selected.State == "stopped" || selected.State == "exited") {
		lifetime = "Codex：Enter 新对话 · h 选择历史 · q 退出列表"
	}
	filter := m.searchPrefix() + plain(m.search.Value())
	if m.searching {
		// matrix/hardened: field padding must not push active keyboard hints
		// beyond the terminal edge; search consumes list shortcuts as text.
		toolbar = "Enter 保留 · Esc 清除 · Ctrl+C 退出"
		footer = toolbar + "（会话继续运行）"
		filter = m.searchPrefix() + cursorInputView(m.search)
		lifetime = "会话内 Ctrl+Q 返回列表 · 会话继续运行"
	}
	// matrix/hardened: the launcher's project stays visible before first create.
	path := "目录：" + plain(m.defaultCwd)
	if selected, ok := m.selected(); ok {
		path = "目录：" + plain(selected.Dir)
	}
	message := plain(m.notice)
	if m.noticeErr {
		message = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(message)
	}
	if m.height < 8 {
		// Very small windows retain an action line before optional detail.
		if !m.searching {
			footer += "（会话继续运行）"
		}
		shortToolbar := toolbar
		if !m.searching {
			shortToolbar += " · q 退出"
		}
		lines := []string{shortToolbar}
		if selected, ok := m.selected(); ok {
			lines = append(lines, "› "+plain(selected.Name)+" · "+stateLabel(selected.State))
		} else {
			if m.searching || len(m.allItems) > 0 {
				lines = append(lines, "没有匹配的会话；Esc 清除搜索。")
			} else {
				lines = append(lines, "没有会话；按 n 新建。")
			}
		}
		return append(lines, filter, path, message, footer)
	}
	rows := m.height - 7
	lines := []string{header, toolbar, filter}
	if len(m.items) == 0 {
		// matrix/hardened: usable defaults make the first n → Enter flow clear.
		empty := "没有会话；按 n 新建，Enter 直接进入 " + toolLabel(m.tool) + "。"
		if m.searching {
			empty = "没有匹配的会话；Esc 清除搜索。"
		} else if len(m.allItems) > 0 {
			empty = "没有匹配的会话；Esc 清除筛选。"
		}
		lines = append(lines, empty)
		for len(lines) < 3+rows {
			lines = append(lines, "")
		}
	} else {
		start := max(0, m.cursor-rows+1)
		// Keep the selected row visible while retaining previous rows as context.
		end := min(len(m.items), start+rows)
		for i := start; i < end; i++ {
			info := m.items[i]
			prefix := "  "
			if info.ID == m.selectedID {
				prefix = "› "
			}
			name, tool, state, group := plain(info.Name), toolLabel(info.Tool), stateLabel(info.State), groupLabel(info.Group)
			row := prefix + name + " · " + group + " · " + tool + " · " + state
			if m.width >= 64 {
				nameWidth := m.width - 44
				name = clipped(name, nameWidth)
				group = clipped(group, 14)
				row = prefix + name + strings.Repeat(" ", max(0, nameWidth-ansi.StringWidth(name))) + "  " + group + strings.Repeat(" ", max(0, 14-ansi.StringWidth(group))) + "  " + tool + "  " + state
			}
			if info.ID == m.selectedID {
				row = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Render(row)
			}
			lines = append(lines, row)
		}
		for len(lines) < 3+rows {
			lines = append(lines, "")
		}
	}
	return append(lines, path, message, lifetime, footer)
}

func (m *Model) formLines() []string {
	// matrix/hardened: creation immediately attaches to the new hosted process.
	title := "新建会话 · Enter 创建并进入"
	if m.mode == renameMode {
		title = "修改会话名称"
	}
	if m.mode == moveMode {
		return formViewport([]string{"移动会话分组", plain(m.target.Name) + " · " + stateLabel(m.target.State), "› 分组：" + cursorInputView(m.group), "留空使用默认分组；当前进程继续运行。", plain(m.notice), "Enter 保存 · Esc 取消"}, 2, m.height)
	}
	namePrefix, dirPrefix, toolPrefix, groupPrefix := "  ", "  ", "  ", "  "
	switch m.focus {
	case 0:
		namePrefix = "› "
	case 1:
		dirPrefix = "› "
	case 2:
		toolPrefix = "› "
	case 3:
		groupPrefix = "› "
	}
	lines := []string{title, namePrefix + "名称：" + cursorInputView(m.name)}
	if m.mode == createMode {
		choices := []string{"Shell", "Codex", "PowerShell"}
		for i, tool := range [...]string{"shell", "codex", "powershell"} {
			if m.tool == tool {
				choices[i] = "[" + choices[i] + "]"
			}
		}
		lines = append(lines, dirPrefix+"目录："+cursorInputView(m.dir), toolPrefix+"工具："+strings.Join(choices, "   "), groupPrefix+"分组："+cursorInputView(m.group)+createGroupHint)
	}
	footer := "Enter 保存 · Esc 取消"
	if m.mode == createMode {
		lines = append(lines, "确认名称和目录后，按 Enter 创建并进入 "+toolLabel(m.tool)+"。")
		footer = "Enter 创建并进入 · Tab/↑↓ 切换 · ←→ 选工具 · Esc 取消"
	}
	lines = append(lines, plain(m.notice), footer)
	return formViewport(lines, m.focus+1, m.height)
}

// matrix/hardened: short forms keep the active field reachable when the group
// is below the ordinary viewport, without changing the four-field tab order.
func formViewport(lines []string, active, height int) []string {
	if len(lines) <= height {
		return lines
	}
	if height <= 1 {
		return []string{lines[active]}
	}
	footer := lines[len(lines)-1]
	if height == 2 {
		return []string{lines[active], footer}
	}
	rows := height - 2
	start := max(1, active-rows+1)
	end := min(len(lines)-1, start+rows)
	compact := append([]string{lines[0]}, lines[start:end]...)
	return append(compact, footer)
}

func (m *Model) confirmationLines() []string {
	// matrix/hardened: stop is explicit and separate from a reversible detach.
	title, detail := "停止此会话？", "确认后结束这次运行；列表记录和项目文件保留。"
	if m.mode == confirmRemoveMode {
		title, detail = "移除此会话？", "移除列表记录，不删除项目文件。"
		if m.target.State == "running" {
			detail = "会先停止此会话，再移除列表记录；项目文件保留。"
		}
	}
	lines := []string{
		title, plain(m.target.Name) + " · " + stateLabel(m.target.State),
		detail,
	}
	if m.mode == confirmStopMode {
		// matrix/hardened: stop ends the process; history selection is explicit.
		if m.target.Tool == "codex" {
			lines = append(lines, "停止后 Enter 新对话；h 选择此目录的 Codex 历史。")
		}
		lines = append(lines, "只需返回列表请在会话内按 Ctrl+Q。")
	}
	lines = append(lines, "目录："+plain(m.target.Dir))
	footer := "Enter 确认 · Esc 取消 · Ctrl+C 退出界面"
	// matrix/hardened: extra lifecycle guidance cannot hide confirmation keys.
	if len(lines) >= m.height {
		lines = lines[:max(0, m.height-1)]
	}
	return append(lines, footer)
}
