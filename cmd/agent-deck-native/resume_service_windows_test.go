//go:build windows

package main

// matrix/hardened: a private same-user IPC fixture verifies the exact wire
// request without launching Codex, reading account data, or touching a registry.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/google/uuid"
)

func nativeServiceWireFixture(t *testing.T, respond func(terminalhost.Request) terminalhost.Response) (string, <-chan terminalhost.Request) {
	t.Helper()
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 to allow an isolated native pipe")
	}
	pipe := pipePrefix + "service-wire-" + uuid.NewString()
	listener, err := winipc.Listen(pipe)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan terminalhost.Request, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
			var request terminalhost.Request
			if err := json.NewDecoder(connection).Decode(&request); err != nil {
				t.Errorf("read service request: %v", err)
				_ = connection.Close()
				return
			}
			requests <- request
			if err := json.NewEncoder(connection).Encode(respond(request)); err != nil {
				t.Errorf("reply to service request: %v", err)
			}
			_ = connection.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("service fixture did not close its exact pipe")
		}
	})
	return pipe, requests
}

func TestWindowsNativeServiceResumeSendsDisplayedRunWithoutLookup(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "stale"}[rejected], func(t *testing.T) {
			id, oldRun, newRun := uuid.NewString(), uuid.NewString(), uuid.NewString()
			pipe, requests := nativeServiceWireFixture(t, func(request terminalhost.Request) terminalhost.Response {
				if request.Op != "fleet-resume" {
					return terminalhost.Response{Error: "must not look up a replacement run"}
				}
				if rejected {
					return terminalhost.Response{Error: "native session run changed"}
				}
				return terminalhost.Response{OK: true, Sessions: []terminalhost.SessionInfo{{ID: id, RunID: newRun, Tool: "codex", State: "running", Group: nativestatestore.DefaultGroup}}}
			})
			service := &nativeService{pipe: pipe, store: "statedb"}
			info, err := service.Resume(context.Background(), id, oldRun)
			request := <-requests
			if request.Op != "fleet-resume" || request.SessionID != id || request.RunID != oldRun || request.Launch != nil || len(request.Data) != 0 || len(requests) != 0 {
				t.Fatalf("resume replaced its selected identity or retried: %+v", request)
			}
			if rejected {
				if err == nil || info.ID != "" {
					t.Fatalf("stale resume hid failure: %+v %v", info, err)
				}
			} else if err != nil || info.ID != id || info.RunID != newRun || info.Group != "" {
				t.Fatalf("resume lost returned run/group: %+v %v", info, err)
			}
		})
	}
}

func TestWindowsNativeServiceExecutablePreferenceAffectsOnlyNewCodex(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex.exe")
	for _, tool := range []string{"codex", "shell", "powershell"} {
		t.Run(tool, func(t *testing.T) {
			id, runID := uuid.NewString(), uuid.NewString()
			pipe, requests := nativeServiceWireFixture(t, func(request terminalhost.Request) terminalhost.Response {
				return terminalhost.Response{OK: true, Sessions: []terminalhost.SessionInfo{{ID: id, RunID: runID, Tool: tool, State: "running"}}}
			})
			service := &nativeService{pipe: pipe, codexExecutable: executable}
			cwd := t.TempDir()
			if _, err := service.CreateInGroup(context.Background(), "新会话", tool, cwd, "研发"); err != nil {
				t.Fatal(err)
			}
			request := <-requests
			if request.Op != "fleet-create" || !request.AutoStart || request.Tool != tool || request.Group != "研发" || request.Launch == nil || request.Launch.Dir != cwd || len(request.Launch.Env) != 0 {
				t.Fatalf("new-session preference changed create context: %+v", request)
			}
			if tool == "codex" {
				if request.Launch.Executable != executable || len(request.Launch.Args) != 0 {
					t.Fatal("new Codex omitted explicit native executable or added flags")
				}
			} else if request.Launch.Executable == executable {
				t.Fatalf("Codex preference replaced a %s program", tool)
			}
		})
	}
}

func TestWindowsManagedResumeRoutesExplicitAndOptionalRunIDs(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "lookup current", true: "explicit old"}[explicit], func(t *testing.T) {
			id, oldRun, currentRun := uuid.NewString(), uuid.NewString(), uuid.NewString()
			config := options{Command: "resume", Store: "statedb", StateDir: t.TempDir(), SessionID: id}
			if explicit {
				config.RunID = oldRun
			}
			pipe, requests := nativeServiceWireFixture(t, func(request terminalhost.Request) terminalhost.Response {
				switch request.Op {
				case "fleet-status":
					return terminalhost.Response{OK: true, HostPID: os.Getpid(), RunID: uuid.NewString(), ManagerStateDir: config.StateDir, ManagerVersion: nativeManagerVersion(config), ProtocolVersion: nativeManagerProtocol}
				case "fleet-list":
					return terminalhost.Response{OK: true, Sessions: []terminalhost.SessionInfo{{ID: id, RunID: currentRun, Tool: "codex", State: "stopped"}}}
				case "fleet-resume":
					return terminalhost.Response{OK: true, Sessions: []terminalhost.SessionInfo{{ID: id, RunID: uuid.NewString(), Tool: "codex", State: "running"}}}
				default:
					return terminalhost.Response{Error: "unexpected route"}
				}
			})
			config.Pipe = pipe
			if err := runFleet(config, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			want := []string{"fleet-status", "fleet-resume"}
			wantRun := oldRun
			if !explicit {
				want, wantRun = []string{"fleet-status", "fleet-list", "fleet-resume"}, currentRun
			}
			var ops []string
			for len(requests) > 0 {
				request := <-requests
				ops = append(ops, request.Op)
				if request.Op == "fleet-resume" && (request.SessionID != id || request.RunID != wantRun) {
					t.Fatalf("CLI history replaced its expected run: %+v", request)
				}
			}
			if !reflect.DeepEqual(ops, want) {
				t.Fatalf("CLI resume did not use its declared route: got=%s want=%s", strings.Join(ops, ","), strings.Join(want, ","))
			}
		})
	}
}
