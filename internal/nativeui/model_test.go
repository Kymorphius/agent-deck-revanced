// matrix/hardened: exercise keyboard actions through the real service boundary.
package nativeui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type mutation struct{ id, runID string }

type fakeService struct {
	mu        sync.Mutex
	items     []terminalhost.SessionInfo
	lists     int
	creates   []terminalhost.SessionInfo
	starts    []string
	resumes   []mutation
	stops     []mutation
	removes   []mutation
	moves     []struct{ id, runID, group string }
	renames   []struct{ id, name string }
	attaches  []terminalhost.SessionInfo
	createErr error
	startErr  error
	resumeErr error
	listErr   error
	autoStart bool
	attach    func() *exec.Cmd
}

func (s *fakeService) List(context.Context) ([]terminalhost.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	return append([]terminalhost.SessionInfo(nil), s.items...), s.listErr
}

func (s *fakeService) Create(_ context.Context, name, tool, dir string) (terminalhost.SessionInfo, error) {
	return s.create(name, tool, dir, "")
}

func (s *fakeService) CreateInGroup(_ context.Context, name, tool, dir, group string) (terminalhost.SessionInfo, error) {
	return s.create(name, tool, dir, group)
}

func (s *fakeService) create(name, tool, dir, group string) (terminalhost.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := terminalhost.SessionInfo{ID: fmt.Sprintf("new-%d", len(s.creates)), Name: name, Tool: tool, Dir: dir, Group: group, State: "stopped"}
	if s.autoStart {
		info.State, info.RunID = "running", "created-run"
	}
	s.creates = append(s.creates, info)
	s.items = append(s.items, info)
	return info, s.createErr
}

func (s *fakeService) Move(_ context.Context, id, runID, group string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id && s.items[i].RunID == runID {
			s.moves = append(s.moves, struct{ id, runID, group string }{id, runID, group})
			s.items[i].Group = group
			return nil
		}
	}
	return errors.New("stale instance")
}

// matrix/hardened: model the service-side fence after the UI's List check.
type restartBeforeMoveService struct {
	*fakeService
	attempts []mutation
}

func (s *restartBeforeMoveService) Move(ctx context.Context, id, runID, group string) error {
	s.attempts = append(s.attempts, mutation{id, runID})
	s.mu.Lock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].RunID = "restarted-after-check"
		}
	}
	s.mu.Unlock()
	return s.fakeService.Move(ctx, id, runID, group)
}

func (s *fakeService) Start(_ context.Context, id string) (terminalhost.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, id)
	if s.startErr != nil {
		return terminalhost.SessionInfo{}, s.startErr
	}
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].State, s.items[i].RunID = "running", "new-run"
			return s.items[i], nil
		}
	}
	return terminalhost.SessionInfo{}, errors.New("missing session")
}

// matrix/hardened: the history route requires the old run displayed by the UI.
func (s *fakeService) Resume(_ context.Context, id, runID string) (terminalhost.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumes = append(s.resumes, mutation{id, runID})
	if s.resumeErr != nil {
		return terminalhost.SessionInfo{}, s.resumeErr
	}
	for i := range s.items {
		if s.items[i].ID == id && s.items[i].RunID == runID && s.items[i].Tool == "codex" && (s.items[i].State == "stopped" || s.items[i].State == "exited") {
			s.items[i].State, s.items[i].RunID = "running", "resumed-run"
			return s.items[i], nil
		}
	}
	return terminalhost.SessionInfo{}, errors.New("stale or unsupported resume")
}

func (s *fakeService) Stop(_ context.Context, id, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id && s.items[i].RunID == runID {
			s.stops = append(s.stops, mutation{id, runID})
			s.items[i].State = "stopped"
			return nil
		}
	}
	return errors.New("stale instance")
}

func (s *fakeService) Rename(_ context.Context, id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renames = append(s.renames, struct{ id, name string }{id, name})
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].Name = name
			return nil
		}
	}
	return errors.New("missing session")
}

func (s *fakeService) Remove(_ context.Context, id, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id && s.items[i].RunID == runID {
			s.removes = append(s.removes, mutation{id, runID})
			s.items = append(s.items[:i], s.items[i+1:]...)
			return nil
		}
	}
	return errors.New("stale instance")
}

func (s *fakeService) AttachCommand(info terminalhost.SessionInfo) (*exec.Cmd, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attaches = append(s.attaches, info)
	if s.attach != nil {
		return s.attach(), nil
	}
	return exec.Command("native-test-attach"), nil
}

func (s *fakeService) replace(items ...terminalhost.SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]terminalhost.SessionInfo(nil), items...)
}

func info(id, name, state, runID string) terminalhost.SessionInfo {
	return terminalhost.SessionInfo{ID: id, Name: name, State: state, RunID: runID, Tool: "shell", Dir: `C:\项目\原生终端`}
}

func modelWith(s *fakeService) *Model {
	m := New(s, `C:\项目\原生终端`).(*Model)
	m.Update(m.refresh()())
	return m
}

func key(value string) tea.KeyMsg {
	switch value {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
	}
}

func press(m *Model, value string) tea.Cmd {
	_, cmd := m.Update(key(value))
	return cmd
}

func runOperation(t *testing.T, m *Model, cmd tea.Cmd) operationResultMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected operation command")
	}
	msg, ok := cmd().(operationResultMsg)
	if !ok {
		t.Fatal("command did not perform service operation")
	}
	m.Update(msg)
	return msg
}

func TestSelectionUsesStableUUIDAcrossRefreshes(t *testing.T) {
	a, b, c := info("a", "相同名称", "running", "a1"), info("b", "相同名称", "running", "b1"), info("c", "第三个", "stopped", "")
	s := &fakeService{items: []terminalhost.SessionInfo{a, b, c}}
	m := modelWith(s)
	press(m, "j")
	s.replace(c, a, b)
	m.Update(m.refresh()())
	if m.selectedID != "b" || m.cursor != 2 {
		t.Fatalf("reorder changed selection: id=%s cursor=%d", m.selectedID, m.cursor)
	}
	press(m, "up")
	if m.selectedID != "a" {
		t.Fatalf("up selected %q", m.selectedID)
	}
	s.replace(c, b)
	m.Update(m.refresh()())
	if m.selectedID != "b" || m.cursor != 1 {
		t.Fatalf("removed row did not retain nearest selection: id=%s cursor=%d", m.selectedID, m.cursor)
	}
	s.replace()
	m.Update(m.refresh()())
	if m.selectedID != "" || m.cursor != 0 {
		t.Fatal("empty list retained dangling selection")
	}
}

