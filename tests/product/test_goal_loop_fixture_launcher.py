"""Targeted launcher checks; no Docker or deployed services required.

python3 -m unittest tests.product.test_goal_loop_fixture_launcher -v
"""
import contextlib
import gc
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
import unittest
import warnings
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / 'tools/goal-loop-fixture.py'


class GoalFixtureLauncherTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not SCRIPT.is_file():
            raise AssertionError('goal-loop-fixture launcher has not been implemented')
        spec = importlib.util.spec_from_file_location('goal_fixture', SCRIPT)
        cls.mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.mod)

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / 'evals/results').mkdir(parents=True)
        self.root_patch = patch.object(self.mod, 'ROOT', self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)
        self.out = self.root / 'evals/results/demo'

    def test_output_is_new_direct_child_and_dsn_is_never_inherited(self):
        for var in ('BBA_TASKENGINE_DSN', 'DATABASE_URL', 'BBA_FIXTURE_DSN', 'PGHOST', 'PGSERVICE'):
            with self.subTest(var=var), self.assertRaises(ValueError):
                self.mod.prepare_output(self.out, {var: 'private-production-value'})
            self.assertFalse(self.out.exists())
        for out in (self.root / 'elsewhere', self.out / 'nested'):
            with self.assertRaises(ValueError):
                self.mod.prepare_output(out, {})
        self.mod.prepare_output(self.out, {})
        self.assertEqual(self.out.stat().st_mode & 0o777, 0o700)
        with self.assertRaises(ValueError):
            self.mod.prepare_output(self.out, {})

    def test_private_file_modes_and_symlinks(self):
        self.mod.prepare_output(self.out, {})
        private = self.out / 'secret'
        self.mod.write_private(private, 'secret-test-value')
        self.assertEqual(private.stat().st_mode & 0o777, 0o600)
        target = self.root / 'untouched'
        target.write_text('original')
        link = self.out / 'symlink'
        link.symlink_to(target)
        with self.assertRaises((ValueError, OSError)):
            self.mod.write_private(link, 'replacement')
        self.assertEqual(target.read_text(), 'original')

    def test_runtime_env_and_flags_are_explicit_and_fixture_only(self):
        dsn = 'postgres://bba_dev:synthetic@127.0.0.1:31234/bba_dev?sslmode=disable'
        env = self.mod.runtime_env({'PATH': '/usr/bin', 'BBA_REAL_MODEL_KEY': 'private',
                                    'PGPASSWORD': 'private', 'HTTP_PROXY': 'http://proxy.invalid'}, dsn)
        self.assertEqual(env['BBA_FIXTURE_ONLY'], '1')
        self.assertEqual(env['BBA_TASKENGINE_DSN'], dsn)
        self.assertNotIn('BBA_REAL_MODEL_KEY', env)
        self.assertNotIn('PGPASSWORD', env)
        self.assertNotIn('HTTP_PROXY', env)
        args = self.mod.server_args(31235, self.out)
        self.assertIn('-development-operations', args)
        self.assertIn('-offline-goal-fixture', args)
        self.assertNotIn('-worker', args)
        self.assertEqual(args[args.index('-model-registry-file') + 1], str(self.out / 'registry.json'))
        self.assertIn('127.0.0.1:31235', args)
        with self.assertRaises(ValueError):
            self.mod.server_args(5432, self.out)

    def test_saved_dsn_cannot_point_restart_at_a_different_database(self):
        self.mod.prepare_output(self.out, {})
        state = self.mod.initial_state(self.out, '123456abcdef', 31234, 31235)
        good = 'postgres://bba_dev:synthetic@127.0.0.1:31235/bba_dev?sslmode=disable'
        self.mod.write_private(self.out / 'dsn.txt', good)
        self.assertEqual(self.mod.fixture_dsn(self.out, state), good)
        for bad in (good.replace('127.0.0.1', '192.0.2.1'), good.replace('/bba_dev?', '/production?'),
                    good.replace(':31235/', ':5432/'), good + '&host=192.0.2.1'):
            self.mod.write_private(self.out / 'dsn.txt', bad)
            with self.assertRaises(ValueError):
                self.mod.fixture_dsn(self.out, state)

    def test_api_refuses_redirects_instead_of_forwarding_operator_credentials(self):
        hits = []
        class RedirectAPI(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                hits.append(self.path)
                self.send_response(302)
                self.send_header('Location', '/redirect-target')
                self.end_headers()
        server = ThreadingHTTPServer(('127.0.0.1', 0), RedirectAPI)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        with self.assertRaisesRegex(RuntimeError, 'HTTP 302') as error:
            self.mod.api(f'http://127.0.0.1:{server.server_address[1]}', 'secret-test-value',
                         '/v1/operator/goal-runs')
        self.assertEqual(hits, ['/v1/operator/goal-runs'])
        self.assertNotIn('secret-test-value', str(error.exception))

    def test_exact_container_ownership_required(self):
        instance = '123456abcdef'
        name = 'bba-goalfix-' + instance
        self.mod.guard_container(name, {self.mod.LABEL_KEY: instance}, instance)
        for wrong_name, labels in ((name, {}), (name, {self.mod.LABEL_KEY: 'other'}),
                                   ('bba-dev-stack-default-db', {self.mod.LABEL_KEY: instance})):
            with self.assertRaises(ValueError):
                self.mod.guard_container(wrong_name, labels, instance)

    def test_process_identity_includes_executable_and_start_time(self):
        binary = self.root / 'sleep'
        shutil.copyfile('/bin/sleep', binary)
        binary.chmod(0o700)
        proc = subprocess.Popen([str(binary), '30'])
        self.addCleanup(lambda: (proc.terminate(), proc.wait()) if proc.poll() is None else None)
        start = self.mod.process_start(proc.pid)
        self.assertTrue(self.mod.owned_process(proc.pid, binary, start))
        self.assertFalse(self.mod.owned_process(proc.pid, binary, 'wrong-start'))
        self.assertFalse(self.mod.owned_process(proc.pid, '/bin/sleep', start))

    def test_spawn_server_returns_owned_session_pid_and_stop_reaps_without_warning(self):
        self.mod.prepare_output(self.out, {})
        binary = self.out / 'sleep'
        shutil.copyfile('/bin/sleep', binary)
        binary.chmod(0o700)
        log = self.out / 'server.log'
        self.mod.write_private(log, 'existing log')
        state = self.mod.initial_state(self.out, '123456abcdef', 31234, 31235)
        state['binary'] = str(binary)
        with warnings.catch_warnings(record=True) as observed:
            warnings.simplefilter('always', ResourceWarning)
            pid = self.mod.spawn_server(binary, ['30'], {}, log)
            self.assertIsInstance(pid, int)
            state.update(server_pid=pid, process_start=self.mod.process_start(pid))
            try:
                gc.collect()  # a discarded Popen would warn while the child is alive
                self.assertTrue(self.mod.owned_process(pid, binary, state['process_start']))
                self.assertEqual(os.getsid(pid), pid)
                self.assertEqual(os.readlink(f'/proc/{pid}/fd/0'), '/dev/null')
                self.assertEqual(os.readlink(f'/proc/{pid}/fd/1'), str(log))
                self.assertEqual(os.readlink(f'/proc/{pid}/fd/2'), str(log))
                self.assertEqual(log.stat().st_mode & 0o777, 0o600)
                self.assertEqual(log.read_text(), 'existing log\n')
            finally:
                self.mod.stop_server(self.out, state)
            gc.collect()
        self.assertEqual([w for w in observed if issubclass(w.category, ResourceWarning)], [])
        self.assertIsNone(state['server_pid'])
        with self.assertRaises(ChildProcessError):
            os.waitpid(pid, os.WNOHANG)

    def test_spawn_rejects_symlink_log_without_touching_target(self):
        self.mod.prepare_output(self.out, {})
        target = self.out / 'untouched'
        target.write_text('keep')
        log = self.out / 'server.log'
        log.symlink_to(target)
        with self.assertRaises(OSError):
            self.mod.spawn_server('/bin/sleep', ['30'], {}, log)
        self.assertEqual(target.read_text(), 'keep')

    def test_stop_reaps_a_spawned_child_that_already_exited(self):
        self.mod.prepare_output(self.out, {})
        state = self.mod.initial_state(self.out, '123456abcdef', 31234, 31235)
        pid = self.mod.spawn_server('/bin/sleep', ['0'], {}, self.out / 'server.log')
        state.update(server_pid=pid, process_start=self.mod.process_start(pid))
        try:
            deadline = time.monotonic() + 3
            while self.mod.process_start(pid) and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertIsNone(self.mod.process_start(pid))
        finally:
            self.mod.stop_server(self.out, state)
        with self.assertRaises(ChildProcessError):
            os.waitpid(pid, os.WNOHANG)

    def test_start_server_uses_owned_pid_readiness_without_live_popen_handle(self):
        self.mod.prepare_output(self.out, {})
        (self.out / 'bin').mkdir()
        binary = self.out / 'bin/sleep'
        shutil.copyfile('/bin/sleep', binary)
        binary.chmod(0o700)
        state = self.mod.initial_state(self.out, '123456abcdef', 31234, 31235)
        state['binary'] = str(binary)
        state['binary_sha256'] = hashlib.sha256(binary.read_bytes()).hexdigest()
        self.mod.write_private(self.out / 'operator-token', 'not-a-real-token')
        self.mod.write_private(self.out / 'dsn.txt',
                               'postgres://bba_dev:synthetic@127.0.0.1:31235/bba_dev?sslmode=disable')
        with warnings.catch_warnings(record=True) as observed:
            warnings.simplefilter('always', ResourceWarning)
            try:
                with patch.object(self.mod, 'server_args', return_value=['30']), \
                        patch.object(self.mod, 'port_open', return_value=False), \
                        patch.object(self.mod, 'api', return_value={'data': [], 'meta': {'fixture_enabled': True}}):
                    self.mod.start_server(self.out, state, {})
                gc.collect()
                self.assertEqual(state['status'], 'running')
                self.assertTrue(self.mod.owned_process(state['server_pid'], binary, state['process_start']))
                self.assertEqual(json.loads((self.out / 'state.json').read_text())['server_pid'], state['server_pid'])
            finally:
                self.mod.stop_server(self.out, state)
        self.assertEqual([w for w in observed if issubclass(w.category, ResourceWarning)], [])

    def test_seed_uses_only_configuration_apis_and_distinct_saved_agents(self):
        requests = []
        class ConfigAPI(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_POST(self):
                value = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                requests.append((self.path, value, self.headers.get('Authorization')))
                kind = self.path.rsplit('/', 1)[-1]
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.end_headers()
                self.wfile.write(json.dumps({'data': dict(value, id=f'{kind}_{len(requests)}', version=1)}).encode())
        server = ThreadingHTTPServer(('127.0.0.1', 0), ConfigAPI)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        port = server.server_address[1]
        with contextlib.redirect_stdout(io.StringIO()) as stdout:
            result = self.mod.seed_registry(f'http://127.0.0.1:{port}', 'not-a-real-token')
        self.assertEqual([r[0] for r in requests], ['/v1/operator/models', '/v1/operator/skills',
                                                  '/v1/operator/agents', '/v1/operator/agents'])
        self.assertNotEqual(result['planner_ref']['id'], result['analyst_ref']['id'])
        self.assertEqual(requests[0][1]['base_url'], 'https://127.0.0.1:1/v1')
        self.assertEqual(requests[2][1]['model_id'], 'models_1')
        self.assertEqual(requests[3][1]['skills'], [{'id': 'skills_2', 'version': 1}])
        self.assertNotIn('not-a-real-token', stdout.getvalue())
        self.assertEqual(requests[2][1]['tools'], [])

    def test_command_errors_never_print_captured_credentials(self):
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
            with self.assertRaises(RuntimeError) as caught:
                self.mod.run(['/bin/sh', '-c', 'printf secret-test-value >&2; exit 7'])
        self.assertNotIn('secret-test-value', str(caught.exception) + out.getvalue() + err.getvalue())
        self.assertIn('7', str(caught.exception))

    def test_build_failure_cleans_owned_database_and_records_terminal_state(self):
        active = {}
        def command(argv, **kwargs):
            if argv[:2] == ['docker', 'run']:
                name = argv[argv.index('--name') + 1]
                label = argv[argv.index('--label') + 1].split('=', 1)
                if name.startswith('bba-goalbuild-'):
                    raise RuntimeError('offline build failed')
                active[name] = {'Id': 'fixture-container-id', 'Config': {'Labels': {label[0]: label[1]}},
                                'State': {'Running': True}}
                return subprocess.CompletedProcess(argv, 0, 'fixture-container-id\n', '')
            if argv[:2] == ['docker', 'inspect']:
                name = argv[-1]
                if name in active:
                    return subprocess.CompletedProcess(argv, 0, json.dumps([active[name]]), '')
                return subprocess.CompletedProcess(argv, 1, '', 'not found')
            if argv[:2] == ['docker', 'rm']:
                active.pop(argv[-1], None)
            return subprocess.CompletedProcess(argv, 0, '', '')
        with patch.object(self.mod, 'run', side_effect=command):
            with self.assertRaisesRegex(RuntimeError, 'offline build failed'):
                self.mod.cmd_start(self.out, {})
        self.assertEqual(active, {}, 'failed startup leaked an owned DB container')
        state = json.loads((self.out / 'state.json').read_text())
        self.assertEqual(state['status'], 'failed')
        self.assertTrue(json.loads((self.out / 'cleanup.json').read_text())['containers_gone'])

    def test_state_cannot_redirect_control_to_another_output_or_binary(self):
        self.mod.prepare_output(self.out, {})
        state = self.mod.initial_state(self.out, '123456abcdef', 31234, 31235)
        self.mod.write_state(self.out, state)
        self.assertEqual(self.mod.load_state(self.out)['instance'], '123456abcdef')
        for field, value in [('binary', '/bin/sleep'), ('output', '/tmp/other'),
                             ('container', 'bba-dev-stack-default-db')]:
            self.mod.write_state(self.out, dict(state, **{field: value}))
            with self.assertRaises(ValueError):
                self.mod.load_state(self.out)


if __name__ == '__main__':
    unittest.main()
