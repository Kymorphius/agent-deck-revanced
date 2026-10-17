package tmux

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// matrix/hardened: these fixtures use only private named sockets under the
// isolated test runner. No real terminal, AI pane or production viewer is used.
func matrixMetadataStarts(category string) uint64 {
	for _, metric := range SubprocessMetrics().Categories {
		if metric.Category == category {
			return metric.Started
		}
	}
	return 0
}

func matrixMetadataNativeRun(t *testing.T, socket string, args ...string) {
	t.Helper()
	if output, err := tmuxExec(socket, args...).CombinedOutput(); err != nil {
		t.Fatalf("private setup %v: %v %s", args, err, output)
	}
}

func matrixMetadataCacheCleanup(t *testing.T) {
	t.Helper()
	oldSocket, oldPipes := DefaultSocketName(), GetPipeManager()
	SetPipeManager(nil)
	sessionCacheMu.Lock()
	sessions, sessionAt := sessionCacheData, sessionCacheTime
	sessionCacheData, sessionCacheTime = nil, time.Time{}
	sessionCacheMu.Unlock()
	paneCacheMu.Lock()
	panes, paneAt := paneCacheData, paneCacheTime
	paneCacheData, paneCacheTime = nil, time.Time{}
	paneCacheMu.Unlock()
	windowCacheMu.Lock()
	windows, windowAt := windowCacheData, windowCacheTime
	windowCacheData, windowCacheTime = nil, time.Time{}
	windowCacheMu.Unlock()
	windowToolCacheMu.Lock()
	tools := windowToolCacheData
	windowToolCacheData = nil
	windowToolCacheMu.Unlock()
	t.Cleanup(func() {
		SetDefaultSocketName(oldSocket)
		SetPipeManager(oldPipes)
		sessionCacheMu.Lock()
		sessionCacheData, sessionCacheTime = sessions, sessionAt
		sessionCacheMu.Unlock()
		paneCacheMu.Lock()
		paneCacheData, paneCacheTime = panes, paneAt
		paneCacheMu.Unlock()
		windowCacheMu.Lock()
		windowCacheData, windowCacheTime = windows, windowAt
		windowCacheMu.Unlock()
		windowToolCacheMu.Lock()
		windowToolCacheData = tools
		windowToolCacheMu.Unlock()
	})
}

