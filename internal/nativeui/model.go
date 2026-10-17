// Package nativeui implements the independent Windows native session list.
// matrix/hardened: no tmux-backed UI, global state, or AI activity inference.
package nativeui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// Service owns persistence and native hosts. The UI never owns their lifetime.
// RunID fences mutations against a session restarted by another client.
type Service interface {
	List(context.Context) ([]terminalhost.SessionInfo, error)
	Create(context.Context, string, string, string) (terminalhost.SessionInfo, error)
	CreateInGroup(context.Context, string, string, string, string) (terminalhost.SessionInfo, error)
	Start(context.Context, string) (terminalhost.SessionInfo, error)
	// matrix/hardened: history selection carries the displayed old generation.
	Resume(context.Context, string, string) (terminalhost.SessionInfo, error)
	Stop(context.Context, string, string) error
	Rename(context.Context, string, string) error
	Remove(context.Context, string, string) error
	Move(context.Context, string, string, string) error
	AttachCommand(terminalhost.SessionInfo) (*exec.Cmd, error)
}

type mode uint8

const (
	listMode mode = iota
	createMode
	renameMode
	moveMode
	confirmStopMode
	confirmRemoveMode
	workingMode
	attachMode
)

const operationTimeout = 30 * time.Second

// Model contains only one window's UI state. Construct a separate model for
// every list window, even when they share the same Service.
type Model struct {
	service     Service
	defaultCwd  string
	items       []terminalhost.SessionInfo
	allItems    []terminalhost.SessionInfo
	selectedID  string
	cursor      int
	width       int
	height      int
	mode        mode
	notice      string
	noticeErr   bool
	listFailure bool
	busyLabel   string

	// matrix/hardened: closing/opening a dialog invalidates old list results.
	generation   uint64
	listRequest  uint64
	listInFlight bool
	pollToken    uint64
	quitting     bool
	// matrix/hardened: only RunWithOptions pairs private frame metadata with
	// its filtering output; public New/View callers keep plain terminal views.
	cursorFrames bool

	target      terminalhost.SessionInfo
	name        textinput.Model
	dir         textinput.Model
	focus       int
	tool        string
	group       textinput.Model
	search      textinput.Model
	searching   bool
	allGroups   bool
	filterGroup string
}

type listResultMsg struct {
	generation uint64
	request    uint64
	items      []terminalhost.SessionInfo
	err        error
}

type pollMsg struct{ token uint64 }

type operationResultMsg struct {
	generation   uint64
	selectedID   string
	command      *exec.Cmd
	notice       string
	err          error
	clearFilters bool
}

type attachReturnedMsg struct {
	generation uint64
	err        error
}

// Options controls the defaults for one native list window.
// matrix/hardened: the caller selects a tool once without changing user config.
type Options struct {
	DefaultTool string
}

// New creates the native list with the legacy Shell default.
func New(service Service, defaultCwd string) tea.Model {
	return NewWithOptions(service, defaultCwd, Options{})
}

// NewWithOptions creates the native list without starting or stopping any
// hosted process. An empty or unsupported default tool falls back to Shell.
func NewWithOptions(service Service, defaultCwd string, options Options) tea.Model {
	tool := options.DefaultTool
	switch tool {
	case "shell", "codex", "powershell":
	default:
		tool = "shell"
	}
	name := textinput.New()
	name.CharLimit = 120
	name.Prompt = ""
	dir := textinput.New()
	dir.CharLimit = 4096
	dir.Prompt = ""
	group := textinput.New()
	group.CharLimit = 64
	group.Prompt = ""
	search := textinput.New()
	search.CharLimit = 256
	search.Prompt = ""
	return &Model{
		service: service, defaultCwd: defaultCwd,
		width: 80, height: 24, generation: 1, pollToken: 1,
		name: name, dir: dir, group: group, search: search, tool: tool, allGroups: true,
	}
}

// Run displays the list. Quitting this program leaves hosted sessions alive.
func Run(service Service, defaultCwd string) error {
	return RunWithOptions(service, defaultCwd, Options{})
}

