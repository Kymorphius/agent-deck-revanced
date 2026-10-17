// matrix/hardened: groups change durable descriptions only; these tests bind
// process generations and verify the launcher and project metadata stay intact.
package nativefleet

import (
	"reflect"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

func TestGroupCreateAndMovePreserveLiveProcessAndLaunch(t *testing.T) {
	l := &launcher{}
	s, store, _ := fixture(t, l.launch)
	address, _ := serve(t, s)
	spec := launchSpec()
	spec.Args = append(spec.Args, "exact argument 中文")
	spec.Env = []string{"NATIVE_GROUP_TEST=launch override"}
	created := call(t, address, terminalhost.Request{Op: "fleet-create", Name: "分组会话", Tool: "shell", Group: " 研究/原生 ", Launch: &spec, AutoStart: true})
	before, err := store.Get(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Group != "研究/原生" || len(created.Sessions) != 1 || created.Sessions[0].Group != before.Group {
		t.Fatalf("create did not return its canonical group: %+v %+v", created, before)
	}
	move := forRun("fleet-move", created)
	move.Group = "工具/Windows"
	moved := call(t, address, move)
	after, err := store.Get(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Group != move.Group || len(moved.Sessions) != 1 || moved.Sessions[0].Group != move.Group {
		t.Fatalf("move lost registry or wire group: %+v %+v", moved, after)
	}
	if moved.SessionID != created.SessionID || moved.RunID != created.RunID || moved.PID != created.PID || !moved.Running || l.count() != 1 {
		t.Fatalf("move replaced or stopped the live process: %+v -> %+v", created, moved)
	}
	if after.ID != before.ID || after.Name != before.Name || after.Tool != before.Tool || !after.CreatedAt.Equal(before.CreatedAt) || !reflect.DeepEqual(after.Launch, before.Launch) {
		t.Fatalf("move changed immutable launch metadata: %+v -> %+v", before, after)
	}
	listed := call(t, address, terminalhost.Request{Op: "fleet-list"})
	if len(listed.Sessions) != 1 || listed.Sessions[0].Group != move.Group || listed.Sessions[0].RunID != created.RunID {
		t.Fatalf("list did not reflect the same grouped run: %+v", listed)
	}
	input := forRun("send", moved)
	input.Data = []byte("after move 中文\r")
	requireOK(t, s.Dispatch(input))
	if l.latest().received() != string(input.Data) {
		t.Fatal("moving broke input to the original process")
	}
}

func TestInvalidGroupCreationDoesNotSaveOrLaunch(t *testing.T) {
	l := &launcher{}
	s, store, _ := fixture(t, l.launch)
	spec := launchSpec()
	for _, group := range []string{" ", "\t", "research\n", "bad\x00group", strings.Repeat("组", 65), strings.Repeat("😀", 65)} {
		r := s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "must not save", Tool: "shell", Group: group, Launch: &spec, AutoStart: true})
		if r.OK || r.SessionID != "" || r.RunID != "" || len(r.Sessions) != 0 {
			t.Fatalf("invalid group creation returned a saved run for %q: %+v", group, r)
		}
	}
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	listed := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-list"}))
	if len(records) != 0 || len(listed.Sessions) != 0 || l.count() != 0 {
		t.Fatalf("bad creates saved or launched: records=%d sessions=%d launches=%d", len(records), len(listed.Sessions), l.count())
	}
}

func TestInvalidGroupMoveLeavesDurableAndCachedRecordUnchanged(t *testing.T) {
	l := &launcher{}
	s, store, _ := fixture(t, l.launch)
	created := create(t, s, "valid default", true)
	before, err := store.Get(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{" ", "bad\x1bgroup", strings.Repeat("组", 65)} {
		move := forRun("fleet-move", created)
		move.Group = group
		r := s.Dispatch(move)
		if r.OK || len(r.Sessions) != 1 || r.Sessions[0].Group != before.Group {
			t.Fatalf("failed move changed its returned description: %+v", r)
		}
		after, err := store.Get(created.SessionID)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("failed move changed persisted metadata: before=%+v after=%+v err=%v", before, after, err)
		}
	}
	listed := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-list"}))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Group != before.Group || listed.Sessions[0].RunID != created.RunID || listed.Sessions[0].PID != created.PID || l.count() != 1 {
		t.Fatalf("failed move changed cached metadata or runtime: %+v", listed)
	}
}

