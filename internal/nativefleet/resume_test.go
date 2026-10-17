// matrix/hardened: history selection reuses the existing registry and lifetime
// ownership, while fake processes keep agent accounts and real projects untouched.
package nativefleet

import (
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

func codexLaunch() terminalhost.LaunchSpec {
	executable := "codex"
	if runtime.GOOS == "windows" {
		executable = "codex.exe"
	}
	return terminalhost.LaunchSpec{Executable: filepath.Join("native-bin", executable), Dir: "owned-project", Env: []string{"AD_RESUME=explicit override"}, Cols: 80, Rows: 24}
}

func TestCodexHistoryUsesTemporaryArgumentsAndPreservesBothStores(t *testing.T) {
	for _, storeType := range []string{"prototype", "statedb"} {
		t.Run(storeType, func(t *testing.T) {
			root := testRoot(t)
			var store Registry
			var err error
			if storeType == "statedb" {
				store, err = nativestatestore.Open(root)
			} else {
				store, err = nativeregistry.Open(root)
			}
			if err != nil {
				t.Fatal(err)
			}
			l := &launcher{}
			var launches []terminalhost.LaunchSpec
			s, err := New(store, func(spec terminalhost.LaunchSpec) (terminalhost.Process, error) {
				launches = append(launches, spec)
				return l.launch(spec)
			})
			if err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			spec := codexLaunch()
			original := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "同目录历史", Tool: "codex", Group: "研发", Launch: &spec, AutoStart: true}))
			before, err := store.Get(original.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			requireOK(t, s.Dispatch(forRun("fleet-stop", original)))
			resumed := requireOK(t, s.Dispatch(forRun("fleet-resume", original)))
			want := spec
			want.Args = []string{"resume"}
			if len(launches) != 2 || !reflect.DeepEqual(launches[0], spec) || !reflect.DeepEqual(launches[1], want) {
				t.Fatalf("history launch changed cwd/env or added flags: %+v", launches)
			}
			if !resumed.Running || resumed.SessionID != original.SessionID || resumed.RunID == original.RunID || resumed.PID == original.PID || len(resumed.Sessions) != 1 || resumed.Sessions[0].Group != before.Group {
				t.Fatalf("history did not create a new owned generation: %+v -> %+v", original, resumed)
			}
			after, err := store.Get(original.SessionID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("temporary resume persisted into descriptor: before=%+v after=%+v error=%v", before, after, err)
			}
			for _, op := range []string{"fleet-resume", "fleet-start", "fleet-stop", "fleet-remove", "fleet-move", "send", "resize"} {
				request := forRun(op, original)
				request.Data, request.Group, request.Cols, request.Rows = []byte("stale"), "stale", 90, 30
				if rejected := s.Dispatch(request); rejected.OK || !strings.Contains(rejected.Error, "run changed") {
					t.Fatalf("%s accepted old viewer generation: %+v", op, rejected)
				}
			}
			requireOK(t, s.Dispatch(forRun("fleet-stop", resumed)))
			fresh := requireOK(t, s.Dispatch(forRun("fleet-start", resumed)))
			if len(launches) != 3 || !reflect.DeepEqual(launches[2], spec) || fresh.RunID == resumed.RunID {
				t.Fatal("ordinary explicit Start reused temporary history arguments")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if storeType == "statedb" {
				store, err = nativestatestore.Open(root)
			} else {
				store, err = nativeregistry.Open(root)
			}
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := store.Get(original.SessionID)
			if err != nil || !reflect.DeepEqual(before, recovered) {
				_ = store.Close()
				t.Fatalf("reopened store changed saved bare launch: %+v %v", recovered, err)
			}
			second, err := New(store, s.launch)
			if err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = second.Close() })
			listed := requireOK(t, second.Dispatch(terminalhost.Request{Op: "fleet-list"}))
			if len(listed.Sessions) != 1 || listed.Sessions[0].State != "stopped" || listed.Sessions[0].RunID != "" || len(launches) != 3 {
				t.Fatalf("recovered manager launched or retained a previous run: %+v", listed)
			}
			if stale := second.Dispatch(forRun("fleet-resume", fresh)); stale.OK || !strings.Contains(stale.Error, "run changed") || len(launches) != 3 {
				t.Fatalf("recovered manager accepted old manager generation: %+v", stale)
			}
			resumedAgain := requireOK(t, second.Dispatch(terminalhost.Request{Op: "fleet-resume", SessionID: original.SessionID}))
			if !resumedAgain.Running || resumedAgain.RunID == fresh.RunID || len(launches) != 4 || !reflect.DeepEqual(launches[3], want) {
				t.Fatalf("recovered empty-generation description could not choose history: %+v", resumedAgain)
			}
		})
	}
}

func TestConcurrentCodexHistoryStartsOnceFromDisplayedRun(t *testing.T) {
	l := &launcher{}
	s, _, _ := fixture(t, l.launch)
	spec := codexLaunch()
	created := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "并发选历史", Tool: "codex", Launch: &spec, AutoStart: true}))
	requireOK(t, s.Dispatch(forRun("fleet-stop", created)))
	var wg sync.WaitGroup
	results := make(chan terminalhost.Response, 12)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.Dispatch(forRun("fleet-resume", created))
		}()
	}
	wg.Wait()
	close(results)
	started := 0
	for result := range results {
		if result.OK {
			started++
			if !result.Running || result.RunID == created.RunID {
				t.Fatalf("concurrent history did not start a fresh owned run: %+v", result)
			}
		} else if !strings.Contains(result.Error, "run changed") {
			t.Fatalf("concurrent history returned unexpected error: %+v", result)
		}
	}
	if started != 1 || l.count() != 2 {
		t.Fatalf("concurrent history started more than one new process: successes=%d launches=%d", started, l.count())
	}
}

