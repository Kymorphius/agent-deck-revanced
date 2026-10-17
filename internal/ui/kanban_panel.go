package ui

// matrix/hardened: World Store task board (alt+t). Tasks live only in World
// Store; this panel reads them when it opens or on `r`, and writes only after
// the user confirms one action. Status changes follow World Store's flow
// (start / pause / resume / complete), the same keys as its own TUI. A write
// whose outcome is unknown locks every further write until a fresh read
// (never retried automatically).

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/worldstore"
)

// kanbanSession is what the board needs to know about an agent-deck session.
type kanbanSession struct {
	ID     string
	Title  string
	Path   string
	Group  string
	Status session.Status
}

// kanbanEnv is everything the panel needs to reach World Store and launch.
type kanbanEnv struct {
	client  *worldstore.Client
	device  string
	exe     string
	profile string
	// execCategory is the 任务｜执行分类 value the default view shows.
	execCategory string
	// send queues a message for a session; nil means `agent-deck session send`.
	send func(ctx context.Context, sessionID, text string) error
	// open hands a URL to the system browser; nil means open / xdg-open.
	open func(url string) error
}

type kanbanConfirm struct {
	kind string // start, pause, resume, done, link, unlink, launch, review, reject
	task worldstore.Task
	// reason is the text typed for a rejected review (matrix/hardened).
	reason string
	// operationID is fixed when the confirmation opens, so a start the user
	// confirms is one World Store operation however it ends.
	operationID string
}

type kanbanLoadedMsg struct {
	seq   int
	board *worldstore.Board
	err   error
}

type kanbanWrittenMsg struct {
	notice string
	err    error
}

type kanbanJumpMsg struct{ sessionID string }

// KanbanPanel is the World Store task board overlay.
type KanbanPanel struct {
	visible bool
	width   int
	height  int

	env      *kanbanEnv
	setupErr string
	// context is the session selected when the board opened: the one `L`
	// links and whose path `n` launches in.
	context *kanbanSession
	lookup  func(id string) (kanbanSession, bool)

	board  *worldstore.Board
	cols   []worldstore.Column
	col    int
	rows   []int
	scroll []int

	seq     int
	loading bool
	busy    bool
	locked  bool
	notice  string
	err     string
	confirm *kanbanConfirm

	filter        string
	filterEditing bool
	// showAll switches from the execution-category view to every task.
	showAll bool
}

// NewKanbanPanel creates a hidden board.
func NewKanbanPanel() *KanbanPanel { return &KanbanPanel{} }

// IsVisible reports whether the board is shown.
func (p *KanbanPanel) IsVisible() bool { return p != nil && p.visible }

// SetSize records the terminal size.
func (p *KanbanPanel) SetSize(w, h int) {
	if p == nil {
		return
	}
	p.width, p.height = w, h
}

// Hide closes the board.
func (p *KanbanPanel) Hide() {
	if p == nil {
		return
	}
	p.visible = false
	p.confirm = nil
	p.filterEditing = false
}

// Open shows the board and starts a read. env is nil when World Store is
// not configured; setupErr then says why.
func (p *KanbanPanel) Open(env *kanbanEnv, setupErr string, ctx *kanbanSession, lookup func(string) (kanbanSession, bool)) tea.Cmd {
	p.visible = true
	p.env, p.setupErr, p.context, p.lookup = env, setupErr, ctx, lookup
	p.confirm, p.filterEditing, p.notice, p.err = nil, false, "", ""
	return p.reload()
}

func (p *KanbanPanel) reload() tea.Cmd {
	if p.env == nil || p.loading {
		return nil
	}
	p.loading = true
	p.seq++
	seq, client := p.seq, p.env.client
	return func() tea.Msg {
		board, err := client.Board(context.Background())
		return kanbanLoadedMsg{seq: seq, board: board, err: err}
	}
}

func (p *KanbanPanel) handleLoaded(msg kanbanLoadedMsg) {
	if msg.seq != p.seq {
		return
	}
	p.loading = false
	if msg.err != nil {
		p.err = kanbanErrorText(msg.err)
		if worldstore.CodeOf(msg.err) == worldstore.CodeOwnerChanged {
			p.board, p.cols = nil, nil
		}
		return
	}
	if p.board != nil && p.board.Owner != msg.board.Owner {
		p.notice = "World Store account or space changed; showing the current one"
	}
	selected := p.selectedID()
	p.board, p.locked, p.err = msg.board, false, ""
	p.rebuild(selected)
}