// matrix/hardened: tool defaults belong to this window, not the shared manager.
func RunWithOptions(service Service, defaultCwd string, options Options) error {
	// matrix/hardened: Windows resize observation shares this window's cancellation,
	// so a Send paused by an attached process also ends when the program exits.
	ctx, cancel := context.WithCancel(context.Background())
	model := NewWithOptions(service, defaultCwd, options).(*Model)
	output, filtersCursorFrames := nativeUIOutput()
	model.cursorFrames = filtersCursorFrames
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithOutput(output), consoleKeyProgramOption())
	waitResize := observeProgramWindowSize(ctx, program)
	defer func() {
		cancel()
		waitResize()
	}()
	_, err := program.Run()
	return err
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), pollAfter(m.pollToken))
}

func pollAfter(token uint64) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return pollMsg{token} })
}

func (m *Model) refresh() tea.Cmd {
	if m.mode != listMode || m.listInFlight || m.quitting {
		return nil
	}
	m.listRequest++
	m.listInFlight = true
	generation, request, service := m.generation, m.listRequest, m.service
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
		defer cancel()
		if service == nil {
			return listResultMsg{generation: generation, request: request, err: errors.New("会话服务不可用")}
		}
		items, err := service.List(ctx)
		return listResultMsg{generation: generation, request: request, items: items, err: err}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.resizeInputs()
		return m, nil
	case listResultMsg:
		if m.mode != listMode || msg.generation != m.generation || msg.request != m.listRequest {
			return m, nil
		}
		m.listInFlight = false
		if msg.err != nil {
			m.setNotice("读取列表失败："+msg.err.Error(), true)
			m.listFailure = true
			return m, nil
		}
		if m.listFailure {
			m.setNotice("", false)
		}
		m.setItems(msg.items)
		return m, nil
	case pollMsg:
		if m.mode != listMode || msg.token != m.pollToken {
			return m, nil
		}
		return m, tea.Batch(m.refresh(), pollAfter(m.pollToken))
	case operationResultMsg:
		if m.mode != workingMode || msg.generation != m.generation {
			return m, nil
		}
		if msg.selectedID != "" {
			m.selectedID = msg.selectedID
		}
		if msg.clearFilters {
			m.clearFilters()
		}
		if msg.err != nil {
			m.setNotice(msg.err.Error(), true)
			return m, m.showList()
		}
		if msg.command != nil {
			m.mode = attachMode
			generation := m.generation
			return m, tea.ExecProcess(msg.command, func(err error) tea.Msg {
				return attachReturnedMsg{generation: generation, err: err}
			})
		}
		m.setNotice(msg.notice, false)
		return m, m.showList()
	case attachReturnedMsg:
		if m.mode != attachMode || msg.generation != m.generation {
			return m, nil
		}
		if msg.err != nil {
			m.setNotice("返回列表："+msg.err.Error(), true)
		} else {
			// matrix/hardened: reconnect stays discoverable after detaching.
			m.setNotice("已返回列表；运行中的会话继续保留，按 Enter 可再次进入。", false)
		}
		return m, m.showList()
	case tea.KeyMsg:
		// matrix/hardened: Ctrl+C always exits the UI, including every modal.
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}
		if m.mode == createMode || m.mode == renameMode || m.mode == moveMode {
			return m, m.updateForm(msg)
		}
		if m.mode == confirmStopMode || m.mode == confirmRemoveMode {
			return m, m.updateConfirmation(msg)
		}
		if m.mode == listMode {
			if m.searching {
				return m, m.updateSearchKey(msg)
			}
			return m, m.updateListKey(msg)
		}
		return m, nil
	}
	// textinput cursor blink belongs to the active form, never to list commands.
	if m.mode == createMode || m.mode == renameMode || m.mode == moveMode {
		return m, m.updateInput(msg)
	}
	if m.mode == listMode && m.searching {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) setNotice(notice string, failure bool) {
	m.notice, m.noticeErr = notice, failure
	m.listFailure = false
}

func (m *Model) invalidate() {
	m.generation++
	m.pollToken++
	m.listInFlight = false
}

func (m *Model) showList() tea.Cmd {
	m.invalidate()
	m.mode = listMode
	m.name.Blur()
	m.dir.Blur()
	m.group.Blur()
	return tea.Batch(m.refresh(), pollAfter(m.pollToken))
}