func TestTextInputsDoNotInvokeListShortcuts(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("a", "原始", "running", "run")}}
	m := modelWith(s)
	press(m, "n")
	m.name.SetValue("")
	for _, char := range "qnsdrjko中文" {
		press(m, string(char))
	}
	if m.quitting || m.mode != createMode || m.name.Value() != "qnsdrjko中文" {
		t.Fatalf("text became an action: mode=%v name=%q quit=%v", m.mode, m.name.Value(), m.quitting)
	}
	press(m, "tab")
	m.dir.SetValue("")
	press(m, "qndsr")
	if m.dir.Value() != "qndsr" || len(s.creates)+len(s.stops)+len(s.removes)+len(s.renames)+len(s.attaches) != 0 {
		t.Fatal("path input invoked a list operation")
	}
	press(m, "esc")
	if m.mode != listMode {
		t.Fatal("escape did not cancel form")
	}
}

func TestCreateKeyboardFlowStartsAndAttachesChosenTool(t *testing.T) {
	s := &fakeService{}
	m := modelWith(s)
	press(m, "n")
	if m.dir.Value() != m.defaultCwd || m.name.Value() == "" {
		t.Fatal("new form lacks usable defaults")
	}
	m.name.SetValue("  原生 Codex  ")
	m.dir.SetValue(`C:\开发\项目`)
	press(m, "tab")
	press(m, "tab")
	press(m, "right")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || m.mode != attachMode {
		t.Fatalf("create did not attach: err=%v mode=%v", result.err, m.mode)
	}
	if len(s.creates) != 1 || s.creates[0].Name != "原生 Codex" || s.creates[0].Tool != "codex" || s.creates[0].Dir != `C:\开发\项目` {
		t.Fatalf("incorrect create arguments: %+v", s.creates)
	}
	if len(s.starts) != 1 || s.starts[0] != "new-0" || len(s.attaches) != 1 || s.attaches[0].RunID != "new-run" {
		t.Fatalf("create/start/attach boundary broken: starts=%v attaches=%+v", s.starts, s.attaches)
	}
	m.Update(attachReturnedMsg{generation: m.generation})
	if m.mode != listMode || m.selectedID != "new-0" || len(s.stops)+len(s.removes) != 0 {
		t.Fatal("return from attach lost selection or stopped process")
	}
}

// matrix/hardened: launch defaults reduce first-use steps without changing the
// legacy API or allowing arbitrary executable names into a creation request.
func TestOptionsChooseSupportedToolAndKeepLegacyShellDefault(t *testing.T) {
	for _, tool := range []string{"shell", "codex", "powershell", "", "custom.exe", "Codex"} {
		t.Run(tool, func(t *testing.T) {
			s := &fakeService{autoStart: true}
			m := NewWithOptions(s, `C:\项目\原生终端`, Options{DefaultTool: tool}).(*Model)
			want := tool
			if tool != "shell" && tool != "codex" && tool != "powershell" {
				want = "shell"
			}
			if len(s.creates)+len(s.starts)+len(s.attaches) != 0 {
				t.Fatal("constructing defaults started a hosted process")
			}
			press(m, "n")
			result := runOperation(t, m, press(m, "enter"))
			if result.err != nil || len(s.creates) != 1 || s.creates[0].Tool != want || len(s.attaches) != 1 || m.mode != attachMode {
				t.Fatalf("n then Enter did not create and attach default tool %q: result=%+v creates=%+v", want, result, s.creates)
			}
		})
	}
	legacy := New(&fakeService{}, `C:\项目`).(*Model)
	press(legacy, "n")
	if legacy.tool != "shell" {
		t.Fatalf("legacy constructor changed default to %q", legacy.tool)
	}
}

// matrix/hardened: a window remembers its explicit tool choice after cancelling
// a dialog and after returning from an attached session, without saving config.
func TestCreateDialogRetainsChosenToolAcrossCancelAndReturn(t *testing.T) {
	s := &fakeService{autoStart: true}
	m := NewWithOptions(s, `C:\项目`, Options{DefaultTool: "codex"}).(*Model)
	press(m, "n")
	if m.tool != "codex" {
		t.Fatal("first creation forgot launch default")
	}
	press(m, "tab")
	press(m, "tab")
	press(m, "right")
	press(m, "esc")
	press(m, "n")
	if m.tool != "powershell" || m.focus != 0 || len(s.creates) != 0 {
		t.Fatalf("cancel reset tool or created a process: tool=%q focus=%d creates=%+v", m.tool, m.focus, s.creates)
	}
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.creates) != 1 || s.creates[0].Tool != "powershell" {
		t.Fatalf("retained choice did not reach service: result=%+v creates=%+v", result, s.creates)
	}
	m.Update(attachReturnedMsg{generation: m.generation})
	press(m, "n")
	if m.tool != "powershell" || len(s.stops)+len(s.removes) != 0 {
		t.Fatal("return and new creation reset tool or stopped prior session")
	}
}

func TestCreatedDescriptorSurvivesStartFailure(t *testing.T) {
	s := &fakeService{startErr: errors.New("codex missing")}
	m := modelWith(s)
	press(m, "n")
	result := runOperation(t, m, press(m, "enter"))
	if result.err == nil || m.mode != listMode || m.selectedID != "new-0" || len(s.items) != 1 || len(s.attaches) != 0 {
		t.Fatalf("start failure lost descriptor or attached: %+v mode=%v", result, m.mode)
	}
}

func TestObservationErrorStillAllowsExactOwnedRunCleanup(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("owned", "异常会话", "error", "exact-run")}}
	m := modelWith(s)
	press(m, "s")
	if m.mode != confirmStopMode {
		t.Fatal("observation error hid owned run cleanup")
	}
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.stops) != 1 || s.stops[0] != (mutation{"owned", "exact-run"}) || len(s.removes) != 0 {
		t.Fatalf("error cleanup lost exact run or removed descriptor: result=%+v stops=%+v removes=%+v", result, s.stops, s.removes)
	}
}