func TestMatrixMetadataQueryExplicitSessionGrammar(t *testing.T) {
	for _, args := range [][]string{
		{"list-panes", "-s", "-t", "same", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "same '$ 中文", "-F", "#{session_name}|#{window_id}|#{pane_pid}"},
	} {
		if _, ok := matrixQueryCommands(args); !ok {
			t.Fatalf("explicit session query rejected: %q", args)
		}
	}
	for _, args := range [][]string{
		{"list-panes", "-s", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "sam*", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", ":", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", ":0", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "foo:", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "foo:0.0", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", ".", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "{current}", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "$1", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "%1", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "@1", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "!", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "=foo", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "#{client_session}", "-F", "#{pane_pid}"},
		{"list-panes", "-t", "same", "-s", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "same", "-t", "other", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-s", "-t", "same", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-a", "-t", "same", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "same", "-F", "#{pane_pid}", "-F", "#{pane_id}"},
		{"list-panes", "-s", "-t", "same", "-F", "#{pane_pid}", "extra"},
		{"list-windows", "-s", "-t", "same", "-F", "#{pane_pid}"},
		{"list-sessions", "-s", "-t", "same", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "same\nkill-server", "-F", "#{pane_pid}"},
		{"list-panes", "-s", "-t", "same", "-F", "#{client_width}"},
		{"list-panes", "-s", "-t", "same", "-F", "#{E:pane_title}"},
		{"list-panes", "-s", "-t", "same", "-F", "#(touch /tmp/unsupported)"},
		{"list-panes", "-s", "-t", "same", "-F", "#{pane_pid}", ";", "kill-server"},
	} {
		if _, ok := matrixQueryCommands(args); ok {
			t.Fatalf("unsupported session query accepted: %q", args)
		}
	}
}

func TestMatrixMetadataQueryPrivateSessionAndSocketParity(t *testing.T) {
	a := matrixQueryFixture(t, "metadata-a")
	b := matrixQueryFixture(t, "metadata-b")
	matrixMetadataNativeRun(t, a.SocketName, "new-window", "-d", "-t", a.Name, "-n", "two | ' 中文", "sleep 120")
	matrixMetadataNativeRun(t, a.SocketName, "new-session", "-d", "-s", "other", "sleep 120")
	for _, s := range []*Session{a, b} {
		matrixMetadataNativeRun(t, s.SocketName, "set-environment", "-t", s.Name, "MATRIX_METADATA_VALUE", s.SocketName)
	}
	// Keep an existing independent client. The new query client must neither
	// kill it nor change the selected window, pane processes or their geometry.
	viewer, err := NewControlPipe(a.Name, a.SocketName)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	geometryArgs := []string{"list-panes", "-a", "-F", "#{session_name}|#{window_id}|#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}"}
	beforeA := matrixNativeQuery(t, a.SocketName, geometryArgs...)
	beforeB := matrixNativeQuery(t, b.SocketName, geometryArgs...)
	selection := matrixNativeQuery(t, a.SocketName, "display-message", "-p", "-t", a.Name, "#{window_id}|#{pane_id}")
	clients := matrixNativeQuery(t, a.SocketName, "list-clients", "-F", "#{client_pid}")
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	// An anchor in another session must not change an explicit -s target.
	m.RegisterTargets("fixture", []*Session{{Name: "other", SocketName: a.SocketName}, b})
	listStarts := matrixMetadataStarts("list-panes")
	for _, s := range []*Session{a, b} {
		args := []string{"list-panes", "-s", "-t", s.Name, "-F", "#{pane_pid}"}
		want := matrixNativeQuery(t, s.SocketName, args...)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		got, err := matrixMetadataQueryOutput(ctx, s.SocketName, args...)
		cancel()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("socket %s all windows = %q, %v; native %q", s.SocketName, got, err, want)
		}
		count := 1
		if s == a {
			count = 2
		}
		if len(strings.Fields(string(got))) != count {
			t.Fatalf("session/window boundary mismatch: %q", got)
		}
		if value, err := s.ReadEnvironment("MATRIX_METADATA_VALUE"); err != nil || value != s.SocketName {
			t.Fatalf("environment crossed socket/anchor: %q, %v", value, err)
		}
	}
	if m.Diagnostics().Successes != 4 || matrixMetadataStarts("list-panes") != listStarts {
		t.Fatalf("metadata used native starts or failed channel: %+v", m.Diagnostics())
	}
	if !bytes.Equal(beforeA, matrixNativeQuery(t, a.SocketName, geometryArgs...)) || !bytes.Equal(beforeB, matrixNativeQuery(t, b.SocketName, geometryArgs...)) || !bytes.Equal(selection, matrixNativeQuery(t, a.SocketName, "display-message", "-p", "-t", a.Name, "#{window_id}|#{pane_id}")) {
		t.Fatal("metadata changed pane processes/geometry/current window")
	}
	m.Close()
	if !bytes.Equal(clients, matrixNativeQuery(t, a.SocketName, "list-clients", "-F", "#{client_pid}")) || !viewer.IsAlive() {
		t.Fatal("query lifetime changed the original viewer client")
	}
}

