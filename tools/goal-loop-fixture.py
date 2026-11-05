#!/usr/bin/env python3
"""Owned, offline goal-loop demo using the real server and isolated PostgreSQL.

  python3 tools/goal-loop-fixture.py start --output evals/results/<new-directory>
  python3 tools/goal-loop-fixture.py status --output evals/results/<directory>
  python3 tools/goal-loop-fixture.py restart --output evals/results/<directory>
  python3 tools/goal-loop-fixture.py stop --output evals/results/<directory>

start returns with the demo running. restart keeps the same DB/binary/registry;
stop deletes its labelled containers and DB. The private output is retained.
No inherited DSN, provider request, Task worker, QQ runtime or writer is used.
"""
import argparse
import contextlib
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
_spec = importlib.util.spec_from_file_location('goal_fixture_b7_guards', ROOT / 'tools/b7-manual-fixture.py')
B7 = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(B7)
LABEL_KEY = 'bba.goal-loop-fixture'
DB_NAME = DB_USER = 'bba_dev'
WORKSPACE = 'goal-fixture'
PRINCIPAL = 'op-goal'
# Refuse connection sources even when libpq would otherwise ignore them.
DSN_VARS = ('BBA_TASKENGINE_DSN', 'DATABASE_URL', 'BBA_FIXTURE_DSN')
PERMISSIONS = ['operator.read', 'conversation.read', 'conversation.write',
               'model.configure', 'agent.configure', 'skill.configure']
SEED_SQL = """BEGIN;
INSERT INTO core.workspace(id) VALUES ('goal-fixture');
INSERT INTO core.principal(workspace_id,id) VALUES ('goal-fixture','op-goal');
COMMIT;
"""


def guard_environment(env):
    B7.guard_inherited_dsn(env)
    for name, value in env.items():
        if value and (name in DSN_VARS or name.startswith('PG')):
            raise ValueError(f'{name} is set; unset inherited database configuration before using the fixture')


def prepare_output(output, env):
    guard_environment(env)
    out = B7.guard_output_dir(output, ROOT)
    out.mkdir(parents=True, mode=0o700)
    out.chmod(0o700)
    return out.resolve()


def write_private(path, text):
    path = Path(path)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(text if text.endswith('\n') else text + '\n')


def private_read(path):
    path = Path(path)
    mode = path.lstat().st_mode
    if not stat.S_ISREG(mode) or mode & 0o777 != 0o600 or path.stat().st_uid != os.getuid():
        raise ValueError('fixture file must be an owned regular file with mode 0600')
    return path.read_text()


def run(argv, *, check=True, env=None, input=None, timeout=180):
    """Never echo argv, subprocess output, DSNs or secrets on failure."""
    try:
        result = subprocess.run(argv, input=input, env=env, capture_output=True,
                                text=True, timeout=timeout)
    except (OSError, subprocess.TimeoutExpired):
        raise RuntimeError('fixture subprocess failed or timed out; no command/output echoed') from None
    if check and result.returncode:
        raise RuntimeError(f'fixture subprocess exited {result.returncode}; no command/output echoed')
    return result


def initial_state(out, instance, http_port, db_port):
    return dict(instance=instance, output=str(out), container='bba-goalfix-' + instance,
                build_container='bba-goalbuild-' + instance, container_id=None,
                http_port=http_port, db_port=db_port, db_user=DB_USER, db_name=DB_NAME,
                binary=str(out / 'bin/taskengine-server'), server_pid=None,
                process_start=None, status='starting', restart_count=0)


def write_state(out, state):
    temporary = out / 'state.pending'
    write_private(temporary, json.dumps(state, indent=2))
    temporary.replace(out / 'state.json')


def existing_output(output):
    out = Path(output)
    if not out.is_absolute():
        out = ROOT / out
    results = (ROOT / 'evals/results').resolve()
    if out.is_symlink() or out.resolve().parent != results:
        raise ValueError('--output must be an owned direct child of evals/results')
    out = out.resolve()
    if not out.is_dir() or out.stat().st_uid != os.getuid() or out.stat().st_mode & 0o777 != 0o700:
        raise ValueError('fixture output must be an owned directory with mode 0700')
    return out


