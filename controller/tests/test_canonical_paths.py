import tempfile
from pathlib import Path
import unittest
from test_worktrees import fixture
from bba_controller.controller import Settings, Controller
from bba_controller.worktrees import Worktrees, BoundaryError, no_symlinks

class CanonicalPathTests(unittest.TestCase):
    def test_state_dotdot_rejected_before_any_write(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo,_=fixture(root);(root/'jump').mkdir()
            state=root/'jump'/'..'/'fixture'/'controller-state'
            before=set(repo.rglob('*'))
            with self.assertRaises(BoundaryError):
                Controller(Settings(state,repo))
            self.assertEqual(set(repo.rglob('*')),before)
            self.assertFalse((repo/'controller-state').exists())

    def test_worktree_root_dotdot_and_ancestor_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo,_=fixture(root);(root/'jump').mkdir()
            for target in (root/'jump'/'..'/'fixture'/'work',repo,root):
                with self.subTest(target=target), self.assertRaises(BoundaryError):
                    Worktrees(repo,target,True)
            self.assertFalse((repo/'work').exists())

    def test_symlink_before_dotdot_is_not_hidden_by_normalization(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);(root/'real').mkdir();(root/'link').symlink_to(root/'real')
            with self.assertRaises(BoundaryError):
                no_symlinks(root/'link'/'..'/'new')
            self.assertEqual(no_symlinks(root/'real'/'..'/'new'),root/'new')
