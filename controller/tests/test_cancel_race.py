import asyncio
from pathlib import Path
import tempfile
import unittest

from bba_controller.controller import Controller, Settings
from test_worktrees import fixture


class CancelRaceTests(unittest.IsolatedAsyncioTestCase):
    async def test_cancel_claimed_before_worker_starts_does_not_send_prompt(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            repo,sha=fixture(root)
            c=Controller(Settings(root/'state',repo,fixture_authorized=True))
            try:
                task=c.store.submit({'kind':'simulation', 'request_id':__import__('uuid').uuid4().hex,'base_sha':sha,'write_set':['src']})
                c.store.claim(task['id'])
                result=await c.call('bba_task_cancel',{'task_id':task['id']})
                self.assertTrue(result['ok'])
                await c._execute(task['id'])
                methods=[e['data'].get('method') for e in c.store.events(task['id'],limit=1000)]
                self.assertNotIn('session/prompt',methods)
                self.assertIsNone(c.store.latest_session(task['id']))
                self.assertEqual(c.store.slot_count(),1)
                self.assertEqual(c.store.get(task['id'])['state'],'UNKNOWN')
            finally:
                await c.close()