func TestPersistedCreateErrorSelectsDescriptorWithoutStartRetry(t *testing.T) {
	createErr := errors.New("auto-start failed")
	s := &fakeService{createErr: createErr}
	m := modelWith(s)
	press(m, "n")
	result := press(m, "enter")().(operationResultMsg)
	_, refresh := m.Update(result)
	if !errors.Is(result.err, createErr) || result.selectedID != "new-0" || m.mode != listMode || m.selectedID != "new-0" {
		t.Fatalf("persisted creation error lost selected descriptor: result=%+v mode=%v selected=%q", result, m.mode, m.selectedID)
	}
	if !m.noticeErr || !strings.HasPrefix(m.notice, "会话已创建，启动失败：") || len(s.creates) != 1 || len(s.items) != 1 || len(s.starts)+len(s.attaches) != 0 {
		t.Fatalf("auto-start error was retried or mislabelled: notice=%q creates=%d descriptors=%d starts=%v attaches=%v", m.notice, len(s.creates), len(s.items), s.starts, s.attaches)
	}
	// Execute the actual list refresh command after returning from the form.
	batch := refresh().(tea.BatchMsg)
	m.Update(batch[0]())
	m.Update(m.refresh()())
	if len(m.items) != 1 || m.selectedID != "new-0" || m.items[0].ID != "new-0" || len(s.starts)+len(s.attaches) != 0 {
		t.Fatalf("refresh changed selection or retried failed start: selected=%q items=%+v starts=%v attaches=%v", m.selectedID, m.items, s.starts, s.attaches)
	}
}

func TestAlreadyRunningCreateDoesNotRestartHost(t *testing.T) {
	s := &fakeService{autoStart: true}
	m := modelWith(s)
	press(m, "n")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.starts) != 0 || len(s.attaches) != 1 || s.attaches[0].RunID != "created-run" {
		t.Fatalf("auto-started creation was restarted: %+v starts=%v attaches=%+v", result, s.starts, s.attaches)
	}
}

func TestOpenStartsStoppedOrExitedAndPreservesRunning(t *testing.T) {
	for _, state := range []string{"stopped", "exited", "running", "unknown"} {
		t.Run(state, func(t *testing.T) {
			s := &fakeService{items: []terminalhost.SessionInfo{info("target", "终端", state, "original")}}
			m := modelWith(s)
			result := runOperation(t, m, press(m, "o"))
			if state == "unknown" {
				if result.err == nil || len(s.starts)+len(s.attaches) != 0 {
					t.Fatal("unknown process state launched a new host")
				}
				return
			}
			if result.err != nil || len(s.attaches) != 1 || s.attaches[0].ID != "target" || m.mode != attachMode {
				t.Fatalf("failed to open target: %+v attaches=%v", result, s.attaches)
			}
			if state == "running" && len(s.starts) != 0 {
				t.Fatal("opening running process restarted it")
			}
			if state != "running" && (len(s.starts) != 1 || s.starts[0] != "target") {
				t.Fatal("stopped process was not explicitly started")
			}
		})
	}
}

// matrix/hardened: h selects history explicitly; Enter retains the previous
// bare-start behavior, and neither route follows similarly named rows.
func TestCodexHistoryUsesDisplayedIdentityAndAttachesNewRun(t *testing.T) {
	for _, state := range []string{"stopped", "exited"} {
		t.Run(state, func(t *testing.T) {
			a, b := info("a", "同名", state, "old-a"), info("b", "同名", state, "old-b")
			a.Tool, b.Tool = "codex", "codex"
			s := &fakeService{items: []terminalhost.SessionInfo{a, b}}
			m := modelWith(s)
			press(m, "down")
			result := runOperation(t, m, press(m, "h"))
			if result.err != nil || len(s.resumes) != 1 || s.resumes[0] != (mutation{b.ID, b.RunID}) || len(s.starts) != 0 || len(s.attaches) != 1 || s.attaches[0].ID != b.ID || s.attaches[0].RunID != "resumed-run" || m.mode != attachMode {
				t.Fatalf("history lost exact identity or started a fresh dialog: result=%+v resumes=%+v attaches=%+v", result, s.resumes, s.attaches)
			}
			if s.items[0] != a {
				t.Fatal("history affected another similarly named row")
			}
		})
	}
	for _, openKey := range []string{"enter", "o"} {
		a := info("target", "Codex", "stopped", "old")
		a.Tool = "codex"
		s := &fakeService{items: []terminalhost.SessionInfo{a}}
		m := modelWith(s)
		result := runOperation(t, m, press(m, openKey))
		if result.err != nil || len(s.starts) != 1 || len(s.resumes) != 0 || s.attaches[0].RunID != "new-run" {
			t.Fatalf("ordinary %s changed stopped Codex into history selection", openKey)
		}
	}
}

func TestCodexHistoryRejectsUnsupportedRowsBeforeServiceCall(t *testing.T) {
	for _, current := range []terminalhost.SessionInfo{
		{ID: "live", RunID: "live-run", Tool: "codex", State: "running"},
		{ID: "error", RunID: "error-run", Tool: "codex", State: "error"},
		{ID: "unknown", Tool: "codex", State: "unknown"},
		{ID: "shell", Tool: "shell", State: "stopped"},
		{ID: "custom", Tool: "custom", State: "exited"},
	} {
		s := &fakeService{items: []terminalhost.SessionInfo{current}}
		m := modelWith(s)
		if cmd := press(m, "h"); cmd != nil || m.mode != listMode || len(s.resumes)+len(s.starts)+len(s.attaches) != 0 || !strings.Contains(m.notice, "已停止或退出") {
			t.Fatalf("unsupported history row reached service: %+v mode=%d notice=%s", current, m.mode, m.notice)
		}
	}
}

func TestCodexHistoryRechecksStateAndRunWithoutAdoptingReplacement(t *testing.T) {
	for _, change := range []string{"restart", "running same run", "changed tool", "removed"} {
		t.Run(change, func(t *testing.T) {
			a := info("target", "原 Codex", "stopped", "old")
			a.Tool = "codex"
			s := &fakeService{items: []terminalhost.SessionInfo{a}}
			m := modelWith(s)
			cmd := press(m, "h")
			replacement := a
			switch change {
			case "restart":
				replacement.RunID = "replacement"
			case "running same run":
				replacement.State = "running"
			case "changed tool":
				replacement.Tool = "custom"
			}
			if change == "removed" {
				s.replace()
			} else {
				s.replace(replacement)
			}
			result := runOperation(t, m, cmd)
			if result.err == nil || len(s.resumes)+len(s.starts)+len(s.attaches) != 0 {
				t.Fatalf("history followed replacement after %s: %+v", change, result)
			}
		})
	}
}