func (m *Model) setItems(items []terminalhost.SessionInfo) {
	m.allItems = append([]terminalhost.SessionInfo(nil), items...)
	m.applyFilters()
}

// matrix/hardened: filtering never changes descriptors or hosted processes.
func (m *Model) applyFilters() {
	query := strings.ToLower(strings.TrimSpace(m.search.Value()))
	m.items = nil
	for _, item := range m.allItems {
		if !m.allGroups && item.Group != m.filterGroup {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{item.Name, item.Dir, item.Tool, item.Group, groupLabel(item.Group)}, "\n"))
		if query != "" && !strings.Contains(haystack, query) {
			continue
		}
		m.items = append(m.items, item)
	}
	for i := range m.items {
		if m.items[i].ID == m.selectedID {
			m.cursor = i
			return
		}
	}
	if len(m.items) == 0 {
		m.cursor, m.selectedID = 0, ""
		return
	}
	m.cursor = min(max(0, m.cursor), len(m.items)-1)
	m.selectedID = m.items[m.cursor].ID
}

func (m *Model) clearFilters() {
	m.search.SetValue("")
	m.search.Blur()
	m.searching, m.allGroups, m.filterGroup = false, true, ""
}

func (m *Model) cycleGroup(direction int) {
	groups := []string{""}
	seen := map[string]bool{"": true}
	for _, item := range m.allItems {
		if !seen[item.Group] {
			seen[item.Group] = true
			groups = append(groups, item.Group)
		}
	}
	sort.Strings(groups[1:])
	index := 0 // all groups precedes the default and named groups.
	if !m.allGroups {
		for i, group := range groups {
			if group == m.filterGroup {
				index = i + 1
				break
			}
		}
	}
	index = (index + direction + len(groups) + 1) % (len(groups) + 1)
	m.allGroups = index == 0
	if index != 0 {
		m.filterGroup = groups[index-1]
	}
	m.applyFilters()
}

func (m *Model) updateSearchKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.search.SetValue("")
		m.searching = false
		m.search.Blur()
		m.applyFilters()
		return nil
	case "enter":
		m.searching = false
		m.search.Blur()
		return nil
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	m.applyFilters()
	return cmd
}

func (m *Model) selected() (terminalhost.SessionInfo, bool) {
	if m.cursor < 0 || m.cursor >= len(m.items) || m.items[m.cursor].ID != m.selectedID {
		return terminalhost.SessionInfo{}, false
	}
	return m.items[m.cursor], true
}

func (m *Model) updateListKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "/":
		m.searching = true
		m.resizeInputs()
		return m.search.Focus()
	case "esc":
		m.clearFilters()
		m.applyFilters()
	case "tab":
		m.cycleGroup(1)
	case "shift+tab":
		m.cycleGroup(-1)
	case "q":
		m.quitting = true
		return tea.Quit
	case "up", "k":
		if len(m.items) > 0 {
			m.cursor = max(0, m.cursor-1)
			m.selectedID = m.items[m.cursor].ID
		}
	case "down", "j":
		if len(m.items) > 0 {
			m.cursor = min(len(m.items)-1, m.cursor+1)
			m.selectedID = m.items[m.cursor].ID
		}
	case "n":
		return m.openCreate()
	case "enter", "o":
		if info, ok := m.selected(); ok {
			return m.openSession(info)
		}
	case "h":
		// matrix/hardened: Enter remains a new dialog after stop; history is an
		// explicit Codex-only action and never replaces a live process.
		if info, ok := m.selected(); ok {
			if info.Tool != "codex" || info.State != "stopped" && info.State != "exited" {
				m.setNotice("只有已停止或退出的 Codex 会话可按 h 选择历史。", false)
				return nil
			}
			return m.resumeSession(info)
		}
	case "s", "d", "r", "g":
		info, ok := m.selected()
		if !ok {
			return nil
		}
		if msg.String() == "s" && info.State != "running" && !(info.State == "error" && info.RunID != "") {
			m.setNotice("只有正在运行的会话需要停止。", false)
			return nil
		}
		m.invalidate()
		m.target = info
		m.setNotice("", false)
		switch msg.String() {
		case "s":
			m.mode = confirmStopMode
		case "d":
			m.mode = confirmRemoveMode
		case "r":
			m.mode, m.focus = renameMode, 0
			m.name.SetValue(info.Name)
			m.resizeInputs()
			return m.name.Focus()
		case "g":
			m.mode, m.focus = moveMode, 3
			m.group.SetValue(info.Group)
			m.resizeInputs()
			return m.group.Focus()
		}
	}
	return nil
}

