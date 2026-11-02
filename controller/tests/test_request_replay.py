import concurrent.futures
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from bba_controller.store import Store, Conflict

class RequestReplayTests(unittest.TestCase):
    def test_duplicate_conflict_and_distinct_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            s=Store(Path(tmp)/'state.db')
            spec={'kind':'simulation','request_id':'request-one','write_set':['src']}
            a=s.submit(spec)
            self.assertEqual(s.submit(dict(spec))['id'],a['id'])
            with self.assertRaises(Conflict): s.submit({**spec,'write_set':['other']})
            self.assertNotEqual(s.submit({**spec,'request_id':'request-two'})['id'],a['id'])
            self.assertEqual(len(s.list()),2)

    def test_concurrent_replay_is_one_task(self):
        with tempfile.TemporaryDirectory() as tmp:
            db=Path(tmp)/'state.db';s=Store(db)
            spec={'kind':'simulation','request_id':'concurrent'}
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                ids=list(pool.map(lambda _:s.submit(spec)['id'],range(16)))
            self.assertEqual(len(set(ids)),1)
            self.assertEqual(len(s.list()),1)

    def test_commit_then_crash_loses_response_but_not_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            db=Path(tmp)/'state.db'
            code='from bba_controller.store import Store; import os,sys; Store(sys.argv[1]).submit({"kind":"simulation","request_id":"lost-response"});os._exit(23)'
            result=subprocess.run([sys.executable,'-c',code,str(db)])
            self.assertEqual(result.returncode,23)
            s=Store(db); original=s.list()[0]
            self.assertEqual(s.submit({'kind':'simulation','request_id':'lost-response'})['id'],original['id'])
            self.assertEqual(len(s.events(original['id'])),1)
