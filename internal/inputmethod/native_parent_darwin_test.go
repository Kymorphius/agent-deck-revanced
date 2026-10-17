//go:build darwin && cgo

package inputmethod

// matrix/hardened: compare the real ancestry with ps, including privileged
// login/sshd ancestors that reject PROC_PIDTBSDINFO for an ordinary user.
import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestInputMethodNativeParentChain(t *testing.T) {
	output, err := exec.Command("/bin/ps", "-axo", "pid=,ppid=,uid=").Output()
	if err != nil {
		t.Fatal(err)
	}
	type process struct{ parent, uid int }
	processes := map[int]process{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, e1 := strconv.Atoi(fields[0])
		parent, e2 := strconv.Atoi(fields[1])
		uid, e3 := strconv.Atoi(fields[2])
		if e1 == nil && e2 == nil && e3 == nil {
			processes[pid] = process{parent, uid}
		}
	}
	pid, privileged := os.Getpid(), 0
	for depth := 0; pid > 1 && depth < 64; depth++ {
		p, ok := processes[pid]
		if !ok {
			t.Fatalf("ancestor %d missing from ps snapshot", pid)
		}
		if p.uid != os.Getuid() {
			privileged++
		}
		if got := nativeParentPID(pid); got != p.parent {
			t.Fatalf("pid %d uid %d: native parent=%d, ps parent=%d", pid, p.uid, got, p.parent)
		}
		pid = p.parent
	}
	if pid != 1 {
		t.Fatalf("ancestry stopped at %d before launchd", pid)
	}
	if nativeParentPID(-1) != 0 || nativeParentPID(1<<30) != 0 {
		t.Fatal("invalid process IDs must not supply an ancestor")
	}
	t.Logf("verified complete ancestry including %d ancestors owned by another user", privileged)
}
