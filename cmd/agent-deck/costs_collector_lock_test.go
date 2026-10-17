//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func matrixCostCollectorProfileDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func matrixWaitCostCollector(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// matrix/hardened: consumers use the real owner loop and real kernel lock;
// only transcript reading is injected to count expensive first passes.
func TestMatrixCostCollectorOnlyOwnerReadsForManyConsumers(t *testing.T) {
	for _, consumers := range []int{1, 4, 16} {
		t.Run(fmt.Sprintf("consumers-%d", consumers), func(t *testing.T) {
			dir := matrixCostCollectorProfileDir(t)
			var reads atomic.Int32
			started := make(chan struct{}, consumers)
			collect := func(ctx context.Context, lock *costCollectorLock) error {
				if err := lock.valid(); err != nil {
					return err
				}
				reads.Add(1)
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}
			stops := []func(){startCostCollectorOwner(context.Background(), dir, 10*time.Millisecond, collect)}
			t.Cleanup(func() {
				// Stop waiting consumers first so shutdown cannot initiate takeover.
				for n := len(stops) - 1; n >= 0; n-- {
					stops[n]()
				}
			})
			matrixWaitCostCollector(t, started, "initial owner")
			for n := 1; n < consumers; n++ {
				stops = append(stops, startCostCollectorOwner(context.Background(), dir, 10*time.Millisecond, collect))
			}
			time.Sleep(60 * time.Millisecond)
			if got := reads.Load(); got != 1 {
				t.Fatalf("%d consumers performed %d first reads, want one", consumers, got)
			}
		})
	}
}