def load_state(out):
    out = existing_output(out)
    state = json.loads(private_read(out / 'state.json'))
    instance = state['instance']
    if not re.fullmatch(r'[a-f0-9]{12}', instance):
        raise ValueError('invalid fixture instance identity')
    expected = initial_state(out, instance, state['http_port'], state['db_port'])
    for name in ('output', 'container', 'build_container', 'binary', 'db_user', 'db_name'):
        if state.get(name) != expected[name]:
            raise ValueError('fixture state does not match this output/instance')
    B7.guard_port(state['http_port'])
    B7.guard_port(state['db_port'])
    if state['http_port'] == state['db_port']:
        raise ValueError('fixture ports must differ')
    return state


@contextlib.contextmanager
def lifecycle_lock(out):
    fd = os.open(out / 'lifecycle.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as stream:
        os.fchmod(stream.fileno(), 0o600)
        try:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError('another lifecycle command owns this fixture') from None
        yield


def runtime_env(env, dsn):
    clean = {k: v for k, v in env.items() if not k.startswith(('BBA_', 'PG'))
             and k not in ('DATABASE_URL', 'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY',
                           'http_proxy', 'https_proxy', 'all_proxy')}
    clean.update(BBA_FIXTURE_ONLY='1', BBA_TASKENGINE_DSN=dsn)
    return clean


def fixture_dsn(out, state):
    dsn = private_read(out / 'dsn.txt').strip()
    parsed = urlsplit(dsn)
    if (parsed.scheme != 'postgres' or parsed.hostname != '127.0.0.1'
            or parsed.port != state['db_port'] or parsed.username != DB_USER
            or parsed.path != '/' + DB_NAME or parsed.query != 'sslmode=disable'
            or not parsed.password or parsed.fragment):
        raise ValueError('fixture DSN does not match the owned loopback database')
    return dsn


def server_args(port, out):
    return B7.server_args(port, out / 'resolver.json') + [
        '-development-operations', '-offline-goal-fixture',
        '-model-registry-file', str(out / 'registry.json')]


def guard_container(name, labels, instance):
    if name not in ('bba-goalfix-' + instance, 'bba-goalbuild-' + instance) or labels.get(LABEL_KEY) != instance:
        raise ValueError('container name and exact ownership label do not match the fixture')


def inspect_container(state, build=False):
    name = state['build_container' if build else 'container']
    result = run(['docker', 'inspect', name], check=False, timeout=10)
    if result.returncode:
        # A daemon error must not be reported as proof of cleanup.
        run(['docker', 'info', '--format', '{{.ServerVersion}}'], timeout=10)
        return None
    info = json.loads(result.stdout)[0]
    guard_container(name, info['Config'].get('Labels') or {}, state['instance'])
    if not build and state.get('container_id') and info['Id'] != state['container_id']:
        raise ValueError('owned container identity changed')
    return info


def process_start(pid):
    try:
        values = Path(f'/proc/{int(pid)}/stat').read_text().rsplit(') ', 1)[1].split()
        return None if values[0] == 'Z' else values[19]
    except (OSError, ValueError, IndexError, TypeError):
        return None


def owned_process(pid, binary, start):
    if not start or process_start(pid) != start:
        return False
    try:
        exe = Path(f'/proc/{pid}/exe')
        cmd = Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')[0].decode()
        return exe.resolve() == Path(binary).resolve() and cmd == str(binary)
    except OSError:
        return False


def stop_server(out, state):
    pid = state.get('server_pid')
    if pid and process_start(pid):
        if not owned_process(pid, state['binary'], state.get('process_start')):
            raise ValueError('refusing to signal a process without exact fixture identity')
        os.kill(pid, signal.SIGTERM)
        deadline = time.monotonic() + 8
        while owned_process(pid, state['binary'], state['process_start']) and time.monotonic() < deadline:
            time.sleep(0.1)
        if owned_process(pid, state['binary'], state['process_start']):
            os.kill(pid, signal.SIGKILL)
    if pid:
        try:
            os.waitpid(pid, 0)
        except ChildProcessError:
            pass
    state.update(server_pid=None, process_start=None)
    write_state(out, state)


def port_open(port):
    with socket.socket() as sock:
        sock.settimeout(0.2)
        return sock.connect_ex(('127.0.0.1', port)) == 0


def cleanup(out, state):
    failures = []
    try:
        stop_server(out, state)
    except (ValueError, OSError, RuntimeError):
        failures.append('server identity/stop')
    for build in (True, False):
        try:
            if inspect_container(state, build):
                run(['docker', 'rm', '-f', '-v', state['build_container' if build else 'container']], timeout=30)
        except (ValueError, RuntimeError):
            failures.append('owned container cleanup')
    gone = False
    try:
        gone = inspect_container(state, True) is None and inspect_container(state) is None
    except (ValueError, RuntimeError):
        failures.append('container cleanup verification')
    deadline = time.monotonic() + 3
    while any(port_open(state[k]) for k in ('http_port', 'db_port')) and time.monotonic() < deadline:
        time.sleep(0.1)
    closed = not any(port_open(state[k]) for k in ('http_port', 'db_port'))
    report = dict(containers_gone=gone, ports_closed=closed, failures=failures)
    write_private(out / 'cleanup.json', json.dumps(report, indent=2))
    if failures or not gone or not closed:
        raise RuntimeError('fixture cleanup incomplete; inspect private cleanup.json')
    return report


def build(out, state):
    (out / 'bin').mkdir(mode=0o700)
    # The B7 helper has no standalone build function: reuse its pinned cache layout.
    run(['docker', 'run', '--rm', '--pull', 'never', '--name', state['build_container'],
         '--label', LABEL_KEY + '=' + state['instance'], '--network', 'none',
         '--user', f'{os.getuid()}:{os.getgid()}', '-e', 'HOME=/tmp/home',
         '-e', 'GOPATH=/work/.cache/gopath', '-e', 'GOMODCACHE=/work/.cache/gomod',
         '-e', 'GOCACHE=/work/.cache/gobuild', '-e', 'GOPROXY=off', '-e', 'GOSUMDB=off',
         '-e', 'CGO_ENABLED=0', '-v', f'{ROOT}:/work', '-v', f'{out}:/out',
         '-w', '/work/backend', B7.GO_IMAGE, '/bin/sh', '-c',
         'set -e; go build -trimpath -o /out/bin/taskengine-migrate ./cmd/taskengine-migrate; '
         'go build -trimpath -o /out/bin/taskengine-server ./cmd/taskengine-server'], timeout=300)
    state['binary_sha256'] = hashlib.sha256(Path(state['binary']).read_bytes()).hexdigest()
    write_state(out, state)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def api(origin, token, path, body=None):
    if not path.startswith('/v1/operator/') or '?' in path or '#' in path or '://' in path:
        raise ValueError('fixture API path must remain within operator routes')
    parsed = urlsplit(origin)
    if parsed.scheme != 'http' or parsed.hostname != '127.0.0.1' or parsed.path or parsed.username:
        raise ValueError('fixture origin must be direct loopback HTTP')
    headers = {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'}
    if body is not None:
        headers['Idempotency-Key'] = 'goal-fixture-' + secrets.token_hex(12)
    request = urllib.request.Request(origin + path, headers=headers,
                                     data=None if body is None else json.dumps(body).encode())
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(request, timeout=3) as response:
            raw = response.read(2 * 1024 * 1024 + 1)
            if len(raw) > 2 * 1024 * 1024:
                raise RuntimeError('fixture API response exceeded its bound')
            return json.loads(raw)
    except urllib.error.HTTPError as error:
        code = error.code
        error.close()
        raise RuntimeError(f'fixture API returned HTTP {code}; no response body echoed') from None
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError):
        raise RuntimeError('fixture API unavailable or invalid; no headers/body echoed') from None


def seed_registry(origin, token):
    model = api(origin, token, '/v1/operator/models', dict(
        name='Offline fixture configuration (never invoked)', model='deterministic-unused',
        base_url='https://127.0.0.1:1/v1', enabled=True, is_default=False,
        timeout_seconds=30, max_concurrent=1, expected_version=0,
        secret_action='replace', api_key='synthetic-not-a-provider-key'))['data']
    skill = api(origin, token, '/v1/operator/skills', dict(
        name='Offline evidence analysis', description='Only deterministic synthetic evidence',
        enabled=True, goal='Identify missing demand, budget and time evidence',
        inputs=['synthetic window'], steps=['Check evidence references', 'Report information gaps'],
        required_capabilities=['structured_output'], outputs=['structured_output'],
        stop_conditions=['Invalid or expired input', 'Operator stop or budget limit'],
        constraints=['No model, tools or QQ calls'], tools=[], published=True, expected_version=0))['data']
    refs = {}
    for role in ('planner', 'analyst'):
        agent = api(origin, token, '/v1/operator/agents', dict(
            name='Offline ' + role, description='Deterministic fixture role', enabled=True,
            system_prompt='Analyze only synthetic group windows; no external actions.',
            model_id=model['id'], skills=[dict(id=skill['id'], version=skill['version'])],
            tools=[], web_search=False, expected_version=0))['data']
        refs[role + '_ref'] = dict(id=agent['id'], version=agent['version'])
    return dict(refs, model_id=model['id'], skill_id=skill['id'])


def spawn_server(binary, args, env, log_file):
    """Return only the owned child PID; no detached Popen object to finalize."""
    log_file = Path(log_file)
    # Keep the log private even if it already exists, and reject symlink targets.
    fd = os.open(log_file, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
    try:
        os.fchmod(fd, 0o600)
    finally:
        os.close(fd)
    return os.posix_spawn(str(binary), [str(binary)] + args, env, setsid=True,
                          file_actions=[
                              (os.POSIX_SPAWN_OPEN, 0, '/dev/null', os.O_RDONLY, 0),
                              (os.POSIX_SPAWN_OPEN, 1, str(log_file),
                               os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW, 0o600),
                              (os.POSIX_SPAWN_DUP2, 1, 2),
                          ])


def start_server(out, state, env):
    binary = Path(state['binary'])
    if binary.is_symlink() or hashlib.sha256(binary.read_bytes()).hexdigest() != state['binary_sha256']:
        raise ValueError('restart requires the original fixture binary')
    if port_open(state['http_port']):
        raise ValueError('fixture HTTP port is occupied; refusing to replace its listener')
    dsn = fixture_dsn(out, state)
    pid = spawn_server(binary, server_args(state['http_port'], out),
                       runtime_env(env, dsn), out / 'server.log')
    state.update(server_pid=pid, process_start=process_start(pid), status='starting')
    write_state(out, state)
    origin = f"http://127.0.0.1:{state['http_port']}"
    token = private_read(out / 'operator-token').strip()
    for _ in range(100):
        if not owned_process(pid, binary, state['process_start']):
            raise RuntimeError('fixture server exited or lost identity; inspect private server.log')
        try:
            result = api(origin, token, '/v1/operator/goal-runs')
            if result.get('meta', {}).get('fixture_enabled') is True:
                state['status'] = 'running'
                write_state(out, state)
                return
        except RuntimeError:
            pass
        time.sleep(0.1)
    raise RuntimeError('fixture server did not become ready with a verified fixture gate')


def public_status(out, state):
    return dict(status=state['status'], server_running=owned_process(
        state.get('server_pid'), state['binary'], state.get('process_start')),
        product_url=f"http://127.0.0.1:{state['http_port']}/product/chat",
        token_file=str(out / 'operator-token'), config_file=str(out / 'fixture-config.json'),
        output=str(out), restart_count=state['restart_count'],
        note='deterministic_fixture; no model/QQ calls; stop removes this isolated DB')


def cmd_start(output, env=None):
    env = dict(os.environ if env is None else env)
    out = prepare_output(output, env)
    with lifecycle_lock(out):
        ports = {B7.pick_free_port()}
        while len(ports) < 2:
            ports.add(B7.pick_free_port())
        http_port, db_port = ports
        state = initial_state(out, secrets.token_hex(6), http_port, db_port)
        write_state(out, state)  # persist ownership BEFORE creating any resource
        try:
            token, password = secrets.token_hex(32), secrets.token_hex(32)
            write_private(out / 'operator-token', token)
            resolver = dict(credentials=[dict(token_sha256=B7.sha256_hex(token), workspace_id=WORKSPACE,
                                             principal_id=PRINCIPAL, permissions=PERMISSIONS)])
            write_private(out / 'resolver.json', json.dumps(resolver))
            B7.guard_resolver_file(out / 'resolver.json')
            write_private(out / 'postgres.env', f'POSTGRES_USER={DB_USER}\nPOSTGRES_DB={DB_NAME}\nPOSTGRES_PASSWORD={password}')
            dsn = f'postgres://{DB_USER}:{password}@127.0.0.1:{db_port}/{DB_NAME}?sslmode=disable'
            write_private(out / 'dsn.txt', dsn)
            result = run(['docker', 'run', '-d', '--pull', 'never', '--name', state['container'],
                          '--label', LABEL_KEY + '=' + state['instance'], '--env-file', str(out / 'postgres.env'),
                          '-p', f'127.0.0.1:{db_port}:5432', '--restart', 'no', B7.PG_IMAGE])
            state['container_id'] = result.stdout.strip()
            write_state(out, state)
            for _ in range(100):
                ready = run(['docker', 'exec', state['container'], 'pg_isready', '-U', DB_USER, '-d', DB_NAME], check=False, timeout=5)
                if ready.returncode == 0:
                    break
                time.sleep(0.1)
            else:
                raise RuntimeError('owned database did not become ready')
            build(out, state)
            run([str(out / 'bin/taskengine-migrate')], env=runtime_env(env, dsn))
            run(['docker', 'exec', '-i', state['container'], 'psql', '-X', '-v', 'ON_ERROR_STOP=1',
                 '-U', DB_USER, '-d', DB_NAME], input=SEED_SQL)
            start_server(out, state, env)
            origin = f'http://127.0.0.1:{http_port}'
            config = dict(origin=origin, product_url=origin + '/product/chat', workspace_id=WORKSPACE,
                          principal_id=PRINCIPAL, thread_id='offline-thread-1', group_id='20001',
                          token_file=str(out / 'operator-token'), resolver_file=str(out / 'resolver.json'),
                          registry_file=str(out / 'registry.json'), dsn_file=str(out / 'dsn.txt'),
                          db_container=state['container'], state_file=str(out / 'state.json'),
                          **seed_registry(origin, token))
            write_private(out / 'fixture-config.json', json.dumps(config, indent=2))
            print(json.dumps(public_status(out, state), indent=2))
            return 0
        except BaseException:
            state['status'] = 'failed'
            write_state(out, state)
            cleanup(out, state)
            raise


def cmd_restart(output, env=None):
    env = dict(os.environ if env is None else env)
    guard_environment(env)
    out = existing_output(output)
    with lifecycle_lock(out):
        state = load_state(out)
        info = inspect_container(state)
        if not info or not info['State']['Running'] or state['status'] in ('stopped', 'failed'):
            raise ValueError('restart requires this fixture database still running')
        try:
            stop_server(out, state)
            start_server(out, state, env)
            state['restart_count'] += 1
            write_state(out, state)
            print(json.dumps(public_status(out, state), indent=2))
            return 0
        except BaseException:
            state['status'] = 'failed'
            write_state(out, state)
            cleanup(out, state)
            raise


def cmd_stop(output):
    out = existing_output(output)
    with lifecycle_lock(out):
        state = load_state(out)
        report = cleanup(out, state)
        state['status'] = 'stopped'
        write_state(out, state)
        print(json.dumps(report, indent=2))
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('command', choices=['start', 'stop', 'status', 'restart'])
    parser.add_argument('--output', required=True)
    args = parser.parse_args(argv)
    def interrupted(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    try:
        if args.command == 'start':
            return cmd_start(args.output)
        if args.command == 'restart':
            return cmd_restart(args.output)
        if args.command == 'stop':
            return cmd_stop(args.output)
        out = existing_output(args.output)
        with lifecycle_lock(out):
            print(json.dumps(public_status(out, load_state(out)), indent=2))
        return 0
    except (ValueError, RuntimeError, OSError, KeyError, KeyboardInterrupt):
        # Exception strings may include filesystem or environment secrets: keep CLI errors bounded.
        print('goal-loop-fixture: operation failed; check private state/cleanup/server log; '
              'start/restart require unset inherited DB variables and cached build dependencies', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
