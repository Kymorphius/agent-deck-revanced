package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: integration uses only the isolated runner's private socket.
func matrixMonitorQueryFixture(t *testing.T) (string, func(...string) string) {
	t.Helper()
	if os.Getenv("TMUX_TMPDIR") == "" {
		t.Fatal("run via scripts/run-isolated.py")
	}
	socket := fmt.Sprintf("monitor-query-%d", time.Now().UnixNano())
	native := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-u", "-L", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("private tmux: %v %s", err, out)
		}
		return string(out)
	}
	native("new-session", "-d", "-s", "query-first", "-x", "101", "-y", "37", "printf 'query fixture\\n'; sleep 120")
	native("new-session", "-d", "-s", "query-second", "-x", "101", "-y", "37", "sleep 120")
	// The isolated runner owns server teardown and removal of its socket.
	prior := tmux.DefaultSocketName()
	tmux.SetDefaultSocketName(socket)
	t.Cleanup(func() { tmux.SetDefaultSocketName(prior) })
	return socket, native
}

type matrixQueryRegistry struct {
	instances []*session.Instance
	loaded    bool
	failed    atomic.Bool
	closed    atomic.Bool
}

func (r *matrixQueryRegistry) Changed() (bool, error) {
	if r.failed.Load() {
		return false, errors.New("query fixture registry unavailable")
	}
	return !r.loaded, nil
}
func (r *matrixQueryRegistry) Load() ([]*session.Instance, error) {
	r.loaded = true
	return r.instances, nil
}
func (r *matrixQueryRegistry) Close() error { r.closed.Store(true); return nil }
func matrixQueryInstances(socket string) []*session.Instance {
	var instances []*session.Instance
	for _, name := range []string{"query-first", "query-second"} {
		inst := session.NewInstanceWithTool(name, "/test", "shell")
		inst.ID = name
		inst.Status = session.StatusWaiting
		inst.CreatedAt = time.Now().Add(-time.Hour)
		inst.SetTmuxSessionForTest(&tmux.Session{Name: name, SocketName: socket, Command: "sh"})
		instances = append(instances, inst)
	}
	return instances
}
func matrixQueryEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("query lifecycle condition did not settle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMatrixMonitorQueryTransportLeaseAndHTTPStop(t *testing.T) {
	socket, native := matrixMonitorQueryFixture(t)
	format := "#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}"
	before := native("list-panes", "-a", "-F", format)
	m := tmux.EnableQueryTransport(context.Background())
	t.Cleanup(m.Close)
	instances := matrixQueryInstances(socket)
	// Ineligible entries deliberately precede the live anchor on its socket.
	archived := session.NewInstanceWithTool("archived", "/test", "shell")
	archived.ID, archived.ArchivedAt = "archived", time.Now()
	archived.SetTmuxSessionForTest(&tmux.Session{Name: "absent-archived", SocketName: socket})
	stopped := session.NewInstanceWithTool("stopped", "/test", "shell")
	stopped.ID, stopped.Status = "stopped", session.StatusStopped
	stopped.SetTmuxSessionForTest(&tmux.Session{Name: "absent-stopped", SocketName: socket})
	missing := session.NewInstanceWithTool("no-pane", "/test", "shell")
	missing.ID = "no-pane"
	missing.SetTmuxSessionForTest(nil)
	reg := &matrixQueryRegistry{instances: append([]*session.Instance{archived, stopped, missing}, instances...)}
	s, path := startTestServer(t, Options{QueryTransport: m, Interval: 40 * time.Millisecond, DemandTTL: 500 * time.Millisecond, PassBudget: 2 * time.Second,
		LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }, ValidateProfile: func(string) error { return nil }})
	client := NewClient(path, nil)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := client.Snapshot(ctx, "query-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != 5 {
		t.Fatal("missing original source observations")
	}
	for _, row := range first.Sessions {
		if !strings.HasPrefix(row.ID, "query-") {
			continue
		}
		if row.Error != "" || row.ObservedAt.IsZero() || !row.TmuxAliveKnown || !row.TmuxAlive {
			t.Fatalf("source verdict lost: %+v", row)
		}
	}
	stats := s.Status().QueryTransport
	if stats.Connections != 1 || stats.Successes == 0 || stats.Starts != 1 {
		t.Fatalf("collector did not share a query client: %+v", stats)
	}
	for range 3 {
		if err := client.Refresh(ctx, "query-test"); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Status().QueryTransport; got.Starts != stats.Starts {
		t.Fatalf("stable profile order churned anchor: %+v", got)
	}
	reg.failed.Store(true)
	if err := client.Refresh(ctx, "query-test"); err == nil {
		t.Fatal("registry failure promoted to successful observation")
	}
	reg.failed.Store(false)
	if err := client.Refresh(ctx, "query-test"); err != nil {
		t.Fatal(err)
	}
	matrixQueryEventually(t, func() bool { return s.Status().ActiveProfiles == 0 && m.Diagnostics().Connections == 0 })
	if got := strings.TrimSpace(native("list-clients", "-F", "#{client_pid}")); got != "" {
		t.Fatalf("expired profile retained client: %s", got)
	}
	if _, err := client.Snapshot(ctx, "query-test"); err != nil {
		t.Fatal(err)
	}
	// Re-registration is lazy: tiered status sampling may not need new pane
	// bytes immediately. A fresh ordinary reader proves the renewed route.
	reader := &tmux.Session{Name: "query-first", SocketName: socket}
	if _, err := reader.CapturePane(); err != nil {
		t.Fatal(err)
	}
	if m.Diagnostics().Connections != 1 || m.Diagnostics().Starts <= stats.Starts {
		t.Fatal("new demand did not reopen query transport")
	}
	if err := client.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	matrixQueryEventually(t, func() bool { return m.Diagnostics().Connections == 0 && reg.closed.Load() })
	if got := native("list-panes", "-a", "-F", format); got != before {
		t.Fatalf("monitor query lifecycle changed panes: before %q after %q", before, got)
	}
	if got := strings.TrimSpace(native("list-clients", "-F", "#{client_pid}")); got != "" {
		t.Fatalf("stopped monitor retained client: %s", got)
	}
}