// matrix/hardened: cover the remaining race between the List precheck and the
// manager's locked request; the selected old run must reach Resume unchanged.
type restartBeforeResumeService struct {
	*fakeService
}

func (s *restartBeforeResumeService) Resume(ctx context.Context, id, runID string) (terminalhost.SessionInfo, error) {
	s.mu.Lock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].RunID = "replacement-after-check"
		}
	}
	s.mu.Unlock()
	return s.fakeService.Resume(ctx, id, runID)
}

func TestCodexHistoryServiceRaceAndFailureNeverFallBackOrRetry(t *testing.T) {
	for _, test := range []string{"backend race", "launch error", "attach error"} {
		t.Run(test, func(t *testing.T) {
			a := info("target", "Codex", "stopped", "old")
			a.Tool = "codex"
			s := &fakeService{items: []terminalhost.SessionInfo{a}}
			var service Service = s
			if test == "backend race" {
				service = &restartBeforeResumeService{fakeService: s}
			}
			if test == "launch error" {
				s.resumeErr = errors.New("resume unavailable")
			}
			if test == "attach error" {
				s.attach = func() *exec.Cmd { return nil }
			}
			m := New(service, a.Dir).(*Model)
			m.Update(m.refresh()())
			result := runOperation(t, m, press(m, "h"))
			if result.err == nil || len(s.resumes) != 1 || s.resumes[0] != (mutation{a.ID, a.RunID}) || len(s.starts) != 0 || m.mode != listMode || m.selectedID != a.ID {
				t.Fatalf("history failure retried or switched routes: result=%+v resumes=%+v starts=%+v", result, s.resumes, s.starts)
			}
			wantAttaches := 0
			if test == "attach error" {
				wantAttaches = 1
			}
			if len(s.attaches) != wantAttaches {
				t.Fatal("history failure retried attachment")
			}
		})
	}
}

func TestCodexHistoryGuidanceAndSearchKeyOwnership(t *testing.T) {
	a := info("target", "Codex", "stopped", "old")
	a.Tool = "codex"
	s := &fakeService{items: []terminalhost.SessionInfo{a}}
	m := modelWith(s)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "h 历史") || !strings.Contains(view, "Enter 新对话 · h 选择历史") {
		t.Fatalf("stopped Codex hides separate launch choices: %q", view)
	}
	press(m, "/")
	press(m, "h")
	if m.search.Value() != "h" || len(s.resumes) != 0 {
		t.Fatal("history key stole search text")
	}
	m.mode, m.target = confirmStopMode, a
	view = ansi.Strip(m.View())
	if !strings.Contains(view, "停止后 Enter 新对话；h 选择此目录的 Codex 历史") {
		t.Fatalf("stop confirmation does not explain history restoration: %q", view)
	}
}

func TestStopAndRemoveConfirmationsBindUUIDAndRunID(t *testing.T) {
	for _, action := range []string{"s", "d"} {
		t.Run(action, func(t *testing.T) {
			a, b := info("a", "重复名称", "running", "a-run"), info("b", "重复名称", "running", "b-run")
			s := &fakeService{items: []terminalhost.SessionInfo{a, b}}
			m := modelWith(s)
			press(m, "j")
			press(m, action)
			if m.target.ID != "b" || m.target.RunID != "b-run" {
				t.Fatal("confirmation lost original binding")
			}
			press(m, "esc")
			if len(s.stops)+len(s.removes) != 0 {
				t.Fatal("cancel mutated service")
			}
			// Finish the refresh started when closing the dialog.
			m.Update(listResultMsg{generation: m.generation, request: m.listRequest, items: []terminalhost.SessionInfo{a, b}})
			press(m, action)
			result := runOperation(t, m, press(m, "enter"))
			if result.err != nil {
				t.Fatal(result.err)
			}
			mutations := s.stops
			if action == "d" {
				mutations = s.removes
			}
			if len(mutations) != 1 || mutations[0] != (mutation{"b", "b-run"}) {
				t.Fatalf("wrong mutation target: %+v", mutations)
			}
		})
	}
}

func TestConfirmationRejectsRestartedInstanceInAnotherWindow(t *testing.T) {
	for _, action := range []string{"s", "d"} {
		t.Run(action, func(t *testing.T) {
			original := info("same-id", "终端", "running", "old-run")
			s := &fakeService{items: []terminalhost.SessionInfo{original}}
			m := modelWith(s)
			press(m, action)
			changed := original
			changed.RunID = "new-run"
			s.replace(changed)
			result := runOperation(t, m, press(m, "enter"))
			if result.err == nil || len(s.stops)+len(s.removes) != 0 || m.mode != listMode {
				t.Fatalf("stale confirmation reached mutation: err=%v stops=%v removes=%v", result.err, s.stops, s.removes)
			}
		})
	}
}

func TestConfirmationRejectsChangedLocalSelection(t *testing.T) {
	a, b := info("a", "一", "running", "a1"), info("b", "二", "running", "b1")
	s := &fakeService{items: []terminalhost.SessionInfo{a, b}}
	m := modelWith(s)
	press(m, "s")
	m.setItems([]terminalhost.SessionInfo{b})
	press(m, "enter")
	if m.mode != listMode || len(s.stops) != 0 {
		t.Fatal("confirmation followed replacement selection")
	}
}

func TestRenameSavesThroughService(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("a", "原名", "running", "a1")}}
	m := modelWith(s)
	press(m, "r")
	m.name.SetValue("新名称")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.renames) != 1 || s.renames[0].id != "a" || s.renames[0].name != "新名称" || s.items[0].Name != "新名称" {
		t.Fatalf("rename not persisted: %+v %+v", result, s.renames)
	}
}

