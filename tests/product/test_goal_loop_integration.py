"""Opt-in real-server fixture verification, never a caller-selected DSN.

Start tools/goal-loop-fixture.py first, then:
BBA_GOAL_FIXTURE_OUTPUT=evals/results/<owned-fixture> python3 -m unittest tests.product.test_goal_loop_integration -v
The test restarts only that labelled fixture and restores its dump into an
additional temporary DB in the same owned container. No external writers exist.
"""
import importlib.util
import json
import os
from pathlib import Path
import secrets
import time
import unittest
import urllib.error
import urllib.request

from tools.validate_contracts import ContractLoader

ROOT = Path(__file__).resolve().parents[2]


@unittest.skipUnless(os.environ.get('BBA_GOAL_FIXTURE_OUTPUT'), 'explicit owned goal fixture required')
class GoalLoopIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec = importlib.util.spec_from_file_location('goal_integration_launcher', ROOT / 'tools/goal-loop-fixture.py')
        cls.launcher = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.launcher)
        cls.output = cls.launcher.existing_output(os.environ['BBA_GOAL_FIXTURE_OUTPUT'])
        cls.state = cls.launcher.load_state(cls.output)
        if not cls.launcher.inspect_container(cls.state):
            raise AssertionError("owned fixture container is missing")
        cls.config = json.loads(cls.launcher.private_read(cls.output / 'fixture-config.json'))
        cls.origin = f"http://127.0.0.1:{cls.state['http_port']}"
        cls.token = cls.launcher.private_read(cls.output / 'operator-token').strip()
        cls.contracts = ContractLoader(ROOT / 'contracts')
        cls.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def api(self, suffix='', body=None, expected=200):
        headers = {'Authorization': 'Bearer ' + self.token}
        if body is not None:
            headers.update({'Content-Type': 'application/json', 'Idempotency-Key': secrets.token_hex(16)})
        req = urllib.request.Request(self.origin + '/v1/operator/goal-runs' + suffix,
                                     data=None if body is None else json.dumps(body).encode(), headers=headers)
        try:
            with self.opener.open(req, timeout=5) as response:
                status, result = response.status, json.load(response)
        except urllib.error.HTTPError as error:
            status, result = error.code, json.load(error)
            error.close()
        self.assertEqual(status, expected, result.get('error', {}).get('code'))
        name = 'ErrorEnvelope' if expected != 200 else 'GoalRunListEnvelope' if isinstance(result['data'], list) else 'GoalRunEnvelope'
        schema, source = self.contracts.resolve_component(name)
        self.assertEqual(self.contracts.schema_errors(result, schema, source), [], f'actual HTTP contract mismatch: {name}')
        return result

    def wait(self, run_id, state, round_number):
        for _ in range(80):
            run = self.api('/' + run_id)['data']
            if run['state'] == state and run['round'] == round_number:
                return run
            time.sleep(.1)
        self.fail('bounded goal fixture wait timed out')

    @staticmethod
    def window(event, text, expired=False):
        now = time.time()
        stamp = lambda offset: time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(now + offset))
        return dict(event_id=event, group_id='20001', window_start=stamp(-60), window_end=stamp(0),
                    expires_at=stamp(-1 if expired else 600), messages=[dict(id=event + '-msg', at=stamp(-1), text=text)])

    def sql(self, statement, database='bba_dev'):
        result = self.launcher.run(['docker', 'exec', self.state['container'], 'psql', '-X', '-v', 'ON_ERROR_STOP=1',
                                    '-U', 'bba_dev', '-d', database, '-Atc', statement])
        return result.stdout.strip()

    def test_http_loop_restart_restore_and_zero_qq_effects(self):
        unique = secrets.token_hex(6)
        refs = {name: self.config[name] for name in ('planner_ref', 'analyst_ref')}
        run = self.api(body=dict(thread_id='integration-' + unique, group_id='20001',
                                goal='分析指定测试群的近期需求，证据不足时列出信息缺口。', **refs))['data']
        path = '/' + run['id']
        self.api(path + '/fixture-windows', self.window('sparse-' + unique, '需求：课程'))
        first = self.wait(run['id'], 'WAITING_INPUT', 1)
        self.assertEqual(len(first['memory']['information_gaps']), 2)
        self.launcher.cmd_restart(self.output, env={})
        resumed = self.api(path)['data']
        self.assertEqual(resumed['rounds'], first['rounds'])
        self.assertEqual(resumed['memory'], first['memory'])
        self.api(path + '/fixture-windows', self.window('complete-' + unique, '需求：课程；预算：1000；时间：本周'))
        ready = self.wait(run['id'], 'AWAITING_CONFIRMATION', 2)
        self.assertIn('复核上一轮缺口', ready['rounds'][1]['change_reason'])
        self.assertNotEqual(first['rounds'][0]['consumed_evidence_ids'], ready['rounds'][1]['consumed_evidence_ids'])
        self.assertTrue(ready['graph']['anchors'])
        failure = self.api(path + '/fixture-windows', self.window('expired-' + unique, 'anything', True), expected=409)
        self.assertEqual(failure['error']['code'], 'WINDOW_EXPIRED')
        confirmed = self.api(path + '/confirm', {'expected_version': ready['version']})['data']
        self.assertEqual(confirmed['state'], 'CONFIRMED')
        for table in ('task', 'work_item', 'execution', 'dispatch_permit', 'chat_send_permit', 'model_request'):
            self.assertEqual(self.sql('SELECT count(*) FROM core.' + table), '0', table)
        # Full data restore at current schema, not a JSON/UI-only snapshot.
        restored = 'goal_restore_' + unique
        dump = self.launcher.run(['docker', 'exec', self.state['container'], 'pg_dump', '-U', 'bba_dev', '-d', 'bba_dev', '--no-owner']).stdout
        try:
            self.sql('CREATE DATABASE ' + restored)
            self.launcher.run(['docker', 'exec', '-i', self.state['container'], 'psql', '-X', '-v', 'ON_ERROR_STOP=1',
                               '-U', 'bba_dev', '-d', restored], input=dump)
            self.assertEqual(self.sql('SELECT max(version) FROM core.schema_version', restored), '27')
            for table in ('goal_run', 'goal_command', 'goal_event', 'goal_round', 'goal_step', 'goal_result', 'goal_evidence'):
                query = 'SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),\'[]\')::text FROM core.' + table + ' t'
                self.assertEqual(self.sql(query), self.sql(query, restored), table)
        finally:
            self.sql('DROP DATABASE IF EXISTS ' + restored + ' WITH (FORCE)')
