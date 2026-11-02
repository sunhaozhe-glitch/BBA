import copy
import json
from pathlib import Path
import tempfile
import threading
import unittest

from bba_controller.models import ModelRouter, ModelDenied, default_config
from bba_controller.store import Store


class ModelPolicyTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.store=Store(Path(self.tmp.name)/'state.db',slots=2)
        self.config=default_config()
        self.router=ModelRouter(self.store,self.config)

    def task(self, policy='simulation-default@1'):
        spec={'kind':'simulation','write_set':['src']}
        t=self.store.submit(spec,policy_snapshot=self.router.snapshot(policy))
        self.store.claim(t['id'])
        self.store.session(t['id'],'sim-session','simulation')
        return t['id']

    def test_multiple_local_profiles_switch_separate_audit_and_budget_settle(self):
        tid=self.task()
        a=self.router.begin(tid,'sim-session','mock-local-builder','builder',3)
        self.router.finish(a,2,'SUCCEEDED')
        b=self.router.begin(tid,'sim-session','mock-local-reviewer','reviewer',2)
        self.router.finish(b,1,'SUCCEEDED')
        evidence=self.router.evidence(tid)
        self.assertEqual(evidence['budget'],{'limit_units':10,'reserved_units':0,'spent_units':3})
        self.assertEqual(len(evidence['calls']),2)
        self.assertEqual({x['profile'] for x in evidence['calls']},{'mock-local-builder','mock-local-reviewer'})
        self.assertTrue(all(x['task_id']==tid and x['session_id']=='sim-session' and x['start'] and x['end'] for x in evidence['calls']))
        switches=[e for e in self.store.events(tid) if e['kind']=='model_switch']
        self.assertEqual(len(switches),1)
        self.assertEqual(switches[0]['data']['to'],'mock-local-reviewer')
        self.assertTrue(all(e['kind']!='permission_denied' for e in self.store.events(tid)))

    def test_unknown_profile_role_provider_nonlocal_advanced_fallback_and_session_deny(self):
        tid=self.task()
        for profile,role,session in [('missing','builder','sim-session'),('mock-local-reviewer','builder','sim-session'),('reserved-advanced','advisor','sim-session'),('mock-local-builder','builder','wrong-session')]:
            with self.assertRaises(ModelDenied): self.router.begin(tid,session,profile,role,1)
        with self.assertRaises(ModelDenied): self.router.begin(tid,'sim-session','mock-local-builder','builder',1,fallback=True)
        for version, mutation in enumerate(('provider','is_local','enabled','simulation'),2):
            config=default_config()
            config['profiles'][0][mutation]={'provider':'external','is_local':False,'enabled':False,'simulation':False}[mutation]
            config['profiles'][0]['version']=version
            config['policies'][0]['id']='deny-'+mutation.replace('_','-')
            config['policies'][0]['model_role']['builder']=[f'mock-local-builder@{version}']
            config['policies'][0]['default_profile']=f'mock-local-builder@{version}'
            router=ModelRouter(self.store,config,register_existing=True)
            denied=self.task('deny-'+mutation.replace('_','-')+'@1')
            with self.assertRaises(ModelDenied): router.begin(denied,'sim-session','mock-local-builder','builder',1)

    def test_budget_reservation_concurrency_unknown_and_restart_no_replay(self):
        tid=self.task()
        results=[]
        def reserve():
            try: results.append(self.router.begin(tid,'sim-session','mock-local-builder','builder',6))
            except ModelDenied: results.append(None)
        threads=[threading.Thread(target=reserve) for _ in range(2)]
        for t in threads:t.start()
        for t in threads:t.join()
        self.assertEqual(sum(x is not None for x in results),1)
        call=next(x for x in results if x)
        self.router.finish(call,None,'UNKNOWN','transport timeout')
        restored=ModelRouter(self.store,self.config)
        restored.recover()
        evidence=restored.evidence(tid)
        self.assertEqual(evidence['budget']['reserved_units'],6)
        self.assertEqual(evidence['calls'][0]['outcome'],'UNKNOWN')
        self.assertEqual(evidence['calls'][0]['failure_reason'],'transport timeout')
        with self.assertRaises(ModelDenied): restored.begin(tid,'sim-session','mock-local-builder','builder',5)
        with self.assertRaises(ModelDenied): restored.finish(call,1,'SUCCEEDED')

    def test_immutable_task_snapshot_and_versioned_policy_registration(self):
        tid=self.task()
        config=default_config()
        config['policies'][0]['version']=2
        config['policies'][0]['model_budget']['limit_units']=99
        newer=ModelRouter(self.store,config)
        self.assertEqual(newer.evidence(tid)['snapshot']['policy']['version'],1)
        self.assertEqual(newer.evidence(tid)['budget']['limit_units'],10)
        with self.assertRaises(ModelDenied): newer.snapshot('unregistered@1')
        changed=default_config()
        changed['policies'][0]['model_budget']['limit_units']=100
        with self.assertRaises(ModelDenied): ModelRouter(self.store,changed)

    def test_over_settlement_does_not_release_reservation(self):
        tid=self.task()
        call=self.router.begin(tid,'sim-session','mock-local-builder','builder',2)
        with self.assertRaises(ModelDenied): self.router.finish(call,3,'SUCCEEDED')
        self.assertEqual(self.router.evidence(tid)['budget']['reserved_units'],2)