func TestStaleAsyncResultsCannotOverwriteDialogsOrOperations(t *testing.T) {
	a := info("a", "原名", "running", "a1")
	s := &fakeService{items: []terminalhost.SessionInfo{a}}
	m := modelWith(s)
	oldRefresh := m.refresh()
	oldResult := oldRefresh().(listResultMsg)
	press(m, "r")
	m.name.SetValue("刚保存的名字")
	m.Update(oldResult)
	if m.mode != renameMode || m.name.Value() != "刚保存的名字" {
		t.Fatal("pending list replaced active rename form")
	}
	runOperation(t, m, press(m, "enter"))
	m.Update(listResultMsg{generation: m.generation, request: m.listRequest, items: s.items})
	m.Update(oldResult)
	if len(m.items) != 1 || m.items[0].Name != "刚保存的名字" {
		t.Fatal("old refresh overwrote successful rename")
	}
	press(m, "n")
	m.Update(operationResultMsg{generation: m.generation - 1, notice: "stale"})
	if m.mode != createMode || m.notice == "stale" {
		t.Fatal("old operation replaced newer modal")
	}
}

func TestPollOnlyRunsWhileListVisibleAndAllowsOneFetch(t *testing.T) {
	s := &fakeService{}
	m := modelWith(s)
	token := m.pollToken
	_, cmd := m.Update(pollMsg{token: token})
	if cmd == nil || !m.listInFlight {
		t.Fatal("visible list did not schedule refresh")
	}
	request := m.listRequest
	m.Update(pollMsg{token: token})
	if m.listRequest != request {
		t.Fatal("overlapping poll scheduled another list fetch")
	}
	press(m, "n")
	_, cmd = m.Update(pollMsg{token: token})
	if cmd != nil || m.listInFlight {
		t.Fatal("dialog continued polling list")
	}
	press(m, "esc")
	newRequest := m.listRequest
	_, cmd = m.Update(pollMsg{token: token})
	if cmd != nil || m.listRequest != newRequest {
		t.Fatal("obsolete tick restarted another polling chain")
	}
}

func TestRecoveredListClearsTransientFailureAndKeepsOperationNotice(t *testing.T) {
	s := &fakeService{listErr: errors.New("temporarily unavailable")}
	m := modelWith(s)
	if !m.noticeErr || !m.listFailure {
		t.Fatal("list error was not displayed")
	}
	s.listErr = nil
	m.Update(m.refresh()())
	if m.noticeErr || m.notice != "" {
		t.Fatal("successful refresh retained obsolete failure")
	}
	m.setNotice("会话名称已保存。", false)
	m.Update(m.refresh()())
	if m.notice != "会话名称已保存。" {
		t.Fatal("routine refresh cleared an operation result")
	}
}

func TestSeparateModelsHaveIndependentSelectionAndForms(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("a", "一", "running", "a1"), info("b", "二", "running", "b1")}}
	first, second := modelWith(s), modelWith(s)
	press(first, "j")
	press(first, "n")
	first.name.SetValue("窗口一")
	if second.selectedID != "a" || second.mode != listMode || second.name.Value() == "窗口一" {
		t.Fatal("UI state leaked between windows")
	}
}

func TestQuitNeverStopsOrRemovesHostedProcesses(t *testing.T) {
	for _, currentMode := range []mode{listMode, createMode, renameMode, moveMode, confirmStopMode, confirmRemoveMode, workingMode, attachMode} {
		t.Run(fmt.Sprint(currentMode), func(t *testing.T) {
			s := &fakeService{items: []terminalhost.SessionInfo{info("a", "一", "running", "a1")}}
			m := modelWith(s)
			m.mode = currentMode
			cmd := press(m, "ctrl+c")
			if cmd == nil || !m.quitting {
				t.Fatal("Ctrl+C did not quit this UI mode")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok || len(s.stops)+len(s.removes) != 0 {
				t.Fatal("UI quit stopped native process")
			}
		})
	}
}

func TestViewBoundsChineseNamesAndAllModalSizes(t *testing.T) {
	s := &fakeService{items: []terminalhost.SessionInfo{info("a", strings.Repeat("中文会话🐱", 30)+"\x1b[31m\rInjected", "running", "a1")}}
	m := modelWith(s)
	m.target = s.items[0]
	m.name.SetValue("\x1b[31m\r\n\t名称\x00\a\u0085" + strings.Repeat("名称", 100))
	m.dir.SetValue("\x1b]52;c;clipboard\a\r\n\t路径\x00\u009b" + strings.Repeat("路径", 100))
	m.group.SetValue("\x1b[31m\r\n\t分组\x00\a\u0085" + strings.Repeat("分组🐱", 30))
	m.search.SetValue("\x1b[31m\r\n\t搜索\x00\a\u0085" + strings.Repeat("搜索🐱", 30))
	s.items[0].Group = strings.Repeat("分组🐱", 30) + "\x1b[31m\rInjected"
	m.setItems(s.items)
	for _, currentMode := range []mode{listMode, createMode, renameMode, moveMode, confirmStopMode, confirmRemoveMode, workingMode, attachMode} {
		for _, size := range [][2]int{{80, 24}, {40, 12}, {20, 6}, {5, 3}, {1, 1}} {
			m.mode = currentMode
			m.searching = currentMode == listMode
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) > size[1] {
				t.Fatalf("mode=%d size=%v height=%d", currentMode, size, len(lines))
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("mode=%d size=%v width=%d line=%q", currentMode, size, ansi.StringWidth(line), line)
				}
				for _, r := range ansi.Strip(line) {
					if unicode.IsControl(r) {
						t.Fatalf("unsafe control character rendered: %q", line)
					}
				}
			}
		}
	}
}

func TestShortFormKeepsActiveFieldAndSaveCancelControlsVisible(t *testing.T) {
	for _, currentMode := range []mode{createMode, renameMode, moveMode} {
		fields := []string{"名称："}
		if currentMode == createMode {
			fields = []string{"名称：", "目录：", "工具：", "分组："}
		}
		if currentMode == moveMode {
			fields = []string{"分组："}
		}
		for focus, label := range fields {
			for _, height := range []int{1, 2, 3, 4} {
				m := modelWith(&fakeService{})
				m.mode, m.focus = currentMode, focus
				if currentMode == moveMode {
					m.focus = 3
				}
				m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
				view := ansi.Strip(m.View())
				if !strings.Contains(view, "› "+label) {
					t.Fatalf("mode=%v focus=%d height=%d hid active field: %q", currentMode, focus, height, view)
				}
				// matrix/hardened: creation enters the new terminal immediately.
				action := "Enter 保存"
				if currentMode == createMode {
					action = "Enter 创建并进入"
				}
				if height > 1 && (!strings.Contains(view, action) || !strings.Contains(view, "Esc 取消")) {
					t.Fatalf("mode=%v focus=%d height=%d hid save/cancel: %q", currentMode, focus, height, view)
				}
			}
		}
	}
}

