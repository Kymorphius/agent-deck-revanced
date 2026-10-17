package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: real subprocess fixtures count table, argv, pane and
// fallback probes without replacing process-wide execution functions.
type matrixProcessFixture struct {
	t   *testing.T
	dir string
}

func newMatrixProcessFixture(t *testing.T, table string) *matrixProcessFixture {
	t.Helper()
	f := &matrixProcessFixture{t: t, dir: t.TempDir()}
	t.Setenv("MATRIX_PROCESS_FIXTURE", f.dir)
	t.Setenv("CODEX_HOME", f.dir)
	f.write("table", table)
	f.write("legacy-table", "100 1\n101 100\n102 100\n200 1\n201 200\n")
	f.write("tmux", `#!/bin/sh
printf 'tmux %s\n' "$*" >> "$MATRIX_PROCESS_FIXTURE/calls"
if [ "$1" = -u ]; then shift; fi
if [ "$1" = -L ]; then shift 2; fi
case "$1" in
has-session) exit 0;;
list-panes)
 while [ "$#" -gt 0 ]; do
  if [ "$1" = -t ]; then target=$2; break; fi
  shift
 done
 /bin/cat "$MATRIX_PROCESS_FIXTURE/panes_${target#matrix_}";;
*) exit 1;;
esac
`)
	f.write("ps", `#!/bin/sh
printf 'ps %s\n' "$*" >> "$MATRIX_PROCESS_FIXTURE/calls"
case "$*" in
*pid=,ppid=,args=*)
 /bin/cat "$MATRIX_PROCESS_FIXTURE/table"
 if [ -f "$MATRIX_PROCESS_FIXTURE/ps-error" ]; then exit 2; fi;;
*pid=,ppid=*) /bin/cat "$MATRIX_PROCESS_FIXTURE/legacy-table";;
*) /bin/cat "$MATRIX_PROCESS_FIXTURE/args_$2" 2>/dev/null;;
esac
`)
	f.write("pgrep", `#!/bin/sh
printf 'pgrep %s\n' "$*" >> "$MATRIX_PROCESS_FIXTURE/calls"
if [ -f "$MATRIX_PROCESS_FIXTURE/pgrep-error" ]; then exit 2; fi
if [ -s "$MATRIX_PROCESS_FIXTURE/children_$2" ]; then
 /bin/cat "$MATRIX_PROCESS_FIXTURE/children_$2"
else exit 1
fi
`)
	f.write("docker", `#!/bin/sh
printf 'docker exec\n' >> "$MATRIX_PROCESS_FIXTURE/calls"
/bin/cat "$MATRIX_PROCESS_FIXTURE/docker-output"
`)
	f.write("lsof", `#!/bin/sh
printf 'lsof %s\n' "$*" >> "$MATRIX_PROCESS_FIXTURE/calls"
for arg; do pid=$arg; done
/bin/cat "$MATRIX_PROCESS_FIXTURE/lsof_$pid" 2>/dev/null
`)
	for _, command := range []string{"tmux", "ps", "pgrep", "docker", "lsof"} {
		if err := os.Chmod(filepath.Join(f.dir, command), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, pid := range []int{100, 101, 102, 200, 201} {
		args := "sh\n"
		if pid == 101 || pid == 201 {
			args = "node /opt/codex.js --full argv with spaces\n"
		}
		f.write("args_"+strconv.Itoa(pid), args)
	}
	f.write("children_100", "101\n102\n")
	f.write("children_200", "201\n")
	t.Setenv("PATH", f.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func (f *matrixProcessFixture) write(name, contents string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(contents), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *matrixProcessFixture) instance(pid int) *Instance {
	f.t.Helper()
	f.write("panes_"+strconv.Itoa(pid), strconv.Itoa(pid)+"\n")
	return &Instance{
		Tool: "codex", TmuxSocketName: "matrix-process-test",
		tmuxSession: &tmux.Session{Name: fmt.Sprintf("matrix_%d", pid), SocketName: "matrix-process-test"},
	}
}

func (f *matrixProcessFixture) count(fragment string) int {
	f.t.Helper()
	out, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, fragment) {
			count++
		}
	}
	return count
}

// matrix/hardened: keep full argv matching, including a wrapper whose
// executable is node and only a later path/argument contains Codex.
func TestMatrixProcessSnapshotParserAndArgs(t *testing.T) {
	args := `/Applications/Codex CLI/node /path with  two spaces/CODEX.js --prompt "a b"`
	snapshot := matrixParseHostProcesses([]byte("100 1 sh\n101\t100  " + args + "\n200 0 bash\n201 200 codex\n"))
	if snapshot.err != nil || snapshot.argsByPID[101] != args {
		t.Fatalf("argv = %q, error = %v, want %q", snapshot.argsByPID[101], snapshot.err, args)
	}
	pids, err := snapshot.processTree([]int{100, 200, 100})
	if err != nil || !reflect.DeepEqual(pids, []int{100, 200, 101, 201}) {
		t.Fatalf("process forest = (%v, %v)", pids, err)
	}
	if _, err := snapshot.processTree([]int{999}); err == nil {
		t.Fatal("missing fresh pane root was accepted")
	}
	for name, output := range map[string]string{
		"empty": "\n", "partial": "100 1 sh\nbad row\n", "pid": "0 1 sh\n",
		"parent": "100 -1 sh\n", "missing args": "100 1\n", "duplicate": "100 1 sh\n100 1 codex\n",
		"scanner failure": "100 1 " + strings.Repeat("x", 1024*1024),
	} {
		t.Run(name, func(t *testing.T) {
			parsed := matrixParseHostProcesses([]byte(output))
			if _, err := parsed.processTree([]int{100}); err == nil {
				t.Fatal("incomplete process/argv table was accepted")
			}
		})
	}
}

// matrix/hardened: concurrent workers share exactly one host table/argv
// command while every session still enumerates its own fresh tmux panes.
func TestMatrixProcessSnapshotSharedConcurrentWorkers(t *testing.T) {
	f := newMatrixProcessFixture(t, "100 1 sh\n101 100 node /opt/CODEX.js with  spaces\n102 100 bash\n200 1 sh\n201 200 codex resume id\n301 999 codex unrelated\n")
	instances := []*Instance{f.instance(100), f.instance(200)}
	var pass StatusUpdatePass
	var workers sync.WaitGroup
	errorsCh := make(chan error, 20)
	for n := 0; n < 20; n++ {
		workers.Add(1)
		go func(n int) {
			defer workers.Done()
			candidates, err, shared := instances[n%2].collectCodexProcessCandidatesForPass(&pass, false)
			want := []int{101 + 100*(n%2)}
			if err != nil || !shared || !reflect.DeepEqual(candidates, want) {
				errorsCh <- fmt.Errorf("candidates = (%v, %v, shared=%v), want %v", candidates, err, shared, want)
			}
		}(n)
	}
	workers.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if tables, args, panes, fallback := f.count("pid=,ppid=,args="), f.count("ps -p "), f.count(" list-panes "), f.count("pgrep "); tables != 1 || args != 0 || panes != 20 || fallback != 0 {
		t.Fatalf("table=%d, per-PID argv=%d, fresh panes=%d, fallback=%d", tables, args, panes, fallback)
	}
	f.write("table", "100 1 sh\n101 100 bash\n")
	var nextPass StatusUpdatePass
	candidates, err, _ := instances[0].collectCodexProcessCandidatesForPass(&nextPass, false)
	if err != nil || len(candidates) != 0 || f.count("pid=,ppid=,args=") != 2 {
		t.Fatalf("new pass reused a previous snapshot: candidates=%v, error=%v", candidates, err)
	}
}

// matrix/hardened: standalone CLI probes retain fresh full trees and per-PID
// argv inspection instead of sharing any snapshot across independent calls.
func TestMatrixProcessSnapshotStandaloneStaysFresh(t *testing.T) {
	f := newMatrixProcessFixture(t, "")
	i := f.instance(100)
	for n := 0; n < 2; n++ {
		candidates, err := i.collectCodexProcessCandidates()
		if err != nil || !reflect.DeepEqual(candidates, []int{101}) {
			t.Fatalf("standalone candidates = (%v, %v)", candidates, err)
		}
	}
	if full, legacy, argv := f.count("pid=,ppid=,args="), f.count("ps -eo pid=,ppid="), f.count("ps -p "); full != 0 || legacy != 2 || argv != 6 {
		t.Fatalf("standalone shared table=%d, fresh table=%d, fresh argv=%d", full, legacy, argv)
	}
}

// matrix/hardened: malformed/empty/failed shared tables retain complete live
// pgrep and per-PID argv fallback; a failed fallback remains undetermined.
func TestMatrixProcessSnapshotFallbackCompleteness(t *testing.T) {
	for _, kind := range []string{"empty", "partial", "missing root", "ps failure", "pgrep failure", "argv failure"} {
		t.Run(kind, func(t *testing.T) {
			table := "100 1 sh\n101 100 codex\n"
			switch kind {
			case "empty", "pgrep failure", "argv failure":
				table = ""
			case "partial":
				table += "malformed row\n"
			case "missing root":
				table = "500 1 codex\n"
			}
			f := newMatrixProcessFixture(t, table)
			i := f.instance(100)
			if kind == "ps failure" {
				f.write("ps-error", "")
			}
			if kind == "pgrep failure" {
				f.write("pgrep-error", "")
			}
			if kind == "argv failure" {
				if err := os.Remove(filepath.Join(f.dir, "args_101")); err != nil {
					t.Fatal(err)
				}
			}
			var pass StatusUpdatePass
			for n := 0; n < 2; n++ {
				candidates, err, shared := i.collectCodexProcessCandidatesForPass(&pass, false)
				wantErr := kind == "pgrep failure" || kind == "argv failure"
				if shared || (err != nil) != wantErr || (!wantErr && !reflect.DeepEqual(candidates, []int{101})) {
					t.Fatalf("fallback candidates = (%v, %v, shared=%v)", candidates, err, shared)
				}
			}
			if f.count("pid=,ppid=,args=") != 1 || f.count("pgrep ") == 0 || f.count("ps -p ") == 0 {
				t.Fatal("fallback did not retain one shared enumeration and fresh process probes")
			}
			if kind == "pgrep failure" {
				if _, _, err := i.queryCodexSessionFromProcessFilesForPass(&pass); err == nil {
					t.Fatal("incomplete discovery became definitive absence")
				}
				i.CodexStartedAt = time.Now().Add(-time.Second).UnixMilli()
				seedCodexRolloutWithMeta(t, f.dir, uniqueSID(t), "user", "", false)
				i.updateCodexSessionForPass(nil, true, &pass, false)
				if i.CodexSessionID != "" {
					t.Fatalf("incomplete live evidence bound a disk guess: %q", i.CodexSessionID)
				}
			}
		})
	}
}

// matrix/hardened: host candidates share ps while native FD reads and the
// rejection of subagent rollouts still happen separately for every session.
func TestMatrixProcessSnapshotNativePositiveKeepsFreshFDAndFilter(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux uses /proc directly; candidate-sharing and fallback contracts are platform-independent")
	}
	f := newMatrixProcessFixture(t, "100 1 sh\n101 100 node /opt/codex.js\n200 1 sh\n201 200 codex\n")
	instances := []*Instance{f.instance(100), f.instance(200)}
	ids := []string{uniqueSID(t), uniqueSID(t)}
	paths := []string{
		seedCodexRolloutWithMeta(t, f.dir, ids[0], "user", "", false),
		seedCodexRolloutWithMeta(t, f.dir, ids[1], "user", "", false),
	}
	subPath := seedCodexRolloutWithMeta(t, f.dir, uniqueSID(t), "subagent", ids[0], false)
	restore := procfdOpenVnodePaths
	t.Cleanup(func() { procfdOpenVnodePaths = restore })
	fdCalls := map[int]int{}
	procfdOpenVnodePaths = func(pid int) ([]string, error) {
		fdCalls[pid]++
		if pid == 101 {
			return []string{subPath, paths[0]}, nil
		}
		return []string{paths[1]}, nil
	}
	var pass StatusUpdatePass
	for n := 0; n < 4; n++ {
		got, missing, err := instances[n%2].queryCodexSessionFromProcessFilesForPass(&pass)
		if got != ids[n%2] || missing != "" || err != nil {
			t.Fatalf("live native binding = (%q, %q, %v), want %q", got, missing, err, ids[n%2])
		}
	}
	if fdCalls[101] != 2 || fdCalls[201] != 2 || f.count("pid=,ppid=,args=") != 1 || f.count("ps -p ") != 0 || f.count("pgrep ") != 0 {
		t.Fatalf("FD probes=%v, table=%d, argv=%d, pgrep=%d", fdCalls, f.count("pid=,ppid=,args="), f.count("ps -p "), f.count("pgrep "))
	}
}

// matrix/hardened: shared absence may predate a new child; discover it with a
// live tree/argv/FD check instead of bootstrapping from a historical rollout.
func TestMatrixProcessSnapshotNegativeRechecksNewChild(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("native FD seam is for the non-Linux query path")
	}
	f := newMatrixProcessFixture(t, "100 1 sh\n")
	i := f.instance(100)
	id := uniqueSID(t)
	path := seedCodexRolloutWithMeta(t, f.dir, id, "user", "", false)
	restore := procfdOpenVnodePaths
	t.Cleanup(func() { procfdOpenVnodePaths = restore })
	fdCalls := 0
	procfdOpenVnodePaths = func(pid int) ([]string, error) {
		fdCalls++
		if pid != 101 {
			t.Errorf("unexpected native candidate %d", pid)
		}
		return []string{path}, nil
	}
	var pass StatusUpdatePass
	got, _, err := i.queryCodexSessionFromProcessFilesForPass(&pass)
	if got != id || err != nil || fdCalls != 1 || f.count("pid=,ppid=,args=") != 1 || f.count("pgrep ") != 3 || f.count("ps -p ") != 3 || f.count(" list-panes ") != 2 {
		t.Fatalf("new child = (%q, %v), FD=%d, table=%d, pgrep=%d, argv=%d, panes=%d", got, err, fdCalls, f.count("pid=,ppid=,args="), f.count("pgrep "), f.count("ps -p "), f.count(" list-panes "))
	}
}

