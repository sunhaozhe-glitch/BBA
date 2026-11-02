"""Executable boundary regressions; only harmless isolated marker files."""
import asyncio
from pathlib import Path
import tempfile
import unittest
from test_worktrees import fixture, git
from bba_controller.controller import Controller, Settings


class TrustedPeerTests(unittest.IsolatedAsyncioTestCase):
    async def test_shadow_package_never_executes_during_start_restore_or_exit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo, _ = fixture(root)
            marker = root/'outside-marker'
            payload = f'from pathlib import Path\nPath({str(marker)!r}).write_text("untrusted")\n'
            package = repo/'bba_controller'
            package.mkdir()
            (package/'__init__.py').write_text(payload)
            (package/'sim_peer.py').write_text(payload)
            (repo/'sitecustomize.py').write_text(payload)
            (repo/'usercustomize.py').write_text(payload)
            (repo/'json.py').write_text(payload)
            git(repo, 'add', '.')
            git(repo, 'commit', '-m', 'hostile import fixture')
            git(repo, 'push', 'origin', 'main')
            sha = git(repo, 'rev-parse', 'HEAD')
            c = Controller(Settings(root/'state', repo, fixture_authorized=True, slots=2))
            try:
                for behavior in ('success', 'hang', 'exit'):
                    result = await c.call('bba_task_submit', {'kind':'simulation', 'request_id':__import__('uuid').uuid4().hex, 'base_sha':sha,
                                          'behavior':behavior, 'write_set':[behavior]})
                    self.assertTrue(result['ok'], result)
                    tid = result['data']['task']['id']
                    await c.call('bba_task_wait', {'task_id':tid, 'timeout_ms':1000})
                    self.assertFalse(marker.exists(), f'worktree code executed: {behavior}')
                    self.assertEqual(c.store.get(tid)['state'], 'SUCCEEDED' if behavior=='success' else 'UNKNOWN')
                    if behavior == 'hang':
                        response = await c.call('bba_task_resume', {'task_id':tid})
                        self.assertTrue(response['ok'], response)
                        self.assertTrue(response['data']['restored'])
                        self.assertFalse(marker.exists(), 'restore executed worktree code')
                        await c.call('bba_task_resume', {'task_id':tid,'simulation_receipt':'CANCELLED'})
            finally:
                await c.close()
            self.assertFalse(marker.exists())