// matrix/hardened: cancellation must not expose the lease until storage/read
// cleanup returns, while a waiting view must take over after that boundary.
func TestMatrixCostCollectorTakeoverWaitsForReaderCleanup(t *testing.T) {
	dir := matrixCostCollectorProfileDir(t)
	ownerStarted, readerCanceled, readerClosed, stopDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	stopOwner := startCostCollectorOwner(context.Background(), dir, 10*time.Millisecond, func(ctx context.Context, _ *costCollectorLock) error {
		close(ownerStarted)
		<-ctx.Done()
		close(readerCanceled)
		<-readerClosed
		return ctx.Err()
	})
	matrixWaitCostCollector(t, ownerStarted, "initial owner")
	secondStarted := make(chan struct{}, 1)
	stopSecond := startCostCollectorOwner(context.Background(), dir, 10*time.Millisecond, func(ctx context.Context, _ *costCollectorLock) error {
		secondStarted <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	t.Cleanup(stopSecond)
	go func() { stopOwner(); close(stopDone) }()
	matrixWaitCostCollector(t, readerCanceled, "owner cancellation")
	if lock, owned, err := tryCostCollectorLock(dir); err != nil || owned {
		if lock != nil {
			_ = lock.Close()
		}
		close(readerClosed)
		t.Fatalf("reader cleanup released ownership early: owned=%v err=%v", owned, err)
	}
	select {
	case <-secondStarted:
		close(readerClosed)
		t.Fatal("second collector started before reader cleanup completed")
	default:
	}
	close(readerClosed)
	matrixWaitCostCollector(t, stopDone, "owner stop")
	matrixWaitCostCollector(t, secondStarted, "second owner takeover")
	// Concurrent or repeated stop calls remain safe and return promptly.
	stopOwner()
	stopSecond()
	stopSecond()
}

func TestMatrixCostCollectorProfilesAreIndependentAndCancelWhileWaiting(t *testing.T) {
	first := matrixCostCollectorProfileDir(t)
	second := matrixCostCollectorProfileDir(t)
	firstLock, owned, err := tryCostCollectorLock(first)
	if err != nil || !owned {
		t.Fatalf("first profile: %v, %v", owned, err)
	}
	defer firstLock.Close()
	secondLock, owned, err := tryCostCollectorLock(second)
	if err != nil || !owned {
		t.Fatalf("second profile was blocked by first: %v, %v", owned, err)
	}
	defer secondLock.Close()
	var reads atomic.Int32
	stop := startCostCollectorOwner(context.Background(), first, time.Hour, func(context.Context, *costCollectorLock) error { reads.Add(1); return nil })
	start := time.Now()
	stop()
	stop()
	if time.Since(start) > 250*time.Millisecond || reads.Load() != 0 {
		t.Fatal("waiting consumer ignored cancellation or read without ownership")
	}
}

func TestMatrixCostCollectorRejectsUnsafeOwnershipPaths(t *testing.T) {
	for _, kind := range []string{"profile-symlink", "profile-shared", "directory-symlink", "directory-shared", "file-symlink", "file-hardlink", "file-shared", "file-fifo"} {
		t.Run(kind, func(t *testing.T) {
			profile := matrixCostCollectorProfileDir(t)
			dir := filepath.Join(profile, costCollectorDirectory)
			path := filepath.Join(dir, costCollectorLockName)
			if strings.HasPrefix(kind, "profile-") {
				if kind == "profile-shared" {
					if err := os.Chmod(profile, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					alias := filepath.Join(matrixCostCollectorProfileDir(t), "linked-profile")
					if err := os.Symlink(profile, alias); err != nil {
						t.Fatal(err)
					}
					profile = alias
				}
			} else if kind == "directory-symlink" {
				if err := os.Symlink(matrixCostCollectorProfileDir(t), dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "directory-shared":
					if err := os.Chmod(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				case "file-symlink", "file-hardlink":
					target := filepath.Join(matrixCostCollectorProfileDir(t), "private-target")
					if err := os.WriteFile(target, []byte("preserve target"), 0o600); err != nil {
						t.Fatal(err)
					}
					link := os.Link
					if kind == "file-symlink" {
						link = os.Symlink
					}
					if err := link(target, path); err != nil {
						t.Fatal(err)
					}
				case "file-shared":
					if err := os.WriteFile(path, nil, 0o644); err != nil {
						t.Fatal(err)
					}
				case "file-fifo":
					if err := unix.Mkfifo(path, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			lock, owned, err := tryCostCollectorLock(profile)
			if lock != nil {
				_ = lock.Close()
			}
			if err == nil || owned {
				t.Fatalf("unsafe path accepted: owned=%v err=%v", owned, err)
			}
		})
	}
}

func TestMatrixCostCollectorLockIdentityAndPermissionsSurviveClose(t *testing.T) {
	dir := matrixCostCollectorProfileDir(t)
	lock, owned, err := tryCostCollectorLock(dir)
	if err != nil || !owned {
		t.Fatalf("acquire: %v, %v", owned, err)
	}
	path := lock.path
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != 0o600 {
		t.Fatalf("lock permissions: %o", before.Mode().Perm())
	}
	pidBytes, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(pidBytes)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("owner PID readback missing: %q, %v", pidBytes, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("close removed/replaced the stable lock inode")
	}
	lock, owned, err = tryCostCollectorLock(dir)
	if err != nil || !owned {
		t.Fatalf("takeover after close: %v, %v", owned, err)
	}
	defer lock.Close()
	if err := os.Rename(path, path+".retired"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lock.valid(); err == nil {
		t.Fatal("replaced ownership inode remained valid")
	}
}

// matrix/hardened: the inherited helper HOME is already isolated by the runner;
// the existing TestMain helper lane prevents replacing its explicit lock path.
func TestMatrixCostCollectorLockProcessHelper(t *testing.T) {
	if os.Getenv("AGENTDECK_TEST_COST_LOCK_HELPER") == "" {
		return
	}
	lock, owned, err := tryCostCollectorLock(os.Getenv("AGENTDECK_TEST_COST_LOCK_DIR"))
	if err != nil || !owned {
		t.Fatalf("child lock: owned=%v error=%v", owned, err)
	}
	_ = lock // Intentionally rely on kernel process-exit release in this helper.
	fmt.Fprintln(os.Stdout, "COST_LOCK_READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestMatrixCostCollectorCrossProcessExitReleasesOwnership(t *testing.T) {
	dir := matrixCostCollectorProfileDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestMatrixCostCollectorLockProcessHelper$")
	cmd.Env = append(os.Environ(), "AGENT_DECK_TASK6_HELPER_PROCESS=1", "AGENTDECK_TEST_COST_LOCK_HELPER=1", "AGENTDECK_TEST_COST_LOCK_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "COST_LOCK_READY" {
				close(ready)
				return
			}
		}
	}()
	matrixWaitCostCollector(t, ready, "cross-process lock holder")
	lockPath := filepath.Join(dir, costCollectorDirectory, costCollectorLockName)
	childPID, err := os.ReadFile(lockPath)
	if err != nil || strings.TrimSpace(string(childPID)) != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("child owner PID readback missing: %q, %v", childPID, err)
	}
	lock, owned, err := tryCostCollectorLock(dir)
	if lock != nil {
		_ = lock.Close()
	}
	if err != nil || owned {
		t.Fatalf("parent entered child ownership: owned=%v err=%v", owned, err)
	}
	stillChildPID, err := os.ReadFile(lockPath)
	if err != nil || string(stillChildPID) != string(childPID) {
		t.Fatal("waiting consumer rewrote the owner's PID")
	}
	// Kill only this owned helper to prove crash release, without stale-PID waits.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	finished = true
	lock, owned, err = tryCostCollectorLock(dir)
	if err != nil || !owned {
		t.Fatalf("process exit did not release lock: owned=%v err=%v", owned, err)
	}
	defer lock.Close()
	newPID, err := os.ReadFile(lockPath)
	if err != nil || strings.TrimSpace(string(newPID)) != strconv.Itoa(os.Getpid()) {
		t.Fatal("takeover did not update the diagnostic owner PID")
	}
}
