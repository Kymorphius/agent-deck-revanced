#!/usr/bin/env python3
"""matrix/hardened: prove tmux isolation using only this test's private sockets.

The external sentinel also lives in the sandbox. Before invoking bare tmux,
the child checks that the wrapper supplied a new private socket directory;
a broken wrapper therefore fails without ever contacting the host default.
"""
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parents[2]
RUNNER = ROOT / 'scripts' / 'run-isolated.py'
TMUX = shutil.which('tmux')

CHILD = r'''
import json, os, pathlib, stat, subprocess, sys, time

root = pathlib.Path(os.environ.get('TMUX_TMPDIR', '')).resolve()
# Fail closed before bare tmux can touch any socket. /tmp is canonicalized on
# macOS, and the wrapper's owned directory must be a short, private child.
assert root.parent == pathlib.Path('/tmp').resolve(), root
assert root.name.startswith('ad-it-'), root
assert root != pathlib.Path(sys.argv[2]).resolve(), root
assert stat.S_IMODE(root.stat().st_mode) == 0o700, root
assert root.stat().st_uid == os.getuid(), root
assert 'TMUX' not in os.environ
identity = [root.stat().st_dev, root.stat().st_ino]
# Socket symlinks and directory symlinks do not transfer cleanup ownership.
(root / 'foreign.sock').symlink_to(pathlib.Path(sys.argv[2]) / 'sentinel.sock')
(root / 'foreign-dir').symlink_to(pathlib.Path(sys.argv[2]), target_is_directory=True)
(root / 'ordinary-file').write_text('not a socket')

def tmux(*args):
    return subprocess.check_output(['tmux', *args], text=True).strip()

servers = []
mode = sys.argv[3]
selectors = (([], 'bare'), (['-L', 'named'], 'named'),
             (['-S', str(root / 'extra.sock')], 'explicit'))
if mode.startswith('replace-') or mode == 'hardlink':
    selectors = ()
elif mode == 'single':
    selectors = selectors[:1]
for selector, name in selectors:
    tmux(*selector, '-f', '/dev/null', 'new-session', '-d', '-s', name, 'sleep 30')
    socket = tmux(*selector, 'display-message', '-p', '#{socket_path}')
    pid = int(tmux(*selector, 'display-message', '-p', '#{pid}'))
    assert root in pathlib.Path(socket).parents, socket
    assert len(os.fsencode(socket)) < 104, socket
    servers.append({'socket': socket, 'pid': pid})
report = pathlib.Path(sys.argv[1])
pending = report.with_suffix('.pending')
actual = {'root': str(root), 'identity': identity, 'home': os.environ['HOME'],
          'tmp': os.environ['TMPDIR'], 'servers': servers}
if mode.startswith('replace-'):
    moved = root.with_name(root.name + '-moved')
    assert not moved.exists(), moved
    root.rename(moved)
    root.symlink_to(pathlib.Path(sys.argv[2]), target_is_directory=True)
    actual['moved'] = str(moved)
elif mode == 'hardlink':
    try:
        os.link(pathlib.Path(sys.argv[2]) / 'sentinel.sock', root / 'hardlinked.sock')
        actual['hardlink_supported'] = True
    except OSError:
        actual['hardlink_supported'] = False
pending.write_text(json.dumps(actual))
pending.replace(report)
if mode == 'hold':
    while True:
        time.sleep(1)
sys.exit(int(mode.split('-')[1]) if mode.startswith('replace-') else
         0 if mode in ('single', 'hardlink') else int(mode))
'''


