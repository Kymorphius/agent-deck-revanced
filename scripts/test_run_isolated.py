#!/usr/bin/env python3
"""matrix/hardened: test lifecycle without downloading or compiling dependencies."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

RUNNER = Path(__file__).with_name('run-isolated.py')


class IsolatedRunnerTest(unittest.TestCase):
    def test_lifecycle_and_shared_caches(self):
        with tempfile.TemporaryDirectory() as fixture:
            base = Path(fixture)
            bin_dir = base / 'bin'
            bin_dir.mkdir()
            caches = {k: str(base / k) for k in ('GOPATH', 'GOMODCACHE', 'GOCACHE')}
            caches['GOTOOLCHAIN'] = 'local'
            for k in ('GOPATH', 'GOMODCACHE', 'GOCACHE'):
                Path(caches[k]).mkdir()
                (Path(caches[k]) / 'keep').write_text('shared')
            fake_go = bin_dir / 'go'
            fake_go.write_text('#!' + sys.executable + '\nimport json\nprint(' + repr(json.dumps(caches)) + ')\n')
            fake_go.chmod(0o755)
            env = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ['PATH'],
                       CLAUDE_CONFIG_DIR='/should/not/inherit', CODEX_HOME='/should/not/inherit')
            homes = []
            for mode in ('success', 'failure', 'term', 'int', 'hup'):
                with self.subTest(mode=mode):
                    report = base / (mode + '.json')
                    code = ('import os,json,time,sys; '
                            'open(sys.argv[1],"w").write(json.dumps(dict(os.environ))); '
                            + ('time.sleep(30)' if mode in ('term', 'int', 'hup') else
                               'sys.exit(7)' if mode == 'failure' else 'sys.exit(0)'))
                    p = subprocess.Popen([sys.executable, str(RUNNER), sys.executable, '-c', code, str(report)], env=env)
                    try:
                        deadline = time.monotonic() + 10
                        while not report.exists() and time.monotonic() < deadline:
                            time.sleep(0.02)
                        self.assertTrue(report.exists())
                        if mode in ('term', 'int', 'hup'):
                            sig = {'term': signal.SIGTERM, 'int': signal.SIGINT, 'hup': signal.SIGHUP}[mode]
                            p.send_signal(sig)
                            expected = 128 + sig
                        else:
                            expected = 7 if mode == 'failure' else 0
                        self.assertEqual(p.wait(timeout=8), expected)
                        actual = json.loads(report.read_text())
                        homes.append(actual['HOME'])
                        self.assertFalse(Path(actual['HOME']).exists())
                        for key, value in caches.items():
                            self.assertEqual(actual[key], value)
                        for key in ('XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME', 'TMPDIR'):
                            self.assertTrue(actual[key].startswith(actual['HOME'] + '/'))
                        self.assertNotIn('CLAUDE_CONFIG_DIR', actual)
                        self.assertNotIn('CODEX_HOME', actual)
                    finally:
                        if p.poll() is None:
                            p.terminate()
                            p.wait(timeout=8)
            self.assertEqual(len(homes), len(set(homes)))
            for key in ('GOPATH', 'GOMODCACHE', 'GOCACHE'):
                self.assertEqual((Path(caches[key]) / 'keep').read_text(), 'shared')


if __name__ == '__main__':
    unittest.main()
