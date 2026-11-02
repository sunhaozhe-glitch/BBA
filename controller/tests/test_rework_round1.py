"""Independent-review regressions, using isolated Git and real fixture peers."""
import asyncio
from pathlib import Path
import tempfile
import threading
from unittest.mock import patch
import unittest

from bba_controller.acp import Supervisor
from bba_controller.store import Store, Conflict
import subprocess
import sys
from bba_controller.models import ModelRouter, ModelDenied, default_config
import copy

from bba_controller.controller import Controller, Settings
from test_worktrees import fixture


class ReworkTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.repo, self.sha = fixture(self.root)
        self.c = Controller(Settings(self.root/'state', self.repo, fixture_authorized=True, slots=2))
        self.addAsyncCleanup(self.c.close)

    async def submit(self, behavior='success', writes=None, **extra):
        result = await self.c.call('bba_task_submit', {'kind':'simulation', 'base_sha':self.sha,
                                  'write_set':writes or ['src'], 'behavior':behavior, **extra})
        self.assertTrue(result['ok'], result)
        return result['data']['task']['id']

    async def until(self, predicate, timeout=5):
        async with asyncio.timeout(timeout):
            while not predicate():
                await asyncio.sleep(.01)

    async def test_f1_history_does_not_hide_queued_and_blocked_queue_is_fair(self):
        for _ in range(105):
            tid = await self.submit()
            result = await self.c.call('bba_task_cancel', {'task_id':tid})
            self.assertTrue(result['ok'], result)
        tid = await self.submit()
        result = await self.c.call('bba_task_wait', {'task_id':tid, 'timeout_ms':1000})
        self.assertEqual(result['data']['task']['state'], 'SUCCEEDED', result)
        # A long queue of dependency-blocked tasks must not hide runnable work.
        blocker = await self.submit('hang', ['held'])
        await self.until(lambda: self.c.store.get(blocker)['state']=='UNKNOWN')
        for i in range(90):
            await self.submit(writes=[f'blocked{i}'], dependencies=[blocker])
        tail = await self.submit(writes=['free'])
        result = await self.c.call('bba_task_wait', {'task_id':tail, 'timeout_ms':1000})
        self.assertEqual(result['data']['task']['state'], 'SUCCEEDED', result)

    async def test_f5_custom_simulation_profile_end_to_end(self):
        await self.c.close()
        config = default_config()
        config['profiles'].append({**config['profiles'][0], 'id':'mock-local-custom', 'version':7})
        policy = config['policies'][0]
        policy['model_role']['builder'] = ['mock-local-custom@7']
        policy['default_profile'] = 'mock-local-custom@7'
        self.c = Controller(Settings(self.root/'custom', self.repo, fixture_authorized=True, slots=2, model_config=config))
        self.addAsyncCleanup(self.c.close)
        tid = await self.submit()
        await self.until(lambda: tid not in self.c.running and self.c.store.get(tid)['state']!='QUEUED')
        self.assertEqual(self.c.store.get(tid)['state'], 'SUCCEEDED')
        self.assertEqual(self.c.store.slot_count(), 0)
        model = self.c.models.evidence(tid)
        self.assertEqual(model['budget']['reserved_units'], 0)
        self.assertEqual(model['calls'][0]['profile_version'], 7)
        selection = [e['data'] for e in self.c.store.events(tid,limit=1000) if e['kind']=='model_selected']
        self.assertEqual(selection[0]['profile_ref'], 'mock-local-custom@7')

    async def test_f4_exact_default_version_switch_and_ambiguous_legacy_deny(self):
        await self.c.close()
        config = default_config()
        config['profiles'].append({**config['profiles'][0], 'version':2})
        policy = config['policies'][0]
        policy['model_role']['builder'].append('mock-local-builder@2')
        policy['default_profile'] = 'mock-local-builder@2'
        self.c = Controller(Settings(self.root/'v2', self.repo, fixture_authorized=True, slots=2, model_config=config))
        self.addAsyncCleanup(self.c.close)
        tid = await self.submit()
        await self.until(lambda: tid not in self.c.running and self.c.store.get(tid)['state']!='QUEUED')
        call = self.c.models.evidence(tid)['calls'][0]
        self.assertEqual(call['profile_version'], 2)
        self.assertEqual(self.c.store.get(tid)['state'], 'SUCCEEDED')
        selected = [e for e in self.c.store.events(tid,limit=1000) if e['kind']=='model_selected']
        self.assertEqual(selected[0]['data']['profile_ref'], 'mock-local-builder@2')
        # Direct API compatibility is accepted only when the name is unambiguous.
        t = self.c.store.submit({'kind':'simulation','write_set':['unit']}, self.c.models.snapshot())
        self.c.store.claim(t['id'])
        self.c.store.session(t['id'], 'sim-unit', 'simulation')
        with self.assertRaises(ModelDenied):
            self.c.models.begin(t['id'],'sim-unit','mock-local-builder','builder',1)
        for ref in ('mock-local-builder@1','mock-local-builder@2'):
            cid = self.c.models.begin(t['id'],'sim-unit',ref,'builder',1)
            self.c.models.finish(cid,1,'SUCCEEDED')
        switch = [e for e in self.c.store.events(t['id']) if e['kind']=='model_switch'][-1]['data']
        self.assertEqual(switch['from_ref'], 'mock-local-builder@1')
        self.assertEqual(switch['to_ref'], 'mock-local-builder@2')
        invalid = copy.deepcopy(config)
        invalid['policies'][0]['version'] = 2
        invalid['policies'][0]['default_profile'] = 'mock-local-reviewer@1'
        router = ModelRouter(self.c.store, invalid)
        with self.assertRaises(ModelDenied):
            router.snapshot('simulation-default@2')

    async def test_f2_terminal_cleanup_crash_is_reconcilable(self):
        # A terminal outcome persisted before cleanup is still an occupied execution.
        snapshot = self.c.models.snapshot()
        task = self.c.store.submit({'kind':'simulation','write_set':['crash']},snapshot)
        tid = task['id']
        self.c.store.claim(tid,cleanup_required=True)
        peer = Supervisor([sys.executable,'-m','bba_controller.sim_peer'], self.repo,
                          self.root/'crash-peer', lambda event:self.c._record(tid,event), simulation=True)
        self.c.store.transition(tid,2,'SUCCEEDED')
        self.assertEqual(self.c.store.slot_count(),1)
        await self.c.close()
        self.c = Controller(Settings(self.root/'state',self.repo,fixture_authorized=True,slots=2))
        self.addAsyncCleanup(self.c.close)
        try:
            result = await self.c.call('bba_task_resume',{'task_id':tid,'simulation_receipt':'SUCCEEDED'})
            self.assertFalse(result['ok'])
            peer.close()
            result = await self.c.call('bba_task_resume',{'task_id':tid,'simulation_receipt':'SUCCEEDED'})
            self.assertTrue(result['ok'],result)
            self.assertEqual(self.c.store.slot_count(),0)
        finally:
            peer.close()

    async def test_f2_cleanup_failure_restart_retains_barrier_until_peer_exits(self):
        original = Supervisor.close
        def failed(peer):
            raise OSError('injected cleanup failure')
        with patch.object(Supervisor, 'close', failed):
            tid = await self.submit('hang')
            await self.until(lambda: tid not in self.c.running and tid in self.c.peers)
            peer = self.c.peers[tid]
            self.assertIsNone(peer.proc.poll())
            self.assertTrue(self.c.store.cleanup_pending(tid))
        # Release this owner without pretending cleanup succeeded (crash boundary).
        self.c.scheduler.cancel()
        await asyncio.gather(self.c.scheduler, return_exceptions=True)
        self.c.closed = True
        self.c.owner.close()
        self.c = Controller(Settings(self.root/'state', self.repo, fixture_authorized=True, slots=2))
        self.addAsyncCleanup(self.c.close)
        result = await self.c.call('bba_task_resume', {'task_id':tid,'simulation_receipt':'CANCELLED'})
        self.assertFalse(result['ok'], result)
        self.assertEqual(self.c.store.slot_count(), 1)
        original(peer)
        result = await self.c.call('bba_task_resume', {'task_id':tid,'simulation_receipt':'CANCELLED'})
        self.assertTrue(result['ok'], result)
        self.assertEqual(self.c.store.slot_count(), 0)

    async def test_f3_supervisor_record_limit_keeps_cleanup_audit(self):
        events = []
        peer = Supervisor([sys.executable,'-m','bba_controller.sim_peer'], self.repo,
                          self.root/'record-limit', events.append, simulation=True)
        try:
            peer.event_count = 1000
            peer.close()
            self.assertIn('cleanup',[event['kind'] for event in events])
        finally:
            peer.close()

    async def test_f3_constructor_cleanup_failure_keeps_durable_ownership(self):
        original_record = self.c.store.process_started
        spawned = []
        original_spawn = subprocess.Popen
        def spawn(*args, **kwargs):
            proc = original_spawn(*args, **kwargs)
            if '-m' in args[0] and 'bba_controller.sim_peer' in args[0]:
                spawned.append(proc)
            return proc
        def broken_record(*args):
            raise Conflict('injected constructor audit failure')
        def broken_close(peer):
            raise OSError('injected cleanup failure')
        try:
            with patch.object(self.c.store,'process_started',broken_record), patch.object(Supervisor,'close',broken_close), patch('bba_controller.acp.subprocess.Popen',spawn):
                tid = await self.submit()
                await self.until(lambda: spawned and tid not in self.c.running)
                self.assertTrue(self.c.store.cleanup_pending(tid))
                self.assertEqual(self.c.store.slot_count(),1)
                self.assertIn(tid,self.c.peers)
        finally:
            for proc in spawned:
                if proc.poll() is None:
                    import os,signal
                    os.killpg(proc.pid,signal.SIGKILL)
                    proc.wait()
                for stream in (proc.stdin,proc.stdout,proc.stderr):
                    stream.close()

    async def test_f3_safety_channel_bounded_visible_and_still_recovers(self):
        tid = await self.submit('hang')
        await self.until(lambda: tid not in self.c.running and self.c.store.get(tid)['state']=='UNKNOWN')
        for i in range(160):
            self.c.store.event(tid, 'audit_error', {'ordinal':i}, critical=True)
        with self.c.store.connection() as db:
            count = db.execute('SELECT count(*) FROM task_events WHERE task_id=? AND critical=1',(tid,)).fetchone()[0]
        self.assertLessEqual(count,128)
        latest = self.c.store.events(tid,limit=3000)[-1]
        self.assertGreater(latest['data'].get('safety_events_dropped',0),0)
        result = await self.c.call('bba_task_resume', {'task_id':tid,'simulation_receipt':'CANCELLED'})
        self.assertTrue(result['ok'], result)
        self.assertEqual(self.c.store.slot_count(),0)

    async def test_f3_close_audit_failure_does_not_skip_other_peers(self):
        first = await self.submit('hang', ['one'])
        second = await self.submit('hang', ['two'])
        await self.until(lambda: len(self.c.peers)==2)
        peers = list(self.c.peers.values())
        def broken(event):
            raise Conflict('injected close audit failure')
        peers[0].record_callback = broken
        try:
            await self.c.close()
        except Conflict:
            pass
        try:
            self.assertTrue(all(p.proc.poll() is not None for p in peers), 'close skipped a live peer')
        finally:
            for peer in peers:
                peer.record_callback = lambda e: None
                peer.close()

    async def test_f3_constructor_and_close_audit_failures_reap_process(self):
        spawned = []
        original = subprocess.Popen
        def spawn(*args, **kwargs):
            proc = original(*args, **kwargs)
            spawned.append(proc)
            return proc
        def broken(event):
            raise Conflict('injected audit failure')
        with patch('bba_controller.acp.subprocess.Popen', spawn):
            try:
                with self.assertRaises(Conflict):
                    Supervisor([sys.executable,'-m','bba_controller.sim_peer'], self.repo,
                               self.root/'broken', broken, simulation=True)
                self.assertIsNotNone(spawned[-1].poll(), 'constructor leaked peer')
                peer = Supervisor([sys.executable,'-m','bba_controller.sim_peer'], self.repo,
                                  self.root/'close', lambda e: None, simulation=True)
                peer.record_callback = broken
                with self.assertRaises(Conflict):
                    peer.close()
                self.assertIsNotNone(peer.proc.poll(), 'close skipped kill after audit failure')
            finally:
                for proc in spawned:
                    if proc.poll() is None:
                        import os, signal
                        os.killpg(proc.pid, signal.SIGKILL)
                        proc.wait()

    async def test_f3_public_resume_quota_never_spawns_after_exhaustion(self):
        tid = await self.submit('hang')
        await self.until(lambda: tid not in self.c.running and self.c.store.get(tid)['state']=='UNKNOWN')
        spawned = []
        original = subprocess.Popen
        def spawn(*args, **kwargs):
            proc = original(*args, **kwargs)
            spawned.append(proc)
            return proc
        try:
            with patch('bba_controller.acp.subprocess.Popen', spawn):
                for _ in range(400):
                    before = len(spawned)
                    result = await self.c.call('bba_task_resume', {'task_id':tid})
                    if not result['ok']:
                        self.assertEqual(len(spawned), before, 'quota rejection spawned peer')
                        break
                else:
                    self.fail('ordinary event quota was removed')
            self.assertTrue(all(p.poll() is not None for p in spawned))
            receipt = await self.c.call('bba_task_resume', {'task_id':tid,'simulation_receipt':'CANCELLED'})
            self.assertTrue(receipt['ok'], receipt)
            self.assertEqual(self.c.store.slot_count(), 0)
        finally:
            for proc in spawned:
                if proc.poll() is None:
                    import os, signal
                    os.killpg(proc.pid, signal.SIGKILL)
                    proc.wait()

    async def test_f3_full_quota_timeout_and_restart_recover_unknown(self):
        tid = await self.submit('hang')
        await self.until(lambda: any(e['kind']=='rpc_send' and e['data'].get('method')=='session/prompt'
                                    for e in self.c.store.events(tid, limit=3000)))
        while True:
            try:
                self.c.store.event(tid, 'note', {})
            except Conflict:
                break
        await self.until(lambda: tid not in self.c.running)
        self.assertEqual(self.c.store.get(tid)['state'], 'UNKNOWN')
        self.assertEqual(self.c.models.evidence(tid)['calls'][0]['outcome'], 'UNKNOWN')
        self.assertEqual(self.c.store.slot_count(), 1)
        self.assertFalse(self.c.store.cleanup_pending(tid))
        await self.c.close()
        # Explicit crash-boundary injection: STARTED + RUNNING at ordinary quota.
        with self.c.store.tx() as db:
            db.execute("UPDATE tasks SET state='RUNNING' WHERE id=?", (tid,))
            db.execute("UPDATE model_call_events SET outcome='STARTED' WHERE task_id=?", (tid,))
        self.c = Controller(Settings(self.root/'state', self.repo, fixture_authorized=True, slots=2))
        self.addAsyncCleanup(self.c.close)
        self.assertEqual(self.c.store.get(tid)['state'], 'UNKNOWN')
        self.assertEqual(self.c.models.evidence(tid)['calls'][0]['outcome'], 'UNKNOWN')
        receipt = await self.c.call('bba_task_resume', {'task_id':tid,'simulation_receipt':'CANCELLED'})
        self.assertTrue(receipt['ok'], receipt)
        self.assertEqual(self.c.store.slot_count(), 0)

    async def test_f2_cleanup_barrier_normal_and_receipt(self):
        original = Supervisor.close
        for behavior in ('success', 'hang'):
            entered, release = threading.Event(), threading.Event()
            captured = []
            def gated(peer):
                if not entered.is_set():
                    captured.append(peer)
                    entered.set()
                    if not release.wait(5):
                        raise RuntimeError('test cleanup gate timed out')
                return original(peer)
            with patch.object(Supervisor, 'close', gated):
                try:
                    tid = await self.submit(behavior, ['barrier'])
                    await self.until(entered.is_set)
                    self.assertIsNone(captured[0].proc.poll())
                    self.assertEqual(self.c.store.slot_count(), 1)
                    waiting = await self.c.call('bba_task_wait', {'task_id':tid,'timeout_ms':0})
                    self.assertTrue(waiting['data']['timed_out'])
                    # A fresh Store sees the durable barrier as well.
                    other_store = Store(self.c.store.path, slots=2)
                    tail = await self.submit(writes=['barrier'])
                    dependent = await self.submit(writes=['independent'], dependencies=[tid])
                    self.assertFalse(other_store.claim(tail))
                    self.assertFalse(other_store.claim(dependent))
                    if behavior == 'hang':
                        receipt = await self.c.call('bba_task_resume', {'task_id':tid,'simulation_receipt':'CANCELLED'})
                        self.assertFalse(receipt['ok'], receipt)
                        self.assertEqual(self.c.store.get(tid)['state'], 'UNKNOWN')
                    await asyncio.sleep(.05)
                    self.assertEqual(self.c.store.get(tail)['state'], 'QUEUED')
                finally:
                    release.set()
                await self.until(lambda: tid not in self.c.running)
                self.assertIsNotNone(captured[0].proc.poll())
                if behavior == 'hang':
                    receipt = await self.c.call('bba_task_resume', {'task_id':tid,'simulation_receipt':'CANCELLED'})
                    self.assertTrue(receipt['ok'], receipt)
                await self.until(lambda: self.c.store.get(tail)['state']=='SUCCEEDED')
                if behavior == 'success':
                    await self.until(lambda: self.c.store.get(dependent)['state']=='SUCCEEDED')
                else:
                    await self.c.call('bba_task_cancel', {'task_id':dependent})
