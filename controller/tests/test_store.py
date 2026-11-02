import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import threading
import unittest

from bba_controller.store import Store, Conflict


class StoreTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = Path(self.tmp.name) / 'state.db'
        self.s = Store(self.db, slots=2)

    def task(self, paths=None, deps=None):
        return self.s.submit({'kind': 'simulation', 'write_set': paths, 'dependencies': deps or []})

    def test_atomic_events_cas_and_duplicate_content(self):
        t = self.task(['src'])
        self.assertEqual(self.s.get(t['id'])['version'], 1)
        self.s.event(t['id'], 'note', {'same': True})
        self.s.event(t['id'], 'note', {'same': True})
        self.assertEqual(len(self.s.events(t['id'])), 3)
        self.assertEqual(len({e['id'] for e in self.s.events(t['id'])}), 3)
        with self.assertRaises(Conflict):
            self.s.transition(t['id'], 9, 'RUNNING')
        self.assertEqual(len(self.s.events(t['id'])), 3)
        with self.s.connection() as c:
            self.assertEqual(c.execute('pragma journal_mode').fetchone()[0], 'wal')

    def test_transactional_slots_parent_paths_unknown_and_dependencies(self):
        a, b, c = self.task(['src']), self.task(['src/a']), self.task(['docs'])
        self.assertTrue(self.s.claim(a['id']))
        self.assertFalse(self.s.claim(b['id']))
        self.assertTrue(self.s.claim(c['id']))
        self.assertEqual(self.s.slot_count(), 2)
        self.s.transition(a['id'], 2, 'UNKNOWN')
        self.assertEqual(self.s.slot_count(), 2)
        self.s.reconcile(a['id'], 'CANCELLED', 'simulation authoritative receipt')
        self.assertEqual(self.s.slot_count(), 1)
        self.assertTrue(self.s.claim(b['id']))
        d = self.task(['other'], [c['id']])
        self.assertFalse(self.s.claim(d['id']))
        self.s.transition(c['id'], 2, 'SUCCEEDED')
        self.s.transition(b['id'], 2, 'SUCCEEDED')
        self.assertTrue(self.s.claim(d['id']))

    def test_unknown_write_set_serializes(self):
        a, b = self.task(None), self.task(['docs'])
        self.assertTrue(self.s.claim(a['id']))
        self.assertFalse(self.s.claim(b['id']))

    def test_concurrent_sqlite_claims_global_limit(self):
        ids = [self.task([f'p{i}'])['id'] for i in range(12)]
        results = []
        threads = [threading.Thread(target=lambda i=i: results.append(Store(self.db, slots=2).claim(i))) for i in ids]
        for t in threads: t.start()
        for t in threads: t.join()
        self.assertEqual(sum(results), 2)
        self.assertEqual(self.s.slot_count(), 2)

    def test_restart_preserves_unknown_slot_and_session(self):
        t = self.task(['a'])
        self.s.claim(t['id'])
        self.s.session(t['id'], 'sim-session', 'simulation')
        restarted = Store(self.db, slots=2)
        restarted.recover()
        self.assertEqual(restarted.get(t['id'])['state'], 'UNKNOWN')
        self.assertEqual(restarted.slot_count(), 1)
        self.assertEqual(restarted.latest_session(t['id'])['session_id'], 'sim-session')
        self.assertFalse(restarted.claim(t['id']))

    def test_subprocess_crash_rolls_back_projection_and_event(self):
        t = self.task(['a'])
        code = "import sqlite3,os,sys;c=sqlite3.connect(sys.argv[1]);c.execute('BEGIN IMMEDIATE');c.execute(\"UPDATE tasks SET state='RUNNING' WHERE id=?\",(sys.argv[2],));os._exit(23)"
        p = subprocess.run([sys.executable, '-c', code, str(self.db), t['id']], check=False)
        self.assertEqual(p.returncode, 23)
        self.assertEqual(Store(self.db, slots=2).get(t['id'])['state'], 'QUEUED')
        self.assertEqual(len(self.s.events(t['id'])), 1)

    def test_rework_bound_and_evidence_redaction(self):
        t = self.task(['a'])
        for n in range(3):
            self.s.rework(t['id'], 'failed test', 'change', 'failed')
        self.assertEqual(self.s.get(t['id'])['state'], 'PAUSED')
        with self.assertRaises(Conflict):
            self.s.rework(t['id'], 'again', 'change', 'failed')
        self.s.artifact(t['id'], 'test', {'api_key':'secret-value', 'text':'Authorization: Bearer abc.def token=hidden password=hush'})
        raw = json.dumps(self.s.evidence(t['id']))
        for value in ('secret-value','abc.def','hidden','hush'):
            self.assertNotIn(value, raw)


if __name__ == '__main__':
    unittest.main()
