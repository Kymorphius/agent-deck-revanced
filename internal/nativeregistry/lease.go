// matrix/hardened: share exact native filesystem ownership without creating a second registry database.
package nativeregistry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// matrix/hardened: the two opt-in inventories must never coexist in one root.
const SharedStateDatabaseFilename = "state.db"

// Lease owns the directory, owner lock and exact database file. Callers close
// every SQLite connection before releasing it. It never deletes state files.
type Lease struct {
	mu                sync.Mutex
	dir, filename     string
	root, owner, data *os.File
	closeErr          error
}

// AcquireLease creates only an explicit private root and its named data file.
// The owner lock is shared across storage implementations in the same root.
func AcquireLease(dir, filename string) (_ *Lease, resultErr error) {
	if filename == "" || filename == "." || filename == ".." || filename == OwnerFilename || strings.ContainsAny(filename, `/\\:?#%`) || strings.HasSuffix(filename, ".") || strings.HasSuffix(filename, " ") {
		return nil, fmt.Errorf("%w: require a simple database filename", ErrUnsafePath)
	}
	for _, char := range filename {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '-' || char == '_') {
			return nil, fmt.Errorf("%w: require an ASCII database filename", ErrUnsafePath)
		}
	}
	if err := prepareDirectory(dir); err != nil {
		return nil, err
	}
	l := &Lease{dir: dir, filename: filename}
	defer func() {
		if resultErr != nil {
			_ = l.Close()
		}
	}()
	var err error
	l.owner, err = acquireOwner(filepath.Join(dir, OwnerFilename))
	if err != nil {
		return nil, err
	}
	// Check only pathname presence while holding the native owner lock. Never
	// open, harden or follow an alternate database, including an unsafe alias.
	if err := refuseAlternateDatabase(dir, filename); err != nil {
		return nil, err
	}
	l.root, err = os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("hold registry root: %w", err)
	}
	l.data, err = prepareData(filepath.Join(dir, filename))
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err := protectOptionalData(filepath.Join(dir, filename) + suffix); err != nil {
			return nil, err
		}
	}
	if err := l.Verify(); err != nil {
		return nil, err
	}
	return l, nil
}

func refuseAlternateDatabase(dir, filename string) error {
	known := filename
	if runtime.GOOS == "windows" {
		known = strings.ToLower(known)
	}
	var alternate string
	switch known {
	case DatabaseFilename:
		alternate = SharedStateDatabaseFilename
	case SharedStateDatabaseFilename:
		alternate = DatabaseFilename
	default:
		return nil
	}
	_, err := os.Lstat(filepath.Join(dir, alternate))
	if err == nil {
		return fmt.Errorf("%w: native state root already contains alternate database %s", ErrInvalidData, alternate)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("%w: cannot verify absence of alternate native database %s: %v", ErrUnsafePath, alternate, err)
}

func (l *Lease) verify() error {
	if l.owner == nil {
		return ErrClosed
	}
	for path, file := range map[string]*os.File{l.dir: l.root, filepath.Join(l.dir, OwnerFilename): l.owner, filepath.Join(l.dir, l.filename): l.data} {
		if file == nil {
			return fmt.Errorf("%w: incomplete file ownership", ErrUnsafePath)
		}
		if err := verifyHeldPath(path, file); err != nil {
			return err
		}
	}
	return nil
}

// Verify refuses replaced paths and incomplete or released ownership.
func (l *Lease) Verify() error {
	if l == nil {
		return ErrClosed
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.verify()
}

func (l *Lease) Directory() (string, error) {
	if l == nil {
		return "", ErrClosed
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.verify(); err != nil {
		return "", err
	}
	return l.dir, nil
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.data != nil {
		l.closeErr = errors.Join(l.closeErr, l.data.Close())
		l.data = nil
	}
	if l.root != nil {
		l.closeErr = errors.Join(l.closeErr, l.root.Close())
		l.root = nil
	}
	if l.owner != nil {
		l.closeErr = errors.Join(l.closeErr, releaseOwner(l.owner))
		l.owner = nil
	}
	return l.closeErr
}
