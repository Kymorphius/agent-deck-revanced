package ui

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/clipboard"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/terminalbackend"
)

// matrix/hardened: drive the existing Home actions against an immutable fake
// run; these tests do not launch tmux, a Windows process or a model turn.
type originalNativeTerminalFake struct {
	mu         sync.Mutex
	sessionID  string
	runID      string
	alive      bool
	observeErr error
	visible    string
	captureErr error
	sendErr    error
	stopErr    error
	observes   int
	captures   []bool
	sends      [][]byte
	stops      int
}

func (f *originalNativeTerminalFake) Binding() terminalbackend.Binding {
	return terminalbackend.Binding{SessionID: f.sessionID, RunID: f.runID}
}

func (f *originalNativeTerminalFake) Observe(ctx context.Context) (terminalbackend.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observes++
	if err := ctx.Err(); err != nil {
		return terminalbackend.Observation{}, err
	}
	return terminalbackend.Observation{Alive: f.alive, RunID: f.runID}, f.observeErr
}

func (f *originalNativeTerminalFake) Capture(ctx context.Context, full bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captures = append(f.captures, full)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return f.visible, f.captureErr
}

func (f *originalNativeTerminalFake) Send(ctx context.Context, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.alive {
		return errors.New("immutable UI run is stale")
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sends = append(f.sends, append([]byte(nil), data...))
	return nil
}

func (f *originalNativeTerminalFake) Resize(context.Context, int, int) error { return nil }

func (f *originalNativeTerminalFake) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.alive = false
	return nil
}

func originalNativeHome(t *testing.T, f *originalNativeTerminalFake) (*Home, *session.Instance) {
	t.Helper()
	h := NewHome()
	h.width, h.height, h.initialLoading = 120, 40, false
	t.Cleanup(func() {
		h.exitInsertMode()
		h.cancel()
		if h.storage != nil {
			_ = h.storage.Close()
		}
	})
	i := &session.Instance{
		ID: "11111111-1111-4111-8111-111111111111", Title: "原会话 中文",
		ProjectPath: t.TempDir(), Tool: "codex", Status: session.StatusUnknown,
	}
	if err := i.MarkTerminalBackendPending(); err != nil {
		t.Fatal(err)
	}
	if f != nil {
		f.sessionID, f.runID = i.ID, "22222222-2222-4222-8222-222222222222"
		if err := i.BindTerminalBackend(f); err != nil {
			t.Fatal(err)
		}
	}
	h.instances = []*session.Instance{i}
	h.instanceByID = map[string]*session.Instance{i.ID: i}
	h.groupTree = session.NewGroupTree(h.instances)
	h.rebuildFlatItems()
	for n, item := range h.flatItems {
		if item.Type == session.ItemTypeSession && item.Session == i {
			h.cursor = n
			break
		}
	}
	h.insertBatchDuration = 0
	h.insertOpenKeySender = func(insertTargetRef) (insertKeySender, error) {
		t.Fatal("native UI input tried to open a tmux or remote sender")
		return nil, nil
	}
	return h, i
}

func originalNativePress(t *testing.T, h *Home, msg tea.KeyMsg) *Home {
	t.Helper()
	model, _ := h.Update(msg)
	return model.(*Home)
}

func TestOriginalNativeRuntimePaneCopyUsesVisibleUnicodeScreen(t *testing.T) {
	f := &originalNativeTerminalFake{alive: true, visible: "\x1b[31m你好 世界\x1b[0m  \r\n第二行\t内容  \r\n\r\n"}
	h, i := originalNativeHome(t, f)
	var copied string
	h.paneClipboard = func(text string, _ bool) (*clipboard.CopyResult, error) {
		copied = text
		return &clipboard.CopyResult{LineCount: 2}, nil
	}
	_, cmd := h.handleMainKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'V'}})
	if cmd == nil {
		t.Fatal("original copy-pane action did not dispatch")
	}
	result, ok := cmd().(copyPaneResultMsg)
	if !ok || result.err != nil || result.sessionTitle != i.Title || result.lineCount != 2 {
		t.Fatalf("copy result = %#v", result)
	}
	if copied != "你好 世界\n第二行\t内容" || !reflect.DeepEqual(f.captures, []bool{false}) {
		t.Fatalf("copied %q, capture scope %v", copied, f.captures)
	}
	if i.GetTmuxSession() != nil || i.GetStatusThreadSafe() != session.StatusUnknown {
		t.Fatal("copy exposed a tmux target or inferred an AI activity state")
	}
}

