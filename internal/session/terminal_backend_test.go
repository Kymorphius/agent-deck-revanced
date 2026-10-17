package session

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalbackend"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: these fakes exercise the original Instance without spawning
// a tmux server, an agent, or a native process. Liveness is not an AI verdict.
type instanceTerminalFake struct {
	mu          sync.Mutex
	binding     TerminalBinding
	observation TerminalObservation
	observeErr  error
	captureErr  error
	sendErr     error
	resizeErr   error
	stopErr     error
	visible     string
	history     string
	observeFn   func(context.Context) (TerminalObservation, error)
	stopFn      func(context.Context) error
	observes    int
	captures    []bool
	sends       [][]byte
	sizes       [][2]int
	stops       int
}

func (f *instanceTerminalFake) Binding() TerminalBinding {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.binding
}

// Embedding the narrower interface intentionally hides Binding(), so a fake
// that can operate a terminal still cannot pass identity admission by accident.
type terminalWithoutBinding struct{ TerminalBackend }

func (f *instanceTerminalFake) Observe(ctx context.Context) (TerminalObservation, error) {
	f.mu.Lock()
	f.observes++
	fn, observation, err := f.observeFn, f.observation, f.observeErr
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return observation, err
}

func (f *instanceTerminalFake) Capture(_ context.Context, full bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captures = append(f.captures, full)
	if full {
		return f.history, f.captureErr
	}
	return f.visible, f.captureErr
}

func (f *instanceTerminalFake) Send(_ context.Context, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, append([]byte(nil), data...))
	return f.sendErr
}

func (f *instanceTerminalFake) Resize(_ context.Context, cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes = append(f.sizes, [2]int{cols, rows})
	return f.resizeErr
}

func (f *instanceTerminalFake) Stop(ctx context.Context) error {
	f.mu.Lock()
	f.stops++
	fn, err := f.stopFn, f.stopErr
	if fn == nil && err == nil {
		f.observation.Alive = false
	}
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return err
}

func (f *instanceTerminalFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observes + len(f.captures) + len(f.sends) + len(f.sizes) + f.stops
}

func terminalSeamInstance(t *testing.T) *Instance {
	t.Helper()
	return &Instance{
		ID:                  "terminal-seam-instance",
		Title:               "原来的会话",
		ProjectPath:         t.TempDir(),
		GroupPath:           "Tools/Native",
		Tool:                "codex",
		Status:              StatusWaiting,
		Command:             "codex",
		Account:             "preserved-account",
		CodexSessionID:      "11111111-2222-4333-8444-555555555555",
		ClaudeSessionID:     "22222222-2222-4333-8444-555555555555",
		ClaudeDetectedAt:    time.Unix(1700000000, 0),
		CreatedAt:           time.Unix(1690000000, 0),
		IsForkAwaitingStart: true,
		ForkStartCommand:    "preserved-fork-command",
	}
}

func markTerminalSeamPending(t *testing.T, i *Instance) {
	t.Helper()
	if err := i.MarkTerminalBackendPending(); err != nil {
		t.Fatal(err)
	}
}

func bindTerminalSeam(t *testing.T, i *Instance, f *instanceTerminalFake) {
	t.Helper()
	f.binding = TerminalBinding{SessionID: i.ID, RunID: "pinned-run"}
	markTerminalSeamPending(t, i)
	if err := i.BindTerminalBackend(f); err != nil {
		t.Fatal(err)
	}
}

// Only fields that launching or stopping could incorrectly clear are compared.
type terminalSeamIdentity struct {
	id, title, path, group, tool, command, account string
	codexID, claudeID, forkCommand                 string
	createdAt, detectedAt                          time.Time
	forkPending                                    bool
}

func terminalIdentity(i *Instance) terminalSeamIdentity {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return terminalSeamIdentity{
		id: i.ID, title: i.Title, path: i.ProjectPath, group: i.GroupPath,
		tool: i.Tool, command: i.Command, account: i.Account,
		codexID: i.CodexSessionID, claudeID: i.ClaudeSessionID,
		forkCommand: i.ForkStartCommand, createdAt: i.CreatedAt,
		detectedAt: i.ClaudeDetectedAt, forkPending: i.IsForkAwaitingStart,
	}
}

