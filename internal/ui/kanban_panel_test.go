package ui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/worldstore"
)

// matrix/hardened: World Store task board panel.

type kanbanFakeEntry struct {
	ops    []map[string]any
	answer func(req map[string]any) string
}

func (f *kanbanFakeEntry) env() *kanbanEnv {
	run := func(_ context.Context, stdin []byte) ([]byte, error) {
		var req map[string]any
		_ = json.Unmarshal(stdin, &req)
		f.ops = append(f.ops, req)
		return []byte(f.answer(req)), nil
	}
	return &kanbanEnv{client: worldstore.NewWithRunner(run), device: "host", execCategory: "AI"}
}

func (f *kanbanFakeEntry) last() map[string]any { return f.ops[len(f.ops)-1] }

// t2 is running (linked to sess-1), t4 is paused, t1/t3 are to-do.
const kanbanSnapshot = `{"ok":true,"result":{"accountId":"a","spaceId":"s","pending":false,"tuiCanWrite":true,
 "availableFields":["任务｜agent-deck 会话关联","任务｜执行分类"],
 "groups":{"tasks":[
  {"id":"t1","name":"写周报","fields":{"任务｜状态":"待办","任务｜执行分类":"AI"}},
  {"id":"t2","name":"修复登录","fields":{"任务｜状态":"待办","任务｜agent-deck 会话关联":"{\"version\":1,\"links\":[{\"source\":\"agent-deck\",\"deviceId\":\"host\",\"sessionId\":\"sess-1\",\"linkedAt\":1}]}"}},
  {"id":"t3","name":"历史导入","imported":true,"fields":{"任务｜状态":"待办"}},
  {"id":"t4","name":"整理行李","fields":{"任务｜状态":"待办"}}
 ],"executions":[
  {"id":"e1","fields":{"执行｜状态":"进行中","执行｜关联任务":["t2"],"执行｜开始时间":1791500000}},
  {"id":"e4","fields":{"执行｜状态":"暂停","执行｜关联任务":["t4"],"执行｜开始时间":1791400000}}
 ]}}}`

func kanbanEntry() *kanbanFakeEntry {
	return &kanbanFakeEntry{answer: func(req map[string]any) string {
		if req["op"] == "snapshot" {
			return kanbanSnapshot
		}
		return `{"ok":true,"result":{"links":[]}}`
	}}
}

func kanbanKey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// openKanban opens a panel on the fake entry in the all-tasks view and
// delivers the first read.
func openKanban(t *testing.T, f *kanbanFakeEntry, ctx *kanbanSession) *KanbanPanel {
	t.Helper()
	p := NewKanbanPanel()
	p.showAll = true
	p.SetSize(140, 30)
	lookup := func(id string) (kanbanSession, bool) {
		if id == "sess-1" {
			return kanbanSession{ID: id, Title: "login-fix", Status: session.StatusWaiting}, true
		}
		return kanbanSession{}, false
	}
	cmd := p.Open(f.env(), "", ctx, lookup)
	p.handleLoaded(cmd().(kanbanLoadedMsg))
	if p.board == nil {
		t.Fatalf("board not loaded: %s", p.err)
	}
	return p
}

// selectTask moves the cursor onto task id.
func selectTask(t *testing.T, p *KanbanPanel, id string) {
	t.Helper()
	for c := range p.cols {
		for i, task := range p.cols[c].Tasks {
			if task.ID == id {
				p.col, p.rows[c] = c, i
				return
			}
		}
	}
	t.Fatalf("task %s not on board", id)
}

