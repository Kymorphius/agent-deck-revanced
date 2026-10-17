#!/usr/bin/env python3
"""matrix/hardened: isolated test HOME/tmux, shared Go caches, owned cleanup.

Usage: scripts/run-isolated.py go test ./internal/example/...
Requires Python 3 and Go. This does not isolate launchd or the OS.
"""
import errno
import json
import os
from pathlib import Path
import signal
import socket as unix_socket
import stat
import subprocess
import sys
import tempfile
import time


def directory_identity(path):
    try:
        info = os.lstat(path)
    except OSError:
        return None
    return (info.st_dev, info.st_ino) if stat.S_ISDIR(info.st_mode) else None


def socket_stopped(path):
    # A successful tmux kill-server can leave its socket file behind. Only
    # ENOENT/ECONNREFUSED prove it cannot accept connections; other errors retain it.
    with unix_socket.socket(unix_socket.AF_UNIX) as connection:
        connection.settimeout(0.2)
        try:
            connection.connect(path)
        except OSError as error:
            return error.errno in (errno.ENOENT, errno.ECONNREFUSED)
    return False


def cleanup_tmux_sockets(root, identity, env):
    # matrix/hardened: only contact sockets in this run's private directory,
    # while it still exists. Retain sockets if shutdown cannot be confirmed.
    # Directory fds anchor traversal/deletion even if a test replaces a path.
    def retain(reason):
        print('Retaining isolated tmux directory ' + root + ' (dev=' + str(identity[0]) +
              ', ino=' + str(identity[1]) + '): ' + reason, file=sys.stderr)
        return False

    def unchanged(relative, directory_fd):
        if directory_identity(root) != identity:
            return False
        path = Path(root)
        for part in Path(relative).parts:
            path /= part
            if directory_identity(path) is None:
                return False
        info = os.fstat(directory_fd)
        return directory_identity(path) == (info.st_dev, info.st_ino)

    root_fd = None
    try:
        if directory_identity(root) != identity:
            return retain('root path was removed or replaced; cleanup ownership is unproven')
        root_fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        info = os.fstat(root_fd)
        if (info.st_dev, info.st_ino) != identity:
            return retain('root identity changed while opening it')
        failed = False
        stopped_sockets = {}
        for relative, _dirs, files, directory_fd in os.fwalk('.', dir_fd=root_fd,
                                                            follow_symlinks=False):
            if not unchanged(relative, directory_fd):
                return retain('directory path was replaced: ' + str(Path(root, relative)))
            for name in files:
                info = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
                if not stat.S_ISSOCK(info.st_mode):
                    continue
                socket = str(Path(root, relative, name))
                if info.st_nlink != 1:
                    return retain('socket has multiple hard links; ownership is unproven: ' + socket)
                actual = os.lstat(socket)
                socket_identity = (info.st_dev, info.st_ino)
                if not unchanged(relative, directory_fd) or \
                        (actual.st_dev, actual.st_ino, actual.st_mode) != \
                        (info.st_dev, info.st_ino, info.st_mode):
                    return retain('socket path was replaced: ' + socket)
                try:
                    result = subprocess.run(['tmux', '-S', socket, 'kill-server'], env=env,
                                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                            timeout=5, check=False)
                    if result.returncode:
                        retain('kill-server exited ' + str(result.returncode) + ': ' + socket)
                        failed = True
                        continue
                    deadline = time.monotonic() + 2
                    while True:
                        if not unchanged(relative, directory_fd):
                            return retain('directory path changed during shutdown: ' + socket)
                        if socket_stopped(socket):
                            stopped_sockets[socket] = socket_identity
                            break
                        if time.monotonic() >= deadline:
                            retain('socket shutdown remains unconfirmed after kill-server: ' + socket)
                            failed = True
                            break
                        time.sleep(0.02)
                except (OSError, subprocess.TimeoutExpired) as error:
                    retain('could not shut down ' + socket + ': ' + str(error))
                    failed = True
        if failed:
            return False

        # Only delete through the verified root fd. A symlink substituted for
        # the root must never reach TemporaryDirectory/shutil recursive cleanup.
        for relative, dirs, files, directory_fd in os.fwalk('.', topdown=False,
                                                            dir_fd=root_fd, follow_symlinks=False):
            if not unchanged(relative, directory_fd):
                return retain('directory path changed before removal: ' + str(Path(root, relative)))
            for name in files:
                info = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
                if stat.S_ISSOCK(info.st_mode):
                    socket = str(Path(root, relative, name))
                    if stopped_sockets.get(socket) != (info.st_dev, info.st_ino) or \
                            not socket_stopped(socket):
                        return retain('socket shutdown is unproven before removal: ' + socket)
                os.unlink(name, dir_fd=directory_fd)
            for name in dirs:
                info = os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
                if stat.S_ISLNK(info.st_mode):
                    os.unlink(name, dir_fd=directory_fd)
                else:
                    os.rmdir(name, dir_fd=directory_fd)
        if directory_identity(root) != identity:
            return retain('root path changed before removal')
        os.rmdir(root)
        return True
    except OSError as error:
        return retain(str(error))
    finally:
        if root_fd is not None:
            os.close(root_fd)


