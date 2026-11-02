import asyncio
import json
from pathlib import Path
import tempfile
import time
import unittest

from bba_controller.controller import Controller, Settings
from bba_controller.contracts import TOOLS, OUTPUT
from jsonschema import validate
from test_worktrees import fixture


class ControllerTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.repo, self.sha = fixture(self.root)
        self.c = Controller(Settings(self.root/'state', self.repo, fixture_authorized=True, slots=2))
        self.addAsyncCleanup(self.c.close)

    async def call(self, tool, args):
        result = await self.c.call('bba_task_' + tool, args)
        validate(result, OUTPUT)
        return result

    def spec(self, behavior='success', writes=None):
        return {'kind':'simulation', 'request_id':__import__('uuid').uuid4().hex,'base_sha':self.sha,'write_set':writes or ['src'], 'behavior':behavior}

    async def test_seven_tools_schemas_and_lifecycle_evidence(self):
        self.assertEqual(len(TOOLS), 7)
        r = await self.call('submit', self.spec())
        self.assertTrue(r['ok'])
        tid = r['data']['task']['id']
        for _ in range(50):
            result = await self.call('wait', {'task_id':tid,'timeout_ms':100})
            if result['data']['task']['state'] == 'SUCCEEDED': break
        self.assertEqual(result['data']['task']['state'], 'SUCCEEDED')
        self.assertEqual((await self.call('status', {'task_id':tid}))['data']['task']['id'], tid)
        self.assertEqual(len((await self.call('list', {}))['data']['tasks']), 1)
        evidence = (await self.call('evidence', {'task_id':tid}))['data']
        self.assertTrue(evidence['session']['session_id'].startswith('sim-'))
        self.assertTrue(evidence['artifacts'])
        self.assertTrue(any(e['kind'] == 'permission_denied' for e in self.c.store.events(tid, limit=1000)))
        self.assertFalse((await self.call('resume', {'task_id':tid}))['ok'])
        self.assertFalse((await self.call('cancel', {'task_id':tid}))['ok'])

    async def test_permissions_reject_prompt_mode_repo_and_path_injection(self):
        for args in ({'kind':'real','prompt':'do business'}, {**self.spec(),'mode':'DEVELOPMENT'}, {**self.spec(),'repo':'/arbitrary'}, {**self.spec(),'write_set':['../outside']}, {**self.spec(),'write_set':['x;touch y']}):
            result = await self.call('submit', args)
            self.assertFalse(result['ok'])
        observer = Controller(Settings(self.root/'readonly', self.repo))
        self.addAsyncCleanup(observer.close)
        result = await observer.call('bba_task_submit', self.spec())
        self.assertEqual(result['error']['code'], 'PERMISSION_DENIED')
        with self.assertRaises(PermissionError): Settings(self.root/'dev', self.repo, mode='DEVELOPMENT')

    async def test_cancel_timeout_restart_resume_reconcile_and_nonblocking_wait(self):
        r = await self.call('submit', self.spec('cancel'))
        tid = r['data']['task']['id']
        for _ in range(100):
            if any(e['kind']=='rpc_send' and e['data'].get('method')=='session/prompt' for e in self.c.store.events(tid,limit=1000)): break
            await asyncio.sleep(.01)
        waiting = asyncio.create_task(self.call('wait', {'task_id':tid,'timeout_ms':300}))
        before = time.monotonic()
        self.assertTrue((await self.call('list', {}))['ok'])
        self.assertLess(time.monotonic()-before, .2)
        result = await self.call('cancel', {'task_id':tid})
        self.assertTrue(result['ok'])
        await waiting
        for _ in range(50):
            if self.c.store.get(tid)['state'] != 'RUNNING': break
            await asyncio.sleep(.01)
        self.assertEqual(self.c.store.get(tid)['state'], 'CANCELLED')
        r = await self.call('submit', self.spec('hang', ['other']))
        unknown = r['data']['task']['id']
        await self.call('wait', {'task_id':unknown,'timeout_ms':1000})
        self.assertEqual(self.c.store.get(unknown)['state'], 'UNKNOWN')
        self.assertEqual(self.c.store.slot_count(), 1)
        await self.c.close()
        new = Controller(Settings(self.root/'state', self.repo, fixture_authorized=True, slots=2))
        self.addAsyncCleanup(new.close)
        restored = await new.call('bba_task_resume', {'task_id':unknown})
        self.assertTrue(restored['ok'])
        self.assertEqual(new.store.get(unknown)['state'], 'UNKNOWN')
        self.assertEqual(new.store.slot_count(), 1)
        reconciled = await new.call('bba_task_resume', {'task_id':unknown, 'simulation_receipt':'CANCELLED'})
        self.assertTrue(reconciled['ok'])
        self.assertEqual(new.store.slot_count(), 0)

    async def test_limits_and_error_codes(self):
        for tool, args, code in [('status', {'task_id':'0'*32}, 'NOT_FOUND'), ('wait', {'task_id':'0'*32,'timeout_ms':60000}, 'INVALID_ARGUMENT'), ('submit', {**self.spec(),'extra':'x'*40000}, 'LIMIT_EXCEEDED')]:
            self.assertEqual((await self.call(tool,args))['error']['code'], code)
        self.assertEqual((await self.c.call('unknown', {}))['error']['code'], 'NOT_FOUND')