func TestMatrixMetadataReadersUseBrokerAndKeepCaches(t *testing.T) {
	matrixMetadataCacheCleanup(t)
	s := matrixQueryFixture(t, "metadata-caches")
	SetDefaultSocketName(s.SocketName)
	matrixMetadataNativeRun(t, s.SocketName, "select-pane", "-t", s.Name, "-T", "pane | ' 中文")
	matrixMetadataNativeRun(t, s.SocketName, "set-environment", "-t", s.Name, "MATRIX_METADATA_CACHE", " alpha '$ 中文  ")
	windowsArgs := []string{"list-windows", "-a", "-F", tmuxFmt("#{session_name}", "#{window_activity}", "#{window_index}", "#{window_id}", "#{window_name}")}
	panesArgs := []string{"list-panes", "-a", "-F", tmuxFmt("#{session_name}", "#{pane_current_command}", "#{pane_dead}", "#{window_index}", "#{pane_index}", "#{pane_title}")}
	wantSessions, wantWindows := parseListWindowsOutput(string(matrixNativeQuery(t, s.SocketName, windowsArgs...)))
	wantPanes, wantTools := parseListPanesOutput(string(matrixNativeQuery(t, s.SocketName, panesArgs...)))
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{s})
	paneStarts, windowStarts := matrixMetadataStarts("list-panes"), matrixMetadataStarts("list-windows")
	RefreshSessionCache()
	RefreshPaneInfoCache()
	if !reflect.DeepEqual(sessionCacheData, wantSessions) || !reflect.DeepEqual(windowCacheData, wantWindows) || !reflect.DeepEqual(paneCacheData, wantPanes) || !reflect.DeepEqual(windowToolCacheData, wantTools) {
		t.Fatalf("metadata caches diverged from native parser results: sessions=%v panes=%v", sessionCacheData, paneCacheData)
	}
	if m.Diagnostics().Successes != 2 || matrixMetadataStarts("list-panes") != paneStarts || matrixMetadataStarts("list-windows") != windowStarts {
		t.Fatalf("bulk metadata spawned native queries: %+v", m.Diagnostics())
	}
	if got, err := s.GetEnvironment("MATRIX_METADATA_CACHE"); err != nil || got != " alpha '$ 中文" {
		t.Fatalf("GetEnvironment native trim semantics changed: %q, %v", got, err)
	}
	queries := m.Diagnostics().Queries
	if _, err := s.GetEnvironment("MATRIX_METADATA_CACHE"); err != nil || m.Diagnostics().Queries != queries {
		t.Fatal("positive hit did not retain original TTL", err)
	}
	s.envCacheMu.Lock()
	entry := s.envCache["MATRIX_METADATA_CACHE"]
	entry.time = time.Now().Add(-envCacheTTL - time.Second)
	s.envCache["MATRIX_METADATA_CACHE"] = entry
	s.envCacheMu.Unlock()
	if _, err := s.GetEnvironment("MATRIX_METADATA_CACHE"); err != nil || m.Diagnostics().Queries != queries+1 {
		t.Fatal("expired positive entry did not query", err)
	}
	for n := 0; n < 2; n++ {
		if got, err := s.ReadEnvironment("MATRIX_METADATA_CACHE"); err != nil || got != " alpha '$ 中文  " {
			t.Fatalf("fresh full environment changed bytes: %q, %v", got, err)
		}
	}
	if m.Diagnostics().Queries != queries+3 {
		t.Fatal("ReadEnvironment unexpectedly reused cache")
	}
	if got, err := s.ReadEnvironment("MATRIX_METADATA_UNSET"); got != "" || err != nil {
		t.Fatalf("whole-env missing/failure distinction changed: %q, %v", got, err)
	}
	missStarts := matrixMetadataStarts("show-environment")
	if _, err := s.GetEnvironment("MATRIX_METADATA_UNSET"); err == nil || matrixMetadataStarts("show-environment") != missStarts+1 {
		t.Fatal("framed missing-key error did not retain native fallback", err)
	}
	queries = m.Diagnostics().Queries
	if _, err := s.GetEnvironment("MATRIX_METADATA_UNSET"); err == nil || m.Diagnostics().Queries != queries || matrixMetadataStarts("show-environment") != missStarts+1 {
		t.Fatal("negative cache hit queried again", err)
	}
	s.envCacheMu.Lock()
	entry = s.envCache["MATRIX_METADATA_UNSET"]
	entry.time = time.Now().Add(-envNegativeCacheTTL - time.Second)
	s.envCache["MATRIX_METADATA_UNSET"] = entry
	s.envCacheMu.Unlock()
	if _, err := s.GetEnvironment("MATRIX_METADATA_UNSET"); err == nil || matrixMetadataStarts("show-environment") != missStarts+2 {
		t.Fatal("expired negative entry did not retain native error probe", err)
	}
	if err := s.SetEnvironment("MATRIX_METADATA_UNSET", "bound"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetEnvironment("MATRIX_METADATA_UNSET"); got != "bound" || err != nil {
		t.Fatalf("set did not invalidate negative cache: %q, %v", got, err)
	}
	// Preserve the existing different cache failure policies with an unregistered,
	// missing server: session cache clears; a still-fresh pane cache survives.
	SetDefaultSocketName("metadata-cache-missing")
	RefreshSessionCache()
	RefreshPaneInfoCache()
	if sessionCacheData != nil || !sessionCacheTime.IsZero() || !reflect.DeepEqual(paneCacheData, wantPanes) {
		t.Fatal("metadata failure changed cache preservation policy")
	}
}

