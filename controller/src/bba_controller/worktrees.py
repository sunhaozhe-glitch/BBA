"""Pinned-repository worktrees. Writes require startup fixture authority."""
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import threading
import selectors
import signal
import time


class BoundaryError(Exception):
    pass


def run_bounded(argv, env, timeout=20):
    proc = subprocess.Popen(argv, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    selector = selectors.DefaultSelector()
    output = bytearray()
    total = 0
    deadline = time.monotonic() + timeout
    try:
        selector.register(proc.stdout, selectors.EVENT_READ)
        selector.register(proc.stderr, selectors.EVENT_READ)
        while selector.get_map():
            remaining = deadline-time.monotonic()
            if remaining <= 0:
                raise BoundaryError('git timeout')
            for key, _ in selector.select(min(.1, remaining)):
                chunk = os.read(key.fileobj.fileno(), 4096)
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                total += len(chunk)
                if total > 65536:
                    raise BoundaryError('git output limit')
                if key.fileobj is proc.stdout:
                    output.extend(chunk)
        if proc.wait(timeout=max(.01, deadline-time.monotonic())):
            raise BoundaryError('git operation refused or failed')
        return output.decode('utf-8').strip()
    except (OSError, subprocess.TimeoutExpired) as error:
        raise BoundaryError('git unavailable or timed out') from error
    finally:
        selector.close()
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        proc.wait()
        proc.stdout.close()
        proc.stderr.close()


def no_symlinks(path):
    p = Path(path).absolute()
    if any(part.is_symlink() for part in [p, *p.parents]):
        raise BoundaryError('symlink path refused')
    return p


class Worktrees:
    def __init__(self, repo, root, fixture_authorized=False):
        self.repo = no_symlinks(repo)
        self.root = no_symlinks(root)
        self.fixture_authorized = fixture_authorized
        self.lock = threading.Lock()
        self.env = {'PATH': os.defpath, 'LANG': 'C', 'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': os.devnull, 'GIT_TERMINAL_PROMPT': '0'}
        if self.git('rev-parse', '--show-toplevel') != str(self.repo):
            raise BoundaryError('repository must be exact top-level')
        if self.root == self.repo or self.repo in self.root.parents:
            raise BoundaryError('worktrees must be outside repository')

    def git(self, *args):
        return run_bounded(['git', '-c', 'core.hooksPath=/dev/null', '-c', 'protocol.file.allow=always', '-C', str(self.repo), *args], self.env)

    def validate_write_set(self, paths):
        if paths is None:
            return None
        for value in paths:
            if not isinstance(value, str) or not value or len(value) > 256:
                raise BoundaryError('invalid path')
            if value.startswith(('/', '-')) or '\\' in value or any(ord(x) < 32 for x in value):
                raise BoundaryError('unsafe path')
            if any(x in ('', '.', '..', '.git') for x in value.split('/')):
                raise BoundaryError('non-canonical path')
            if not re.fullmatch(r'[A-Za-z0-9_./-]+', value):
                raise BoundaryError('unsafe path characters')
            no_symlinks(self.repo / PurePosixPath(value))
        return paths

    def create(self, task, base):
        with self.lock:
            return self._create(task, base)

    def _create(self, task, base):
        if not self.fixture_authorized:
            raise BoundaryError('OBSERVE repository is read-only; fixture authority absent')
        if not re.fullmatch(r'[a-f0-9]{32}', task) or not re.fullmatch(r'[a-f0-9]{40}', base):
            raise BoundaryError('invalid task or base SHA')
        no_symlinks(self.root)
        if self.git('status', '--porcelain', '--untracked-files=all'):
            raise BoundaryError('dirty repository protected')
        # In phase 1, fixture remotes are local exact repositories only. No network/credentials.
        remote = self.git('remote', 'get-url', 'origin')
        if not os.path.isabs(remote):
            raise BoundaryError('fixture origin must be local absolute path')
        no_symlinks(remote)
        self.git('fetch', '--no-tags', 'origin', '+refs/heads/main:refs/remotes/origin/main')
        self.git('merge-base', '--is-ancestor', base, 'refs/remotes/origin/main')
        path = self.root / task
        if path.exists() or path.is_symlink():
            raise BoundaryError('existing worktree retained')
        self.root.mkdir(parents=True, exist_ok=True)
        self.git('worktree', 'add', '-b', f'controller/{task}', str(path), base)
        return path

    def retain(self, path):
        path = no_symlinks(path)
        if path.parent != self.root:
            raise BoundaryError('worktree outside configured root')
        return {'retained': True, 'reason': 'user data is never automatically removed'}
