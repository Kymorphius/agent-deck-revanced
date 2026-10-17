package main

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/update"
)

// #2472 verifier round 1, finding 2: the automatic timer heal is wired into
// the daemon start, the real unattended run and the explicit remote update.
// Each test drives the production wiring and fails when it is removed.

// matrix/hardened: the notify daemon's real start sequence never heals the
// update timer (upstream healed it exactly once per start).
func TestNotifyDaemonStart_NeverHealsTheTimer(t *testing.T) {
	var calls atomic.Int32
	prev := daemonEnsureUpdateTimer
	daemonEnsureUpdateTimer = func(*slog.Logger) (update.TimerEnsureResult, error) {
		calls.Add(1)
		return update.TimerEnsureResult{Action: update.TimerActionNone}, nil
	}
	t.Cleanup(func() { daemonEnsureUpdateTimer = prev })

	start := realNotifyDaemonStart()
	// The other start work is replaced so the test touches nothing real.
	var polled atomic.Int32
	start.watchVersion = func(context.Context, context.CancelFunc) {}
	start.headlessAutoInstall = func(context.Context) { polled.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start.begin(ctx, cancel)
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), calls.Load(), "matrix/hardened: no timer heal at daemon start")
	assert.Equal(t, int32(1), polled.Load())
}

// matrix/hardened: the real unattended run never installs or heals the timer.
func TestRealUnattendedDeps_NeverHealsTheTimer(t *testing.T) {
	setupTask6XDGEnv(t)
	deps, closeLog := realUnattendedDeps("timer")
	t.Cleanup(closeLog)
	assert.Nil(t, deps.ensureTimer, "matrix/hardened: update --unattended must not touch the timer")
}

// ensureCountingRemote is a current remote that counts the timer heals
// `remote update` asks it for.
type ensureCountingRemote struct {
	ensureOnly int
	explicit   int
}

func (s *ensureCountingRemote) CheckBinary(context.Context) (string, bool) { return Version, true }
func (s *ensureCountingRemote) DetectPlatform(context.Context) (string, string, error) {
	return "", "", errors.New("stub: a current remote is never installed onto")
}
func (s *ensureCountingRemote) InstallBinary(context.Context, []byte, string) error { return nil }
func (s *ensureCountingRemote) FetchTimerStatus(context.Context) update.TimerStatus {
	return update.TimerStatus{Kind: update.TimerKindSystemd, Installed: true, Active: true}
}
func (s *ensureCountingRemote) InstallUpdateTimer(_ context.Context, ensureOnly bool) (session.RemoteTimerInstall, error) {
	if ensureOnly {
		s.ensureOnly++
	} else {
		s.explicit++
	}
	return session.RemoteTimerInstall{TimerEnsureResult: update.TimerEnsureResult{
		Action: update.TimerActionNone,
		Status: update.TimerStatus{Kind: update.TimerKindSystemd, Installed: true, Active: true},
	}}, nil
}

// matrix/hardened: `remote update <name>` never asks a remote to install or
// heal its timer (upstream ran `update --ensure-timer` on each remote).
func TestHandleRemoteUpdate_NeverAsksTheRemoteToEnsureItsTimer(t *testing.T) {
	setupRemoteListJSONTest(t)
	stub := &ensureCountingRemote{}
	prev := remoteUpdateRunner
	remoteUpdateRunner = func(string, session.RemoteConfig) session.RemoteBinaryInstaller { return stub }
	t.Cleanup(func() { remoteUpdateRunner = prev })

	out := captureStdout(t, func() { handleRemoteUpdate([]string{"lab"}) })
	assert.Equal(t, 0, stub.ensureOnly, "matrix/hardened: no remote timer heal; output:\n%s", out)
	assert.Equal(t, 0, stub.explicit)
}