func (m *Model) openCreate() tea.Cmd {
	m.invalidate()
	// matrix/hardened: repeated creation keeps this window's last tool choice.
	m.mode, m.focus = createMode, 0
	m.setNotice("", false)
	defaultName := filepath.Base(m.defaultCwd)
	if defaultName == "." || defaultName == string(filepath.Separator) || defaultName == "" {
		defaultName = "新会话"
	}
	m.name.SetValue(defaultName)
	m.dir.SetValue(m.defaultCwd)
	m.group.SetValue("")
	if !m.allGroups {
		m.group.SetValue(m.filterGroup)
	}
	m.resizeInputs()
	m.dir.Blur()
	m.group.Blur()
	m.search.Blur()
	m.searching = false
	return m.name.Focus()
}

func (m *Model) resizeInputs() {
	// matrix/hardened: textinput's rendered cursor uses one extra cell. Size
	// each field from its actual prefix, including the current search group.
	width := max(1, m.width-ansi.StringWidth("› 名称：")-1)
	groupWidth := width
	if m.mode == createMode {
		// matrix/hardened: textinput pads its field and adds a cursor cell;
		// reserve both that cell and the default-group hint after resizing.
		groupWidth = max(1, width-ansi.StringWidth(createGroupHint))
	}
	for _, field := range []struct {
		input *textinput.Model
		width int
	}{
		{&m.name, width}, {&m.dir, width}, {&m.group, groupWidth},
		{&m.search, max(1, m.width-ansi.StringWidth(m.searchPrefix())-1)},
	} {
		if field.input.Width == field.width {
			continue
		}
		field.input.Width = field.width
		// matrix/hardened: changing Width alone leaves textinput's old scroll
		// offsets. Its public cursor API recomputes the viewport immediately,
		// keeping the focused cursor visible before the next IME composition.
		position := field.input.Position()
		field.input.CursorEnd()
		field.input.SetCursor(position)
	}
}

func (m *Model) updateForm(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return m.showList()
	case "tab", "down":
		return m.moveFocus(1)
	case "shift+tab", "up":
		return m.moveFocus(-1)
	case "enter":
		group := strings.TrimSpace(m.group.Value())
		if m.mode == createMode || m.mode == moveMode {
			if err := validateGroup(group); err != nil {
				m.setNotice(err.Error(), true)
				return nil
			}
		}
		if m.mode == moveMode {
			selected, ok := m.selected()
			if !ok || selected.ID != m.target.ID || selected.RunID != m.target.RunID {
				m.setNotice("会话已改变，请刷新后重新确认。", true)
				return m.showList()
			}
			return m.moveSession(group)
		}
		name := strings.TrimSpace(m.name.Value())
		if name == "" {
			m.setNotice("请输入会话名称。", true)
			return nil
		}
		if m.mode == renameMode {
			return m.renameSession(name)
		}
		dir := strings.TrimSpace(m.dir.Value())
		if dir == "" {
			m.setNotice("请输入项目目录。", true)
			return nil
		}
		return m.createSession(name, m.tool, dir, group)
	}
	if m.mode == createMode && m.focus == 2 {
		switch msg.String() {
		case "left":
			m.cycleTool(-1)
		case "right", " ":
			m.cycleTool(1)
		}
		return nil
	}
	return m.updateInput(msg)
}

// matrix/hardened: native PowerShell follows the existing Shell/Codex choices.
func (m *Model) cycleTool(direction int) {
	tools := [...]string{"shell", "codex", "powershell"}
	index := 0
	for i, tool := range tools {
		if tool == m.tool {
			index = i
			break
		}
	}
	m.tool = tools[(index+direction+len(tools))%len(tools)]
}

