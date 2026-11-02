import asyncio,tempfile
from pathlib import Path
import unittest
from test_worktrees import fixture
from bba_controller.controller import Controller,Settings

class PublicReplayTests(unittest.IsolatedAsyncioTestCase):
    async def test_replay_survives_changed_path_and_policy(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo,sha=fixture(root)
            c=Controller(Settings(root/'state',repo,fixture_authorized=True))
            try:
                spec={'kind':'simulation','base_sha':sha,'request_id':'stable-path','write_set':['target']}
                first=await c.call('bba_task_submit',spec)
                self.assertTrue(first['ok'])
                tid=first['data']['task']['id']
                await c.call('bba_task_wait',{'task_id':tid,'timeout_ms':1000})
                (repo/'target').symlink_to(root,target_is_directory=True)
                before=c.store.events(tid)
                repeated=await c.call('bba_task_submit',spec)
                self.assertTrue(repeated['ok'],repeated)
                self.assertEqual(repeated['data']['task']['id'],tid)
                changed=await c.call('bba_task_submit',{**spec,'behavior':'exit'})
                self.assertEqual(changed['error']['code'],'CONFLICT')
                def unavailable(*args,**kwargs): raise AssertionError('replay consulted current policy')
                c.models.snapshot=unavailable
                repeated=await c.call('bba_task_submit',spec)
                self.assertTrue(repeated['ok'],repeated)
                self.assertEqual(before,c.store.events(tid))
                self.assertEqual(len(c.store.list()),1)
            finally: await c.close()

    async def test_request_identity_rejects_trailing_newline(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo,sha=fixture(root)
            c=Controller(Settings(root/'state',repo,fixture_authorized=True))
            try:
                result=await c.call('bba_task_submit',{'kind':'simulation','base_sha':sha,'request_id':'newline\n'})
                self.assertFalse(result['ok'],result)
                self.assertEqual(result['error']['code'],'INVALID_ARGUMENT')
                self.assertEqual(len(c.store.list()),0)
            finally: await c.close()

    async def test_response_loss_replay_and_conflict_use_public_tool(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo,sha=fixture(root)
            c=Controller(Settings(root/'state',repo,fixture_authorized=True))
            try:
                spec={'kind':'simulation','base_sha':sha,'request_id':'public-lost-response'}
                first=await c.call('bba_task_submit',spec)
                repeated=await c.call('bba_task_submit',spec)
                self.assertTrue(first['ok']);self.assertTrue(repeated['ok'])
                self.assertEqual(first['data']['task']['id'],repeated['data']['task']['id'])
                changed=await c.call('bba_task_submit',{**spec,'behavior':'exit'})
                self.assertEqual(changed['error']['code'],'CONFLICT')
                missing=await c.call('bba_task_submit',{'kind':'simulation','base_sha':sha})
                self.assertEqual(missing['error']['code'],'INVALID_ARGUMENT')
                await c.call('bba_task_wait',{'task_id':first['data']['task']['id'],'timeout_ms':1000})
                self.assertEqual(len(c.store.list()),1)
            finally: await c.close()
