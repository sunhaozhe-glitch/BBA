#!/usr/bin/env python3
"""Fail-closed runner regressions, no daemon or fixture needed."""
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest

spec = importlib.util.spec_from_file_location('check', Path(__file__).with_name('b3-character-check.py'))
check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check)


class RunnerTests(unittest.TestCase):
    def test_exact_absence_only(self):
        for kind, text in [('container', 'Error response from daemon: No such container: fixture'),
                           ('volume', 'Error response from daemon: get fixture: no such volume')]:
            self.assertTrue(check.exact_absence(kind, 'fixture', 1, '[]\n', text+'\n'))
            for code, out, err in [(1, '', 'Cannot connect to the Docker daemon'),
                                   (1, '[]', 'permission denied'), (127, '', 'docker: not found'),
                                   (1, '[]', text.replace('fixture', 'other')), (0, '[]', text),
                                   (1, '[{}]', text), (1, '[]', text+'\nother failure')]:
                with self.subTest(kind=kind, err=err):
                    self.assertFalse(check.exact_absence(kind, 'fixture', code, out, err))

    def test_nonempty_fixture_set(self):
        for output in ['', '+ FIXTURE_NAME=unknown', '+ FIXTURE_NAME=bba-p004-011-123-4 extra']:
            with self.assertRaises(ValueError):
                check.fixture_names(output)
        self.assertEqual(check.fixture_names('+ FIXTURE_NAME=bba-p004-011-123-4\n'), ['bba-p004-011-123-4'])
        with self.assertRaises(ValueError):
            check.cleanup_readback([])

    def test_replacement_failure(self):
        source = (check.ROOT/'tools/kernel.sh').read_text()
        self.assertIn('BBA_CHARACTER_INTEGRATION=1', check.make_runner(source, Path('/node')))
        for broken in ['', source.replace('GO_COMMAND=(go test -race -count=1 ./internal/taskengine)', 'changed'), source+source]:
            with self.assertRaises(ValueError):
                check.make_runner(broken, Path('/node'))

    def test_test_must_run_pass_and_finish_node_assertions(self):
        output = '''=== RUN   TestB3CharacterRealGateway
{"default_package_entry":"/work/dist/index.js","exports_entry":"/work/dist/index.js"}
{"result":"PASS","real_go_gateway":true,"writer":6,"local_transport":0,"restart_replay":"no additional writer","lost_receipt":"unknown"}
--- PASS: TestB3CharacterRealGateway (1.0s)
'''
        self.assertTrue(check.validate_run(output, 0))
        for broken in ['', output.replace('=== RUN', 'absent'), output.replace('--- PASS:', '--- SKIP:'),
                       output.replace('"writer":6', '"writer":0'), output.replace('/dist/', '/lib/'),
                       output+'--- FAIL: TestB3CharacterRealGateway (1.0s)\n']:
            self.assertFalse(check.validate_run(broken, 0))
        self.assertFalse(check.validate_run(output, 1))

    def test_daemon_failure_cannot_pass_cleanup(self):
        def unavailable(*args, **kwargs):
            return SimpleNamespace(returncode=1, stdout='', stderr='Cannot connect to the Docker daemon')
        self.assertFalse(all(x['absent'] for x in check.cleanup_readback(['fixture'], unavailable)))


if __name__ == '__main__':
    unittest.main(verbosity=2)
