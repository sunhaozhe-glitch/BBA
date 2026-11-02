import asyncio
from pathlib import Path
import tempfile
import unittest

from bba_controller.controller import Controller,Settings
from bba_controller.models import default_config
from test_worktrees import fixture


class ModelIntegrationTests(unittest.IsolatedAsyncioTestCase):
    async def test_policy_reference_and_two_mock_profiles_are_wired_to_acp_audit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            repo,sha=fixture(root)
            config=default_config()
            config['policies'].append({**config['policies'][0],'id':'review-only','default_profile':'mock-local-reviewer@1','default_role':'reviewer'})
            c=Controller(Settings(root/'state',repo,fixture_authorized=True,model_config=config))
            try:
                for ref,expected in [('simulation-default@1','mock-local-builder'),('review-only@1','mock-local-reviewer')]:
                    result=await c.call('bba_task_submit',{'kind':'simulation','base_sha':sha,'write_set':['src'],'model_policy_ref':ref})
                    tid=result['data']['task']['id']
                    result=await c.call('bba_task_wait',{'task_id':tid,'timeout_ms':1000})
                    self.assertEqual(result['data']['task']['state'],'SUCCEEDED')
                    evidence=(await c.call('bba_task_evidence',{'task_id':tid}))['data']['model']
                    self.assertEqual(evidence['calls'][0]['profile'],expected)
                    self.assertEqual(evidence['calls'][0]['outcome'],'SUCCEEDED')
                    self.assertEqual(evidence['budget']['spent_units'],1)
                    self.assertEqual(evidence['snapshot']['policy']['id'],ref.split('@')[0])
                denied=await c.call('bba_task_submit',{'kind':'simulation','base_sha':sha,'model_policy_ref':'advanced@1'})
                self.assertEqual(denied['error']['code'],'PERMISSION_DENIED')
                upgrade=await c.call('bba_task_submit',{'kind':'simulation','base_sha':sha,'model_budget':9999})
                self.assertEqual(upgrade['error']['code'],'INVALID_ARGUMENT')
            finally:
                await c.close()

    async def test_unknown_model_call_retains_budget_and_no_fallback(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            repo,sha=fixture(root)
            c=Controller(Settings(root/'state',repo,fixture_authorized=True))
            try:
                r=await c.call('bba_task_submit',{'kind':'simulation','base_sha':sha,'behavior':'hang'})
                tid=r['data']['task']['id']
                await c.call('bba_task_wait',{'task_id':tid,'timeout_ms':1000})
                model=(await c.call('bba_task_evidence',{'task_id':tid}))['data']['model']
                self.assertEqual(model['calls'][0]['outcome'],'UNKNOWN')
                self.assertEqual(model['budget']['reserved_units'],1)
                self.assertEqual(len(model['calls']),1)
            finally:
                await c.close()
