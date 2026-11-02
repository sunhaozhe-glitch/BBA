"""NDJSON ACP supervisor; uncertain transports are never replayed."""
import concurrent.futures
import json
import os
from pathlib import Path
import signal
import subprocess
import threading
import sys

from .evidence import redact, summary
from .worktrees import no_symlinks

MAX_FRAME = 65536
MAX_STREAM = 1024 * 1024


def process_identity(proc):
    return {'pgid':proc.pid, 'boot_id':Path('/proc/sys/kernel/random/boot_id').read_text().strip()}


def group_alive(identity):
    if identity['boot_id'] != Path('/proc/sys/kernel/random/boot_id').read_text().strip():
        return False
    for entry in Path('/proc').iterdir():
        if not entry.name.isdigit():
            continue
        try:
            fields = (entry/'stat').read_text().rsplit(')',1)[1].split()
            if int(fields[2]) == identity['pgid'] and fields[0] != 'Z':
                return True
        except FileNotFoundError:
            continue
    return False


class TransportUnknown(Exception):
    pass


def omp_command(executable):
    if not os.path.isabs(executable) or any(x in executable for x in ('\n', '\x00')):
        raise ValueError('absolute executable required')
    return [executable, 'acp', '--no-tools', '--no-extensions', '--no-skills', '--no-context-files', '--no-lsp', '--no-title', '--approval-mode', 'always-ask']


