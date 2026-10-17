package monitor

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/asheshgoplani/agent-deck/internal/agentpaths"
	"github.com/asheshgoplani/agent-deck/internal/core/daemon"
)

// Paths is user-scoped, deliberately separate from every command daemon.
func Paths() (daemon.Paths, error) {
	dir, err := agentpaths.EffectiveDataPath(filepath.Join("runtime", "monitor"), "runtime")
	if err != nil {
		return daemon.Paths{}, err
	}
	return daemon.PathsIn(dir), nil
}

// ValidatePaths rejects symlinked ownership files and directories before core's
// proven flock/stale-owner takeover. matrix/hardened: never chmod a symlink.
func ValidatePaths(p daemon.Paths) error {
	if !filepath.IsAbs(p.Dir) || filepath.Clean(p.Dir) != p.Dir || p.Socket != filepath.Join(p.Dir, "daemon.sock") || p.Lock != filepath.Join(p.Dir, "daemon.lock") {
		return fmt.Errorf("monitor ownership paths must stay inside its absolute directory")
	}
	for path := p.Dir; path != filepath.Dir(path); path = filepath.Dir(path) {
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("monitor path contains symlink: %s", path)
		}
		if !st.IsDir() {
			return fmt.Errorf("monitor parent is not a directory: %s", path)
		}
		if path == p.Dir {
			if stat, ok := st.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
				return fmt.Errorf("monitor directory is not owned by caller")
			}
			if st.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("monitor directory is not private")
			}
		}
	}
	for _, path := range []string{p.Lock, p.Socket} {
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("monitor owner path is a symlink: %s", path)
		}
		if stat, ok := st.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
			return fmt.Errorf("monitor owner path has invalid ownership: %s", path)
		}
		if st.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("monitor owner path is not private: %s", path)
		}
		if path == p.Lock && !st.Mode().IsRegular() {
			return fmt.Errorf("monitor lock is not a regular file")
		}
		if path == p.Socket && st.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("monitor socket is not a socket")
		}
	}
	return nil
}

// Acquire preserves core's exclusive owner lock and dead-PID/refused-socket
// takeover, adding monitor-specific path validation first.
func Acquire(p daemon.Paths) (*daemon.Owner, error) {
	if err := ValidatePaths(p); err != nil {
		return nil, err
	}
	return daemon.Acquire(p)
}

func checkPeer(c net.Conn, uid int) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("monitor accepts Unix peers only")
	}
	peer, err := daemon.PeerUID(uc)
	if err != nil {
		return err
	}
	if peer != uid {
		return fmt.Errorf("monitor peer UID differs from owner")
	}
	return nil
}
