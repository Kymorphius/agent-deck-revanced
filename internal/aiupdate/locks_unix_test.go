//go:build darwin || linux

package aiupdate

// matrix/hardened: exercise native cross-process lock exclusion without daemon processes or real homes.
import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeLockHelper(t *testing.T) {
	path := os.Getenv("AIUPDATE_TEST_LOCK")
	if path == "" {
		return
	}
	file, err := acquireFileLock(context.Background(), path, os.Getenv("AIUPDATE_TEST_LOCKF") == "1")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fmt.Println("locked")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestNativeLocksExcludeConcurrentLifecycleAndInstaller(t *testing.T) {
	for _, name := range []string{"daemon-flock", "install-flock", "install-fcntl"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "install.lock")
			if name == "daemon-flock" {
				path = filepath.Join(root, "daemon.lock")
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "-test.run=^TestNativeLockHelper$")
			cmd.Env = append(os.Environ(), "AIUPDATE_TEST_LOCK="+path)
			if name == "install-fcntl" {
				cmd.Env = append(cmd.Env, "AIUPDATE_TEST_LOCKF=1")
			}
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
			t.Cleanup(func() { stdin.Close(); cmd.Wait() })
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "locked" {
				t.Fatalf("helper not ready: %q %v", line, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			unlock, err := acquireNativeLocks(ctx, filepath.Join(root, "daemon.lock"), filepath.Join(root, "install.lock"))
			if err == nil {
				unlock()
				t.Fatal("acquired an incompatible concurrent native lock")
			}
			if ctx.Err() == nil {
				t.Fatalf("lock acquisition failed before waiting for held native lock: %v", err)
			}
		})
	}
}

func TestNativeLocksCoordinateDirectoryFallback(t *testing.T) {
	root := t.TempDir()
	lock := filepath.Join(root, "install.lock.d")
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if unlock, err := acquireNativeLocks(ctx, filepath.Join(root, "daemon.lock"), filepath.Join(root, "install.lock")); err == nil {
		unlock()
		t.Fatal("native directory lock was ignored")
	}
	if info, err := os.Stat(lock); err != nil || !info.IsDir() {
		t.Fatal("native lock was removed")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireNativeLocks(context.Background(), filepath.Join(root, "daemon.lock"), filepath.Join(root, "install.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(lock, "pid")); err != nil {
		t.Fatal("directory lock metadata was not published")
	}
	unlock()
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatal("owned lock was not cleaned")
	}
}

func TestNativeLocksRejectSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "daemon.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireNativeLocks(context.Background(), filepath.Join(root, "daemon.lock"), filepath.Join(root, "install.lock")); err == nil {
		t.Fatal("symlink lock accepted")
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "preserve" {
		t.Fatal("symlink target changed")
	}
}