// matrix/hardened: native failures after the live negative recheck keep lsof
// available as the existing compatibility fallback.
func TestMatrixProcessSnapshotNativeFailureKeepsLsof(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux uses /proc without lsof")
	}
	f := newMatrixProcessFixture(t, "100 1 sh\n101 100 codex\n")
	i := f.instance(100)
	id := uniqueSID(t)
	path := seedCodexRolloutWithMeta(t, f.dir, id, "user", "", false)
	f.write("lsof_101", path+"\n")
	restore := procfdOpenVnodePaths
	t.Cleanup(func() { procfdOpenVnodePaths = restore })
	procfdOpenVnodePaths = func(int) ([]string, error) { return nil, os.ErrPermission }
	var pass StatusUpdatePass
	got, missing, err := i.queryCodexSessionFromProcessFilesForPass(&pass)
	if got != id || missing != "" || err != nil || f.count("lsof ") != 1 || f.count("pid=,ppid=,args=") != 1 {
		t.Fatalf("lsof fallback = (%q, %q, %v), calls=%d", got, missing, err, f.count("lsof "))
	}
}

// matrix/hardened: Docker's own /proc path never initializes or reads the
// sweep's host snapshot, even when the caller supplied a status pass.
func TestMatrixProcessSnapshotDockerBypassesHost(t *testing.T) {
	f := newMatrixProcessFixture(t, "")
	i := &Instance{Sandbox: &SandboxConfig{Enabled: true}, SandboxContainer: "matrix-test-container"}
	id := uniqueSID(t)
	path := seedCodexRolloutWithMeta(t, f.dir, id, "user", "", false)
	f.write("docker-output", path+"\n")
	var pass StatusUpdatePass
	got, missing, err := i.queryCodexSessionFromProcessFilesForPass(&pass)
	if got != id || missing != "" || err != nil || pass.hostProcessSnapshot != nil || f.count("ps ") != 0 || f.count("docker exec") != 1 {
		t.Fatalf("Docker binding = (%q, %q, %v), host snapshot=%v", got, missing, err, pass.hostProcessSnapshot)
	}
}
