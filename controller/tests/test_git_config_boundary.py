import tempfile
from pathlib import Path
import unittest
from test_worktrees import fixture, git
from bba_controller.worktrees import Worktrees, BoundaryError

class GitConfigBoundaryTests(unittest.TestCase):
    def test_executable_and_redirect_config_rejected_before_effects(self):
        for key in ('remote.origin.uploadpack','core.fsmonitor','filter.evil.smudge',
                    'filter.evil.process','url.https://example.invalid/.insteadOf',
                    'include.path','core.sshCommand','core.worktree'):
            with self.subTest(key=key), tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp); repo,sha=fixture(root)
                marker=root/'outside-marker'
                script=root/'extra-command'
                script.write_text(f'#!/bin/sh\nprintf executed > {marker}\nexec /usr/bin/git-upload-pack "$@"\n')
                script.chmod(0o700)
                git(repo,'config',key,str(script))
                with self.assertRaises(BoundaryError):
                    manager=Worktrees(repo,root/'work',True)
                    manager.create('a'*32,sha)
                self.assertFalse(marker.exists())
                self.assertFalse((root/'work').exists())

    def test_origin_repository_config_is_checked(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);repo,sha=fixture(root)
            git(root/'remote.git','config','uploadpack.packObjectsHook','touch marker')
            with self.assertRaises(BoundaryError):
                Worktrees(repo,root/'work',True).create('b'*32,sha)
            self.assertFalse((root/'work').exists())
