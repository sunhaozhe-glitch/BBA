from pathlib import Path
import subprocess
import tempfile
import unittest

from bba_controller.worktrees import Worktrees, BoundaryError


def git(cwd, *args):
    return subprocess.check_output(['git', '-C', str(cwd), *args], text=True).strip()


def fixture(root):
    repo = root / 'fixture'
    repo.mkdir()
    git(repo, 'init', '-b', 'main')
    git(repo, 'config', 'user.email', 'fixture@example.invalid')
    git(repo, 'config', 'user.name', 'Fixture')
    (repo / 'README').write_text('fixture\n')
    git(repo, 'add', 'README')
    git(repo, 'commit', '-m', 'fixture base')
    remote = root / 'remote.git'
    subprocess.check_call(['git', 'clone', '--bare', str(repo), str(remote)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    git(repo, 'remote', 'add', 'origin', str(remote))
    return repo, git(repo, 'rev-parse', 'HEAD')


class WorktreeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.repo, self.sha = fixture(self.root)
        self.m = Worktrees(self.repo, self.root / 'work', fixture_authorized=True)

    def test_two_worktrees_base_branch_state_isolation_and_retain(self):
        a = self.m.create('a' * 32, self.sha)
        b = self.m.create('b' * 32, self.sha)
        self.assertEqual(git(a, 'rev-parse', 'HEAD'), self.sha)
        self.assertNotEqual(git(a, 'branch', '--show-current'), git(b, 'branch', '--show-current'))
        (a / 'only-a').write_text('preserve')
        self.assertFalse((b / 'only-a').exists())
        self.m.retain(a)
        self.assertTrue((a / 'only-a').exists())
        self.assertEqual(git(self.repo, 'status', '--porcelain'), '')

    def test_reject_dirty_tree_injection_symlink_and_wrong_repo(self):
        (self.repo / 'untracked').write_text('user data')
        with self.assertRaises(BoundaryError): self.m.create('a' * 32, self.sha)
        (self.repo / 'untracked').unlink()
        for token in ('../escape', '--upload-pack=x', 'a;touch x'):
            with self.assertRaises(BoundaryError): self.m.create(token, self.sha)
            with self.assertRaises(BoundaryError): self.m.create('c' * 32, token)
        (self.repo / 'link').symlink_to(self.root)
        with self.assertRaises(BoundaryError): self.m.validate_write_set(['link/out'])
        for path in ('../x', '/x', '-x', 'a/../b', 'a//b', '.'):
            with self.assertRaises(BoundaryError): self.m.validate_write_set([path])
        with self.assertRaises(BoundaryError): Worktrees(self.root, self.root / 'other', fixture_authorized=True)
        with self.assertRaises(BoundaryError): Worktrees(self.repo, self.root / 'other').create('c'*32, self.sha)

    def test_unrelated_base_and_duplicate_task_fail_closed(self):
        a = self.m.create('a' * 32, self.sha)
        with self.assertRaises(BoundaryError): self.m.create('a' * 32, self.sha)
        self.assertTrue(a.exists())
        with self.assertRaises(BoundaryError): self.m.create('b' * 32, 'f'*40)
