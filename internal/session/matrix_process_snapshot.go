package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// matrix/hardened: a status pass owns this immutable process/argv snapshot.
// Live pane and FD probes remain per session; it is never a global or TTL cache.
type matrixHostProcessSnapshot struct {
	childrenByParent map[int][]int
	argsByPID        map[int]string
	err              error
}

// matrix/hardened: sync.Once makes a zero-valued pass safe for concurrent
// workers and pins both successful and failed enumeration to this sweep only.
func (p *StatusUpdatePass) hostProcesses() *matrixHostProcessSnapshot {
	p.hostProcessOnce.Do(func() {
		// -ww preserves full argv, including long commands and paths with spaces.
		out, err := exec.Command("ps", "-ww", "-eo", "pid=,ppid=,args=").Output()
		p.hostProcessSnapshot = matrixParseHostProcesses(out)
		if err != nil {
			p.hostProcessSnapshot.err = errors.Join(p.hostProcessSnapshot.err,
				fmt.Errorf("codex process probe: shared ps: %w", err))
		}
	})
	return p.hostProcessSnapshot
}

// matrix/hardened: parse only the numeric prefix; preserve the rest of argv
// verbatim rather than reducing it to comm's basename or collapsing spaces.
func matrixParseHostProcesses(output []byte) *matrixHostProcessSnapshot {
	snapshot := &matrixHostProcessSnapshot{
		childrenByParent: make(map[int][]int), argsByPID: make(map[int]string),
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		row := strings.TrimSpace(scanner.Text())
		if row == "" {
			continue
		}
		fields := strings.Fields(row)
		if len(fields) < 3 {
			snapshot.err = errors.Join(snapshot.err, fmt.Errorf("codex process probe: invalid shared ps row %d", line))
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr != nil || ppidErr != nil || pid <= 0 || ppid < 0 {
			snapshot.err = errors.Join(snapshot.err, fmt.Errorf("codex process probe: invalid shared ps pid on row %d", line))
			continue
		}
		if _, duplicate := snapshot.argsByPID[pid]; duplicate {
			snapshot.err = errors.Join(snapshot.err, fmt.Errorf("codex process probe: duplicate shared ps pid %d", pid))
			continue
		}
		tail := strings.TrimSpace(row[len(fields[0]):])
		snapshot.argsByPID[pid] = strings.TrimSpace(tail[len(fields[1]):])
		snapshot.childrenByParent[ppid] = append(snapshot.childrenByParent[ppid], pid)
	}
	snapshot.err = errors.Join(snapshot.err, scanner.Err())
	if len(snapshot.argsByPID) == 0 {
		snapshot.err = errors.Join(snapshot.err, errors.New("codex process probe: shared ps contained no processes"))
	}
	return snapshot
}

// matrix/hardened: traverse the already-parsed table rather than parsing and
// rebuilding its indexes for every session. Missing fresh pane roots require
// the existing live pgrep fallback instead of a false empty process forest.
func (s *matrixHostProcessSnapshot) processTree(rootPIDs []int) ([]int, error) {
	if s.err != nil {
		return nil, s.err
	}
	queue := make([]int, 0, len(rootPIDs))
	seen := make(map[int]bool)
	for _, pid := range rootPIDs {
		if _, present := s.argsByPID[pid]; !present {
			return nil, fmt.Errorf("codex process probe: pane pid %d missing from shared ps process table", pid)
		}
		if !seen[pid] {
			seen[pid] = true
			queue = append(queue, pid)
		}
	}
	var pids []int
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		pids = append(pids, pid)
		for _, child := range s.childrenByParent[pid] {
			if !seen[child] {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
	return pids, nil
}