func TestGroupsPersistWithoutAutomaticRestartAndDefaultAPIRemainsValid(t *testing.T) {
	l := &launcher{}
	s, _, root := fixture(t, l.launch)
	created := create(t, s, "persist grouped", true)
	move := forRun("fleet-move", created)
	move.Group = "持久分组"
	requireOK(t, s.Dispatch(move))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(reopened, l.launch)
	if err != nil {
		_ = reopened.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	listed := requireOK(t, second.Dispatch(terminalhost.Request{Op: "fleet-list"}))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Group != move.Group || listed.Sessions[0].State != "stopped" || listed.Sessions[0].RunID != "" || l.count() != 1 {
		t.Fatalf("group recovery restarted or changed the session: %+v", listed)
	}
	move.RunID, move.Group = "", ""
	returned := requireOK(t, second.Dispatch(move))
	if len(returned.Sessions) != 1 || returned.Sessions[0].Group != "" || l.count() != 1 {
		t.Fatalf("move to default group failed or launched a process: %+v", returned)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	record, err := legacy.Create("legacy API", "shell", launchSpec())
	if err != nil || record.Group != "" {
		t.Fatalf("old create API lost default group: %+v %v", record, err)
	}
}

func TestPowerShellGroupedDescriptionPreservesExplicitToolOnReopen(t *testing.T) {
	l := &launcher{}
	s, store, root := fixture(t, l.launch)
	spec := launchSpec()
	spec.Executable = "powershell.exe"
	spec.Args = []string{"-NoLogo", "-NoProfile"}
	created := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "PowerShell 中文", Tool: "powershell", Group: "Windows", Launch: &spec, AutoStart: true}))
	if len(created.Sessions) != 1 || created.Sessions[0].Tool != "powershell" || l.count() != 1 {
		t.Fatalf("explicit PowerShell tool was not returned: %+v", created)
	}
	move := forRun("fleet-move", created)
	move.Group = "工具/PowerShell"
	requireOK(t, s.Dispatch(move))
	before, err := store.Get(created.SessionID)
	if err != nil || before.Tool != "powershell" || !reflect.DeepEqual(before.Launch, spec) {
		t.Fatalf("move inferred a different tool or launch: %+v, %v", before, err)
	}
	requireOK(t, s.Dispatch(forRun("fleet-stop", created)))
	if after, err := store.Get(created.SessionID); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("stop rewrote grouped PowerShell description: %+v, %v", after, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(reopened, l.launch)
	if err != nil {
		_ = reopened.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	listed := requireOK(t, second.Dispatch(terminalhost.Request{Op: "fleet-list"}))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Tool != "powershell" || listed.Sessions[0].Group != move.Group || listed.Sessions[0].State != "stopped" || l.count() != 1 {
		t.Fatalf("reopen inferred tool or auto-launched PowerShell: %+v", listed)
	}
	if after, err := reopened.Get(created.SessionID); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("reopen rewrote explicit PowerShell launch: %+v, %v", after, err)
	}
}

func TestStaleGroupMoveDoesNotTouchNewRunOrDescription(t *testing.T) {
	l := &launcher{}
	s, store, _ := fixture(t, l.launch)
	first := create(t, s, "generation protected group", true)
	move := forRun("fleet-move", first)
	move.Group = "原始分组"
	requireOK(t, s.Dispatch(move))
	requireOK(t, s.Dispatch(forRun("fleet-stop", first)))
	second := requireOK(t, s.Dispatch(forRun("fleet-start", first)))
	before, err := store.Get(first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{first.RunID, ""} {
		move.RunID, move.Group = runID, "旧窗口错误移动"
		if response := s.Dispatch(move); response.OK || !strings.Contains(response.Error, ErrStaleRun.Error()) {
			t.Fatalf("old or unbound viewer moved the replacement run: %+v", response)
		}
		if after, err := store.Get(first.SessionID); err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("stale move changed group or edit timestamp: %+v, %v", after, err)
		}
	}
	listed := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-list"}))
	if len(listed.Sessions) != 1 || listed.Sessions[0].Group != before.Group || listed.Sessions[0].RunID != second.RunID || listed.Sessions[0].PID != second.PID || listed.Sessions[0].State != "running" || l.count() != 2 {
		t.Fatalf("stale group move changed the active replacement: %+v", listed)
	}
}
