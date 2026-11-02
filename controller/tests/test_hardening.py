import asyncio
import json
from pathlib import Path
import tempfile
import unittest

from bba_controller.controller import Controller, Settings
from bba_controller.evidence import redact
from bba_controller.server import BoundedInput
from bba_controller.store import Conflict
from test_worktrees import fixture


class HardeningTests(unittest.IsolatedAsyncioTestCase):
    async def test_transport_line_bound(self):
        class Input:
            async def readline(self, size):
                return 'x' * size
        stream = BoundedInput(Input(), limit=100)
        with self.assertRaises(ValueError):
            await anext(stream)

    async def test_owner_rework_and_submit_does_not_upgrade_authority(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo, sha = fixture(root)
            settings = Settings(root/'state',repo,fixture_authorized=True)
            c = Controller(settings)
            try:
                with self.assertRaises(Conflict): Controller(settings)
                r = await c.call('bba_task_submit',{'kind':'simulation','base_sha':sha,'write_set':['src'],'rework_failures':3})
                tid = r['data']['task']['id']
                result = await c.call('bba_task_wait',{'task_id':tid,'timeout_ms':1000})
                self.assertEqual(result['data']['task']['state'],'PAUSED')
                events = c.store.events(tid,limit=1000)
                self.assertEqual(len([e for e in events if e['kind']=='rework']),3)
                self.assertEqual(c.store.slot_count(),0)
            finally:
                await c.close()

    async def test_redaction_text_patterns(self):
        value = redact({'nested':{'access_token':'abc'}, 'text':'api_key="quoted-secret" Authorization: Bearer bearer-secret password=pwd https://user:pwd@host sk-proj-abcdef123456789012345678901234'})
        raw = json.dumps(value)
        for secret in ('quoted-secret','bearer-secret','pwd','abcdef123456789012345678901234'):
            self.assertNotIn(secret,raw)
