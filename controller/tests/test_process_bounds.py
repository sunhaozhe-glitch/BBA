import os
from pathlib import Path
import sys
import unittest

from bba_controller.worktrees import run_bounded, BoundaryError


class ProcessBoundsTests(unittest.TestCase):
    def test_git_process_helper_caps_output_and_timeout(self):
        result = run_bounded([sys.executable,'-c','print("fixture")'], dict(os.environ), timeout=1)
        self.assertEqual(result, 'fixture')
        for code in ('print("x"*70000)', 'import time;time.sleep(5)'):
            with self.assertRaises(BoundaryError):
                run_bounded([sys.executable,'-c',code], dict(os.environ), timeout=.1)