// matrix/hardened: a padded input must not clip the explanatory suffix at the
// actual 80-column default or the wider native lifecycle-test window.
func TestCreateGroupHintAndEditableValueRemainVisibleAfterResize(t *testing.T) {
	for _, width := range []int{80, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := modelWith(&fakeService{})
			press(m, "n")
			press(m, "tab")
			press(m, "tab")
			press(m, "tab")
			m.Update(tea.WindowSizeMsg{Width: width, Height: 45})
			press(m, "开发分组")
			view := ansi.Strip(m.View())
			if !strings.Contains(view, "› 分组：开发分组") || !strings.Contains(view, "（留空为默认分组）") {
				t.Fatalf("width %d clipped editable group or its hint: %q", width, view)
			}
			m.group.SetValue(strings.Repeat("开发分组", 16))
			view = ansi.Strip(m.View())
			if !strings.Contains(view, "（留空为默认分组）") {
				t.Fatalf("width %d let a long group hide its hint: %q", width, view)
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > width {
					t.Fatalf("width %d overflowed a form line: %q", width, line)
				}
			}
		})
	}
}

// matrix/hardened: lifecycle guidance must retain the explicit confirm/cancel
// controls in a short terminal instead of making a stop dialog look stuck.
func TestShortConfirmationKeepsControlsAndTargetVisible(t *testing.T) {
	for _, currentMode := range []mode{confirmStopMode, confirmRemoveMode} {
		for height := 1; height <= 8; height++ {
			m := modelWith(&fakeService{})
			m.mode = currentMode
			m.target = terminalhost.SessionInfo{Name: "待确认会话", Tool: "codex", State: "running", Dir: `C:\项目`}
			m.Update(tea.WindowSizeMsg{Width: 100, Height: height})
			view := ansi.Strip(m.View())
			if !strings.Contains(view, "Enter 确认") || !strings.Contains(view, "Esc 取消") || len(strings.Split(view, "\n")) > height {
				t.Fatalf("height %d hid confirmation controls: %q", height, view)
			}
			if height >= 3 && !strings.Contains(view, m.target.Name) {
				t.Fatalf("height %d hid the target identity: %q", height, view)
			}
		}
	}
}

func TestCreateInGroupKeepsToolThirdAndUsesGroupFourth(t *testing.T) {
	s := &fakeService{autoStart: true}
	m := modelWith(s)
	press(m, "n")
	m.name.SetValue("开发会话")
	press(m, "tab")
	press(m, "tab")
	if m.focus != 2 {
		t.Fatal("tool no longer occupies original third field")
	}
	press(m, "right")
	press(m, "tab")
	if m.focus != 3 || !m.group.Focused() {
		t.Fatal("fourth field is not editable group")
	}
	press(m, " 开发 / 原生 🐱 ")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.creates) != 1 || s.creates[0].Group != "开发 / 原生 🐱" || s.creates[0].Tool != "codex" {
		t.Fatalf("group/tool creation lost fields: result=%+v creates=%+v", result, s.creates)
	}
	if len(s.starts)+len(s.stops)+len(s.moves) != 0 || len(s.attaches) != 1 || s.attaches[0].RunID != "created-run" {
		t.Fatal("group creation changed the auto-started process")
	}
}

func TestCreateNativePowerShellChoicePreservesExistingToolOrder(t *testing.T) {
	s := &fakeService{autoStart: true}
	m := modelWith(s)
	press(m, "n")
	press(m, "tab")
	press(m, "tab")
	press(m, "right")
	if m.tool != "codex" {
		t.Fatal("first right key no longer selects Codex")
	}
	press(m, "left")
	if m.tool != "shell" {
		t.Fatal("left from Codex no longer selects Shell")
	}
	press(m, "right")
	press(m, "right")
	if m.tool != "powershell" || !strings.Contains(ansi.Strip(m.View()), "[PowerShell]") {
		t.Fatal("third tool did not select and label PowerShell")
	}
	press(m, "right")
	if m.tool != "shell" {
		t.Fatal("third tool did not wrap to Shell")
	}
	press(m, "left")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.creates) != 1 || s.creates[0].Tool != "powershell" || len(s.attaches) != 1 || s.attaches[0].Tool != "powershell" || len(s.starts) != 0 {
		t.Fatalf("PowerShell creation changed tool or restarted auto-started host: result=%+v creates=%+v attaches=%+v", result, s.creates, s.attaches)
	}
}

func TestCreatePrefillsCurrentFilteredGroupAndDefaultsWhenAll(t *testing.T) {
	a := info("a", "终端", "running", "run")
	a.Group = "研发"
	m := modelWith(&fakeService{items: []terminalhost.SessionInfo{a}})
	press(m, "tab") // default
	press(m, "tab") // named
	press(m, "n")
	if m.group.Value() != "研发" || m.focus != 0 {
		t.Fatal("new session forgot selected group or initial name focus")
	}
	press(m, "esc")
	press(m, "esc") // clear filters
	press(m, "n")
	if m.group.Value() != "" {
		t.Fatal("all groups inherited unrelated group from prior form")
	}
}

func TestGroupedCreatePartialErrorRetainsDescriptorWithoutAutomaticRetry(t *testing.T) {
	a := info("old", "现有", "running", "old-run")
	a.Group = "研发"
	createErr := errors.New("auto-start failed after descriptor write")
	s := &fakeService{items: []terminalhost.SessionInfo{a}, createErr: createErr}
	m := modelWith(s)
	press(m, "tab")
	press(m, "tab")
	press(m, "/")
	press(m, "不匹配")
	press(m, "enter")
	press(m, "n")
	m.name.SetValue("新增原生会话")
	m.group.SetValue("新分组")
	result := runOperation(t, m, press(m, "enter"))
	if !errors.Is(result.err, createErr) || result.selectedID != "new-0" || len(s.creates) != 1 || s.creates[0].Group != "新分组" {
		t.Fatalf("grouped partial creation lost descriptor: result=%+v creates=%+v", result, s.creates)
	}
	m.Update(listResultMsg{generation: m.generation, request: m.listRequest, items: s.items})
	if m.mode != listMode || m.selectedID != "new-0" || !m.allGroups || m.search.Value() != "" || len(m.items) != 2 {
		t.Fatalf("partial creation remained hidden by old filters: mode=%v selected=%q items=%+v", m.mode, m.selectedID, m.items)
	}
	if len(s.starts)+len(s.stops)+len(s.moves)+len(s.removes)+len(s.attaches) != 0 || s.items[0].RunID != "old-run" {
		t.Fatal("partial creation retried a start or changed another process")
	}
}