func TestOriginalNativeRuntimePendingCopyShowsBindingError(t *testing.T) {
	h, i := originalNativeHome(t, nil)
	h.paneClipboard = func(string, bool) (*clipboard.CopyResult, error) {
		t.Fatal("pending terminal copied through the clipboard")
		return nil, nil
	}
	result := h.copyVisiblePane(i)().(copyPaneResultMsg)
	if !errors.Is(result.err, session.ErrTerminalBackendPending) {
		t.Fatalf("pending copy error = %v", result.err)
	}
}

func TestOriginalNativeRuntimeInsertDeliversTextEnterAndControls(t *testing.T) {
	f := &originalNativeTerminalFake{alive: true}
	h, i := originalNativeHome(t, f)
	h = originalNativePress(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'I'}})
	if !h.insertMode || h.insertModeSessionID != i.ID || f.observes != 1 {
		t.Fatalf("input not bound: mode=%v target=%q probes=%d error=%v", h.insertMode, h.insertModeSessionID, f.observes, h.err)
	}
	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("你好")},
		{Type: tea.KeySpace},
		{Type: tea.KeyEnter},
		{Type: tea.KeyBackspace},
		{Type: tea.KeyUp},
		{Type: tea.KeyTab},
		{Type: tea.KeyShiftTab},
		{Type: tea.KeyCtrlA},
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyCtrlD},
		{Type: tea.KeyCtrlZ},
		{Type: tea.KeyRunes, Runes: []rune("界"), Alt: true},
	} {
		h = originalNativePress(t, h, msg)
	}
	want := [][]byte{
		[]byte("你好"), []byte(" "), {'\r'}, {0x7f}, []byte("\x1b[A"), {'\t'},
		[]byte("\x1b[Z"), {0x01}, {0x03}, {0x04}, {0x1a}, []byte("\x1b界"),
	}
	if !bytes.Equal(bytes.Join(f.sends, nil), bytes.Join(want, nil)) {
		t.Fatalf("VT input = %q, want byte stream %q", f.sends, bytes.Join(want, nil))
	}
	if h.err != nil || i.GetStatusThreadSafe() != session.StatusUnknown {
		t.Fatalf("terminal input changed AI state or failed: status=%s error=%v", i.GetStatusThreadSafe(), h.err)
	}
	sendsBeforeDetach := len(f.sends)
	h = originalNativePress(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	if h.insertMode || f.stops != 0 || len(f.sends) != sendsBeforeDetach {
		t.Fatal("Esc did not detach without forwarding or stopping the backend")
	}
}

func TestOriginalNativeRuntimeFailedStopDoesNotRebindOrInventStoppedState(t *testing.T) {
	stale := errors.New("stale native stop run")
	f := &originalNativeTerminalFake{alive: true, stopErr: stale, sendErr: stale}
	h, i := originalNativeHome(t, f)
	if !h.enterInsertMode() {
		t.Fatal(h.err)
	}
	if err := i.KillAndWait(); !errors.Is(err, stale) {
		t.Fatalf("stale stop error = %v", err)
	}
	if i.GetStatusThreadSafe() != session.StatusUnknown || !errors.Is(i.TerminalError(), stale) {
		t.Fatal("uncertain stop invented a stopped state or hid its failure")
	}
	h.dispatchInsertKey("still pinned", false)
	if h.err == nil || !errors.Is(h.err, stale) || len(f.sends) != 0 {
		t.Fatalf("stale bound input: error=%v sends=%q", h.err, f.sends)
	}
	if err := i.BindTerminalBackend(&originalNativeTerminalFake{
		alive: true, sessionID: i.ID, runID: "33333333-3333-4333-8333-333333333333",
	}); !errors.Is(err, session.ErrTerminalBackendBound) {
		t.Fatalf("failed stop allowed runtime rebinding: %v", err)
	}
}

func TestOriginalNativeRuntimeInsertRefusesPendingDeadAndFailedProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *originalNativeTerminalFake
		want string
	}{
		{"pending", nil, session.ErrTerminalBackendPending.Error()},
		{"dead", &originalNativeTerminalFake{}, "terminal is not running"},
		{"failed", &originalNativeTerminalFake{alive: true, observeErr: errors.New("stale native run")}, "stale native run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, i := originalNativeHome(t, tc.fake)
			h = originalNativePress(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'I'}})
			if h.insertMode || h.insertKeySender != nil || h.err == nil || !strings.Contains(h.err.Error(), tc.want) {
				t.Fatalf("entry result: mode=%v sender=%T error=%v", h.insertMode, h.insertKeySender, h.err)
			}
			if i.GetStatusThreadSafe() != session.StatusUnknown || i.GetTmuxSession() != nil {
				t.Fatal("failed input entry inferred state or fell through to tmux")
			}
		})
	}
}