func (m *Model) moveFocus(direction int) tea.Cmd {
	count := 1
	if m.mode == createMode {
		count = 4
	}
	if m.mode == moveMode {
		return m.group.Focus()
	}
	m.focus = (m.focus + direction + count) % count
	m.name.Blur()
	m.dir.Blur()
	m.group.Blur()
	switch m.focus {
	case 0:
		return m.name.Focus()
	case 1:
		return m.dir.Focus()
	case 3:
		return m.group.Focus()
	}
	return nil
}

func (m *Model) updateInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch m.focus {
	case 0:
		m.name, cmd = m.name.Update(msg)
	case 1:
		m.dir, cmd = m.dir.Update(msg)
	case 3:
		m.group, cmd = m.group.Update(msg)
	}
	return cmd
}

func (m *Model) updateConfirmation(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return m.showList()
	case "enter":
		// Confirmations stay bound to the row and instance originally displayed.
		selected, ok := m.selected()
		if !ok || selected.ID != m.target.ID || selected.RunID != m.target.RunID {
			m.setNotice("会话已改变，请刷新后重新确认。", true)
			return m.showList()
		}
		return m.mutateSession(m.mode, m.target)
	}
	return nil
}

func (m *Model) beginOperation(label string, operation func(context.Context) operationResultMsg) tea.Cmd {
	m.invalidate()
	m.mode, m.busyLabel = workingMode, label
	m.name.Blur()
	m.dir.Blur()
	m.group.Blur()
	m.search.Blur()
	m.searching = false
	generation, service := m.generation, m.service
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
		defer cancel()
		if service == nil {
			return operationResultMsg{generation: generation, err: errors.New("会话服务不可用")}
		}
		result := operation(ctx)
		result.generation = generation
		return result
	}
}

// currentInstance is a second check against changes made in another UI window.
// Stop/Remove/Move also receive RunID so the service can close the remaining race.
func currentInstance(ctx context.Context, service Service, target terminalhost.SessionInfo) (terminalhost.SessionInfo, error) {
	items, err := service.List(ctx)
	if err != nil {
		return terminalhost.SessionInfo{}, fmt.Errorf("核对会话失败：%w", err)
	}
	for _, item := range items {
		if item.ID == target.ID {
			if item.RunID != target.RunID {
				return terminalhost.SessionInfo{}, errors.New("会话已在其他窗口重新启动，请刷新后重试。")
			}
			return item, nil
		}
	}
	return terminalhost.SessionInfo{}, errors.New("会话已被移除，请刷新后重试。")
}

func attachResult(service Service, info terminalhost.SessionInfo) operationResultMsg {
	command, err := service.AttachCommand(info)
	if err == nil && command == nil {
		err = errors.New("未能打开会话终端。")
	}
	return operationResultMsg{selectedID: info.ID, command: command, err: err}
}

func (m *Model) openSession(target terminalhost.SessionInfo) tea.Cmd {
	service := m.service
	return m.beginOperation("正在打开会话…", func(ctx context.Context) operationResultMsg {
		info, err := currentInstance(ctx, service, target)
		if err != nil {
			return operationResultMsg{err: err}
		}
		switch info.State {
		case "stopped", "exited":
			info, err = service.Start(ctx, target.ID)
			if err != nil {
				return operationResultMsg{err: fmt.Errorf("启动失败：%w", err)}
			}
		case "running":
		default:
			return operationResultMsg{err: errors.New("当前进程状态未知，请稍后刷新再打开。")}
		}
		return attachResult(service, info)
	})
}

// matrix/hardened: do not silently start a fresh dialog if history launch or
// attachment fails. The backend rechecks this exact generation under its lock.
func (m *Model) resumeSession(target terminalhost.SessionInfo) tea.Cmd {
	service := m.service
	return m.beginOperation("正在打开 Codex 历史选择…", func(ctx context.Context) operationResultMsg {
		info, err := currentInstance(ctx, service, target)
		if err != nil {
			return operationResultMsg{err: err}
		}
		if info.Tool != "codex" || info.State != "stopped" && info.State != "exited" {
			return operationResultMsg{err: errors.New("Codex 当前状态已改变，请刷新后选择历史。")}
		}
		info, err = service.Resume(ctx, target.ID, target.RunID)
		if err != nil {
			return operationResultMsg{selectedID: target.ID, err: fmt.Errorf("打开历史失败：%w", err)}
		}
		return attachResult(service, info)
	})
}

