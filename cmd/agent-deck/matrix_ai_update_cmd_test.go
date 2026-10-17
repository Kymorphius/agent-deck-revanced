package main

// matrix/hardened: pin the user-visible boundary between installation and a
// deliberately confirmed restart, without touching a real Codex installation.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
)

type fakeAIUpdater struct {
	calls  []string
	status aiupdate.Status
	err    error
}

func (f *fakeAIUpdater) result(action string) (aiupdate.Status, error) {
	f.calls = append(f.calls, action)
	return f.status, f.err
}
func (f *fakeAIUpdater) Inspect(context.Context) (aiupdate.Status, error) { return f.result("status") }
func (f *fakeAIUpdater) Check(context.Context) (aiupdate.Status, error)   { return f.result("check") }
func (f *fakeAIUpdater) Install(context.Context) (aiupdate.Status, error) { return f.result("install") }
func (f *fakeAIUpdater) Restart(context.Context) (aiupdate.Status, error) { return f.result("restart") }

func TestAIUpdateCLIInstallDoesNotRestart(t *testing.T) {
	fake := &fakeAIUpdater{status: aiupdate.Status{CLIVersion: "0.163.0", InstalledVersion: "0.164.0", RunningVersion: "0.163.0", Pending: true, Managed: true}}
	var out, diagnostic bytes.Buffer
	err := runAIUpdate(context.Background(), []string{"install", "--json"}, &out, &diagnostic, func() (aiUpdateService, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.calls, ",") != "install" {
		t.Fatalf("install triggered other operations: %v", fake.calls)
	}
	var response struct {
		Success bool            `json:"success"`
		Status  aiupdate.Status `json:"status"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || !response.Status.Pending || response.Status.RunningVersion != "0.163.0" {
		t.Fatalf("lost pending/running distinction: %s", out.String())
	}
}

func TestAIUpdateCLIConfirmationAndHelpAreInert(t *testing.T) {
	for _, args := range [][]string{{"restart", "--json"}, {"--help"}, {"restart", "--help"}, {"install", "unexpected"}, {"unknown"}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			called := false
			err := runAIUpdate(context.Background(), args, &out, &diagnostic, func() (aiUpdateService, error) { called = true; return &fakeAIUpdater{}, nil })
			if called {
				t.Fatal("refused operation/help inspected or changed Codex")
			}
			if strings.Contains(strings.Join(args, " "), "--help") {
				if err != nil || !strings.Contains(out.String(), "Usage:") {
					t.Fatalf("help: %v, %s", err, out.String())
				}
			} else if err == nil {
				t.Fatal("invalid or unconfirmed operation accepted")
			}
		})
	}
}

func TestAIUpdateCLIExplicitRestartAndReadOnlyDefault(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{{nil, "status"}, {[]string{"restart", "--yes"}, "restart"}} {
		fake := &fakeAIUpdater{}
		var out, diagnostic bytes.Buffer
		err := runAIUpdate(context.Background(), tc.args, &out, &diagnostic, func() (aiUpdateService, error) { return fake, nil })
		if err != nil || strings.Join(fake.calls, ",") != tc.want {
			t.Fatalf("args %v: calls=%v error=%v", tc.args, fake.calls, err)
		}
	}
}

func TestAIUpdateCLIPartialFailureIsNotSuccess(t *testing.T) {
	fake := &fakeAIUpdater{status: aiupdate.Status{InstalledVersion: "0.164.0", RunningVersion: "0.163.0", Pending: true}, err: errors.New("version readback failed")}
	var out, diagnostic bytes.Buffer
	err := runAIUpdate(context.Background(), []string{"install", "--json"}, &out, &diagnostic, func() (aiUpdateService, error) { return fake, nil })
	var payload map[string]any
	if json.Unmarshal(out.Bytes(), &payload) != nil || err == nil || payload["success"] != false || payload["status"] == nil {
		t.Fatalf("partial failure was hidden: err=%v output=%s", err, out.String())
	}
}