@unittest.skipUnless(TMUX, 'tmux is required for the private socket integration test')
class IsolatedTmuxTest(unittest.TestCase):
    def setUp(self):
        # matrix/hardened: short paths avoid Darwin's 104-byte socket limit.
        self.sandbox = tempfile.TemporaryDirectory(prefix='ad-iso-t-', dir='/tmp')
        self.base = Path(self.sandbox.name).resolve()
        self.sentinel_root = self.base / 'outside'
        self.sentinel_socket = self.sentinel_root / 'sentinel.sock'
        self.sentinel_root.mkdir(mode=0o700)
        self.env = dict(os.environ)
        self.env.pop('TMUX', None)
        self.env['TMUX_TMPDIR'] = str(self.sentinel_root)
        self.owned_sockets = [str(self.sentinel_socket)]
        self.owned_directories = []
        self.owned_links = []
        try:
            self.tmux(self.sentinel_socket, '-f', '/dev/null', 'new-session', '-d',
                      '-s', 'sentinel', 'sleep 120')
            self.sentinel_before = self.sentinel_state()
        except BaseException:
            self.tearDown()
            raise

    def tearDown(self):
        # Each target is a socket we created, always addressed explicitly.
        for socket in self.owned_sockets:
            subprocess.run([TMUX, '-S', socket, 'kill-server'], env=self.env,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                           timeout=5, check=False)
        for path, target in self.owned_links:
            if path.is_symlink() and os.readlink(path) == target:
                path.unlink()
        for path, identity in self.owned_directories:
            try:
                info = path.lstat()
            except FileNotFoundError:
                continue
            if path.is_dir() and not path.is_symlink() and \
                    (info.st_dev, info.st_ino) == identity:
                shutil.rmtree(path)
        self.sandbox.cleanup()

    def tmux(self, socket, *args):
        return subprocess.check_output([TMUX, '-S', str(socket), *args],
                                       env=self.env, text=True, stderr=subprocess.PIPE).strip()

    def sentinel_state(self):
        return (self.tmux(self.sentinel_socket, 'display-message', '-p', '#{pid}'),
                self.tmux(self.sentinel_socket, 'list-sessions', '-F',
                          '#{session_name}:#{session_id}:#{session_created}'))

    def wrapper_env(self, name='bin', mock_kill=None):
        bin_dir = self.base / name
        bin_dir.mkdir()
        caches = {key: str(self.base / key) for key in ('GOPATH', 'GOMODCACHE', 'GOCACHE')}
        caches['GOTOOLCHAIN'] = 'local'
        fake_go = bin_dir / 'go'
        fake_go.write_text('#!' + sys.executable + '\nimport json\nprint(' +
                           repr(json.dumps(caches)) + ')\n')
        fake_go.chmod(0o755)
        if mock_kill:
            fake_tmux = bin_dir / 'tmux'
            fake_tmux.write_text('#!' + sys.executable + '\nimport os, sys, time\n'
                                 'if sys.argv[-1:] == ["kill-server"]:\n' +
                                 ('    time.sleep(30)\n' if mock_kill == 'timeout' else
                                  '    sys.exit(0)\n' if mock_kill == 'noop' else
                                  '    sys.exit(23)\n') +
                                 'os.execv(' + repr(TMUX) + ', [' + repr(TMUX) +
                                 '] + sys.argv[1:])\n')
            fake_tmux.chmod(0o755)
        # The HOME temp root uses this long parent. tmux must get a separate,
        # short root instead of inheriting TMPDIR's socket-breaking path.
        long_tmp = self.base / ('long-test-tmp-' * 8)
        long_tmp.mkdir(exist_ok=True)
        return dict(self.env, PATH=str(bin_dir) + os.pathsep + os.environ['PATH'],
                    TMPDIR=str(long_tmp), TMUX=str(self.sentinel_socket) + ',1,0')

    def register_report(self, report):
        actual = json.loads(report.read_text())
        self.owned_sockets.extend(server['socket'] for server in actual['servers'])
        directory = Path(actual.get('moved', actual['root']))
        self.owned_directories.append((directory, tuple(actual['identity'])))
        if 'moved' in actual:
            self.owned_links.append((Path(actual['root']), str(self.sentinel_root)))
        return actual

    def test_private_servers_cleanup_and_external_sentinel_survives(self):
        env = self.wrapper_env()
        roots = []
        cases = ((0, None), (7, None), ('hold', signal.SIGINT),
                 ('hold', signal.SIGTERM), ('hold', signal.SIGHUP))
        for index, (mode, signum) in enumerate(cases):
            with self.subTest(exit=mode, signal=signum):
                report = self.base / ('report-' + str(index) + '.json')
                process = subprocess.Popen([sys.executable, str(RUNNER), sys.executable,
                                            '-c', CHILD, str(report), str(self.sentinel_root),
                                            str(mode)], env=env, text=True,
                                           stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                try:
                    if signum is not None:
                        deadline = time.monotonic() + 10
                        while not report.exists() and time.monotonic() < deadline:
                            if process.poll() is not None:
                                break
                            time.sleep(0.02)
                        self.assertTrue(report.exists(), 'child did not create its private servers')
                        process.send_signal(signum)
                    stdout, stderr = process.communicate(timeout=15)
                    expected = 128 + signum if signum is not None else mode
                    self.assertEqual(process.returncode, expected, stdout + stderr)
                    actual = json.loads(report.read_text())
                    roots.append(actual['root'])
                    self.assertGreater(len(os.fsencode(actual['tmp'])), 104)
                    self.assertFalse(Path(actual['root']).exists())
                    self.assertFalse(Path(actual['home']).exists())
                    for server in actual['servers']:
                        self.assertFalse(Path(server['socket']).exists())
                        deadline = time.monotonic() + 5
                        while self.process_exists(server['pid']) and time.monotonic() < deadline:
                            time.sleep(0.02)
                        self.assertFalse(self.process_exists(server['pid']),
                                         'private tmux server survived socket-directory removal')
                    self.assertEqual(self.sentinel_state(), self.sentinel_before)
                finally:
                    if process.poll() is None:
                        process.terminate()
                        process.communicate(timeout=10)
                    if report.exists():
                        self.register_report(report)
        self.assertEqual(len(roots), len(set(roots)), 'runs reused a tmux directory')

    def test_replaced_root_preserves_sentinel_and_original_exit(self):
        env = self.wrapper_env()
        for original, expected in ((0, 1), (7, 7)):
            with self.subTest(original=original):
                report = self.base / ('replace-' + str(original) + '.json')
                result = subprocess.run([sys.executable, str(RUNNER), sys.executable, '-c', CHILD,
                                         str(report), str(self.sentinel_root), 'replace-' + str(original)],
                                        env=env, text=True, capture_output=True, timeout=15)
                actual = self.register_report(report)
                self.assertEqual(result.returncode, expected, result.stderr)
                self.assertIn(actual['root'], result.stderr)
                self.assertIn('root path was removed or replaced', result.stderr)
                self.assertTrue(Path(actual['root']).is_symlink())
                self.assertTrue(Path(actual['moved']).is_dir())
                self.assertFalse(Path(actual['home']).exists())
                self.assertEqual(self.sentinel_state(), self.sentinel_before)

    def test_failed_shutdown_preserves_live_private_socket(self):
        for mode in ('refuse', 'timeout', 'noop'):
            with self.subTest(mode=mode):
                env = self.wrapper_env('bin-' + mode, mock_kill=mode)
                report = self.base / (mode + '.json')
                result = subprocess.run([sys.executable, str(RUNNER), sys.executable, '-c', CHILD,
                                         str(report), str(self.sentinel_root), 'single'],
                                        env=env, text=True, capture_output=True, timeout=15)
                actual = self.register_report(report)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('Retaining isolated tmux directory ' + actual['root'], result.stderr)
                self.assertTrue(Path(actual['root']).is_dir())
                self.assertFalse(Path(actual['home']).exists())
                for server in actual['servers']:
                    self.assertTrue(Path(server['socket']).exists())
                    self.assertIn(server['socket'], result.stderr)
                    self.assertEqual(self.tmux(server['socket'], 'display-message', '-p', '#{pid}'),
                                     str(server['pid']))
                self.assertEqual(self.sentinel_state(), self.sentinel_before)

    def test_hardlinked_foreign_socket_is_not_a_cleanup_target(self):
        env = self.wrapper_env()
        report = self.base / 'hardlink.json'
        result = subprocess.run([sys.executable, str(RUNNER), sys.executable, '-c', CHILD,
                                 str(report), str(self.sentinel_root), 'hardlink'],
                                env=env, text=True, capture_output=True, timeout=15)
        actual = self.register_report(report)
        if not actual['hardlink_supported']:
            self.skipTest('filesystem does not support hard-linking Unix sockets')
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('socket has multiple hard links', result.stderr)
        self.assertTrue(Path(actual['root'], 'hardlinked.sock').exists())
        self.assertEqual(self.sentinel_state(), self.sentinel_before)

    @staticmethod
    def process_exists(pid):
        # Read-only liveness check; cleanup never signals a server by PID.
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        return True


if __name__ == '__main__':
    unittest.main()
