package session

// matrix/hardened: native rows pass through the original storage and Instance
// APIs. Fixtures own synthetic processes and never contact a real AI session.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/nativefleet"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/nativeterminal"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
)

func originalNativeStore(t *testing.T) (*nativestatestore.Store, *Storage, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "shared")
	store, err := nativestatestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	db, err := statedb.Open(filepath.Join(dir, nativestatestore.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, &Storage{db: db, profile: "native-integration"}, root
}

func originalNativeLaunch(root string) terminalhost.LaunchSpec {
	return terminalhost.LaunchSpec{Executable: "fixture.exe", Dir: root, Cols: 80, Rows: 24, Env: []string{"AD_EXPLICIT=中文"}}
}

func TestOriginalStorageHydratesNativeDescriptorAndPreservesLegacy(t *testing.T) {
	store, storage, root := originalNativeStore(t)
	record, err := store.CreateInGroup("原生工作", "shell", "研发/终端", originalNativeLaunch(root))
	if err != nil {
		t.Fatal(err)
	}
	legacy := &statedb.InstanceRow{ID: "12345678-1700000000", Title: "已有会话", ProjectPath: root, GroupPath: "研发", Tool: "codex", Status: "stopped", TmuxSession: "existing-fixture", CreatedAt: time.Now(), Account: "kept-account", Pin: "top", ToolData: json.RawMessage(`{"custom_extra":"keep"}`)}
	if err := storage.db.SaveInstance(legacy); err != nil {
		t.Fatal(err)
	}
	for _, loadLite := range []bool{true, false} {
		if loadLite {
			rows, _, err := storage.LoadLite()
			if err != nil || len(rows) != 2 {
				t.Fatalf("lite=%+v error=%v", rows, err)
			}
			for _, row := range rows {
				if row.ID == record.ID && len(row.NativeBackend) == 0 {
					t.Fatal("lite lost native marker")
				}
			}
			continue
		}
		instances, _, err := storage.LoadWithGroups()
		if err != nil || len(instances) != 2 {
			t.Fatalf("instances=%+v error=%v", instances, err)
		}
		for _, inst := range instances {
			if inst.ID == legacy.ID {
				if inst.UsesTerminalBackend() || inst.GetTmuxSession() == nil || inst.Account != legacy.Account || inst.Pin != PinTop {
					t.Fatal("legacy identity or metadata changed")
				}
				continue
			}
			if inst.ID != record.ID || !inst.UsesTerminalBackend() || inst.GetTmuxSession() != nil || inst.Title != record.Name || inst.GroupPath != record.Group {
				t.Fatal("native row was not hydrated through original model")
			}
			if !errors.Is(inst.Start(), ErrTerminalLaunchUnsupported) {
				t.Fatal("unbound descriptor entered old launch path")
			}
			if _, err := inst.PreviewFull(); !errors.Is(err, ErrTerminalBackendPending) {
				t.Fatalf("unbound preview=%v", err)
			}
			row, err := instanceToRow(inst)
			if err != nil {
				t.Fatal(err)
			}
			if row.TmuxSession != "" {
				t.Fatal("native serialization invented tmux identity")
			}
			if _, native, err := nativestatestore.RecordFromRow(row); err != nil || !native {
				t.Fatalf("native round trip=%v %v", native, err)
			}
			if err := storage.Save([]*Instance{inst}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := storage.db.LoadInstances()
	if err != nil || len(rows) != 2 {
		t.Fatalf("row count after native save=%d %v", len(rows), err)
	}
	for _, row := range rows {
		if row.ID == legacy.ID && (!bytes.Contains(row.ToolData, []byte("custom_extra")) || row.Account != legacy.Account) {
			t.Fatal("native save changed neighboring legacy row")
		}
	}
}

func TestOriginalStorageRejectsDeclaredMalformedNativeWithoutTmuxFallback(t *testing.T) {
	for _, raw := range []string{`null`, `{"version":99,"kind":"conpty"}`, `{"version":1,"kind":"other"}`} {
		t.Run(raw, func(t *testing.T) {
			_, storage, root := originalNativeStore(t)
			row := &statedb.InstanceRow{ID: "12345678-1700000000", Title: "bad", ProjectPath: root, GroupPath: "my-sessions", Tool: "shell", Status: "stopped", TmuxSession: "must-not-reconnect", CreatedAt: time.Now(), ToolData: json.RawMessage(`{"native_backend":` + raw + `}`)}
			if err := storage.db.SaveInstance(row); err != nil {
				t.Fatal(err)
			}
			if _, _, err := storage.LoadLite(); err == nil {
				t.Fatal("lite accepted invalid backend")
			}
			if _, _, err := storage.LoadWithGroups(); err == nil {
				t.Fatal("full load accepted invalid backend")
			}
		})
	}
}

type originalNativeProcess struct {
	mu    sync.Mutex
	input bytes.Buffer
	outR  *io.PipeReader
	outW  *io.PipeWriter
	done  chan struct{}
	once  sync.Once
}

func newOriginalNativeProcess() *originalNativeProcess {
	r, w := io.Pipe()
	return &originalNativeProcess{outR: r, outW: w, done: make(chan struct{})}
}
func (p *originalNativeProcess) PID() uint32      { return 123 }
func (p *originalNativeProcess) Input() io.Writer { return p }
func (p *originalNativeProcess) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.Write(data)
}
func (p *originalNativeProcess) Output() io.Reader     { return p.outR }
func (p *originalNativeProcess) Resize(int, int) error { return nil }
func (p *originalNativeProcess) Wait() (uint32, error) { <-p.done; return 0, nil }
func (p *originalNativeProcess) Close() error {
	p.once.Do(func() { _ = p.outW.Close(); close(p.done) })
	return nil
}

func TestOriginalInstanceUsesSharedNativeRuntimeForPreviewInputAndStop(t *testing.T) {
	store, storage, root := originalNativeStore(t)
	process := newOriginalNativeProcess()
	server, err := nativefleet.New(store, func(terminalhost.LaunchSpec) (terminalhost.Process, error) { return process, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	launch := originalNativeLaunch(root)
	created := server.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "共享运行", Tool: "shell", Group: "研发", Launch: &launch, AutoStart: true})
	if !created.OK || len(created.Sessions) != 1 {
		t.Fatalf("create=%+v", created)
	}
	info := created.Sessions[0]
	instances, _, err := storage.LoadWithGroups()
	if err != nil || len(instances) != 1 {
		t.Fatalf("load=%+v error=%v", instances, err)
	}
	inst := instances[0]
	terminal, err := nativeterminal.NewWithCaller(winipc.PipePrefix+"original-instance-fixture", info.ID, info.RunID, func(_ context.Context, _ string, request terminalhost.Request) (terminalhost.Response, error) {
		return server.Dispatch(request), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.BindTerminalBackend(terminal); err != nil {
		t.Fatal(err)
	}
	if inst.ID != info.ID || inst.GetTmuxSession() != nil {
		t.Fatal("binding replaced original identity")
	}
	if _, err := process.outW.Write([]byte("共享预览 中文\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		preview, err := inst.PreviewFull()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(preview, "共享预览 中文") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("original preview never received native output: %q", preview)
		}
		time.Sleep(time.Millisecond)
	}
	inst.UpdateStatus()
	if inst.GetStatusThreadSafe() != StatusUnknown || !inst.Exists() {
		t.Fatal("process liveness became an AI activity verdict")
	}
	data := []byte("输入 中文\r")
	if err := inst.SendTerminal(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	process.mu.Lock()
	got := append([]byte(nil), process.input.Bytes()...)
	process.mu.Unlock()
	if !bytes.Equal(got, data) {
		t.Fatalf("input changed: %q", got)
	}
	if err := inst.ResizeTerminal(context.Background(), 100, 30); err != nil {
		t.Fatal(err)
	}
	if err := inst.KillAndWait(); err != nil {
		t.Fatal(err)
	}
	if inst.GetStatusThreadSafe() != StatusStopped || inst.Exists() {
		t.Fatal("original stop did not acknowledge exact native runtime teardown")
	}
	if err := inst.Restart(); !errors.Is(err, ErrTerminalLaunchUnsupported) {
		t.Fatalf("restart reached legacy path: %v", err)
	}
}