func TestKanbanBoardRendersColumnsWithinTerminal(t *testing.T) {
	p := openKanban(t, kanbanEntry(), nil)
	view := p.View()
	for _, want := range []string{"进行中 (2)", "待办 (2)", "待验收 (0)", "卡住 (0)", "‖ 整理行李", "修复登录", "写周报", "login-fix (waiting)", "执行进行中"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	lines := strings.Split(view, "\n")
	if len(lines) != 30 {
		t.Fatalf("view is %d lines, terminal is 30", len(lines))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 140 {
			t.Fatalf("line wider than terminal (%d): %q", w, l)
		}
	}
	p.SetSize(60, 20)
	if v := p.View(); !strings.Contains(v, "[进行中 2]") || strings.Count(v, "\n") != 19 {
		t.Fatalf("narrow view:\n%s", v)
	}
}

func TestKanbanStartNeedsConfirmationAndUsesOneOperationID(t *testing.T) {
	f := kanbanEntry()
	f.answer = func(req map[string]any) string {
		if req["op"] == "snapshot" {
			// Nothing running, so a start is allowed.
			return strings.Replace(kanbanSnapshot, `"执行｜状态":"进行中"`, `"执行｜状态":"已完成"`, 1)
		}
		return `{"ok":true,"result":{"verified":true}}`
	}
	p := openKanban(t, f, nil)
	selectTask(t, p, "t1")
	p.Update(kanbanKey("s"))
	if p.confirm == nil || p.confirm.kind != "start" || p.confirm.operationID == "" {
		t.Fatalf("s must open a start confirmation: %+v / %q", p.confirm, p.notice)
	}
	p.Update(kanbanKey("esc"))
	if p.confirm != nil || len(f.ops) != 1 {
		t.Fatalf("esc must cancel without writing: %v", f.ops)
	}
	p.Update(kanbanKey("s"))
	opID := p.confirm.operationID
	_, cmd := p.Update(kanbanKey("enter"))
	written := cmd().(kanbanWrittenMsg)
	payload := f.last()["payload"].(map[string]any)
	if written.err != nil || payload["mode"] != "start" || payload["operationId"] != opID || payload["taskId"] != "t1" {
		t.Fatalf("written=%+v payload=%v", written, payload)
	}
	if p.handleWritten(written) == nil {
		t.Fatal("a confirmed write must be followed by a fresh read")
	}
}

func TestKanbanPauseResumeTargetTheExecution(t *testing.T) {
	f := kanbanEntry()
	p := openKanban(t, f, nil)
	selectTask(t, p, "t2")
	p.Update(kanbanKey("p"))
	_, cmd := p.Update(kanbanKey("enter"))
	p.handleWritten(cmd().(kanbanWrittenMsg))
	if pl := f.last()["payload"].(map[string]any); pl["mode"] != "pause" || pl["executionId"] != "e1" {
		t.Fatalf("pause payload = %v", pl)
	}
	// Resuming t4 while t2 is still running is refused before sending.
	p.loading = false
	selectTask(t, p, "t4")
	before := len(f.ops)
	p.Update(kanbanKey("c"))
	if p.confirm != nil || !strings.Contains(p.notice, "Another task is running") || len(f.ops) != before {
		t.Fatalf("resume over a running task allowed: %q", p.notice)
	}
	// Starting anything while t2 runs is refused too.
	selectTask(t, p, "t1")
	p.Update(kanbanKey("s"))
	if p.confirm != nil {
		t.Fatal("start while another task runs allowed")
	}
}

func TestKanbanRedlineRefusalExplainsAndDoesNotLock(t *testing.T) {
	f := kanbanEntry()
	p := openKanban(t, f, nil)
	f.answer = func(req map[string]any) string {
		if req["op"] == "snapshot" {
			return kanbanSnapshot
		}
		return `{"ok":false,"error":{"code":"REDLINE_REVIEW_REQUIRED","message":"关联红线尚未核对"}}`
	}
	selectTask(t, p, "t2")
	p.Update(kanbanKey("p"))
	_, cmd := p.Update(kanbanKey("enter"))
	if follow := p.handleWritten(cmd().(kanbanWrittenMsg)); follow == nil || p.locked || !strings.Contains(p.err, "World Store") {
		t.Fatalf("redline refusal: locked=%v err=%q", p.locked, p.err)
	}
}

func TestKanbanUncertainWriteLocksUntilReread(t *testing.T) {
	f := kanbanEntry()
	p := openKanban(t, f, &kanbanSession{ID: "sess-9", Title: "other"})
	f.answer = func(req map[string]any) string {
		if req["op"] == "snapshot" {
			return kanbanSnapshot
		}
		return `{"ok":false,"error":{"code":"UNCONFIRMED","message":"写入可能已发出"}}`
	}
	selectTask(t, p, "t1")
	p.Update(kanbanKey("L"))
	_, cmd := p.Update(kanbanKey("enter"))
	if follow := p.handleWritten(cmd().(kanbanWrittenMsg)); follow != nil {
		t.Fatal("an uncertain write must not trigger anything automatically")
	}
	if !p.locked {
		t.Fatal("uncertain write must lock")
	}
	p.Update(kanbanKey("x"))
	if p.confirm != nil {
		t.Fatal("writes must stay locked until a fresh read")
	}
	_, cmd = p.Update(kanbanKey("r"))
	p.handleLoaded(cmd().(kanbanLoadedMsg))
	if p.locked {
		t.Fatal("a fresh read must unlock")
	}
}

func TestKanbanRefusesReadOnlyAndUnreadyLinksWithoutSending(t *testing.T) {
	f := kanbanEntry()
	f.answer = func(req map[string]any) string {
		return strings.Replace(kanbanSnapshot, `"availableFields":["任务｜agent-deck 会话关联","任务｜执行分类"]`, `"availableFields":["任务｜执行分类"]`, 1)
	}
	p := openKanban(t, f, &kanbanSession{ID: "sess-9", Title: "other"})
	selectTask(t, p, "t3")
	p.Update(kanbanKey("x"))
	if p.confirm != nil || !strings.Contains(p.notice, "read-only") {
		t.Fatalf("read-only complete allowed: %q", p.notice)
	}
	selectTask(t, p, "t1")
	p.Update(kanbanKey("L"))
	if p.confirm != nil || !strings.Contains(p.notice, "tasks init --yes") {
		t.Fatalf("link without the relation allowed: %q", p.notice)
	}
	if len(f.ops) != 1 {
		t.Fatalf("entry called: %v", f.ops)
	}
}

func TestKanbanEnterJumpsToLinkedSession(t *testing.T) {
	p := openKanban(t, kanbanEntry(), nil)
	selectTask(t, p, "t2")
	_, cmd := p.Update(kanbanKey("enter"))
	if cmd == nil {
		t.Fatal("no jump")
	}
	if got := cmd().(kanbanJumpMsg).sessionID; got != "sess-1" {
		t.Fatalf("jump to %s", got)
	}
	if mark, _ := p.linkMarker(*p.selected()); mark != "●" {
		t.Fatalf("live linked session marker = %q", mark)
	}
}

func TestKanbanFilterNarrowsColumns(t *testing.T) {
	p := openKanban(t, kanbanEntry(), nil)
	p.Update(kanbanKey("/"))
	for _, r := range "周报" {
		p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	p.Update(kanbanKey("enter"))
	total := 0
	for _, c := range p.cols {
		total += len(c.Tasks)
	}
	if total != 1 || p.selected().ID != "t1" {
		t.Fatalf("filter left %d tasks, selected %+v", total, p.selected())
	}
	p.Update(kanbanKey("esc"))
	if p.filter != "" || !p.IsVisible() {
		t.Fatal("first esc clears the filter and keeps the board open")
	}
}

func TestKanbanUnconfiguredShowsSetupHelp(t *testing.T) {
	p := NewKanbanPanel()
	p.SetSize(100, 20)
	if cmd := p.Open(nil, worldstore.ErrNotConfigured.Error(), nil, nil); cmd != nil {
		t.Fatal("unconfigured board must not try to read")
	}
	if v := p.View(); !strings.Contains(v, "not configured") {
		t.Fatalf("view:\n%s", v)
	}
}

func TestKanbanDefaultViewShowsAITasksAndLinkedOnes(t *testing.T) {
	f := kanbanEntry()
	p := NewKanbanPanel()
	p.SetSize(140, 30)
	p.handleLoaded(p.Open(f.env(), "", nil, nil)().(kanbanLoadedMsg))
	ids := map[string]bool{}
	for _, c := range p.cols {
		for _, task := range c.Tasks {
			ids[task.ID] = true
		}
	}
	// t1 is marked AI, t2 has a linked session; t3/t4 are neither.
	if !ids["t1"] || !ids["t2"] || ids["t3"] || ids["t4"] {
		t.Fatalf("default view = %v", ids)
	}
	if v := p.View(); !strings.Contains(v, "AI (2 of 4)") {
		t.Fatalf("header:\n%s", v)
	}
	p.Update(kanbanKey("a"))
	if v := p.View(); !strings.Contains(v, "all tasks (4)") {
		t.Fatalf("a must switch to all tasks:\n%s", v)
	}
}

func TestKanbanEmptyAIViewExplainsHowToAdd(t *testing.T) {
	f := kanbanEntry()
	f.answer = func(map[string]any) string {
		s := strings.Replace(kanbanSnapshot, `,"任务｜执行分类":"AI"`, "", 1)
		return strings.Replace(s, `"任务｜agent-deck 会话关联":"{`, `"x":"{`, 1)
	}
	p := NewKanbanPanel()
	p.SetSize(120, 24)
	p.handleLoaded(p.Open(f.env(), "", nil, nil)().(kanbanLoadedMsg))
	v := p.View()
	if !strings.Contains(v, "No tasks have 执行分类 = AI yet") || strings.Count(v, "\n") != 23 {
		t.Fatalf("empty view:\n%s", v)
	}
}

func TestKanbanMarkAndUnmarkQuoteTheReadValue(t *testing.T) {
	f := kanbanEntry()
	p := openKanban(t, f, nil)
	selectTask(t, p, "t4")
	p.Update(kanbanKey("e"))
	if p.confirm == nil || p.confirm.kind != "mark" {
		t.Fatalf("e on an unmarked task must offer mark: %+v %q", p.confirm, p.notice)
	}
	_, cmd := p.Update(kanbanKey("enter"))
	p.handleWritten(cmd().(kanbanWrittenMsg))
	req := f.last()
	patch, expected := req["patch"].(map[string]any), req["expected"].(map[string]any)
	if req["op"] != "task.update" || req["id"] != "t4" || patch["任务｜执行分类"] != "AI" || expected["任务｜执行分类"] != "" {
		t.Fatalf("mark request = %v", req)
	}
	p.loading = false
	selectTask(t, p, "t1")
	p.Update(kanbanKey("e"))
	if p.confirm == nil || p.confirm.kind != "unmark" {
		t.Fatalf("e on a marked task must offer unmark: %+v", p.confirm)
	}
	_, cmd = p.Update(kanbanKey("enter"))
	cmd()
	req = f.last()
	if req["patch"].(map[string]any)["任务｜执行分类"] != "" || req["expected"].(map[string]any)["任务｜执行分类"] != "AI" {
		t.Fatalf("unmark request = %v", req)
	}
}

func TestKanbanMarkNeedsTheField(t *testing.T) {
	f := kanbanEntry()
	f.answer = func(map[string]any) string {
		return strings.Replace(kanbanSnapshot, `,"任务｜执行分类"]`, `]`, 1)
	}
	p := openKanban(t, f, nil)
	selectTask(t, p, "t4")
	p.Update(kanbanKey("e"))
	if p.confirm != nil || !strings.Contains(p.notice, "tasks init --yes") {
		t.Fatalf("mark without the field allowed: %q", p.notice)
	}
}

// matrix/hardened: v hands over for review, x accepts, b sends back with a
// reason that is queued for the linked session.
func TestKanbanReviewAcceptAndSendBack(t *testing.T) {
	f := kanbanEntry()
	review := strings.Replace(kanbanSnapshot, `"name":"修复登录","fields":{"任务｜状态":"待办"`, `"name":"修复登录","fields":{"任务｜状态":"待验收"`, 1)
	review = strings.Replace(review, `"执行｜状态":"进行中"`, `"执行｜状态":"已完成"`, 1)
	f.answer = func(req map[string]any) string {
		if req["op"] == "snapshot" {
			return review
		}
		return `{"ok":true,"result":{"verified":true}}`
	}
	p := openKanban(t, f, nil)
	var sent []string
	p.env.send = func(_ context.Context, id, text string) error { sent = append(sent, id+"|"+text); return nil }
	if col := p.cols[p.col]; col.Lane != "doing" {
		t.Fatalf("the board opens on 进行中 when it has tasks, got %s", col.Lane)
	}

	selectTask(t, p, "t2")
	if p.cols[p.col].Lane != "review" {
		t.Fatalf("待验收 task in lane %s", p.cols[p.col].Lane)
	}
	p.Update(kanbanKey("x"))
	if p.confirm == nil || !strings.Contains(p.confirmText(), "Accept") {
		t.Fatalf("x on a review card accepts: %+v", p.confirm)
	}
	p.Update(kanbanKey("esc"))

	p.Update(kanbanKey("b"))
	for _, r := range "缺 q 测试" {
		p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if p.confirm == nil || p.confirm.reason != "缺 q 测试" {
		t.Fatalf("q must type into the reason, not cancel: %+v", p.confirm)
	}
	_, cmd := p.Update(kanbanKey("enter"))
	written := cmd().(kanbanWrittenMsg)
	if written.err != nil || f.last()["op"] != "task.update" || f.last()["patch"].(map[string]any)["任务｜状态"] != "待办" {
		t.Fatalf("send back: %+v %v", written, f.last())
	}
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "sess-1|验收未通过") || !strings.Contains(sent[0], "缺 q 测试") {
		t.Fatalf("reason must be queued for the linked session: %v", sent)
	}
	p.handleWritten(written)

	p.loading = false
	selectTask(t, p, "t1")
	p.Update(kanbanKey("b"))
	if p.confirm != nil {
		t.Fatal("only 待验收 tasks can be sent back")
	}
	p.Update(kanbanKey("v"))
	_, cmd = p.Update(kanbanKey("enter"))
	if w := cmd().(kanbanWrittenMsg); w.err != nil || f.last()["patch"].(map[string]any)["任务｜状态"] != "待验收" {
		t.Fatalf("v: %+v %v", w, f.last())
	}
}

// matrix/hardened: o opens the dispatched task's Multica issue.
func TestKanbanOpensMulticaIssue(t *testing.T) {
	f := kanbanEntry()
	link := `{\"v\":1,\"mode\":\"single\",\"workspaceId\":\"w\",\"root\":{\"id\":\"r\",\"identifier\":\"MC-3\",\"status\":\"in_progress\",\"url\":\"http://h/matrix/issues/r\"},\"children\":[],\"taskStatus\":\"待办\",\"syncedAt\":1}`
	snap := strings.Replace(kanbanSnapshot, `"name":"写周报","fields":{"任务｜状态":"待办"`, `"name":"写周报","fields":{"任务｜状态":"待办","任务｜Multica 关联":"`+link+`"`, 1)
	f.answer = func(req map[string]any) string { return snap }
	p := openKanban(t, f, nil)
	var opened []string
	p.env.open = func(url string) error { opened = append(opened, url); return nil }
	selectTask(t, p, "t1")
	if p.cols[p.col].Lane != "doing" {
		t.Fatalf("an in-progress issue puts the card in 进行中, got %s", p.cols[p.col].Lane)
	}
	p.Update(kanbanKey("o"))
	if len(opened) != 1 || opened[0] != "http://h/matrix/issues/r" || !strings.Contains(p.notice, "MC-3") {
		t.Fatalf("opened %v notice %q", opened, p.notice)
	}
	selectTask(t, p, "t4")
	p.Update(kanbanKey("o"))
	if len(opened) != 1 || !strings.Contains(p.notice, "not been dispatched") {
		t.Fatalf("undispatched: %v %q", opened, p.notice)
	}
}