func (m *Model) createSession(name, tool, dir, group string) tea.Cmd {
	service := m.service
	return m.beginOperation("正在创建会话…", func(ctx context.Context) operationResultMsg {
		var info terminalhost.SessionInfo
		var err error
		if group == "" {
			info, err = service.Create(ctx, name, tool, dir)
		} else {
			info, err = service.CreateInGroup(ctx, name, tool, dir, group)
		}
		if err != nil {
			if info.ID != "" {
				// matrix/hardened: creation may persist before auto-start fails.
				// Preserve the descriptor and never automatically retry Start.
				return operationResultMsg{selectedID: info.ID, clearFilters: true, err: fmt.Errorf("会话已创建，启动失败：%w", err)}
			}
			return operationResultMsg{err: fmt.Errorf("创建失败：%w", err)}
		}
		selectedID := info.ID
		switch info.State {
		case "stopped", "exited":
			info, err = service.Start(ctx, selectedID)
			if err != nil {
				return operationResultMsg{selectedID: selectedID, clearFilters: true, err: fmt.Errorf("会话已创建，启动失败：%w", err)}
			}
		case "running":
		default:
			return operationResultMsg{selectedID: selectedID, clearFilters: true, err: errors.New("会话已创建，进程状态未知，请稍后刷新再打开。")}
		}
		result := attachResult(service, info)
		result.clearFilters = true
		return result
	})
}

// matrix/hardened: Move edits organization only and carries the displayed run.
func (m *Model) moveSession(group string) tea.Cmd {
	service, target := m.service, m.target
	return m.beginOperation("正在保存分组…", func(ctx context.Context) operationResultMsg {
		if _, err := currentInstance(ctx, service, target); err != nil {
			return operationResultMsg{err: err}
		}
		if err := service.Move(ctx, target.ID, target.RunID, group); err != nil {
			return operationResultMsg{err: fmt.Errorf("移动分组失败：%w", err)}
		}
		return operationResultMsg{selectedID: target.ID, clearFilters: true, notice: "会话分组已保存；当前进程继续运行。"}
	})
}

func validateGroup(group string) error {
	if !utf8.ValidString(group) || utf8.RuneCountInString(group) > 64 || len(group) > 256 {
		return errors.New("分组名称最多 64 个字符、256 字节。")
	}
	for _, r := range group {
		if unicode.IsControl(r) {
			return errors.New("分组名称不能包含控制字符。")
		}
	}
	return nil
}

func (m *Model) renameSession(name string) tea.Cmd {
	service, target := m.service, m.target
	return m.beginOperation("正在保存名称…", func(ctx context.Context) operationResultMsg {
		if _, err := currentInstance(ctx, service, target); err != nil {
			return operationResultMsg{err: err}
		}
		if err := service.Rename(ctx, target.ID, name); err != nil {
			return operationResultMsg{err: fmt.Errorf("重命名失败：%w", err)}
		}
		return operationResultMsg{selectedID: target.ID, notice: "会话名称已保存。"}
	})
}

func (m *Model) mutateSession(action mode, target terminalhost.SessionInfo) tea.Cmd {
	service := m.service
	label := "正在停止会话…"
	if action == confirmRemoveMode {
		label = "正在移除会话…"
	}
	return m.beginOperation(label, func(ctx context.Context) operationResultMsg {
		if _, err := currentInstance(ctx, service, target); err != nil {
			return operationResultMsg{err: err}
		}
		if action == confirmStopMode {
			if err := service.Stop(ctx, target.ID, target.RunID); err != nil {
				return operationResultMsg{err: fmt.Errorf("停止失败：%w", err)}
			}
			return operationResultMsg{selectedID: target.ID, notice: "会话已停止。"}
		}
		if err := service.Remove(ctx, target.ID, target.RunID); err != nil {
			return operationResultMsg{err: fmt.Errorf("移除失败：%w", err)}
		}
		return operationResultMsg{notice: "会话已从列表移除；项目文件保留。"}
	})
}
