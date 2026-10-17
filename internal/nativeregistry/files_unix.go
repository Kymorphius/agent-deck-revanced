//go:build !windows

// matrix/hardened: private Unix test roots use no-follow files and real OS flock ownership.
package nativeregistry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func prepareDirectory(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == string(filepath.Separator) {
		return fmt.Errorf("%w: require an explicit absolute dedicated directory", ErrUnsafePath)
	}
	// Reject existing path aliases rather than hardening a directory reached
	// through a symlink. Only the final component may be created.
	if err := checkAncestors(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create native registry root: %w", err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: open native registry root: %v", ErrUnsafePath, err)
	}
	defer unix.Close(fd)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%w: registry root is not owned by the caller", ErrUnsafePath)
	}
	if err := unix.Fchmod(fd, 0700); err != nil {
		return err
	}
	return nil
}

func checkAncestors(dir string) error {
	for current := dir; current != string(filepath.Separator); current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && current == dir {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: trusted ancestor unavailable: %v", ErrUnsafePath, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: registry path contains a non-directory or symlink", ErrUnsafePath)
		}
	}
	return nil
}

func privateFile(path string, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(path, flags, 0600)
	if err != nil {
		return nil, fmt.Errorf("%w: open registry file: %v", ErrUnsafePath, err)
	}
	file := os.NewFile(uintptr(fd), path)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Nlink != 1 || info.Uid != uint32(os.Geteuid()) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: registry file must be a single-link caller-owned regular file", ErrUnsafePath)
	}
	if err := unix.Fchmod(fd, 0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func acquireOwner(path string) (*os.File, error) {
	file, err := privateFile(path, true)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrOwned
		}
		return nil, fmt.Errorf("lock native registry: %w", err)
	}
	return file, nil
}

func releaseOwner(file *os.File) error {
	return errors.Join(unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close())
}

func verifyHeldPath(path string, file *os.File) error {
	actual, err := os.Lstat(path)
	if err != nil || actual.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: registry path changed", ErrUnsafePath)
	}
	held, err := file.Stat()
	if err != nil || !os.SameFile(actual, held) {
		return fmt.Errorf("%w: registry identity changed", ErrUnsafePath)
	}
	var info unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &info); err != nil || info.Uid != uint32(os.Geteuid()) || info.Mode&unix.S_IFMT == unix.S_IFREG && info.Nlink != 1 {
		return fmt.Errorf("%w: held registry ownership or link count changed", ErrUnsafePath)
	}
	return nil
}

func prepareData(path string) (*os.File, error) { return privateFile(path, true) }

func protectOptionalData(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	file, err := privateFile(path, false)
	if err != nil {
		return err
	}
	return file.Close()
}
