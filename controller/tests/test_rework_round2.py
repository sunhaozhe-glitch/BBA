"""ROUND2 fault regressions: real fixture processes, injected storage failures."""
import asyncio
from pathlib import Path
import sqlite3
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

from bba_controller.acp import Supervisor
from bba_controller.controller import Controller, Settings
from test_worktrees import fixture


class Round2Tests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.repo, self.sha = fixture(self.root)
        self.c = Controller(Settings(self.root/'state', self.repo, fixture_authorized=True, slots=2))
        self.addAsyncCleanup(self.c.close)

    def owned_peers(self):
        peers = []
        for i in range(2):
            tid = self.c.store.submit({'kind':'simulation', 'base_sha':self.sha,
                                      'write_set':[str(i)]}, self.c.models.snapshot())['id']
            self.assertTrue(self.c.store.claim(tid, cleanup_required=True))
            peer = Supervisor([sys.executable, '-m', 'bba_controller.sim_peer'], self.repo,
                              self.root/str(i), lambda event, tid=tid:self.c._record(tid,event), simulation=True)
            self.c.peers[tid] = peer
            peers.append(peer)
        return peers

    async def submit(self, **extra):
        response = await self.c.call('bba_task_submit', {'kind':'simulation', 'base_sha':self.sha,
                                     'write_set':['task'], **extra})
        self.assertTrue(response['ok'], response)
        return response['data']['task']['id']

    async def until(self, predicate):
        async with asyncio.timeout(5):
            while not predicate():
                await asyncio.sleep(.01)

    def fill(self, tid):
        from bba_controller.store import Conflict
        while True:
            try:
                self.c.store.event(tid, 'fixture-quota-fill', {})
            except Conflict:
                return

    async def test_r1_2_claim_quota_pauses_only_bad_task_without_retry_storm(self):
        tid = await self.submit()
        self.fill(tid)
        tail = await self.submit(write_set=['unrelated'])
        await self.until(lambda: self.c.scheduler.done() or
                         (self.c.store.get(tail)['state']=='SUCCEEDED' and tail not in self.c.running))
        self.assertFalse(self.c.scheduler.done(), 'single claim failure killed scheduler')
        self.assertEqual(self.c.store.get(tid)['state'], 'PAUSED')
        self.assertEqual(self.c.store.get(tail)['state'], 'SUCCEEDED')
        events = self.c.store.events(tid, limit=3000)
        diagnostic = [e for e in events if e['kind']=='state' and e['data'].get('to')=='PAUSED']
        self.assertEqual(len(diagnostic), 1)
        self.assertEqual(diagnostic[0]['data']['reason'], 'claim failed')
        self.assertEqual(diagnostic[0]['data']['error'], 'Conflict')
        await asyncio.sleep(.08)
        self.assertEqual(len(self.c.store.events(tid,limit=3000)), len(events))
        self.assertEqual(self.c.store.slot_count(), 0)

    async def test_r1_2_unexpected_claim_error_is_visible_and_isolated(self):
        tid = await self.submit()
        tail = await self.submit(write_set=['unrelated'])
        original = self.c.store.claim
        def fail(task, **kwargs):
            if task == tid:
                raise sqlite3.OperationalError('injected task claim fault')
            return original(task, **kwargs)
        with patch.object(self.c.store, 'claim', fail):
            await self.until(lambda:self.c.scheduler.done() or self.c.store.get(tail)['state']=='SUCCEEDED')
            self.assertFalse(self.c.scheduler.done())
            self.assertEqual(self.c.store.get(tid)['state'], 'PAUSED')
            events = self.c.store.events(tid)
            self.assertEqual(events[-1]['data']['error'], 'OperationalError')

    async def test_r1_3_success_receipt_and_acceptance_survive_full_ordinary_quota(self):
        got, release = threading.Event(), threading.Event()
        original = Supervisor.prompt
        observed = []
        def gate(peer, *args):
            result = original(peer, *args)
            observed.append(result)
            got.set()
            if not release.wait(30):
                raise RuntimeError('receipt gate timed out')
            return result
        try:
            with patch.object(Supervisor, 'prompt', gate):
                tid = await self.submit()
                await self.until(got.is_set)
                self.fill(tid)
                release.set()
                await self.until(lambda:tid not in self.c.running)
            self.assertEqual(observed[0]['stopReason'], 'end_turn')
            self.assertEqual(self.c.store.get(tid)['state'], 'SUCCEEDED')
            artifacts = self.c.store.evidence(tid)['artifacts']
            acceptance = [a['data'] for a in artifacts if a['kind']=='acceptance']
            self.assertEqual(len(acceptance), 1)
            self.assertEqual(acceptance[0]['receipt']['stopReason'], 'end_turn')
            self.assertEqual(acceptance[0]['result'], 'SUCCEEDED')
            model = self.c.models.evidence(tid)
            self.assertEqual(model['calls'][0]['outcome'], 'SUCCEEDED')
            self.assertEqual(model['budget']['reserved_units'], 0)
            self.assertEqual(model['budget']['spent_units'], 1)
            self.assertEqual(self.c.store.slot_count(), 0)
            self.assertFalse(self.c.store.cleanup_pending(tid))
            with self.c.store.connection() as db:
                self.assertEqual(db.execute('SELECT count(*) FROM task_events WHERE task_id=? AND critical=0',(tid,)).fetchone()[0], 2000)
        finally:
            release.set()

    async def test_r1_3_model_success_with_acceptance_failure_stays_unknown_with_receipt(self):
        original = Supervisor.prompt
        def exhaust_artifacts(peer, *args):
            result = original(peer, *args)
            tid = next(t for t,p in self.c.peers.items() if p is peer)
            for i in range(99):
                self.c.store.artifact(tid, 'fixture-note', {'i':i})
            return result
        with patch.object(Supervisor, 'prompt', exhaust_artifacts):
            tid = await self.submit()
            await self.until(lambda:self.c.store.get(tid)['state']=='UNKNOWN' and tid not in self.c.running)
        self.assertEqual(self.c.models.evidence(tid)['calls'][0]['outcome'], 'SUCCEEDED')
        evidence = self.c.store.evidence(tid,limit=1000)
        self.assertEqual(len(evidence['artifacts']), 100)
        self.assertFalse(any(a['kind']=='acceptance' for a in evidence['artifacts']))
        receipt = [e for e in evidence['events'] if e['kind']=='prompt_receipt']
        self.assertEqual(receipt[0]['data']['stopReason'], 'end_turn')
        self.assertEqual(self.c.store.slot_count(), 1)
        self.assertFalse(self.c.store.cleanup_pending(tid))

    @staticmethod
    def readonly(*args, **kwargs):
        raise sqlite3.OperationalError('attempt to write a readonly database')

    async def test_r1_1_persistent_audit_failure_reaps_all_peers_and_joins_workers(self):
        peers = self.owned_peers()
        async def worker(peer):
            await asyncio.to_thread(peer.proc.wait)
            raise RuntimeError('injected worker finalization failure')
        workers = [asyncio.create_task(worker(p)) for p in peers]
        for i,w in enumerate(workers):
            key = f'wait{i}'
            self.c.running[key] = w
            w.add_done_callback(lambda _,key=key:self.c.running.pop(key,None))
        try:
            with patch.object(self.c.store, 'event', self.readonly):
                with self.assertRaises(Exception) as raised:
                    await self.c.close()
                errors = getattr(raised.exception, 'cleanup_errors')
                self.assertGreaterEqual(len(errors), 4)
                self.assertEqual(sum(isinstance(e, RuntimeError) for e in errors), 2)
                self.assertEqual([p.proc.poll() is None for p in peers], [False,False])
                self.assertTrue(all(w.done() for w in workers))
                self.assertTrue(self.c.owner.closed)
                self.assertEqual(self.c.store.slot_count(), 2)
                self.assertTrue(all(self.c.store.cleanup_pending(t) for t in self.c.peers))
                self.assertFalse(any(e['kind']=='cleanup_confirmed'
                                     for t in self.c.peers for e in self.c.store.events(t)))
        finally:
            for p in peers:
                p.record_callback = lambda event:None
                p.close()
            await asyncio.gather(*workers, return_exceptions=True)

    async def test_r1_1_double_failure_retains_owner_until_retry_reaps_live_peer(self):
        peers = self.owned_peers()
        original = Supervisor.close
        def broken(peer):
            if peer is peers[0]:
                raise OSError('injected kill/wait failure')
            return original(peer)
        try:
            with patch.object(Supervisor, 'close', broken), patch.object(self.c.store, 'event', self.readonly):
                with self.assertRaises(Exception):
                    await self.c.close()
                self.assertEqual([p.proc.poll() is None for p in peers], [True,False])
                self.assertFalse(self.c.owner.closed, 'live resource must keep its owner')
                self.assertEqual(self.c.store.slot_count(), 2)
            await self.c.close()
            self.assertEqual([p.proc.poll() is None for p in peers], [False,False])
            self.assertTrue(self.c.owner.closed)
            await self.c.close()
        finally:
            for p in peers:
                p.record_callback = lambda event:None
                original(p)