func TestMatrixMetadataQueryNativeErrorParity(t *testing.T) {
	s := matrixQueryFixture(t, "metadata-errors")
	m := EnableQueryTransport(context.Background())
	defer m.Close()
	m.RegisterTargets("fixture", []*Session{s})
	for _, args := range [][]string{
		{"show-environment", "-t", s.Name, "MATRIX_METADATA_NOT_SET"},
		{"show-environment", "-t", "missing-session"},
		{"list-panes", "-s", "-t", "missing-session", "-F", "#{pane_pid}"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		want, wantErr := commandOutput(tmuxExecContext(ctx, s.SocketName, args...))
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		got, gotErr := matrixMetadataQueryOutput(ctx, s.SocketName, args...)
		cancel()
		var native, actual *exec.ExitError
		if !errors.As(wantErr, &native) || !errors.As(gotErr, &actual) || actual.ExitCode() != native.ExitCode() || !bytes.Equal(got, want) || !bytes.Equal(actual.Stderr, native.Stderr) {
			t.Fatalf("error fallback %q changed: got=%q %v; native=%q %v", args, got, gotErr, want, wantErr)
		}
	}
	if value, err := s.ReadEnvironment("MATRIX_METADATA_NOT_SET"); value != "" || err != nil || m.Diagnostics().Starts != 1 {
		t.Fatalf("framed failures damaged healthy channel: %q, %v %+v", value, err, m.Diagnostics())
	}
	if _, err := (&Session{Name: "missing-session", SocketName: s.SocketName}).ReadEnvironment("MATRIX_METADATA_NOT_SET"); err == nil {
		t.Fatal("ReadEnvironment converted a failed probe into a missing variable")
	}
}

func TestMatrixMetadataQueryCallerCancellationKeepsBudget(t *testing.T) {
	for _, immediate := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancelled", false: "deadline"}[immediate], func(t *testing.T) {
			m := EnableQueryTransport(context.Background())
			defer m.Close()
			m.RegisterTargets("fixture", []*Session{{Name: "same", SocketName: "metadata-never-start"}})
			socket := m.sockets[matrixQuerySocketKey("metadata-never-start")]
			socket.gate <- struct{}{}
			defer func() { <-socket.gate }()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			wantErr := context.DeadlineExceeded
			if immediate {
				cancel()
				wantErr = context.Canceled
			}
			starts := matrixMetadataStarts("list-panes")
			started := time.Now()
			_, err := matrixMetadataQueryOutput(ctx, "metadata-never-start", "list-panes", "-s", "-t", "same", "-F", "#{pane_pid}")
			if !errors.Is(err, wantErr) || time.Since(started) > time.Second || m.Diagnostics().Starts != 0 || matrixMetadataStarts("list-panes") != starts {
				t.Fatalf("caller budget reset/spawned native: %v wall=%s %+v", err, time.Since(started), m.Diagnostics())
			}
		})
	}
}