func (p *KanbanPanel) handleWritten(msg kanbanWrittenMsg) tea.Cmd {
	p.busy = false
	if msg.err != nil {
		p.err = kanbanErrorText(msg.err)
		if worldstore.Uncertain(msg.err) {
			p.locked = true
			return nil
		}
		// Nothing was written; show what World Store holds now.
		return p.reload()
	}
	p.notice = msg.notice
	return p.reload()
}

func kanbanErrorText(err error) string {
	switch worldstore.CodeOf(err) {
	case worldstore.CodeUnconfirmed, worldstore.CodeWriteUncertain:
		return "Write not confirmed — it may have been applied. Press r and check before doing anything else. (" + err.Error() + ")"
	case worldstore.CodeRedlineReview, worldstore.CodeRedlineUnavailable:
		return "Red lines must be reviewed before starting this task — start it in World Store. (" + err.Error() + ")"
	case worldstore.CodeBusy:
		return "World Store is saving this record elsewhere; nothing was written. Reloaded — try again."
	case worldstore.CodeOwnerChanged:
		return "World Store account or space changed; reloaded."
	case worldstore.CodeUnavailable:
		return "World Store unreachable — is its data service set up? (" + err.Error() + ")"
	}
	return err.Error()
}

// rebuild regroups the board and keeps the cursor on the same task.
func (p *KanbanPanel) rebuild(selectedID string) {
	if p.board == nil {
		p.cols = nil
		return
	}
	tasks := p.board.Tasks
	if !p.showAll && p.env != nil {
		tasks = nil
		for _, t := range p.board.Tasks {
			if worldstore.OnBoard(t, p.env.execCategory) {
				tasks = append(tasks, t)
			}
		}
	}
	p.cols = worldstore.Columns(tasks, p.filter)
	if len(p.rows) != len(p.cols) {
		p.rows, p.scroll = make([]int, len(p.cols)), make([]int, len(p.cols))
	}
	if p.col >= len(p.cols) {
		p.col = 0
	}
	for c := range p.cols {
		for i, t := range p.cols[c].Tasks {
			if t.ID == selectedID {
				p.col, p.rows[c] = c, i
			}
		}
		if p.rows[c] >= len(p.cols[c].Tasks) {
			p.rows[c] = max(0, len(p.cols[c].Tasks)-1)
		}
	}
	// A filter can empty the current column; land on the first one with tasks.
	// matrix/hardened: with nothing selected yet, start where the agents are:
	// 进行中, then 待验收, before the leftmost lanes.
	if selectedID == "" || len(p.cols[p.col].Tasks) == 0 {
		for _, lane := range []string{"doing", "review", ""} {
			found := false
			for c := range p.cols {
				if len(p.cols[c].Tasks) > 0 && (lane == "" || p.cols[c].Lane == lane) {
					p.col, found = c, true
					break
				}
			}
			if found {
				break
			}
		}
	}
}

func (p *KanbanPanel) selected() *worldstore.Task {
	if p.col < 0 || p.col >= len(p.cols) {
		return nil
	}
	ts := p.cols[p.col].Tasks
	if r := p.rows[p.col]; r >= 0 && r < len(ts) {
		return &ts[r]
	}
	return nil
}

func (p *KanbanPanel) selectedID() string {
	if t := p.selected(); t != nil {
		return t.ID
	}
	return ""
}