func TestInstanceTerminalBackendCannotHideOrReplaceLegacyTarget(t *testing.T) {
	i := terminalSeamInstance(t)
	i.tmuxSession = tmux.NewSession("inert-legacy-object", i.ProjectPath)
	legacy := i.tmuxSession
	before, status, revision := terminalIdentity(i), i.GetStatusThreadSafe(), i.stopRevision
	if i.UsesTerminalBackend() || i.GetTmuxSession() != legacy {
		t.Fatal("unbound original instance lost its legacy target")
	}
	if err := i.MarkTerminalBackendPending(); err == nil {
		t.Fatal("legacy tmux target was marked for native replacement")
	}
	f := &instanceTerminalFake{binding: TerminalBinding{SessionID: i.ID, RunID: "pinned-run"}}
	if err := i.BindTerminalBackend(f); err == nil {
		t.Fatal("legacy tmux target accepted a native binding")
	}
	if i.UsesTerminalBackend() || i.GetTmuxSession() != legacy || i.tmuxSession != legacy || i.GetStatusThreadSafe() != status || i.stopRevision != revision || i.TerminalError() != nil || terminalIdentity(i) != before || f.callCount() != 0 {
		t.Fatal("rejected native admission changed or hid the legacy target")
	}
}

func TestInstanceTerminalBackendBindingIsExplicitAndImmutable(t *testing.T) {
	i := terminalSeamInstance(t)
	var typedNil *instanceTerminalFake
	for _, backend := range []TerminalBackend{nil, typedNil} {
		if err := i.BindTerminalBackend(backend); err == nil {
			t.Fatal("nil terminal backend was accepted")
		}
		if i.UsesTerminalBackend() || i.tmuxSession != nil || i.GetStatusThreadSafe() != StatusWaiting {
			t.Fatal("rejected binding changed the original instance")
		}
	}

	markTerminalSeamPending(t, i)
	if !i.UsesTerminalBackend() || i.GetTmuxSession() != nil || i.tmuxSession != nil {
		t.Fatal("explicit native pending target acquired a tmux route")
	}
	if got := i.GetStatusThreadSafe(); got != StatusUnknown {
		t.Fatalf("pending status = %q, want unknown", got)
	}
	f := &instanceTerminalFake{observation: TerminalObservation{Alive: true, RunID: "pinned-run"}}
	bindTerminalSeam(t, i, f)
	before := terminalIdentity(i)
	for _, replacement := range []TerminalBackend{f, &instanceTerminalFake{binding: f.binding}} {
		if err := i.BindTerminalBackend(replacement); err == nil {
			t.Fatal("bound target was replaced")
		}
	}
	markTerminalSeamPending(t, i)
	if !i.Exists() || i.tmuxSession != nil || i.GetTmuxSession() != nil || terminalIdentity(i) != before {
		t.Fatal("repeated pending/binding operation replaced the pinned target")
	}
}

func TestInstanceTerminalBackendRejectsMissingOrForeignBindingWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
		setup   func(*Instance, *instanceTerminalFake) TerminalBackend
	}{
		{"unmarked-native", false, func(_ *Instance, f *instanceTerminalFake) TerminalBackend { return f }},
		{"missing-bound-interface", true, func(_ *Instance, f *instanceTerminalFake) TerminalBackend { return terminalWithoutBinding{f} }},
		{"missing-session-identity", true, func(_ *Instance, f *instanceTerminalFake) TerminalBackend { f.binding.SessionID = ""; return f }},
		{"foreign-session-identity", true, func(_ *Instance, f *instanceTerminalFake) TerminalBackend {
			f.binding.SessionID = "foreign-session"
			return f
		}},
		{"empty-run-identity", true, func(_ *Instance, f *instanceTerminalFake) TerminalBackend { f.binding.RunID = ""; return f }},
		{"blank-run-identity", true, func(_ *Instance, f *instanceTerminalFake) TerminalBackend { f.binding.RunID = " \t\n"; return f }},
		{"empty-instance-identity", true, func(i *Instance, f *instanceTerminalFake) TerminalBackend {
			i.ID = ""
			f.binding.SessionID = ""
			return f
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := terminalSeamInstance(t)
			f := &instanceTerminalFake{binding: TerminalBinding{SessionID: i.ID, RunID: "pinned-run"}}
			candidate := tc.setup(i, f)
			if tc.pending {
				markTerminalSeamPending(t, i)
			}
			before, status := terminalIdentity(i), i.GetStatusThreadSafe()
			expected, revision, cachedError := i.UsesTerminalBackend(), i.stopRevision, i.TerminalError()
			if err := i.BindTerminalBackend(candidate); err == nil {
				t.Fatal("invalid or unadmitted terminal identity was bound")
			}
			if terminalIdentity(i) != before || i.GetStatusThreadSafe() != status || i.UsesTerminalBackend() != expected || i.stopRevision != revision || i.TerminalError() != cachedError || i.terminalBackend != nil || i.tmuxSession != nil || i.GetTmuxSession() != nil || f.callCount() != 0 {
				t.Fatal("rejected identity changed pending state, routing, or terminal target")
			}
		})
	}
}

