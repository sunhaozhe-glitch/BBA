import asyncio
from pathlib import Path
import tempfile
import unittest

from bba_controller.controller import Controller, Settings
from bba_controller.store import Store
from bba_controller.worktrees import BoundaryError
from test_worktrees import fixture


class RecoveryTests(unittest.IsolatedAsyncioTestCase):
    async def test_startup_preserves_slot_and_rejects_state_symlink(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo, sha = fixture(root)
            state = root/'state'
            s = Store(state/'controller.sqlite3')
            t = s.submit({'kind':'simulation', 'request_id':__import__('uuid').uuid4().hex,'base_sha':sha,'write_set':['src']})
            s.claim(t['id'])
            c = Controller(Settings(state,repo,fixture_authorized=True))
            try:
                self.assertEqual(c.store.get(t['id'])['state'],'UNKNOWN')
                self.assertEqual(c.store.slot_count(),1)
                denied = await c.call('bba_task_resume',{'task_id':t['id']})
                self.assertEqual(denied['error']['code'],'CONFLICT')
            finally:
                await c.close()
            alias = root/'alias'
            alias.mkdir()
            (alias/'controller.sqlite3').symlink_to(state/'controller.sqlite3')
            with self.assertRaises(BoundaryError): Controller(Settings(alias,repo,fixture_authorized=True))

    async def test_readonly_startup_never_schedules_preexisting_queue(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo, sha = fixture(root)
            state = root/'state'
            s = Store(state/'controller.sqlite3')
            task = s.submit({'kind':'simulation', 'request_id':__import__('uuid').uuid4().hex,'base_sha':sha,'write_set':['src']})
            c = Controller(Settings(state,repo))
            try:
                await c.call('bba_task_list',{})
                await asyncio.sleep(.1)
                self.assertEqual(c.store.get(task['id'])['state'],'QUEUED')
                self.assertEqual(c.store.slot_count(),0)
            finally:
                await c.close()
