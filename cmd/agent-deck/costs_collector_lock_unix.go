//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const costCollectorDirectory = "costs-runtime"
const costCollectorLockName = "codex-collector.lock"

// matrix/hardened: descriptor ownership ends with the process, not a PID file
// lease. The directory descriptor anchors the no-follow open against renames.
type costCollectorLock struct {
	file      *os.File
	directory *os.File
	path      string
	once      sync.Once
	err       error
}

func privateCostCollectorDirectory(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("cost collector directory must be private and owned by caller")
	}
	return nil
}

func validCostCollectorFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("cost collector lock must be a private, caller-owned regular file with one link")
	}
	return nil
}

// matrix/hardened: profileDir is derived from the resolved storage's actual
// path. Refuse a linked/non-private profile root; allow canonical OS ancestors
// such as macOS /var -> /private/var without following ownership-file links.
func tryCostCollectorLock(profileDir string) (*costCollectorLock, bool, error) {
	if !filepath.IsAbs(profileDir) || filepath.Clean(profileDir) != profileDir {
		return nil, false, fmt.Errorf("cost collector requires an absolute profile directory")
	}
	profileInfo, err := os.Lstat(profileDir)
	if err != nil {
		return nil, false, err
	}
	if err := privateCostCollectorDirectory(profileInfo); err != nil {
		return nil, false, err
	}
	canonical, err := filepath.EvalSymlinks(profileDir)
	if err != nil {
		return nil, false, err
	}
	fd, err := unix.Open(canonical, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, err
	}
	profile := os.NewFile(uintptr(fd), canonical)
	defer profile.Close()
	openedProfile, err := profile.Stat()
	if err != nil || !os.SameFile(profileInfo, openedProfile) {
		return nil, false, fmt.Errorf("cost collector profile directory changed while opening")
	}
	if err := unix.Mkdirat(fd, costCollectorDirectory, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, false, err
	}
	dirFD, err := unix.Openat(fd, costCollectorDirectory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, err
	}
	dir := os.NewFile(uintptr(dirFD), filepath.Join(canonical, costCollectorDirectory))
	fail := func(err error) (*costCollectorLock, bool, error) { _ = dir.Close(); return nil, false, err }
	dirInfo, err := dir.Stat()
	if err != nil {
		return fail(err)
	}
	if err := privateCostCollectorDirectory(dirInfo); err != nil {
		return fail(err)
	}
	lockFD, err := unix.Openat(dirFD, costCollectorLockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return fail(err)
	}
	lock := &costCollectorLock{file: os.NewFile(uintptr(lockFD), filepath.Join(dir.Name(), costCollectorLockName)), directory: dir, path: filepath.Join(dir.Name(), costCollectorLockName)}
	if err := lock.valid(); err != nil {
		_ = lock.Close()
		return nil, false, err
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if err := lock.valid(); err != nil {
		_ = lock.Close()
		return nil, false, err
	}
	// matrix/hardened: PID is diagnostic only; the kernel flock remains the
	// authority. Waiting consumers never truncate or rewrite this readback.
	if err := lock.file.Truncate(0); err != nil {
		_ = lock.Close()
		return nil, false, err
	}
	if _, err := lock.file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = lock.Close()
		return nil, false, err
	}
	if err := lock.file.Sync(); err != nil {
		_ = lock.Close()
		return nil, false, err
	}
	return lock, true, nil
}

// matrix/hardened: a directory migration or lock-file replacement retires the
// old ownership before its next read; never keep sampling an unlinked inode.
func (l *costCollectorLock) valid() error {
	dirInfo, err := l.directory.Stat()
	if err != nil {
		return err
	}
	currentDir, err := os.Lstat(filepath.Dir(l.path))
	if err != nil || !os.SameFile(dirInfo, currentDir) {
		return fmt.Errorf("cost collector directory changed")
	}
	if err := privateCostCollectorDirectory(currentDir); err != nil {
		return err
	}
	opened, err := l.file.Stat()
	if err != nil {
		return err
	}
	if err := validCostCollectorFile(opened); err != nil {
		return err
	}
	current, err := os.Lstat(l.path)
	if err != nil || !os.SameFile(opened, current) || current.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cost collector lock path changed")
	}
	return validCostCollectorFile(current)
}

func (l *costCollectorLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.err = l.file.Close()
		if err := l.directory.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}
