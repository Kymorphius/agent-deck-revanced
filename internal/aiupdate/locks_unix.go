//go:build darwin || linux

package aiupdate

// matrix/hardened: use the same lock primitives and order as Codex, never process signals.
import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func acquireNativeLocks(ctx context.Context, daemonPath, installPath string) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	daemon, err := acquireFileLock(ctx, daemonPath, false)
	if err != nil {
		return nil, err
	}
	// A running updater retains its original PATH. Hold both possible native file-lock
	// kinds and the directory fallback, rather than guessing its inherited tool availability.
	installer, err := acquireFileLock(ctx, installPath, runtime.GOOS == "darwin")
	if err != nil {
		daemon.Close()
		return nil, err
	}
	var installerLockf *os.File
	if runtime.GOOS == "linux" {
		// Linux flock and POSIX record locks are independent. Darwin locks share the
		// same conflict domain, so its native F_SETLK also excludes a flock installer.
		installerLockf, err = acquireFileLock(ctx, installPath, true)
		if err != nil {
			installer.Close()
			daemon.Close()
			return nil, err
		}
	}
	directoryUnlock, err := acquireInstallerDirectoryLock(ctx, filepath.Dir(installPath))
	if err != nil {
		if installerLockf != nil {
			installerLockf.Close()
		}
		installer.Close()
		daemon.Close()
		return nil, err
	}
	return func() {
		directoryUnlock()
		if installerLockf != nil {
			installerLockf.Close()
		}
		installer.Close()
		daemon.Close()
	}, nil
}

func openReadNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func acquireInstallerDirectoryLock(ctx context.Context, root string) (func(), error) {
	owner, err := os.MkdirTemp(root, ".agent-deck-install-lock-*")
	if err != nil {
		return nil, err
	}
	cleanupOwner := func() { os.RemoveAll(owner) }
	if err := os.WriteFile(filepath.Join(owner, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		cleanupOwner()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(owner, "started_at"), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600); err != nil {
		cleanupOwner()
		return nil, err
	}
	lock := filepath.Join(root, "install.lock.d")
	for {
		if err := os.Symlink(owner, lock); err == nil {
			return func() {
				// Never remove a directory lock replaced by another owner.
				if selected, err := os.Readlink(lock); err == nil && selected == owner {
					os.Remove(lock)
				}
				cleanupOwner()
			}, nil
		} else if !os.IsExist(err) {
			cleanupOwner()
			return nil, err
		}
		// Do not steal or clean a native lock, including one whose owner is unknown.
		select {
		case <-ctx.Done():
			cleanupOwner()
			return nil, fmt.Errorf("waiting for native installer directory lock %s: %w", lock, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func acquireFileLock(ctx context.Context, path string, lockf bool) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open native lock %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("native lock is not a regular file")
	}
	for {
		if lockf {
			flock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
			err = unix.FcntlFlock(file.Fd(), unix.F_SETLK, &flock)
		} else {
			err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		}
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EINTR) {
			file.Close()
			return nil, fmt.Errorf("acquire native lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, fmt.Errorf("waiting for native lock %s: %w", path, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