func TestOriginalNativeRuntimeInsertPinsOldRunAcrossStopAndRowReplacement(t *testing.T) {
	old := &originalNativeTerminalFake{alive: true}
	h, i := originalNativeHome(t, old)
	if !h.enterInsertMode() {
		t.Fatal(h.err)
	}
	sender := h.insertKeySender
	if err := i.BindTerminalBackend(&originalNativeTerminalFake{
		alive: true, sessionID: i.ID, runID: "33333333-3333-4333-8333-333333333333",
	}); !errors.Is(err, session.ErrTerminalBackendBound) {
		t.Fatalf("existing runtime rebound: %v", err)
	}
	if err := i.KillAndWait(); err != nil {
		t.Fatal(err)
	}
	replacementBackend := &originalNativeTerminalFake{
		alive: true, sessionID: i.ID, runID: "33333333-3333-4333-8333-333333333333",
	}
	replacement := &session.Instance{ID: i.ID, Title: "replacement", Tool: "codex"}
	if err := replacement.MarkTerminalBackendPending(); err != nil {
		t.Fatal(err)
	}
	if err := replacement.BindTerminalBackend(replacementBackend); err != nil {
		t.Fatal(err)
	}
	h.instanceByID[i.ID] = replacement
	h.insertBuf.WriteString("old buffered bytes")
	h.flushInsertBuf()
	if h.err == nil || !strings.Contains(h.err.Error(), "stale") {
		t.Fatalf("retired input error = %v", h.err)
	}
	if len(old.sends) != 0 || len(replacementBackend.sends) != 0 || old.stops != 1 {
		t.Fatal("retired input reached a replacement run")
	}
	h.exitInsertMode()
	if err := sender.SendKeys("late"); err == nil || len(replacementBackend.sends) != 0 || old.stops != 1 {
		t.Fatal("closed input binding sent data or stopped a runtime")
	}
}

func TestOriginalNativeRuntimeInputLimitsAndUnsupportedKeysAreVisible(t *testing.T) {
	f := &originalNativeTerminalFake{alive: true}
	h, _ := originalNativeHome(t, f)
	if !h.enterInsertMode() {
		t.Fatal(h.err)
	}
	h = originalNativePress(t, h, tea.KeyMsg{Type: tea.KeyF5})
	if h.err == nil || !strings.Contains(h.err.Error(), "does not support") || len(f.sends) != 0 {
		t.Fatalf("unsupported key: error=%v sends=%q", h.err, f.sends)
	}
	h.clearError()
	h.dispatchInsertKey(strings.Repeat("x", terminalbackend.MaxInputBytes+1), false)
	if h.err == nil || !strings.Contains(h.err.Error(), "exceeds") || len(f.sends) != 0 {
		t.Fatalf("oversized input: error=%v sends=%q", h.err, f.sends)
	}
	h.clearError()
	h.dispatchInsertKey(strings.Repeat("x", terminalbackend.MaxInputBytes), false)
	if h.err != nil || len(f.sends) != 1 || len(f.sends[0]) != terminalbackend.MaxInputBytes {
		t.Fatal("input at the shared validation limit was not sent")
	}
}

func TestOriginalNativeRuntimeNamedKeyTranslationIsExplicit(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"Down", "\x1b[B"}, {"Left", "\x1b[D"}, {"Right", "\x1b[C"},
		{"Home", "\x1b[H"}, {"End", "\x1b[F"}, {"PPage", "\x1b[5~"},
		{"NPage", "\x1b[6~"}, {"IC", "\x1b[2~"}, {"DC", "\x1b[3~"},
		{"Escape", "\x1b"}, {"M-Enter", "\x1b\r"}, {"M-界", "\x1b界"},
		{"M-C-c", "\x1b\x03"},
	} {
		got, err := terminalInsertNamedKey(tc.key)
		if err != nil || !bytes.Equal(got, []byte(tc.want)) {
			t.Fatalf("key %q = %q, %v; want %q", tc.key, got, err, tc.want)
		}
	}
	for _, key := range []string{"F1", "F20", "C-Up", "M-F1", "unknown"} {
		if data, err := terminalInsertNamedKey(key); err == nil || len(data) != 0 {
			t.Fatalf("unsupported key %q = %q, %v", key, data, err)
		}
	}
}

func TestOriginalNativeRuntimeRawEmbeddedAttachIsExplicitlyUnavailable(t *testing.T) {
	f := &originalNativeTerminalFake{alive: true}
	h, _ := originalNativeHome(t, f)
	h.sessionInput = &SessionInputRouter{}
	if h.enterEmbeddedMode() || h.embeddedMode || h.insertMode || h.err == nil || !strings.Contains(h.err.Error(), "use insert mode") {
		t.Fatalf("embedded native attach result: mode=%v error=%v", h.embeddedMode, h.err)
	}
	if len(f.sends) != 0 || f.stops != 0 {
		t.Fatal("unavailable embedded attach changed the runtime")
	}
}