func TestInstanceTerminalBackendRoutesCapturesAndRawInput(t *testing.T) {
	i := terminalSeamInstance(t)
	f := &instanceTerminalFake{
		observation: TerminalObservation{Alive: true, PID: 123, HostPID: 456, RunID: "pinned-run", OutputSequence: 9},
		visible:     "first\nsecond\nthird\n第四行\n",
		history:     "retained scrollback\nfirst\nsecond\nthird\n第四行\n",
	}
	bindTerminalSeam(t, i, f)
	ctx := context.Background()
	observation, err := i.ObserveTerminal(ctx)
	if err != nil || observation != f.observation {
		t.Fatalf("observation = %#v, %v", observation, err)
	}
	if !i.Exists() {
		t.Fatal("alive native target did not exist")
	}
	for _, tc := range []struct {
		name string
		call func() (string, error)
		want string
	}{
		{"visible", func() (string, error) { return i.CaptureTerminal(ctx, false) }, f.visible},
		{"history", func() (string, error) { return i.CaptureTerminal(ctx, true) }, f.history},
		{"preview", i.Preview, "second\nthird\n第四行"},
		{"full", i.PreviewFull, f.history},
		{"primary-full", i.PreviewPrimaryFull, f.history},
		{"window-zero", func() (string, error) { return i.PreviewWindowFull(0) }, f.history},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.call()
			if err != nil || got != tc.want {
				t.Fatalf("capture = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	before := f.callCount()
	for _, index := range []int{-1, 1, 8} {
		if _, err := i.PreviewWindowFull(index); !errors.Is(err, ErrTerminalWindowUnsupported) {
			t.Fatalf("window %d error = %v", index, err)
		}
	}
	if f.callCount() != before {
		t.Fatal("unsupported window reached the native backend")
	}
	input := []byte("中文\x00\x03\x1b[A\r\n")
	if err := i.SendTerminal(ctx, input); err != nil {
		t.Fatal(err)
	}
	if err := i.ResizeTerminal(ctx, 123, 35); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.captures, []bool{false, true, false, true, true, true}) || len(f.sends) != 1 || !bytes.Equal(f.sends[0], input) || !reflect.DeepEqual(f.sizes, [][2]int{{123, 35}}) {
		t.Fatalf("incorrect routing: captures=%v sends=%q sizes=%v", f.captures, f.sends, f.sizes)
	}
}

func TestInstanceTerminalBackendRejectsCancelledAndInvalidOperationsBeforeDispatch(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "pending"}[pending], func(t *testing.T) {
			i := terminalSeamInstance(t)
			f := &instanceTerminalFake{}
			if pending {
				markTerminalSeamPending(t, i)
			} else {
				bindTerminalSeam(t, i, f)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			for _, operation := range []func() error{
				func() error { _, err := i.ObserveTerminal(ctx); return err },
				func() error { _, err := i.CaptureTerminal(ctx, false); return err },
				func() error { return i.SendTerminal(ctx, make([]byte, terminalbackend.MaxInputBytes+1)) },
				func() error { return i.ResizeTerminal(ctx, 0, 0) },
			} {
				if err := operation(); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled operation error = %v, want context.Canceled", err)
				}
			}
			if f.callCount() != 0 {
				t.Fatal("cancelled operation reached backend")
			}
		})
	}
	i := terminalSeamInstance(t)
	f := &instanceTerminalFake{}
	bindTerminalSeam(t, i, f)
	for _, size := range [][2]int{{0, 1}, {1, 0}, {-1, 1}, {501, 1}, {1, 301}} {
		if err := i.ResizeTerminal(context.Background(), size[0], size[1]); err == nil {
			t.Fatalf("invalid size %v was accepted", size)
		}
	}
	if err := i.SendTerminal(context.Background(), make([]byte, terminalbackend.MaxInputBytes+1)); err == nil {
		t.Fatal("oversized terminal input was accepted")
	}
	if f.callCount() != 0 {
		t.Fatal("invalid operation reached backend")
	}
	if err := i.SendTerminal(context.Background(), make([]byte, terminalbackend.MaxInputBytes)); err != nil {
		t.Fatalf("maximum input rejected: %v", err)
	}
	for _, size := range [][2]int{{1, 1}, {500, 300}} {
		if err := i.ResizeTerminal(context.Background(), size[0], size[1]); err != nil {
			t.Fatalf("boundary size %v rejected: %v", size, err)
		}
	}
}