func TestMoveGroupPersistsOnlyDescriptorAndKeepsExactRun(t *testing.T) {
	a := info("a", "同名", "running", "a-run")
	b := info("b", "同名", "running", "b-run")
	a.Group, b.Group = "旧组", "旧组"
	s := &fakeService{items: []terminalhost.SessionInfo{a, b}}
	m := modelWith(s)
	press(m, "j")
	press(m, "g")
	if m.mode != moveMode || m.target.ID != "b" || m.target.RunID != "b-run" || m.group.Value() != "旧组" {
		t.Fatal("move dialog lost displayed target")
	}
	m.group.SetValue(" 新分组 ")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.moves) != 1 || s.moves[0].id != "b" || s.moves[0].runID != "b-run" || s.moves[0].group != "新分组" {
		t.Fatalf("incorrect move boundary: result=%+v moves=%+v", result, s.moves)
	}
	if s.items[1].RunID != "b-run" || s.items[1].State != "running" || s.items[0].Group != "旧组" || len(s.starts)+len(s.stops)+len(s.attaches)+len(s.removes) != 0 {
		t.Fatal("group move changed current or another process")
	}
	m.Update(listResultMsg{generation: m.generation, request: m.listRequest, items: s.items})
	if m.selectedID != "b" || !m.allGroups || m.items[m.cursor].Group != "新分组" {
		t.Fatal("refresh lost moved descriptor selection")
	}
}

func TestMoveGroupRejectsStaleRunAndChangedLocalTarget(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) {
			a := info("a", "会话", "running", "old-run")
			s := &fakeService{items: []terminalhost.SessionInfo{a}}
			m := modelWith(s)
			press(m, "g")
			m.group.SetValue("新组")
			a.RunID = "new-run"
			s.replace(a)
			if local {
				m.setItems([]terminalhost.SessionInfo{a})
				press(m, "enter")
			} else {
				result := runOperation(t, m, press(m, "enter"))
				if result.err == nil {
					t.Fatal("remote restarted run was not rejected")
				}
			}
			if m.mode != listMode || len(s.moves)+len(s.starts)+len(s.stops)+len(s.removes) != 0 {
				t.Fatal("stale group dialog mutated a process or descriptor")
			}
		})
	}
}

func TestMoveToDefaultAndCancelAreExplicit(t *testing.T) {
	a := info("a", "会话", "running", "run")
	a.Group = "研发"
	s := &fakeService{items: []terminalhost.SessionInfo{a}}
	m := modelWith(s)
	press(m, "g")
	m.group.SetValue("未保存")
	press(m, "esc")
	if len(s.moves) != 0 || s.items[0].Group != "研发" {
		t.Fatal("cancel saved group")
	}
	m.Update(listResultMsg{generation: m.generation, request: m.listRequest, items: s.items})
	press(m, "g")
	m.group.SetValue("")
	result := runOperation(t, m, press(m, "enter"))
	if result.err != nil || len(s.moves) != 1 || s.moves[0].group != "" || s.items[0].Group != "" {
		t.Fatal("empty group did not move to default group")
	}
}

func TestMovePassesDisplayedRunEvenWhenRestartOccursAfterListCheck(t *testing.T) {
	a := info("a", "同名", "running", "displayed-run")
	a.Group = "旧分组"
	s := &restartBeforeMoveService{fakeService: &fakeService{items: []terminalhost.SessionInfo{a}}}
	m := New(s, a.Dir).(*Model)
	m.Update(m.refresh()())
	press(m, "g")
	m.group.SetValue("新分组")
	result := runOperation(t, m, press(m, "enter"))
	if result.err == nil || len(s.attempts) != 1 || s.attempts[0] != (mutation{"a", "displayed-run"}) {
		t.Fatalf("move did not retain displayed instance through service race: result=%+v attempts=%+v", result, s.attempts)
	}
	if m.mode != listMode || len(s.moves)+len(s.starts)+len(s.stops)+len(s.removes)+len(s.attaches) != 0 || s.items[0].Group != "旧分组" {
		t.Fatal("rejected stale move changed group or process lifecycle")
	}
}

func TestSearchFindsNameDirectoryToolAndGroupWithoutMutating(t *testing.T) {
	for _, query := range []string{"中文名称", "开发路径", "CODEX", "研发 🐱"} {
		t.Run(query, func(t *testing.T) {
			a := info("target", "中文名称", "running", "run")
			a.Dir, a.Tool, a.Group = `C:\开发路径\repo`, "codex", "研发 🐱"
			s := &fakeService{items: []terminalhost.SessionInfo{info("other", "别的", "stopped", ""), a}}
			m := modelWith(s)
			press(m, "/")
			press(m, query)
			if len(m.items) != 1 || m.selectedID != "target" || !m.searching || !strings.Contains(m.View(), "1/2 个") {
				t.Fatalf("search failed: items=%+v selected=%q searching=%v view=%q", m.items, m.selectedID, m.searching, m.View())
			}
			press(m, "enter")
			if m.searching || len(m.items) != 1 {
				t.Fatal("Enter did not preserve query and return list shortcuts")
			}
			press(m, "esc")
			if len(m.items) != 2 || m.search.Value() != "" || len(s.creates)+len(s.starts)+len(s.stops)+len(s.moves)+len(s.attaches) != 0 {
				t.Fatal("clearing search modified descriptors or processes")
			}
		})
	}
}

