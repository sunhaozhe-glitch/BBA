import concurrent.futures
import json
from pathlib import Path
import sys
import tempfile
import time
import unittest

from bba_controller.acp import Supervisor, TransportUnknown, omp_command


class ACPTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.events = []

    def peer(self, behavior='success'):
        s = Supervisor([sys.executable, '-m', 'bba_controller.sim_peer', '--behavior', behavior], self.root, self.root/'agent', self.events.append, simulation=True)
        self.addCleanup(s.close)
        return s

    def test_session_lifecycle_notification_permission_and_redacted_stderr(self):
        s = self.peer()
        self.assertEqual(s.initialize()['protocolVersion'], 1)
        sid = s.new_session()
        result = s.prompt(sid, 'fixture simulation')
        self.assertEqual(result['stopReason'], 'end_turn')
        self.assertTrue(any(x['kind'] == 'permission_denied' for x in self.events))
        self.assertEqual(sum(x['kind'] == 'notification' for x in self.events), 2)
        s.close()
        raw = json.dumps(self.events)
        self.assertNotIn('fake-stderr-secret', raw)
        self.assertTrue(any(x['kind'] == 'cleanup' for x in self.events))
        t = self.peer()
        t.initialize()
        self.assertEqual(t.restore(sid, 'load')['sessionId'], sid)
        self.assertEqual(t.restore(sid, 'resume')['sessionId'], sid)

    def test_out_of_order_rpc_correlation(self):
        s = self.peer('reverse')
        a = s.send_request('echo', {'value': 'first'})
        b = s.send_request('echo', {'value': 'second'})
        self.assertEqual(s.wait(b, 2)['value'], 'second')
        self.assertEqual(s.wait(a, 2)['value'], 'first')

    def test_timeout_exit_malformed_overflow_fail_pending_unknown(self):
        for behavior in ('hang', 'exit', 'malformed', 'overflow'):
            with self.subTest(behavior=behavior):
                s = self.peer(behavior)
                s.initialize()
                sid = s.new_session()
                with self.assertRaises(TransportUnknown): s.prompt(sid, 'fixture', timeout=.15)
                s.close()
                self.assertFalse(s.pending)
                self.assertIsNotNone(s.proc.poll())

    def test_cancel_requires_authoritative_prompt_receipt(self):
        s = self.peer('cancel')
        s.initialize()
        sid = s.new_session()
        request = s.send_request('session/prompt', {'sessionId': sid, 'prompt': [{'type':'text','text':'fixture'}]})
        s.cancel(sid)
        self.assertEqual(s.wait(request, 2)['stopReason'], 'cancelled')

    def test_real_process_entry_and_prompt_fail_closed(self):
        cmd = omp_command('/usr/bin/omp')
        self.assertEqual(cmd[:2], ['/usr/bin/omp', 'acp'])
        self.assertNotIn('--session-dir', cmd)
        with self.assertRaises(PermissionError):
            Supervisor(cmd, self.root, self.root/'state', self.events.append)
        s = self.peer()
        s.simulation = False
        with self.assertRaises(PermissionError): s.prompt('any', 'real prompt')