func TestInstanceTerminalBackendPendingCannotFallThroughToTmux(t *testing.T) {
	i := terminalSeamInstance(t)
	legacy := i.tmuxSession
	markTerminalSeamPending(t, i)
	before := terminalIdentity(i)
	for _, operation := range []func() error{
		func() error { _, err := i.ObserveTerminal(context.Background()); return err },
		func() error { _, err := i.CaptureTerminal(context.Background(), true); return err },
		func() error { return i.SendTerminal(context.Background(), []byte("hello")) },
		func() error { return i.ResizeTerminal(context.Background(), 80, 24) },
		func() error { _, err := i.Preview(); return err },
		func() error { _, err := i.PreviewFull(); return err },
		func() error { _, err := i.PreviewPrimaryFull(); return err },
		func() error { _, err := i.PreviewWindowFull(0); return err },
		i.Kill,
		i.KillAndWait,
	} {
		if err := operation(); !errors.Is(err, ErrTerminalBackendPending) {
			t.Fatalf("pending operation error = %v", err)
		}
	}
	if i.Exists() || i.HasUpdated() {
		t.Fatal("pending target claimed a live process or new output")
	}
	if err := i.UpdateStatus(); !errors.Is(err, ErrTerminalBackendPending) {
		t.Fatalf("pending status observation = %v", err)
	}
	if i.GetStatusThreadSafe() != StatusUnknown || i.tmuxSession != legacy || i.GetTmuxSession() != nil || terminalIdentity(i) != before {
		t.Fatal("pending route changed state or exposed legacy target")
	}
}

func TestInstanceTerminalBackendOutputWatermarkAndHonestStatus(t *testing.T) {
	i := terminalSeamInstance(t)
	f := &instanceTerminalFake{observation: TerminalObservation{Alive: true, RunID: "pinned-run"}}
	bindTerminalSeam(t, i, f)
	before := terminalIdentity(i)
	if i.HasUpdated() {
		t.Fatal("initial zero watermark claimed new output")
	}
	for _, sequence := range []uint64{1, 42} {
		f.mu.Lock()
		f.observation.OutputSequence = sequence
		f.mu.Unlock()
		if !i.HasUpdated() || i.HasUpdated() {
			t.Fatalf("output sequence %d was not consumed exactly once", sequence)
		}
	}
	if err := i.UpdateStatus(); err != nil {
		t.Fatal(err)
	}
	if got := i.GetStatusThreadSafe(); got != StatusUnknown {
		t.Fatalf("alive process created AI status %q; want unknown", got)
	}
	if terminalIdentity(i) != before {
		t.Fatal("terminal facts changed existing conversation identity")
	}
}

func TestInstanceTerminalBackendFailuresRemainVisibleWithoutInventingStatus(t *testing.T) {
	failure := errors.New("verified native transport unavailable")
	i := terminalSeamInstance(t)
	f := &instanceTerminalFake{
		observeErr: failure,
		captureErr: failure,
		sendErr:    failure,
		resizeErr:  failure,
	}
	bindTerminalSeam(t, i, f)
	before := terminalIdentity(i)
	for _, operation := range []func() error{
		i.UpdateStatus,
		func() error { _, err := i.ObserveTerminal(context.Background()); return err },
		func() error { _, err := i.Preview(); return err },
		func() error { _, err := i.PreviewFull(); return err },
		func() error { return i.SendTerminal(context.Background(), []byte("raw input")) },
		func() error { return i.ResizeTerminal(context.Background(), 80, 24) },
	} {
		if err := operation(); !errors.Is(err, failure) || !errors.Is(i.TerminalError(), failure) {
			t.Fatalf("native failure was hidden: return=%v cached=%v", err, i.TerminalError())
		}
	}
	if i.Exists() || i.HasUpdated() || i.GetStatusThreadSafe() != StatusUnknown || terminalIdentity(i) != before {
		t.Fatal("failed transport observation invented liveness, activity, or a stopped state")
	}
}

