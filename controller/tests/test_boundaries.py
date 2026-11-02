import asyncio
import json
from pathlib import Path
import tempfile
import unittest

from bba_controller.controller import Controller, Settings
from bba_controller.store import Store
from test_worktrees import fixture


class BoundaryTests(unittest.IsolatedAsyncioTestCase):
    async def test_readonly_cancel_is_denied_and_spec_is_not_silently_truncated(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo, sha = fixture(root)
            store = Store(root/'state'/'controller.sqlite3')
            task = store.submit({'kind':'simulation','base_sha':sha,'write_set':['src']})
            observer = Controller(Settings(root/'state',repo))
            try:
                result = await observer.call('bba_task_cancel',{'task_id':task['id']})
                self.assertEqual(result['error']['code'],'PERMISSION_DENIED')
                self.assertEqual(store.get(task['id'])['state'],'QUEUED')
            finally:
                await observer.close()
            spec = {'kind':'simulation','base_sha':sha,'write_set':[str(n)+'x'*254 for n in range(32)]}
            original = store.submit(spec)
            self.assertEqual(original['spec'], spec)

    async def test_two_concurrent_controller_worktrees_isolate_sessions(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo, sha = fixture(root)
            c = Controller(Settings(root/'state',repo,fixture_authorized=True,slots=2))
            try:
                ids = []
                for writes in (['src'],['docs']):
                    r = await c.call('bba_task_submit',{'kind':'simulation','base_sha':sha,'write_set':writes,'behavior':'cancel'})
                    ids.append(r['data']['task']['id'])
                for _ in range(150):
                    if all(c.store.latest_session(t) for t in ids): break
                    await asyncio.sleep(.005)
                self.assertEqual(c.store.slot_count(),2)
                self.assertEqual(len({c.store.latest_session(t)['session_id'] for t in ids}),2)
                for tid in ids:
                    self.assertTrue((root/'state'/'worktrees'/tid/'.git').is_file())
                    self.assertTrue((root/'state'/'agents'/tid/'sim-sessions').is_dir())
            finally:
                await c.close()
