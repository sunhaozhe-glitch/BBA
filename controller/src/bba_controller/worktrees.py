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
    # Inspect the original traversal before collapsing '..': a symlink followed
    # by '..' must not disappear from validation.
    p = Path(path).absolute()
    if any(part.is_symlink() for part in [p, *p.parents]):
        raise BoundaryError('symlink path refused')
    try:
        return p.resolve(strict=False)
    except (OSError, RuntimeError) as error:
        raise BoundaryError('unresolvable path') from error


class Worktrees:
    def __init__(self, repo, root, fixture_authorized=False):
        self.repo = no_symlinks(repo)
        self.root = no_symlinks(root)
        self.fixture_authorized = fixture_authorized
        self.lock = threading.Lock()
        self.env = {'PATH': os.defpath, 'LANG': 'C', 'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': os.devnull, 'GIT_TERMINAL_PROMPT': '0'}
        if self.git('rev-parse', '--show-toplevel') != str(self.repo):
            raise BoundaryError('repository must be exact top-level')
        if self.root == self.repo or self.repo in self.root.parents or self.root in self.repo.parents:
            raise BoundaryError('worktrees and repository must be disjoint')

    def _check_config(self, config):
        config = no_symlinks(config)
        if not config.is_file() or config.stat().st_size > 32768:
            raise BoundaryError('missing or oversized repository config')
        raw = run_bounded(['/usr/bin/git', 'config', '--file', str(config),
                           '--no-includes', '--null', '--list'], self.env)
        allowed = {'core.repositoryformatversion', 'core.filemode', 'core.bare',
                   'core.logallrefupdates', 'user.name', 'user.email',
                   'remote.origin.url', 'remote.origin.fetch',
                   'branch.main.remote', 'branch.main.merge'}
        seen = set()
        for record in raw.split('\x00'):
            if not record:
                continue
            key, separator, value = record.partition('\n')
            if not separator or key not in allowed or key in seen:
                raise BoundaryError('repository configuration outside fixture allowlist')
            seen.add(key)
            if key == 'core.repositoryformatversion' and value != '0':
                raise BoundaryError('unsupported repository format')
            if key in {'core.filemode','core.bare','core.logallrefupdates'} and value not in {'true','false'}:
                raise BoundaryError('nonstandard core configuration')
            if key == 'remote.origin.fetch' and value != '+refs/heads/*:refs/remotes/origin/*':
                raise BoundaryError('nonstandard fetch mapping')
            if key == 'branch.main.remote' and value != 'origin':
                raise BoundaryError('nonstandard branch remote')
            if key == 'branch.main.merge' and value != 'refs/heads/main':
                raise BoundaryError('nonstandard branch merge')

    def _check_repository(self, repo, bare=False):
        metadata = no_symlinks(repo if bare else repo/'.git')
        if not metadata.is_dir():
            raise BoundaryError('fixture requires ordinary repository metadata')
        self._check_config(metadata/'config')
        for relative in ('config.worktree', 'commondir', 'objects/info/alternates'):
            if (metadata/relative).exists() or (metadata/relative).is_symlink():
                raise BoundaryError('unsupported repository indirection')

    def git(self, *args):
        self._check_repository(self.repo)
        return run_bounded(['/usr/bin/git', '-c', 'core.hooksPath=/dev/null',
                            '-c', 'core.fsmonitor=false', '-c', 'protocol.allow=never',
                            '-c', 'protocol.file.allow=always', '-c', 'gc.auto=0',
                            '-c', 'maintenance.auto=false', '-c', 'submodule.recurse=false',
                            '-c', 'fetch.recurseSubmodules=false', '-C', str(self.repo), *args], self.env)

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
        remote_path = no_symlinks(remote)
        self._check_repository(remote_path, bare=True)
        self.git('fetch', '--no-tags', str(remote_path), '+refs/heads/main:refs/remotes/origin/main')
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