// Update handles keys and the panel's own async results.
func (p *KanbanPanel) Update(msg tea.Msg) (*KanbanPanel, tea.Cmd) {
	if p == nil || !p.visible {
		return p, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	k := key.String()

	if p.confirm != nil {
		// matrix/hardened: a rejection asks for its reason; q types into it.
		if p.confirm.kind == "reject" {
			switch {
			case k == "esc":
				p.confirm, p.notice = nil, "Cancelled; nothing written."
			case k == "enter":
				return p, p.execute()
			case k == "backspace":
				if r := []rune(p.confirm.reason); len(r) > 0 {
					p.confirm.reason = string(r[:len(r)-1])
				}
			case key.Type == tea.KeyRunes || key.Type == tea.KeySpace:
				text := string(key.Runes)
				if key.Type == tea.KeySpace {
					text = " "
				}
				if len([]rune(p.confirm.reason)) < 400 {
					p.confirm.reason += kanbanClean(text)
				}
			}
			return p, nil
		}
		switch k {
		case "esc", "q":
			p.confirm, p.notice = nil, "Cancelled; nothing written."
		case "enter":
			return p, p.execute()
		}
		return p, nil
	}

	if p.filterEditing {
		selected := p.selectedID()
		switch {
		case k == "esc":
			p.filter, p.filterEditing = "", false
		case k == "enter":
			p.filterEditing = false
		case k == "backspace":
			if r := []rune(p.filter); len(r) > 0 {
				p.filter = string(r[:len(r)-1])
			}
		case key.Type == tea.KeyRunes || key.Type == tea.KeySpace:
			text := string(key.Runes)
			if key.Type == tea.KeySpace {
				text = " "
			}
			if len([]rune(p.filter)) < 60 {
				p.filter += kanbanClean(text)
			}
		}
		p.rebuild(selected)
		return p, nil
	}

	p.notice = ""
	switch k {
	case "esc":
		if p.filter != "" {
			selected := p.selectedID()
			p.filter = ""
			p.rebuild(selected)
			return p, nil
		}
		p.Hide()
	case "q", "alt+t":
		p.Hide()
	case "h", "left":
		p.moveCol(-1)
	case "l", "right":
		p.moveCol(1)
	case "j", "down":
		p.moveRow(1)
	case "k", "up":
		p.moveRow(-1)
	case "g", "home":
		p.moveRow(-1 << 20)
	case "G", "end":
		p.moveRow(1 << 20)
	case "r":
		if p.busy {
			p.notice = "Waiting for the write to finish."
			return p, nil
		}
		p.err = ""
		return p, p.reload()
	case "/":
		p.filterEditing = true
	case "a":
		selected := p.selectedID()
		p.showAll = !p.showAll
		p.rebuild(selected)
	case "s", "p", "c", "x", "e", "L", "U", "n", "v", "b":
		p.startConfirm(k)
	case "o": // matrix/hardened: open the task's Multica issue
		p.openMultica()
	case "enter":
		return p, p.jump()
	}
	return p, nil
}

func (p *KanbanPanel) moveCol(d int) {
	if len(p.cols) == 0 {
		return
	}
	p.col = (p.col + d + len(p.cols)) % len(p.cols)
}

func (p *KanbanPanel) moveRow(d int) {
	if p.col >= len(p.cols) {
		return
	}
	n := len(p.cols[p.col].Tasks)
	p.rows[p.col] = max(0, min(n-1, p.rows[p.col]+d))
}

// startConfirm checks an action is allowed and opens its confirmation.
func (p *KanbanPanel) startConfirm(k string) {
	fail := func(s string) { p.notice = s }
	t := p.selected()
	switch {
	case p.env == nil:
		fail("World Store is not configured.")
		return
	case p.busy || p.loading:
		fail("Busy — wait for the current read or write.")
		return
	case p.locked:
		fail("An earlier write is unconfirmed. Press r and check the task first.")
		return
	case t == nil:
		fail("No task selected.")
		return
	case t.ReadOnly:
		fail("Imported task — read-only here; edit it in World Store.")
		return
	case p.board != nil && !p.board.CanWrite:
		fail("World Store reports this space as read-only.")
		return
	}
	closed := t.Status == worldstore.StatusDone || t.Status == worldstore.StatusCancelled
	c := &kanbanConfirm{task: *t}
	switch k {
	case "s":
		switch {
		case closed:
			fail("Task is already " + t.Status + ".")
			return
		case t.Execution != nil:
			fail("Task already has an open execution — p pauses, c resumes.")
			return
		case p.board.Running != nil:
			fail("Another task is running (" + p.runningName() + "); pause or finish it first.")
			return
		}
		c.kind, c.operationID = "start", worldstore.NewOperationID()
	case "p":
		if t.Execution == nil || t.Execution.Status != worldstore.ExecRunning {
			fail("Task is not running.")
			return
		}
		c.kind = "pause"
	case "c":
		switch {
		case t.Execution == nil || t.Execution.Status != worldstore.ExecPaused:
			fail("Task has no paused execution.")
			return
		case p.board.Running != nil && p.board.Running.ID != t.Execution.ID:
			fail("Another task is running (" + p.runningName() + "); pause or finish it first.")
			return
		}
		c.kind = "resume"
	case "e":
		if !p.board.ExecReady {
			fail("Execution category field not set up in World Store: run `agent-deck tasks init --yes` once.")
			return
		}
		c.kind = "mark"
		if t.ExecCategory == p.env.execCategory {
			c.kind = "unmark"
		}
	case "x":
		if closed || t.Status == worldstore.StatusCycle {
			fail("Finish " + t.Status + " tasks in World Store.")
			return
		}
		c.kind = "done"
	case "v":
		switch {
		case closed || t.Status == worldstore.StatusCycle:
			fail("Only open, non-recurring tasks go to review.")
			return
		case t.Status == worldstore.StatusReview && t.Execution == nil:
			fail("Task is already waiting for review.")
			return
		}
		c.kind = "review"
	case "b":
		if t.Status != worldstore.StatusReview || t.Execution != nil {
			fail("Only tasks waiting for review (待验收) can be sent back.")
			return
		}
		c.kind = "reject"
	case "L", "U":
		if !p.board.LinksReady {
			fail("agent-deck link field not set up in World Store: run `agent-deck tasks init --yes` once.")
			return
		}
		if p.context == nil {
			fail("Open the board with a session selected to link it.")
			return
		}
		if t.LinksUnknown {
			fail("This task's stored links are in an unknown format; check it in World Store.")
			return
		}
		linked := t.LinkedTo(p.env.device, p.context.ID)
		if k == "L" && linked {
			fail("Already linked to " + p.context.Title + ".")
			return
		}
		if k == "U" && !linked {
			fail(p.context.Title + " is not linked to this task.")
			return
		}
		c.kind = map[string]string{"L": "link", "U": "unlink"}[k]
	case "n":
		if t.Lane == "done" {
			fail("Task is already done.")
			return
		}
		c.kind = "launch"
	}
	p.confirm = c
}

func (p *KanbanPanel) confirmText() string {
	c := p.confirm
	name := kanbanClean(c.task.Name)
	switch c.kind {
	case "start":
		return fmt.Sprintf("Start %q (World Store creates a running execution)", name)
	case "mark":
		if c.task.ExecCategory != "" {
			return fmt.Sprintf("Set execution category of %q: %s → %s", name, c.task.ExecCategory, p.env.execCategory)
		}
		return fmt.Sprintf("Put %q on the board (执行分类 = %s)", name, p.env.execCategory)
	case "unmark":
		return fmt.Sprintf("Take %q off the board (clear 执行分类 %s)", name, p.env.execCategory)
	case "pause":
		return fmt.Sprintf("Pause the running execution of %q", name)
	case "resume":
		return fmt.Sprintf("Resume the paused execution of %q", name)
	case "done":
		if c.task.Status == worldstore.StatusReview {
			return fmt.Sprintf("Accept %q: mark it complete (records completion time)", name)
		}
		return fmt.Sprintf("Mark %q complete (records completion time, closes its executions)", name)
	case "review":
		if c.task.Execution != nil {
			return fmt.Sprintf("Hand %q over for review: finish its execution round, then set 待验收", name)
		}
		return fmt.Sprintf("Hand %q over for review (set 待验收)", name)
	case "reject":
		to := "linked sessions in this deck"
		if len(p.liveLinks(c.task)) == 0 {
			to = "no linked session here"
		}
		return fmt.Sprintf("Send %q back to 待办 · reason (to %s): %s▏", name, to, c.reason)
	case "link":
		return fmt.Sprintf("Link session %q to %q", p.context.Title, name)
	case "unlink":
		return fmt.Sprintf("Unlink session %q from %q", p.context.Title, name)
	case "launch":
		where := "the default path"
		if p.context != nil && p.context.Path != "" {
			where = p.context.Path
		}
		return fmt.Sprintf("Start a claude session on %q in %s and link it", name, where)
	}
	return ""
}

// execute runs the confirmed action in the background.
func (p *KanbanPanel) execute() tea.Cmd {
	c := p.confirm
	p.confirm = nil
	if c == nil || p.env == nil || p.board == nil || p.busy || p.locked {
		return nil
	}
	p.busy, p.err = true, ""
	// Captured here: the closure runs off the UI goroutine and must not touch p.
	env, owner, task, linksReady, sessions := *p.env, p.board.Owner, c.task, p.board.LinksReady, p.liveLinks(c.task)

	var ctxSession kanbanSession
	if p.context != nil {
		ctxSession = *p.context
	}
	return func() tea.Msg {
		bg := context.Background()
		name := kanbanClean(task.Name)
		done := func(err error, notice string) tea.Msg {
			if err != nil {
				return kanbanWrittenMsg{err: err}
			}
			// matrix/hardened: a decision on a dispatched task reaches Multica now.
			if task.Multica != nil && (c.kind == "done" || c.kind == "review") {
				env.client.TriggerMulticaSync()
			}
			return kanbanWrittenMsg{notice: notice}
		}
		switch c.kind {
		case "start":
			return done(env.client.Start(bg, owner, task, c.operationID), "Started "+name)
		case "mark":
			return done(env.client.SetExecCategory(bg, owner, task, env.execCategory), name+" → "+env.execCategory)
		case "unmark":
			return done(env.client.SetExecCategory(bg, owner, task, ""), name+" taken off the board")
		case "pause":
			return done(env.client.Pause(bg, owner, task), "Paused "+name)
		case "resume":
			return done(env.client.Resume(bg, owner, task), "Resumed "+name)
		case "done":
			return done(env.client.Complete(bg, owner, task, ""), name+" → "+worldstore.StatusDone)
		case "review":
			return done(env.client.SubmitForReview(bg, owner, task), name+" → "+worldstore.StatusReview)
		case "reject":
			if err := env.client.SetStatus(bg, owner, task, worldstore.StatusTodo); err != nil {
				return kanbanWrittenMsg{err: err}
			}
			if task.Multica != nil { // matrix/hardened: the send-back reaches Multica now
				env.client.TriggerMulticaSync()
			}
			reason := strings.TrimSpace(c.reason)
			if reason == "" || len(sessions) == 0 {
				return kanbanWrittenMsg{notice: name + " sent back to " + worldstore.StatusTodo}
			}
			ctx, cancel := context.WithTimeout(bg, time.Minute)
			defer cancel()
			text := "验收未通过，请继续修改：" + name + "\n原因：" + reason
			send := env.send
			if send == nil {
				send = func(ctx context.Context, id, text string) error {
					return worldstore.SendToSession(ctx, env.exe, env.profile, id, text)
				}
			}
			for _, id := range sessions {
				if err := send(ctx, id, text); err != nil {
					return kanbanWrittenMsg{err: fmt.Errorf("sent back to %s, but the reason was not queued: %w", worldstore.StatusTodo, err)}
				}
			}
			return kanbanWrittenMsg{notice: fmt.Sprintf("%s sent back; reason queued for %d session(s)", name, len(sessions))}
		case "link":
			if _, err := env.client.Link(bg, owner, task, env.device, ctxSession.ID); err != nil {
				return kanbanWrittenMsg{err: err}
			}
			return kanbanWrittenMsg{notice: "Linked " + ctxSession.Title}
		case "unlink":
			if _, err := env.client.Unlink(bg, owner, task, env.device, ctxSession.ID); err != nil {
				return kanbanWrittenMsg{err: err}
			}
			return kanbanWrittenMsg{notice: "Unlinked " + ctxSession.Title}
		case "launch":
			full, err := env.client.Get(bg, owner, task.ID)
			if err != nil {
				return kanbanWrittenMsg{err: err}
			}
			ctx, cancel := context.WithTimeout(bg, 5*time.Minute)
			defer cancel()
			id, err := worldstore.Launch(ctx, worldstore.LaunchRequest{
				Exe: env.exe, Profile: env.profile, Path: ctxSession.Path, Group: ctxSession.Group,
				Title: name, Prompt: worldstore.LaunchPrompt(*full), TaskID: task.ID, NoParent: true,
			})
			if err != nil {
				return kanbanWrittenMsg{err: err}
			}
			if !linksReady {
				return kanbanWrittenMsg{notice: "Started session " + id + " (not linked: run `agent-deck tasks init --yes` first)"}
			}
			if _, err := env.client.Link(bg, owner, task, env.device, id); err != nil {
				return kanbanWrittenMsg{err: fmt.Errorf("session %s started but not linked: %w", id, err)}
			}
			return kanbanWrittenMsg{notice: "Started and linked session " + id}
		}
		return kanbanWrittenMsg{}
	}
}

// openMultica opens the selected task's Multica issue in the browser.
func (p *KanbanPanel) openMultica() {
	t := p.selected()
	switch {
	case t == nil:
		p.notice = "No task selected."
		return
	case t.Multica == nil:
		p.notice = "This task has not been dispatched to Multica."
		return
	case !strings.HasPrefix(t.Multica.Root.URL, "http://") && !strings.HasPrefix(t.Multica.Root.URL, "https://"):
		p.notice = "World Store did not record a web address for " + t.Multica.Root.Identifier + "."
		return
	}
	open := p.env.open
	if open == nil {
		open = func(url string) error {
			name := "xdg-open"
			if runtime.GOOS == "darwin" {
				name = "open"
			}
			return exec.Command(name, url).Start()
		}
	}
	if err := open(t.Multica.Root.URL); err != nil {
		p.err = "Could not open the browser: " + err.Error()
		return
	}
	p.notice = "Opened " + t.Multica.Root.Identifier + " in the browser."
}

// liveLinks are the task's linked sessions that exist in this deck.
func (p *KanbanPanel) liveLinks(t worldstore.Task) []string {
	var ids []string
	if p.env == nil || p.lookup == nil {
		return nil
	}
	for _, l := range t.AgentDeckLinks {
		if l.DeviceID != p.env.device {
			continue
		}
		if s, ok := p.lookup(l.SessionID); ok {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// jump returns to the first linked session that exists in this deck.
func (p *KanbanPanel) jump() tea.Cmd {
	t := p.selected()
	if t == nil || p.env == nil || p.lookup == nil {
		return nil
	}
	for _, l := range t.AgentDeckLinks {
		if l.DeviceID != p.env.device {
			continue
		}
		if s, ok := p.lookup(l.SessionID); ok {
			id := s.ID
			return func() tea.Msg { return kanbanJumpMsg{sessionID: id} }
		}
	}
	if len(t.AgentDeckLinks) > 0 {
		p.notice = "Linked sessions are not in this deck (another machine, profile, or deleted)."
	} else {
		p.notice = "No linked session. L links the current session, n starts one."
	}
	return nil
}

// kanbanClean drops control characters so stored text cannot move the cursor.
func kanbanClean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func kanbanFit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(kanbanClean(s), w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

func kanbanStatusColor(st session.Status) lipgloss.Color {
	switch st {
	case session.StatusRunning, session.StatusStarting:
		return ColorGreen
	case session.StatusWaiting:
		return ColorYellow
	case session.StatusError:
		return ColorRed
	}
	return ColorTextDim
}

// linkMarker is the card's session dot: the liveliest linked session's color,
// hollow when no linked session is in this deck.
func (p *KanbanPanel) linkMarker(t worldstore.Task) (string, lipgloss.Color) {
	if t.LinksUnknown {
		return "?", ColorRed
	}
	if len(t.AgentDeckLinks) == 0 {
		return " ", ColorTextDim
	}
	rank := map[session.Status]int{session.StatusRunning: 4, session.StatusStarting: 4, session.StatusWaiting: 3, session.StatusError: 2}
	best, found := session.Status(""), false
	for _, l := range t.AgentDeckLinks {
		if p.env == nil || l.DeviceID != p.env.device || p.lookup == nil {
			continue
		}
		if s, ok := p.lookup(l.SessionID); ok {
			if !found || rank[s.Status] > rank[best] {
				best = s.Status
			}
			found = true
		}
	}
	if !found {
		return "○", ColorTextDim
	}
	return "●", kanbanStatusColor(best)
}

// View renders the board.
func (p *KanbanPanel) View() string {
	if p == nil || !p.visible {
		return ""
	}
	width := max(40, p.width-2)
	inner := width - 2
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ColorBorder).Padding(0, 1).Width(width)
	title := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	red := lipgloss.NewStyle().Foreground(ColorRed)
	rule := dim.Render(strings.Repeat("─", inner))

	var sb strings.Builder
	head := "World Store"
	if p.board != nil {
		shown := 0
		for _, c := range p.cols {
			shown += len(c.Tasks) + c.Hidden
		}
		if p.showAll {
			head += fmt.Sprintf(" · all tasks (%d)", shown)
		} else if p.env != nil {
			head += fmt.Sprintf(" · %s (%d of %d)", p.env.execCategory, shown, len(p.board.Tasks))
		}
	}
	if p.board != nil && !p.board.BlockersKnown {
		head += " · blockers not read (卡住 unknown)"
	}
	if p.context != nil {
		head += " · session: " + kanbanClean(p.context.Title)
	}
	if p.loading {
		head += " · reading…"
	}
	if p.busy {
		head += " · writing…"
	}
	sb.WriteString(title.Render("TASKS") + "  " + dim.Render(kanbanFit(head, inner-7)) + "\n" + rule + "\n")

	if p.env == nil {
		sb.WriteString(red.Render("World Store is not configured.") + "\n")
		for _, line := range strings.Split(p.setupErr, "\n") {
			sb.WriteString(dim.Render(kanbanFit(line, inner)) + "\n")
		}
		sb.WriteString(rule + "\n" + dim.Render("[Esc] Close"))
		return box.Render(sb.String())
	}

	// Rows: border 2, header 2, column titles 1, rule 1, detail 3, rule 1, footer 2.
	bodyH := max(3, p.height-12)
	empty := true
	for _, c := range p.cols {
		empty = empty && len(c.Tasks) == 0
	}
	switch {
	case len(p.cols) == 0:
		msg := "Reading World Store…"
		if !p.loading {
			msg = "No board loaded. Press r to read."
		}
		sb.WriteString(dim.Render(msg) + "\n" + strings.Repeat("\n", bodyH))
	case empty && !p.showAll && p.filter == "":
		// One header row plus bodyH rows, like the columns it replaces.
		sb.WriteString(dim.Render(kanbanFit("No tasks have 执行分类 = "+p.env.execCategory+" yet.", inner)) + "\n")
		sb.WriteString(dim.Render(kanbanFit("Press a to see all tasks, pick one, press e to put it on the board.", inner)) + "\n")
		sb.WriteString(strings.Repeat("\n", bodyH-1))
	default:
		sb.WriteString(p.renderColumns(inner, bodyH))
	}
	sb.WriteString(rule + "\n")
	sb.WriteString(p.renderDetail(inner))
	sb.WriteString(rule + "\n")
	sb.WriteString(p.renderFooter(inner))
	return box.Render(sb.String())
}

func (p *KanbanPanel) renderColumns(inner, bodyH int) string {
	n := len(p.cols)
	colW := (inner - (n - 1)) / n
	narrow := colW < 16
	shown := make([]int, 0, n)
	if narrow {
		colW = inner
		shown = append(shown, p.col)
	} else {
		for c := range p.cols {
			shown = append(shown, c)
		}
	}
	sep := lipgloss.NewStyle().Foreground(ColorBorder).Render("│")
	headActive := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	headIdle := lipgloss.NewStyle().Bold(true).Foreground(ColorText)
	sel := lipgloss.NewStyle().Background(ColorSurface).Foreground(ColorText).Bold(true)
	normal := lipgloss.NewStyle().Foreground(ColorText)
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)

	var sb strings.Builder
	if narrow {
		var tabs []string
		for c, col := range p.cols {
			label := fmt.Sprintf("%s %d", col.Title, len(col.Tasks)+col.Hidden)
			if c == p.col {
				tabs = append(tabs, headActive.Render("["+label+"]"))
			} else {
				tabs = append(tabs, dim.Render(label))
			}
		}
		sb.WriteString(strings.Join(tabs, " ") + "\n")
	} else {
		var heads []string
		for _, c := range shown {
			col := p.cols[c]
			style := headIdle
			if c == p.col {
				style = headActive
			}
			heads = append(heads, style.Render(kanbanFit(fmt.Sprintf("%s (%d)", col.Title, len(col.Tasks)+col.Hidden), colW)))
		}
		sb.WriteString(strings.Join(heads, sep) + "\n")
	}

	for _, c := range shown {
		r, s := p.rows[c], p.scroll[c]
		if r < s {
			s = r
		}
		if r >= s+bodyH {
			s = r - bodyH + 1
		}
		p.scroll[c] = max(0, s)
	}
	for line := 0; line < bodyH; line++ {
		var cells []string
		for _, c := range shown {
			col := p.cols[c]
			i := p.scroll[c] + line
			switch {
			case i < len(col.Tasks):
				t := col.Tasks[i]
				mark, color := p.linkMarker(t)
				label := t.Name
				if t.Execution != nil && t.Execution.Status == worldstore.ExecPaused {
					label = "‖ " + label
				}
				if t.Multica != nil {
					label += " ·" + t.Multica.Summary()
				}
				text := kanbanFit(label, colW-2)
				style, cursor := normal, " "
				if c == p.col && i == p.rows[c] {
					// The glyph keeps the cursor visible without colors.
					style, cursor = sel, "▸"
				}
				cells = append(cells, lipgloss.NewStyle().Foreground(color).Render(mark)+style.Render(cursor+text))
			case i == len(col.Tasks) && col.Hidden > 0:
				cells = append(cells, dim.Render(kanbanFit(fmt.Sprintf("  … %d older", col.Hidden), colW)))
			default:
				cells = append(cells, strings.Repeat(" ", colW))
			}
		}
		sb.WriteString(strings.Join(cells, sep) + "\n")
	}
	return sb.String()
}

func (p *KanbanPanel) renderDetail(inner int) string {
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	t := p.selected()
	if t == nil {
		return "\n\n\n"
	}
	meta := []string{t.Status}
	if t.Category != "" {
		meta = append(meta, t.Category)
	}
	if t.PlanDate != nil {
		meta = append(meta, "计划 "+time.Unix(*t.PlanDate, 0).Local().Format("2006-01-02"))
	}
	if t.CompletedAt != nil {
		meta = append(meta, "完成 "+time.Unix(*t.CompletedAt, 0).Local().Format("2006-01-02"))
	}
	if e := t.Execution; e != nil {
		label := "执行" + e.Status
		if e.StartedAt > 0 {
			label += " 自 " + time.Unix(e.StartedAt, 0).Local().Format("01-02 15:04")
		}
		meta = append(meta, label)
	}
	if t.ExecCategory != "" {
		meta = append(meta, "执行分类 "+t.ExecCategory)
	}
	if t.Multica != nil {
		meta = append(meta, "Multica "+t.Multica.Summary())
	}
	if t.ReadOnly {
		meta = append(meta, "read-only")
	}
	var sessions []string
	switch {
	case t.LinksUnknown:
		sessions = append(sessions, "links: unknown format")
	default:
		for _, l := range t.AgentDeckLinks {
			label := l.SessionID
			if p.env != nil && l.DeviceID == p.env.device && p.lookup != nil {
				if s, ok := p.lookup(l.SessionID); ok {
					label = kanbanClean(s.Title) + " (" + string(s.Status) + ")"
				} else {
					label += " (not in deck)"
				}
			} else {
				label += " @" + l.DeviceID
			}
			sessions = append(sessions, label)
		}
	}
	line3 := "sessions: none"
	if len(sessions) > 0 {
		line3 = "sessions: " + strings.Join(sessions, ", ")
	}
	return lipgloss.NewStyle().Bold(true).Foreground(ColorText).Render(kanbanFit(t.Name, inner)) + "\n" +
		dim.Render(kanbanFit(strings.Join(meta, " · "), inner)) + "\n" +
		dim.Render(kanbanFit(line3, inner)) + "\n"
}

func (p *KanbanPanel) renderFooter(inner int) string {
	dim := lipgloss.NewStyle().Foreground(ColorTextDim)
	accent := lipgloss.NewStyle().Foreground(ColorAccent).Bold(true)
	red := lipgloss.NewStyle().Foreground(ColorRed)
	var status string
	switch {
	case p.confirm != nil:
		status = accent.Render(kanbanFit(p.confirmText(), inner))
	case p.filterEditing:
		status = accent.Render(kanbanFit("Filter: "+p.filter+"▏", inner))
	case p.err != "":
		status = red.Render(kanbanFit(p.err, inner))
	case p.notice != "":
		status = accent.Render(kanbanFit(p.notice, inner))
	case p.filter != "":
		status = dim.Render(kanbanFit("Filter: "+p.filter+"  (Esc clears)", inner))
	default:
		status = ""
	}
	keys := "[a] All/AI  [e] On/off board  [s] Start  [p] Pause  [c] Resume  [v] To review  [x] Done/Accept  [b] Send back  [o] Open issue  [L/U] Link  [n] New session  [Enter] Go to session  [/] Filter  [r] Reload  [Esc] Close"
	if p.confirm != nil && p.confirm.kind == "reject" {
		keys = "Type the reason (optional)  [Enter] Send back  [Esc] Cancel"
	} else if p.confirm != nil {
		keys = "[Enter] Confirm  [Esc] Cancel"
	} else if p.filterEditing {
		keys = "[Enter] Keep filter  [Esc] Clear"
	}
	return status + "\n" + dim.Render(kanbanFit(keys, inner))
}

// runningName names the task whose execution is running, for notices.
func (p *KanbanPanel) runningName() string {
	if p.board == nil || p.board.Running == nil {
		return ""
	}
	if t := p.board.Find(p.board.RunningTaskID); t != nil {
		return kanbanClean(t.Name)
	}
	return "an execution"
}
