import asyncio
import copy
from pathlib import Path
import tempfile
import unittest

from bba_controller.controller import Controller, Settings
from bba_controller.models import ModelRouter, ModelDenied, default_config
from bba_controller.store import Store
from test_worktrees import fixture


class ModelGateTests(unittest.IsolatedAsyncioTestCase):
    async def test_mock_peer_receives_explicit_profile_selection_before_prompt(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            repo,sha=fixture(root)
            c=Controller(Settings(root/'state',repo,fixture_authorized=True))
            try:
                r=await c.call('bba_task_submit',{'kind':'simulation', 'request_id':__import__('uuid').uuid4().hex,'base_sha':sha,'model_policy_ref':'simulation-review@1'})
                self.assertTrue(r['ok'])
                tid=r['data']['task']['id']
                result=await c.call('bba_task_wait',{'task_id':tid,'timeout_ms':1000})
                self.assertEqual(result['data']['task']['state'],'SUCCEEDED')
                events=c.store.events(tid,limit=1000)
                methods=[e['data'].get('method') for e in events if e['kind']=='rpc_send']
                self.assertIn('simulation/select_model',methods)
                self.assertLess(methods.index('simulation/select_model'),methods.index('session/prompt'))
            finally:
                await c.close()

    async def test_model_configuration_schema_rejects_unknown_and_invalid_authority(self):
        with tempfile.TemporaryDirectory() as tmp:
            store=Store(Path(tmp)/'state.db')
            for field,value in [('profiles','not-a-list'),('version',2),('extra',True)]:
                config=default_config()
                config[field]=value
                with self.assertRaises(ModelDenied):ModelRouter(store,config)
            config=default_config()
            config['policies'][0]['fallback_policy']['mode']='allow'
            with self.assertRaises(ModelDenied):ModelRouter(store,config)