func TestInstanceTerminalBackendLaunchGuardsPreserveBinding(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "pending"}[pending], func(t *testing.T) {
			i := terminalSeamInstance(t)
			legacy := i.tmuxSession
			f := &instanceTerminalFake{observation: TerminalObservation{Alive: true}}
			if pending {
				markTerminalSeamPending(t, i)
			} else {
				bindTerminalSeam(t, i, f)
			}
			before := terminalIdentity(i)
			generation, revision, invalidated := i.spawnGen.Load(), i.stopRevision, i.runtimeInvalidatedAt
			for _, operation := range []func() error{
				i.Start,
				func() error { return i.StartWithMessage("must not launch or send") },
				func() error { return i.startWithClaudeAdmission(nil, false) },
				i.Restart,
				func() error { return i.RestartWithEnv(map[string]string{"PRESERVED": "value"}) },
				i.RestartFresh,
			} {
				if err := operation(); !errors.Is(err, ErrTerminalLaunchUnsupported) {
					t.Fatalf("launch error = %v, want ErrTerminalLaunchUnsupported", err)
				}
				if terminalIdentity(i) != before || i.spawnGen.Load() != generation || i.stopRevision != revision || i.runtimeInvalidatedAt != invalidated || i.tmuxSession != legacy || !i.UsesTerminalBackend() || i.GetStatusThreadSafe() != StatusUnknown {
					t.Fatal("unsupported launch changed the pinned target or its identity")
				}
			}
			if i.CanRestart() || i.CanRestartFresh() || i.CanRestartGeneric() || i.CanFork() {
				t.Fatal("native target advertised legacy launch capability")
			}
			if f.callCount() != 0 {
				t.Fatal("launch guard dispatched a terminal operation")
			}
		})
	}
}

func TestInstanceTerminalBackendDirectForksCannotCreateLegacyTargets(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "pending"}[pending], func(t *testing.T) {
			i := terminalSeamInstance(t)
			f := &instanceTerminalFake{}
			if pending {
				markTerminalSeamPending(t, i)
			} else {
				bindTerminalSeam(t, i, f)
			}
			before := terminalIdentity(i)
			for _, operation := range []func() (*Instance, string, error){
				func() (*Instance, string, error) { command, err := i.Fork("child", "Other"); return nil, command, err },
				func() (*Instance, string, error) {
					command, err := i.ForkWithOptions("child", "Other", nil)
					return nil, command, err
				},
				func() (*Instance, string, error) { return i.CreateForkedInstance("child", "Other") },
				func() (*Instance, string, error) { return i.CreateForkedInstanceWithOptions("child", "Other", nil) },
				func() (*Instance, string, error) { return i.CreateForkedInstanceForTool("child", "Other", nil) },
				func() (*Instance, string, error) {
					command, err := i.ForkOpenCode("child", "Other")
					return nil, command, err
				},
				func() (*Instance, string, error) {
					command, err := i.ForkOpenCodeWithOptions("child", "Other", nil)
					return nil, command, err
				},
				func() (*Instance, string, error) { return i.CreateForkedOpenCodeInstance("child", "Other") },
				func() (*Instance, string, error) {
					return i.CreateForkedOpenCodeInstanceWithOptions("child", "Other", nil)
				},
				func() (*Instance, string, error) {
					return i.CreateForkedOpenCodeInstanceWithOptionsAndWorkDir("child", "Other", nil, "missing-worktree", "missing-repo", "branch")
				},
				func() (*Instance, string, error) { return i.CreateForkedPiInstance("child", "Other") },
				func() (*Instance, string, error) { return i.CreateForkedPiInstanceWithOptions("child", "Other", nil) },
				func() (*Instance, string, error) {
					return i.CreateForkedCodexInstanceWithOptions("child", "Other", nil)
				},
				func() (*Instance, string, error) { return i.CreateForkedOMPInstanceWithOptions("child", "Other", nil) },
			} {
				target, command, err := operation()
				if !errors.Is(err, ErrTerminalLaunchUnsupported) || target != nil || command != "" {
					t.Fatalf("direct fork created a target or legacy command: target=%v command=%q error=%v", target, command, err)
				}
				if terminalIdentity(i) != before || f.callCount() != 0 {
					t.Fatal("direct fork changed the pinned source")
				}
			}
			for _, tool := range []string{"claude", "codex", "opencode", "pi", "omp"} {
				i.Tool = tool
				if i.CanFork() || i.CanForkCodex() || i.CanForkOpenCode() || i.CanForkPi() || i.CanForkOMP() {
					t.Fatalf("%s native target advertised a legacy fork capability", tool)
				}
				if target, command, err := i.CreateForkedInstanceForTool("child", "Other", nil); !errors.Is(err, ErrTerminalLaunchUnsupported) || target != nil || command != "" {
					t.Fatalf("%s tool dispatcher bypassed launch guard", tool)
				}
			}
		})
	}
}