func TestGroupFilteringCombinesSearchAndRetainsUUIDOnRefresh(t *testing.T) {
	a, b, c := info("a", "重复", "running", "a1"), info("b", "重复", "running", "b1"), info("c", "默认", "stopped", "")
	a.Group, b.Group = "研发", "研发"
	s := &fakeService{items: []terminalhost.SessionInfo{c, a, b}}
	m := modelWith(s)
	press(m, "tab")
	if m.allGroups || m.filterGroup != "" || len(m.items) != 1 || m.selectedID != "c" {
		t.Fatal("first group filter did not select default")
	}
	press(m, "tab")
	press(m, "j")
	press(m, "/")
	press(m, "重复")
	press(m, "enter")
	if m.filterGroup != "研发" || m.selectedID != "b" || len(m.items) != 2 {
		t.Fatal("combined filters lost selected UUID")
	}
	s.replace(b, c, a)
	m.Update(m.refresh()())
	if m.selectedID != "b" || m.cursor != 0 || len(m.items) != 2 || len(m.allItems) != 3 {
		t.Fatal("filtered refresh followed row position instead of UUID")
	}
	press(m, "shift+tab")
	if m.filterGroup != "" || len(m.items) != 0 || !strings.Contains(m.View(), "没有匹配") {
		t.Fatal("search did not combine with default-group filter")
	}
	press(m, "esc")
	if !m.allGroups || len(m.items) != 3 || m.search.Value() != "" {
		t.Fatal("Esc did not clear both list filters")
	}
}

func TestInputShortcutsRemainTextInSearchAndGroupForms(t *testing.T) {
	a := info("a", "会话", "running", "run")
	s := &fakeService{items: []terminalhost.SessionInfo{a}}
	m := modelWith(s)
	press(m, "/")
	for _, value := range []string{"n", "g", "q", "r", "s", "d", "/"} {
		press(m, value)
	}
	press(m, "tab")
	if m.search.Value() != "ngqrsd/" || !m.searching || m.quitting || !m.allGroups || m.mode != listMode {
		t.Fatal("search text was handled as global action")
	}
	press(m, "esc")
	press(m, "g")
	for _, value := range []string{"n", "g", "q", "r", "s", "d", "/"} {
		press(m, value)
	}
	if m.group.Value() != "ngqrsd/" || m.mode != moveMode || m.quitting || len(s.moves)+len(s.creates)+len(s.stops)+len(s.removes) != 0 {
		t.Fatal("group input was handled as list action")
	}
}

func TestActiveSearchRefreshKeepsTextAndSelectedUUID(t *testing.T) {
	a, b := info("a", "同名", "running", "a1"), info("b", "同名", "running", "b1")
	s := &fakeService{items: []terminalhost.SessionInfo{a, b}}
	m := modelWith(s)
	press(m, "j")
	press(m, "/")
	press(m, "同名")
	s.replace(b, a)
	m.Update(m.refresh()())
	if !m.searching || m.search.Value() != "同名" || m.selectedID != "b" || m.cursor != 0 {
		t.Fatal("live refresh replaced search text or selection")
	}
	press(m, "ctrl+c")
	if !m.quitting || len(s.stops)+len(s.removes) != 0 {
		t.Fatal("leaving active search stopped sessions")
	}
}

func TestSearchKeyboardHintsRemainVisibleWithPaddedInput(t *testing.T) {
	for _, height := range []int{1, 6, 24} {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			s := &fakeService{items: []terminalhost.SessionInfo{info("a", "中文维护", "running", "run")}}
			m := modelWith(s)
			m.width, m.height = 80, height
			press(m, "/")
			press(m, "中文维护")
			view := ansi.Strip(m.View())
			if !strings.Contains(view, "Enter 保留") || !strings.Contains(view, "Esc 清除") || strings.Contains(view, "q/Ctrl+C 退出") || strings.Contains(view, "q 退出") {
				t.Fatalf("search field hid or misreported active keyboard controls: %s", view)
			}
			press(m, "不匹配")
			if strings.Contains(m.View(), "按 n 新建") {
				t.Fatal("empty search advertised a shortcut consumed as text")
			}
			press(m, "esc")
			if strings.Contains(m.View(), "Enter 保留") || !strings.Contains(m.View(), "q") {
				t.Fatal("leaving search did not restore list controls")
			}
		})
	}
}

func TestEmptyRegistrySearchConsumesCreateShortcutAsText(t *testing.T) {
	s := &fakeService{}
	m := modelWith(s)
	press(m, "/")
	press(m, "nq")
	if strings.Contains(m.View(), "按 n 新建") || strings.Contains(m.View(), "q/Ctrl+C 退出") || len(s.creates) != 0 || m.quitting || m.search.Value() != "nq" {
		t.Fatalf("empty search misreported or invoked text keys: %s", m.View())
	}
	press(m, "esc")
	if !strings.Contains(m.View(), "按 n 新建") {
		t.Fatal("empty list did not restore creation after clearing search")
	}
}

func TestStateLabelsContainOnlyProcessFacts(t *testing.T) {
	for state, want := range map[string]string{"running": "正在运行", "exited": "已退出", "stopped": "已停止", "error": "异常", "waiting": "未知", "idle": "未知", "": "未知"} {
		if stateLabel(state) != want {
			t.Fatalf("state %q labelled %q, want %q", state, stateLabel(state), want)
		}
	}
}

// TestNativeUIAttachChild is the harmless child executed by the real Tea loop.
func TestNativeUIAttachChild(t *testing.T) {}

func TestActualExecProcessReturnsToList(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeService{
		items: []terminalhost.SessionInfo{info("target", "终端", "running", "run")},
		attach: func() *exec.Cmd {
			return exec.Command(executable, "-test.run=^TestNativeUIAttachChild$")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var program *tea.Program
	var opened, returned bool
	var filterErr error
	// A concrete EOF reader survives Tea's ReleaseTerminal/RestoreTerminal
	// cycle; nil input cannot be reinitialized by this pinned Tea version.
	program = tea.NewProgram(New(s, "."), tea.WithContext(ctx), tea.WithInput(strings.NewReader("")), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		switch msg := msg.(type) {
		case listResultMsg:
			if !opened {
				opened = true
				go program.Send(key("enter"))
			} else if returned {
				go program.Send(key("q"))
			}
		case attachReturnedMsg:
			returned, filterErr = true, msg.err
		}
		return msg
	}))
	final, err := program.Run()
	if err != nil {
		t.Fatal(err)
	}
	m := final.(*Model)
	if !returned || filterErr != nil || m.mode != listMode || m.selectedID != "target" {
		t.Fatalf("ExecProcess failed to resume list: returned=%v error=%v mode=%d selected=%s", returned, filterErr, m.mode, m.selectedID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.attaches) != 1 || len(s.starts)+len(s.stops)+len(s.removes) != 0 {
		t.Fatalf("attach affected hosted process: attaches=%d starts=%d stops=%d removes=%d", len(s.attaches), len(s.starts), len(s.stops), len(s.removes))
	}
}