func TestMatrixMonitorQueryTransportCloseWithoutServeAndListenerFailure(t *testing.T) {
	socket, native := matrixMonitorQueryFixture(t)
	for _, reason := range []string{"never-served", "listener-error", "context-canceled"} {
		t.Run(reason, func(t *testing.T) {
			m := tmux.EnableQueryTransport(context.Background())
			defer m.Close()
			target := &tmux.Session{Name: "query-first", SocketName: socket}
			m.RegisterTargets("test", []*tmux.Session{target})
			if _, err := target.CapturePane(); err != nil {
				t.Fatal(err)
			}
			if m.Diagnostics().Connections != 1 {
				t.Fatal("fixture did not open persistent client")
			}
			s := New(Options{QueryTransport: m})
			if reason == "never-served" {
				s.Close()
			} else {
				ln, err := net.Listen("unix", canonicalTemp(t)+"/query.sock")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if reason == "listener-error" {
					_ = ln.Close()
				} else {
					cancel()
				}
				if err := s.Serve(ctx, ln); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Fatal(err)
				}
			}
			if m.Diagnostics().Connections != 0 {
				t.Fatal("server exit retained client")
			}
			if got := strings.TrimSpace(native("list-clients", "-F", "#{client_pid}")); got != "" {
				t.Fatalf("owned physical client survived: %s", got)
			}
		})
	}
}

func TestMatrixMonitorQueryTransportProbeOverrideStaysNativeFree(t *testing.T) {
	calls := matrixMonitorBatchFixture(t)
	m := tmux.EnableQueryTransport(context.Background())
	defer m.Close()
	reg := &matrixCaptureBatchRegistry{instances: matrixMonitorBatchInstances(2)}
	s := New(Options{QueryTransport: m, LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }, Probe: fixtureProbe})
	defer s.Close()
	p := newProfile("override", s.epoch)
	s.collect(p)
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("override executed native tmux: %v", err)
	}
	if got := m.Diagnostics(); got.Starts != 0 || got.Queries != 0 {
		t.Fatalf("override used query transport: %+v", got)
	}
}