class Supervisor:
    def __init__(self, argv, cwd, agent_dir, record, *, simulation=False, metadata_authorized=False, fixture_catalog=None):
        if not simulation and not metadata_authorized:
            raise PermissionError('real ACP startup requires separate startup metadata authority')
        # Simulation identity is fixed by this installed package, never by cwd.
        # -I ignores Python environment/cwd; -S prevents site customization.
        # The peer is a standalone stdlib-only script, so no import bootstrap is needed.
        if simulation:
            legacy = [sys.executable, '-m', 'bba_controller.sim_peer']
            supplied = list(argv)
            suffix = supplied[len(legacy):]
            if supplied[:len(legacy)] != legacy or (suffix and
                    (len(suffix) != 2 or suffix[0] != '--behavior' or
                     suffix[1] not in {'success','reverse','hang','exit','malformed','overflow','cancel'})):
                raise PermissionError('unapproved simulation executable')
            peer_script = no_symlinks(Path(__file__).absolute().parent/'sim_peer.py')
            if not peer_script.is_file():
                raise PermissionError('trusted simulation script missing')
            argv = [sys.executable, '-I', '-S', str(peer_script), *suffix]
        self.simulation = simulation
        self.record_callback = record
        self.cwd = no_symlinks(cwd)
        self.agent_dir = no_symlinks(agent_dir)
        self.agent_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        # No inherited provider credentials/config or user home. PYTHONPATH is a fixed package path.
        env = {'PATH': os.defpath, 'LANG': 'C', 'HOME': str(self.agent_dir/'home'), 'PI_CODING_AGENT_DIR': str(self.agent_dir),
               'XDG_CONFIG_HOME': str(self.agent_dir/'config'), 'XDG_CACHE_HOME': str(self.agent_dir/'cache'),
               'PYTHONPATH': str(Path(__file__).resolve().parents[1])}
        if not simulation:
            if list(argv) != omp_command(argv[0]):
                raise PermissionError('unapproved real ACP argv')
        if fixture_catalog is not None:
            if not simulation:
                raise PermissionError('fixture catalog is simulation only')
            raw = json.dumps(fixture_catalog)
            if len(raw.encode()) > 32768:
                raise ValueError('fixture catalog limit')
            env['BBA_FIXTURE_CATALOG'] = raw
        self.pending = {}
        self.counter = 0
        self.lock = threading.RLock()
        self.close_lock = threading.Lock()
        self.write_lock = threading.Lock()
        self.disconnected = False
        self.closed = False
        self.event_count = 0
        self.audit_errors = []
        self.threads = []
        self.cleanup_confirmed = False
        self.proc = subprocess.Popen(argv, cwd=self.cwd, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, start_new_session=True, bufsize=0)
        try:
            self.record('process_start', {'peer': 'simulation' if simulation else 'real-metadata-only', 'identity':process_identity(self.proc)})
            threads = [threading.Thread(target=self._read, daemon=True), threading.Thread(target=self._stderr, daemon=True)]
            for thread in threads:
                thread.start()
                self.threads.append(thread)
        except BaseException:
            try:
                self.close()
            except BaseException as cleanup_error:
                setattr(cleanup_error, 'supervisor', self)
                raise
            raise

    def record(self, kind, data):
        with self.lock:
            if self.event_count >= 1000 and kind not in {'cleanup','disconnect','request_timeout','protocol_error','audit_error'}:
                raise TransportUnknown('ordinary supervisor audit limit reached')
            self.event_count += 1
        try:
            self.record_callback({'kind': kind, 'data': redact(data)})
        except Exception as error:
            self.audit_errors.append(type(error).__name__)
            raise

    def _send(self, message):
        raw = (json.dumps(message) + '\n').encode()
        if len(raw) > MAX_FRAME:
            raise ValueError('request frame too large')
        with self.write_lock:
            if self.disconnected:
                raise TransportUnknown('transport disconnected')
            try:
                self.proc.stdin.write(raw)
                self.proc.stdin.flush()
            except (BrokenPipeError, OSError) as e:
                self._disconnect('write failure')
                raise TransportUnknown('write failure') from e
        self.record('rpc_send', {'method': message.get('method'), 'id': message.get('id'), 'summary': summary(message)})

    def send_request(self, method, params):
        if method == 'session/prompt' and not self.simulation:
            raise PermissionError('phase 1 real model prompt disabled')
        with self.lock:
            if len(self.pending) >= 16:
                raise TransportUnknown('pending queue full')
            self.counter += 1
            request_id = self.counter
            self.pending[request_id] = concurrent.futures.Future()
        try:
            self._send({'jsonrpc': '2.0', 'id': request_id, 'method': method, 'params': params})
        except BaseException:
            with self.lock:
                self.pending.pop(request_id, None)
            raise
        return request_id

    def wait(self, request_id, timeout=5):
        with self.lock:
            future = self.pending.get(request_id)
        if future is None:
            raise TransportUnknown('request unavailable')
        try:
            return future.result(timeout=max(.01, min(timeout, 30)))
        except concurrent.futures.TimeoutError as e:
            self.record('request_timeout', {'id': request_id, 'outcome': 'UNKNOWN'})
            self._disconnect('request timeout')
            raise TransportUnknown('request timeout; no replay') from e
        finally:
            with self.lock:
                self.pending.pop(request_id, None)

    def request(self, method, params, timeout=5):
        return self.wait(self.send_request(method, params), timeout)

    def initialize(self):
        result = self.request('initialize', {'protocolVersion': 1, 'clientInfo': {'name': 'bba-controller', 'version': '0.1.0'},
                                           'clientCapabilities': {'fs': {'readTextFile': False, 'writeTextFile': False}, 'terminal': False}})
        if result.get('protocolVersion') != 1:
            raise TransportUnknown('unsupported protocol')
        self.capabilities = result.get('agentCapabilities', {})
        return result

    def new_session(self):
        return self.request('session/new', {'cwd': str(self.cwd), 'mcpServers': []})['sessionId']

    def restore(self, session_id, operation='load'):
        if operation not in ('load', 'resume'):
            raise ValueError('invalid restore operation')
        caps = getattr(self, 'capabilities', {})
        if operation == 'load' and not caps.get('loadSession'):
            raise TransportUnknown('loadSession capability absent')
        if operation == 'resume' and 'resume' not in caps.get('sessionCapabilities', {}):
            raise TransportUnknown('resume capability absent')
        result = self.request('session/' + operation, {'sessionId': session_id, 'cwd': str(self.cwd), 'mcpServers': []})
        return {**result, 'sessionId': session_id}

    def prompt(self, session_id, text, timeout=5):
        if not self.simulation:
            raise PermissionError('phase 1 real model prompt disabled')
        return self.request('session/prompt', {'sessionId': session_id, 'prompt': [{'type': 'text', 'text': text}]}, timeout)

    def cancel(self, session_id):
        # Model authorization remains independent from reverse tool permissions.
        # ACP cancel is a notification, not an acknowledgement.
        self._send({'jsonrpc': '2.0', 'method': 'session/cancel', 'params': {'sessionId': session_id}})
        self.record('cancel_requested', {'session_id': session_id, 'confirmed': False})

    def select_simulation_model(self, session_id, profile, role):
        if not self.simulation:
            raise PermissionError('real OMP model selection adapter is not enabled')
        result = self.request('simulation/select_model',{'sessionId':session_id,'profile':profile,'role':role})
        if result.get('selected') != profile or result.get('role') != role:
            raise TransportUnknown('simulation selection receipt mismatch')
        self.record('model_selected', {'profile_ref':profile, 'role':role, 'simulation':True})
        return result

    def _disconnect(self, reason):
        with self.lock:
            if self.disconnected:
                return
            self.disconnected = True
            for future in self.pending.values():
                if not future.done():
                    future.set_exception(TransportUnknown(reason))
        self.record('disconnect', {'reason': reason, 'outcome': 'UNKNOWN'})

    def _receive(self, message):
        if not isinstance(message, dict) or message.get('jsonrpc') != '2.0':
            raise ValueError('invalid RPC')
        self.record('rpc_receive', {'method': message.get('method'), 'id': message.get('id'), 'summary': summary(message)})
        if 'method' in message:
            if 'id' in message:
                method = message['method']
                if method == 'session/request_permission':
                    result = {'outcome': {'outcome': 'cancelled'}}
                    reply = {'result': result}
                else:
                    reply = {'error': {'code': -32601, 'message': 'Denied by controller policy'}}
                self.record('permission_denied', {'method': method, 'allowed': False})
                self._send({'jsonrpc': '2.0', 'id': message['id'], **reply})
            else:
                self.record('notification', {'method': message['method'], 'summary': summary(message.get('params'))})
            return
        with self.lock:
            future = self.pending.get(message.get('id'))
            if future is None or future.done():
                self.record('orphan_response', {'id': message.get('id')})
                return
            if 'result' in message:
                future.set_result(message['result'])
            else:
                future.set_exception(TransportUnknown('RPC error; reconcile required'))

    def _read(self):
        total = 0
        try:
            while True:
                line = self.proc.stdout.readline(MAX_FRAME + 1)
                if not line:
                    break
                total += len(line)
                if len(line) > MAX_FRAME or total > MAX_STREAM or not line.endswith(b'\n'):
                    raise ValueError('stdout frame or stream limit')
                self._receive(json.loads(line))
        except Exception:
            self.record('protocol_error', {'reason': 'invalid or oversized stdout'})
        finally:
            self._disconnect('stdout EOF or protocol failure')

    def _stderr(self):
        total = 0
        while True:
            line = self.proc.stderr.readline(4097)
            if not line:
                return
            total += len(line)
            if total > MAX_STREAM:
                self._disconnect('stderr stream limit')
                return
            # Do not retain split raw secrets: only whole short lines are redacted.
            if len(line) > 4096 or not line.endswith(b'\n'):
                self.record('stderr', {'omitted': True})
            else:
                self.record('stderr', {'text': line.decode('utf-8', errors='replace')})

    def close(self):
        with self.close_lock:
            return self._close()

    def _close(self):
        with self.lock:
            if self.cleanup_confirmed:
                return
            self.closed = True
        audit_error = None
        try:
            self._disconnect('cleanup')
        except Exception as error:
            audit_error = error
        try:
            os.killpg(self.proc.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            self.proc.wait(timeout=1)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(self.proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.proc.wait(timeout=2)
        # Kill remaining descendants even when the leader exited on TERM.
        try:
            os.killpg(self.proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        for thread in self.threads:
            thread.join(timeout=2)
        if group_alive(process_identity(self.proc)) or any(t.is_alive() for t in self.threads):
            raise TransportUnknown('process group or reader cleanup unconfirmed')
        for stream in (self.proc.stdin, self.proc.stdout, self.proc.stderr):
            stream.close()
        with self.lock:
            self.pending.clear()
        self.cleanup_confirmed = True
        try:
            self.record('cleanup', {'process_group_terminated': True, 'returncode': self.proc.returncode})
        except Exception as error:
            audit_error = error
        if audit_error:
            raise audit_error