func TestInstanceTerminalBackendStopFailurePreservesIdentityAndState(t *testing.T) {
	stopErr := errors.New("owned process tree teardown is unconfirmed")
	for _, wait := range []bool{false, true} {
		t.Run(map[bool]string{false: "kill", true: "kill-and-wait"}[wait], func(t *testing.T) {
			i := terminalSeamInstance(t)
			f := &instanceTerminalFake{observation: TerminalObservation{Alive: true, RunID: "pinned-run"}, stopErr: stopErr}
			bindTerminalSeam(t, i, f)
			before, status := terminalIdentity(i), i.GetStatusThreadSafe()
			var err error
			if wait {
				err = i.KillAndWait()
			} else {
				err = i.Kill()
			}
			if !errors.Is(err, stopErr) || !errors.Is(i.TerminalError(), stopErr) {
				t.Fatalf("stop error was hidden: return=%v cached=%v", err, i.TerminalError())
			}
			if i.GetStatusThreadSafe() != status || terminalIdentity(i) != before || !i.Exists() || f.stops != 1 {
				t.Fatal("failed teardown claimed stopped or discarded the pinned identity")
			}
		})
	}
}

func TestInstanceTerminalBackendSuccessfulStopPreservesConversationIdentity(t *testing.T) {
	for _, wait := range []bool{false, true} {
		t.Run(map[bool]string{false: "kill", true: "kill-and-wait"}[wait], func(t *testing.T) {
			i := terminalSeamInstance(t)
			f := &instanceTerminalFake{observation: TerminalObservation{Alive: true, RunID: "pinned-run"}}
			bindTerminalSeam(t, i, f)
			before := terminalIdentity(i)
			var err error
			if wait {
				err = i.KillAndWait()
			} else {
				err = i.Kill()
			}
			if err != nil || i.GetStatusThreadSafe() != StatusStopped || i.Exists() || terminalIdentity(i) != before || f.stops != 1 {
				t.Fatalf("successful teardown did not stop only the terminal: error=%v status=%q", err, i.GetStatusThreadSafe())
			}
			if err := i.KillAndWait(); err != nil || f.stops != 1 {
				t.Fatalf("confirmed stop dispatched a second teardown: error=%v calls=%d", err, f.stops)
			}
			if err := i.BindTerminalBackend(&instanceTerminalFake{binding: f.binding}); !errors.Is(err, ErrTerminalBackendBound) {
				t.Fatalf("stopped target silently followed a replacement run: %v", err)
			}
		})
	}
}

func TestInstanceTerminalBackendDelayedObservationCannotUndoSuccessfulStop(t *testing.T) {
	for _, staleErr := range []error{nil, errors.New("delayed observation failure")} {
		name := "stale-alive"
		if staleErr != nil {
			name = "stale-error"
		}
		t.Run(name, func(t *testing.T) {
			i := terminalSeamInstance(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			f := &instanceTerminalFake{observation: TerminalObservation{Alive: true, RunID: "pinned-run"}}
			f.observeFn = func(ctx context.Context) (TerminalObservation, error) {
				close(entered)
				select {
				case <-release:
					return TerminalObservation{Alive: true, RunID: "pinned-run"}, staleErr
				case <-ctx.Done():
					return TerminalObservation{}, ctx.Err()
				}
			}
			bindTerminalSeam(t, i, f)
			observed := make(chan error, 1)
			go func() { observed <- i.UpdateStatus() }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("status observation did not enter backend")
			}
			stopped := make(chan error, 1)
			go func() { stopped <- i.KillAndWait() }()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("delayed observation held Instance lock and blocked stop")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-observed:
			case <-time.After(2 * time.Second):
				t.Fatal("delayed observation did not finish")
			}
			if i.GetStatusThreadSafe() != StatusStopped || i.TerminalError() != nil {
				t.Fatalf("stale observation undid successful stop: status=%q error=%v", i.GetStatusThreadSafe(), i.TerminalError())
			}
		})
	}
}
