import asyncio
from pathlib import Path
import tempfile
import unittest

from bba_controller.controller import Controller, Settings
from bba_controller.store import Conflict
from test_worktrees import fixture


class BindingTests(unittest.IsolatedAsyncioTestCase):
    async def test_state_repository_binding_cannot_change_on_restart(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            first = root/'first'
            second = root/'second'
            first.mkdir()
            second.mkdir()
            repo1, _ = fixture(first)
            repo2, _ = fixture(second)
            c = Controller(Settings(root/'state',repo1,fixture_authorized=True))
            await c.close()
            with self.assertRaises(Conflict):
                Controller(Settings(root/'state',repo2,fixture_authorized=True))