def main():
    command = sys.argv[1:] or ['go', 'test', './...']
    # Resolve BEFORE changing HOME; retain modules and downloaded Go toolchains too.
    cache = json.loads(subprocess.check_output(
        ['go', 'env', '-json', 'GOPATH', 'GOMODCACHE', 'GOCACHE', 'GOTOOLCHAIN'], text=True))
    for key in ('GOPATH', 'GOMODCACHE', 'GOCACHE'):
        if not cache.get(key) or cache[key] == 'off':
            raise RuntimeError('A persistent Go cache is required: ' + key)
    child = None
    interrupted = None

    def stop(signum, _frame):
        nonlocal interrupted
        interrupted = signum
        if child is not None:
            try:
                os.killpg(child.pid, signum)
            except ProcessLookupError:
                pass

    previous = {s: signal.signal(s, stop) for s in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
    try:
        # matrix/hardened: HOME/TMPDIR may be too long for macOS Unix sockets.
        # Do not inherit a production TMUX_TMPDIR or use the host default socket.
        with tempfile.TemporaryDirectory(prefix='agent-deck-test-home-') as root:
            tmux_root = str(Path(tempfile.mkdtemp(prefix='ad-it-', dir='/tmp')).resolve())
            info = os.lstat(tmux_root)
            identity = (info.st_dev, info.st_ino)
            env = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL', 'TERM') if key in os.environ}
            env.update(cache)
            env.update(HOME=root, TZ='UTC', GOTMPDIR=root + '/tmp', TMPDIR=root + '/tmp',
                       TMUX_TMPDIR=tmux_root,
                       XDG_CONFIG_HOME=root + '/config', XDG_DATA_HOME=root + '/data',
                       XDG_CACHE_HOME=root + '/cache', XDG_STATE_HOME=root + '/state',
                       XDG_RUNTIME_DIR=root + '/run')
            try:
                for key in ('TMPDIR', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME',
                            'XDG_STATE_HOME', 'XDG_RUNTIME_DIR'):
                    Path(env[key]).mkdir(mode=0o700)
                if interrupted:
                    return 128 + interrupted
                child = subprocess.Popen(command, env=env, start_new_session=True)
                if interrupted:
                    stop(interrupted, None)
                while True:
                    try:
                        code = child.wait(timeout=0.2)
                        break
                    except subprocess.TimeoutExpired:
                        if interrupted:
                            try:
                                code = child.wait(timeout=3)
                            except subprocess.TimeoutExpired:
                                os.killpg(child.pid, signal.SIGKILL)
                                code = child.wait()
                            break
            finally:
                child = None
                cleaned = cleanup_tmux_sockets(tmux_root, identity, env)
            code = 128 + interrupted if interrupted else (code if code >= 0 else 128 - code)
            return 1 if code == 0 and not cleaned else code
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)


if __name__ == '__main__':
    sys.exit(main())