func TestCodexHistoryRejectsRunningAndStaleRunsWithoutReplacingProcess(t *testing.T) {
	l := &launcher{}
	s, _, _ := fixture(t, l.launch)
	spec := codexLaunch()
	first := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "运行中", Tool: "codex", Launch: &spec, AutoStart: true}))
	if r := s.Dispatch(forRun("fleet-resume", first)); r.OK || !strings.Contains(r.Error, "stops or exits") {
		t.Fatalf("history replaced a live run: %+v", r)
	}
	if l.count() != 1 || l.latest().closeCount != 0 {
		t.Fatal("rejected history stopped or launched a process")
	}
	requireOK(t, s.Dispatch(forRun("fleet-stop", first)))
	for _, runID := range []string{"", "different-generation"} {
		r := s.Dispatch(terminalhost.Request{Op: "fleet-resume", SessionID: first.SessionID, RunID: runID})
		if r.OK || !strings.Contains(r.Error, "run changed") || l.count() != 1 {
			t.Fatalf("history accepted a missing/stale generation: %+v", r)
		}
	}
	second := requireOK(t, s.Dispatch(forRun("fleet-resume", first)))
	if r := s.Dispatch(forRun("fleet-resume", first)); r.OK || l.count() != 2 || !second.Running {
		t.Fatalf("second history replaced a new run: %+v", r)
	}
}

func TestCodexHistoryCanSelectAfterNaturalExit(t *testing.T) {
	l := &launcher{}
	s, _, _ := fixture(t, l.launch)
	spec := codexLaunch()
	first := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "自然退出", Tool: "codex", Launch: &spec, AutoStart: true}))
	if err := l.latest().Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		list := s.Dispatch(terminalhost.Request{Op: "fleet-list"})
		return list.OK && len(list.Sessions) == 1 && list.Sessions[0].State == "exited"
	})
	second := requireOK(t, s.Dispatch(forRun("fleet-resume", first)))
	if !second.Running || second.RunID == first.RunID || l.count() != 2 {
		t.Fatalf("natural exit could not open history: %+v", second)
	}
}

func TestCodexHistoryRejectsCustomLaunchesWithoutChangingRecords(t *testing.T) {
	for _, test := range []struct {
		name, tool, executable string
		args                   []string
	}{
		{name: "shell", tool: "shell"},
		{name: "custom tool", tool: "custom"},
		{name: "script wrapper", tool: "codex", executable: "codex.cmd"},
		{name: "other program", tool: "codex", executable: "other.exe"},
		{name: "saved custom flags", tool: "codex", args: []string{"--model", "custom"}},
		{name: "saved resume flags", tool: "codex", args: []string{"resume", "--last"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			l := &launcher{}
			s, store, _ := fixture(t, l.launch)
			spec := codexLaunch()
			spec.Args = test.args
			if test.executable != "" {
				spec.Executable = test.executable
			}
			created := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "保留自定义", Tool: test.tool, Launch: &spec}))
			before, _ := store.Get(created.SessionID)
			if rejected := s.Dispatch(forRun("fleet-resume", created)); rejected.OK || !strings.Contains(rejected.Error, "native Codex") || l.count() != 0 {
				t.Fatalf("unsupported launch entered history: %+v", rejected)
			}
			after, err := store.Get(created.SessionID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected history changed its record: %+v %v", after, err)
			}
		})
	}
}

func TestCodexHistoryLaunchFailureKeepsRecordAndDoesNotRetry(t *testing.T) {
	l := &launcher{}
	var launches []terminalhost.LaunchSpec
	s, store, _ := fixture(t, func(spec terminalhost.LaunchSpec) (terminalhost.Process, error) {
		launches = append(launches, spec)
		if len(spec.Args) != 0 {
			return nil, errors.New("history fixture startup failed")
		}
		return l.launch(spec)
	})
	spec := codexLaunch()
	first := requireOK(t, s.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "失败保留", Tool: "codex", Launch: &spec, AutoStart: true}))
	before, _ := store.Get(first.SessionID)
	requireOK(t, s.Dispatch(forRun("fleet-stop", first)))
	failed := s.Dispatch(forRun("fleet-resume", first))
	if failed.OK || !strings.Contains(failed.Error, "history fixture startup failed") || len(failed.Sessions) != 1 || failed.Sessions[0].ID != first.SessionID || len(launches) != 2 || l.count() != 1 {
		t.Fatalf("history failure retried, fell back, or lost identity: %+v launches=%+v", failed, launches)
	}
	after, err := store.Get(first.SessionID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("history failure changed durable launch: %+v %v", after, err)
	}
	fresh := requireOK(t, s.Dispatch(forRun("fleet-start", failed)))
	if !fresh.Running || len(launches) != 3 || !reflect.DeepEqual(launches[2], spec) {
		t.Fatal("explicit later Start cannot use the original bare descriptor")
	}
}
