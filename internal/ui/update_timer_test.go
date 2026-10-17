package ui

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/update"
)

// #2472 upstream: the TUI's periodic check installs or heals this host's
// update timer once per process. matrix/hardened: it never does, even with
// update checks opted in and nothing suppressing automatic update work.
func TestUpdateCheck_NeverEnsuresTheTimer(t *testing.T) {
	stubUpdateSettings(t, optedInUpdateSettings())
	calls := 0
	prev := ensureUpdateTimer
	ensureUpdateTimer = func(*slog.Logger) (update.TimerEnsureResult, error) {
		calls++
		return update.TimerEnsureResult{Action: update.TimerActionMigrated, Migrated: update.LegacySystemdTimerTimer}, nil
	}
	prevPending := pendingLaunchAgents
	pendingLaunchAgents = func() bool { return false }
	t.Cleanup(func() { ensureUpdateTimer, pendingLaunchAgents = prev, prevPending })

	h := newRestartTestHome(t)
	h.autoUpdateSuppressedReason = ""
	for i := 0; i < 2; i++ {
		if cmd := h.handleUpdateCheck(updateCheckMsg{info: &update.UpdateInfo{CurrentVersion: "1.16.0", LatestVersion: "1.16.0"}}); cmd != nil {
			t.Fatal("matrix/hardened: the TUI must never start a timer heal")
		}
	}
	if calls != 0 {
		t.Fatalf("ensureUpdateTimer called %d times", calls)
	}
}

// When the check starts the unattended updater, the child (`update
// --unattended`) heals the timer; the TUI does not run a second heal.
func TestUpdateCheck_InstallRunLeavesTheHealToTheChild(t *testing.T) {
	stubUpdateSettings(t, optedInUpdateSettings()) // matrix/hardened: explicit opt-in
	t.Setenv(update.SkipUpdateCheckEnv, "")
	calls := 0
	prev, prevRun := ensureUpdateTimer, runUnattendedUpdate
	ensureUpdateTimer = func(*slog.Logger) (update.TimerEnsureResult, error) {
		calls++
		return update.TimerEnsureResult{Action: update.TimerActionNone}, nil
	}
	f := &fakeUpdater{output: "ok\n"}
	runUnattendedUpdate = f.run
	t.Cleanup(func() { ensureUpdateTimer, runUnattendedUpdate = prev, prevRun })

	h := newRestartTestHome(t)
	h.autoUpdateSuppressedReason = ""
	cmd := h.handleUpdateCheck(updateCheckMsg{info: &update.UpdateInfo{Available: true, CurrentVersion: "1.16.0", LatestVersion: "1.16.1"}})
	if _, ok := cmd().(unattendedInstallFinishedMsg); !ok {
		t.Fatal("an available release starts the updater child")
	}
	if !h.updateTimerEnsureStarted || h.maybeEnsureUpdateTimer() != nil || calls != 0 {
		t.Fatalf("the child's run is this process's heal: started=%v calls=%d", h.updateTimerEnsureStarted, calls)
	}
}

func TestRemoteTimerPreviewLine(t *testing.T) {
	next := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		st   *update.TimerStatus
		want string
	}{
		{nil, ""},
		{&update.TimerStatus{Kind: update.TimerKindUnknown}, "update timer unknown (remote too old to report it)"},
		{&update.TimerStatus{Kind: update.TimerKindNone}, "update timer none · updates only when this controller nudges it"},
		{&update.TimerStatus{Kind: "systemd", Installed: true}, "update timer inactive (systemd)"},
		{&update.TimerStatus{Kind: "systemd", Installed: true, Active: true, NextRun: next.Format(time.RFC3339)}, "update timer active (systemd) · next " + next.Local().Format("Jan 2 15:04")},
		{&update.TimerStatus{Kind: update.TimerKindSystemdLegacy, Installed: true, Active: true}, "update timer active (systemd-legacy) · legacy unit, `remote update --install-timer` migrates it"},
	}
	for _, tc := range cases {
		if got := remoteTimerPreviewLine(tc.st); got != tc.want {
			t.Errorf("remoteTimerPreviewLine(%+v) = %q, want %q", tc.st, got, tc.want)
		}
	}

	// The remote preview shows the line under the version line.
	state := session.RemoteVersionState{Version: "1.16.25", Found: true, CheckedAt: time.Now(), Timer: &update.TimerStatus{Kind: update.TimerKindNone}}
	lines := remotePreviewFieldLines(state, "1.16.25", nil, remoteHostStatsResult{}, false, []string{session.PreviewFieldVersion}, time.Now(), previewLayout{})
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "update timer none") {
		t.Fatalf("preview lines = %q", lines)
	}
	// Without a timer reading the panel is unchanged.
	state.Timer = nil
	if lines := remotePreviewFieldLines(state, "1.16.25", nil, remoteHostStatsResult{}, false, []string{session.PreviewFieldVersion}, time.Now(), previewLayout{}); len(lines) != 1 {
		t.Fatalf("preview lines without a timer = %q", lines)
	}
}

// slowVersionRemote spends its whole command bound on the version probe and
// answers the timer query only while its own context is live.
type slowVersionRemote struct {
	stubFetchRunner
	timerBlocks bool
}

func (s slowVersionRemote) CheckBinary(ctx context.Context) (string, bool) {
	<-ctx.Done()
	return "1.16.25", true
}

func (s slowVersionRemote) FetchTimerStatus(ctx context.Context) update.TimerStatus {
	if s.timerBlocks {
		<-ctx.Done()
	}
	if ctx.Err() != nil {
		return update.TimerStatus{Kind: update.TimerKindUnknown, Note: "timer status unavailable: " + ctx.Err().Error()}
	}
	return update.TimerStatus{Kind: update.TimerKindSystemd, Installed: true, Active: true}
}

// #2472 review nit: the TUI's timer read has its own bound, so a version
// probe that uses up its time neither starves it nor turns a good cached
// timer into "unknown"; a timer read that itself runs out records nothing.
func TestFetchOneRemote_TimerReadHasItsOwnBound(t *testing.T) {
	rc := session.RemoteConfig{Host: "a@slow", CommandTimeoutSeconds: 1}
	for _, tc := range []struct {
		name        string
		timerBlocks bool
		wantKind    string
	}{
		{"slow version probe", false, update.TimerKindSystemd},
		{"timer read runs out", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHomeWithItems(100, 30, nil)
			defer h.cancel()
			h.newRemoteFetchRunner = func(name string, _ session.RemoteConfig) remoteFetchRunner {
				return slowVersionRemote{stubFetchRunner: stubFetchRunner{name: name}, timerBlocks: tc.timerBlocks}
			}
			msg := h.fetchOneRemote(0, "slow", rc, []string{"slow"})
			state, ok := msg.versions["slow"]
			if !ok {
				t.Fatal("the version check did not run")
			}
			switch {
			case tc.wantKind == "" && state.Timer != nil:
				t.Fatalf("a timer read that ran out must record nothing, got %+v", *state.Timer)
			case tc.wantKind != "" && (state.Timer == nil || state.Timer.Kind != tc.wantKind):
				t.Fatalf("timer = %+v, want kind %s", state.Timer, tc.wantKind)
			}
		})
	}
}

// matrix/hardened: a TUI-confirmed remote update must never run
// `update --ensure-timer` on the remote (the deployed stock binary would
// install its auto-update timer there).
func TestDeployRemoteUpdate_NeverEnsuresRemoteTimer(t *testing.T) {
	if remoteUpdateEnsuresTimer {
		t.Fatal("matrix/hardened: deployRemoteUpdate must not ensure the remote update timer")
	}
}
