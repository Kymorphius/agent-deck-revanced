package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: exercise real collect scheduling with no Probe override.
// A private scripted tmux keeps all requests away from production sockets.
type matrixCaptureBatchRegistry struct {
	instances []*session.Instance
	loaded    bool
}

func (r *matrixCaptureBatchRegistry) Changed() (bool, error) { return !r.loaded, nil }
func (r *matrixCaptureBatchRegistry) Load() ([]*session.Instance, error) {
	r.loaded = true
	return r.instances, nil
}
func (*matrixCaptureBatchRegistry) Close() error { return nil }

func matrixMonitorBatchFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MATRIX_MONITOR_BATCH_CALLS"
if [ "$1" = -u ]; then shift; fi
socket=
if [ "$1" = -L ]; then socket="$2"; shift 2; fi
batch=0
for arg in "$@"; do if [ "$arg" = ';' ]; then batch=1; fi; done
if [ "$batch" = 1 ] && [ -n "$MATRIX_MONITOR_BATCH_SLOW_SOCKET" ] && [ "$socket" = "$MATRIX_MONITOR_BATCH_SLOW_SOCKET" ]; then
 touch "$MATRIX_MONITOR_BATCH_READY"
 while [ ! -e "$MATRIX_MONITOR_BATCH_RELEASE" ]; do sleep .002; done
fi
case "$1" in
list-sessions) exit 0 ;;
list-panes) exit 0 ;;
has-session) exit 1 ;;
show-environment) exit 1 ;;
capture-pane) printf 'ordinary\n'; exit 0 ;;
list-windows) exit 0 ;;
esac
while [ "$#" -gt 0 ]; do
 case "$1" in
 ';') shift ;;
 display-message) printf '%s\n' "$3"; shift 3 ;;
 capture-pane) printf '\nraw:%s\n\n' "$3"; shift 5 ;;
 *) exit 1 ;;
 esac
done
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MATRIX_MONITOR_BATCH_CALLS", calls)
	t.Setenv("MATRIX_MONITOR_BATCH_READY", filepath.Join(dir, "ready"))
	t.Setenv("MATRIX_MONITOR_BATCH_RELEASE", filepath.Join(dir, "release"))
	return calls
}

func matrixMonitorBatchCleanup(t *testing.T, s *Server, p *profileState) {
	t.Helper()
	t.Cleanup(func() {
		s.cancel()
		if p.sse != nil {
			p.sse.Stop()
		}
		s.hookMu.Lock()
		hooks, done := s.hooks, s.hookDone
		s.hooks = nil
		s.hookMu.Unlock()
		if hooks != nil {
			hooks.Stop()
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("private hook watcher did not stop")
			}
		}
	})
}

func matrixMonitorBatchInstances(n int) []*session.Instance {
	var instances []*session.Instance
	for i := 0; i < n; i++ {
		inst := session.NewInstanceWithTool(fmt.Sprintf("pane-%d", i), "/test", "shell")
		inst.ID = fmt.Sprintf("pane-%d", i)
		inst.Status = session.StatusWaiting
		inst.CreatedAt = time.Now().Add(-time.Hour)
		inst.SetTmuxSessionForTest(&tmux.Session{Name: "agentdeck_" + inst.ID, SocketName: tmux.DefaultSocketName(), Command: "sh"})
		instances = append(instances, inst)
	}
	return instances
}

func TestMatrixCaptureBatchCollectFlushesReservationsBelowGroupLimit(t *testing.T) {
	for _, workers := range []int{1, 2, 3, 10} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			calls := matrixMonitorBatchFixture(t)
			reg := &matrixCaptureBatchRegistry{instances: matrixMonitorBatchInstances(7)}
			s := New(Options{MaxWorkers: workers, PassBudget: 2 * time.Second, LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }})
			p := newProfile("batch-test", s.epoch)
			matrixMonitorBatchCleanup(t, s, p)
			started := time.Now()
			s.collect(p)
			if time.Since(started) >= 2*time.Second {
				t.Fatal("reserved partial group blocked behind its own full worker channel")
			}
			if len(s.workers) != 0 {
				t.Fatalf("finished probes retained %d worker slots", len(s.workers))
			}
			if p.latest == nil || len(p.latest.snapshot.Sessions) != 7 {
				t.Fatal("collection did not publish all originally eligible rows")
			}
			for _, row := range p.latest.snapshot.Sessions {
				if row.ObservedAt.IsZero() || strings.Contains(row.Error, "in flight") || strings.Contains(row.Error, "pending") {
					t.Fatalf("probe did not finish: %+v", row)
				}
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			batches := 0
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, " ; ") {
					batches++
				}
			}
			if workers == 1 && batches != 0 {
				t.Fatal("single worker unnecessarily prefetched a single-pane batch")
			}
			if workers > 1 && batches == 0 {
				t.Fatal("real bounded collector never launched its partial batch group")
			}
		})
	}
}

func TestMatrixCaptureBatchSlowSocketDoesNotHoldFastSocketProbes(t *testing.T) {
	calls := matrixMonitorBatchFixture(t)
	instances := matrixMonitorBatchInstances(4)
	for i, inst := range instances {
		if i < 2 {
			inst.GetTmuxSession().SocketName = "batch-slow-socket"
		} else {
			inst.GetTmuxSession().SocketName = "batch-fast-socket"
		}
	}
	t.Setenv("MATRIX_MONITOR_BATCH_SLOW_SOCKET", "batch-slow-socket")
	release := filepath.Join(filepath.Dir(calls), "release")
	reg := &matrixCaptureBatchRegistry{instances: instances}
	s := New(Options{MaxWorkers: 4, PassBudget: 150 * time.Millisecond, LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }})
	p := newProfile("socket-test", s.epoch)
	matrixMonitorBatchCleanup(t, s, p)
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	s.collect(p)
	if p.latest == nil || len(p.latest.snapshot.Sessions) != 4 {
		t.Fatal("collector did not publish its bounded pass")
	}
	for i, row := range p.latest.snapshot.Sessions {
		pending := strings.Contains(row.Error, "in flight")
		if (i < 2) != pending {
			t.Fatalf("slow socket contaminated independent fast socket publication: row %d pending=%v, %+v", i, pending, row)
		}
	}
	if n := len(s.workers); n != 2 {
		t.Fatalf("slow calls retained %d slots, want their original 2", n)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(s.workers) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("released slow batch retained its worker slots")
		}
		time.Sleep(time.Millisecond)
	}
	for _, record := range p.records {
		s.finish(p, record)
	}
}

func TestMatrixCaptureBatchProbeOverrideNeverStartsTmux(t *testing.T) {
	calls := matrixMonitorBatchFixture(t)
	reg := &matrixCaptureBatchRegistry{instances: matrixMonitorBatchInstances(9)}
	s := New(Options{MaxWorkers: 3, PassBudget: time.Second, LoadProfile: func(string) (ProfileRegistry, error) { return reg, nil }, Probe: fixtureProbe})
	t.Cleanup(s.cancel)
	p := newProfile("override-test", s.epoch)
	s.collect(p)
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("Probe override touched native tmux: %v", err)
	}
	if len(s.workers) != 0 || p.latest == nil || len(p.latest.snapshot.Sessions) != 9 {
		t.Fatal("override did not preserve bounded ordinary probe completion")
	}
}
