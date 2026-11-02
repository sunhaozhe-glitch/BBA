"""Single-owner asynchronous controller. All execution is fixture simulation."""
import asyncio
from dataclasses import dataclass
import json
import hashlib
import os
from pathlib import Path
import sys
import time

from jsonschema import Draft202012Validator, ValidationError

from .acp import Supervisor, TransportUnknown, process_identity
from .contracts import TOOLS, output_for
from .store import Store, Conflict, TERMINAL
from .worktrees import Worktrees, BoundaryError, no_symlinks
from .models import ModelRouter, ModelDenied

INPUT_VALIDATORS = {name:Draft202012Validator(schema) for name,schema in TOOLS.items()}
OUTPUT_VALIDATORS = {name:Draft202012Validator(output_for(name)) for name in TOOLS}


@dataclass(frozen=True)
class Settings:
    state_dir: Path
    repository: Path
    mode: str = 'OBSERVE'
    fixture_authorized: bool = False
    slots: int = 1
    development_authorized: bool = False
    model_config: dict | None = None

    def __post_init__(self):
        if self.mode not in ('OBSERVE', 'DEVELOPMENT'):
            raise ValueError('invalid startup mode')
        if self.mode == 'DEVELOPMENT' and not self.development_authorized:
            raise PermissionError('DEVELOPMENT requires startup-side authority')
        state = no_symlinks(self.state_dir)
        repository = no_symlinks(self.repository)
        if state == repository or repository in state.parents or state in repository.parents:
            raise BoundaryError('state and protected repository must be disjoint')
        object.__setattr__(self, 'state_dir', state)
        object.__setattr__(self, 'repository', repository)


class Controller:
    def __init__(self, settings):
        self.settings = settings
        self.root = no_symlinks(settings.state_dir)
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        for child in ('owner.lock', 'controller.sqlite3', 'controller.sqlite3-wal', 'controller.sqlite3-shm', 'agents', 'worktrees'):
            no_symlinks(self.root/child)
        # Exclusive owner avoids a second process mistaking live work for crash recovery.
        import fcntl
        self.owner = (self.root/'owner.lock').open('a')
        try:
            fcntl.flock(self.owner, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self.owner.close()
            raise Conflict('controller state already has an owner')
        try:
            self.store = Store(self.root/'controller.sqlite3', slots=settings.slots)
            self.worktrees = Worktrees(settings.repository, self.root/'worktrees', settings.fixture_authorized)
            identity = hashlib.sha256(str(self.worktrees.repo).encode()).hexdigest()
            self.store.bind_repository(identity)
            self.models = ModelRouter(self.store,settings.model_config)
            self.models.recover()
            self.store.recover()
        except BaseException:
            self.owner.close()
            raise
        self.running = {}
        self.peers = {}
        self.closed = False
        self.scheduler = None
        self.dispatch_lock = asyncio.Lock()
        self.close_lock = asyncio.Lock()

    async def _ensure_scheduler(self):
        if self.scheduler is None and self.settings.fixture_authorized:
            self.scheduler = asyncio.create_task(self._schedule())

    async def _schedule(self):
        while not self.closed:
            cursor = None
            while not self.closed:
                page = self.store.queued(cursor)
                if not page:
                    break
                for task in page:
                    tid = task['id']
                    try:
                        claimed = self.store.claim(tid, cleanup_required=True)
                    except Exception as error:
                        # claim rolled back: quarantine once, with a safety event.
                        # If this audit also fails, surface the storage failure;
                        # never silently report dispatch success or spin/retry.
                        self.store.transition(tid, task['version'], 'PAUSED',
                                              {'reason':'claim failed', 'error':type(error).__name__})
                        continue
                    if claimed:
                        worker = asyncio.create_task(self._execute(tid))
                        self.running[tid] = worker
                        worker.add_done_callback(lambda _, tid=tid: self.running.pop(tid, None))
                cursor = (page[-1]['created'], page[-1]['id'])
                await asyncio.sleep(0)
            await asyncio.sleep(.02)

    def _record(self, tid, event):
        if event['kind'] == 'process_start':
            self.store.process_started(tid, event['data'])
            return
        self.store.event(tid, event['kind'], event['data'], critical=event['kind'] in {
            'cleanup', 'disconnect', 'request_timeout', 'protocol_error', 'audit_error'})

    async def _execute(self, tid):
        peer = None
        model_call = None
        try:
            task = self.store.get(tid)
            if self.closed:
                self._unknown(tid,'controller closing before execution')
                return
            if task['state'] != 'RUNNING':
                return
            path = await asyncio.to_thread(self.worktrees.create, tid, task['spec']['base_sha'])
            if self.closed:
                self._unknown(tid,'controller closing during worktree preparation')
                return
            if self.store.get(tid)['state'] != 'RUNNING':
                return
            self.store.artifact(tid, 'worktree', {'base_sha':task['spec']['base_sha'],'branch':f'controller/{tid}','retained':True})
            model_snapshot = self.models.evidence(tid)['snapshot']
            catalog = {ref:definition['model_role'] for ref,definition in model_snapshot['profiles'].items()
                       if definition['simulation'] and definition['is_local'] and definition['enabled']
                       and definition['provider'] in model_snapshot['policy']['provider_allowlist']}
            peer = Supervisor([sys.executable,'-m','bba_controller.sim_peer','--behavior',task['spec'].get('behavior','success')], path, self.root/'agents'/tid,
                                           lambda event: self._record(tid, event), simulation=True, fixture_catalog=catalog)
            self.peers[tid] = peer
            await asyncio.to_thread(peer.initialize)
            sid = await asyncio.to_thread(peer.new_session)
            self.store.session(tid, sid, 'simulation')
            if self.store.get(tid)['state'] != 'RUNNING':
                return
            model_snapshot = self.models.evidence(tid)['snapshot']
            policy = model_snapshot['policy']
            profile = policy['default_profile']
            model_call = self.models.begin(tid,sid,profile,policy['default_role'],1)
            await asyncio.to_thread(peer.select_simulation_model,sid,profile,policy['default_role'])
            if self.store.get(tid)['state'] != 'RUNNING':
                self.models.finish(model_call,None,'UNKNOWN','cancel before prompt dispatch')
                model_call = None
                return
            # The selected profile is a simulated route recorded before dispatch;
            # no claim that OMP itself can enforce this policy is made.
            result = await asyncio.to_thread(peer.prompt, sid, 'fixture simulation only', .3)
            state = 'CANCELLED' if result.get('stopReason') == 'cancelled' else 'SUCCEEDED' if result.get('stopReason') == 'end_turn' else 'UNKNOWN'
            receipt = {'stopReason':result.get('stopReason'), 'environment':'ISOLATED_FIXTURE'}
            self.store.event(tid, 'prompt_receipt', receipt, critical=True)
            self.models.finish(model_call, None if state=='UNKNOWN' else 1,state,'unrecognized stop reason' if state=='UNKNOWN' else None)
            model_call = None
            failures = task['spec'].get('rework_failures', 0)
            for _ in range(failures):
                self.store.rework(tid, 'simulation acceptance failed', 'simulation-only revision; no external write', 'failed')
            if failures == 3:
                state = 'PAUSED'
            elif failures:
                self.store.rework(tid, 'simulation acceptance rerun', 'simulation fixture accepted', 'passed')
            self.store.artifact(tid, 'acceptance', {'environment':'ISOLATED_FIXTURE','model':'NOT_RUN','result':state,
                                                      'receipt':receipt,
                                                      'tests':'simulation ACP lifecycle','commit':'NOT_CREATED','pr':'NOT_CREATED','ci':'NOT_RUN'})
            current = self.store.get(tid)
            if current['state'] == 'RUNNING':
                self.store.transition(tid, current['version'], state)
        except asyncio.CancelledError:
            if model_call:
                self.models.finish(model_call,None,'UNKNOWN','controller shutdown')
            self._unknown(tid, 'controller shutdown; authoritative result absent')
            raise
        except Exception as error:
            if getattr(error, 'supervisor', None) is not None:
                peer = getattr(error, 'supervisor')
                self.peers[tid] = peer
                self.store.process_identity(tid, process_identity(peer.proc))
            if model_call:
                self.models.finish(model_call,None,'UNKNOWN',type(error).__name__)
            self._unknown(tid, type(error).__name__)
        finally:
            cleaned = True
            if peer:
                try:
                    await asyncio.to_thread(peer.close)
                except Exception as error:
                    cleaned = False
                    self.store.event(tid, 'cleanup_error', {'reason':type(error).__name__, 'retained':True}, critical=True)
            if cleaned:
                self.store.cleaned(tid)
                self.peers.pop(tid, None)

    def _unknown(self, tid, reason):
        task = self.store.get(tid)
        if task['state'] == 'RUNNING':
            self.store.transition(tid, task['version'], 'UNKNOWN', {'reason':reason})

    async def call(self, name, arguments):
        if self.closed:
            return self.error('CONFLICT','controller closed')
        try:
            if len(json.dumps(arguments).encode()) > 32768:
                return self.error('LIMIT_EXCEEDED','input exceeds 32768 bytes')
            if name not in TOOLS:
                return self.error('NOT_FOUND','unknown tool')
            try:
                INPUT_VALIDATORS[name].validate(arguments)
            except ValidationError:
                return self.error('INVALID_ARGUMENT','arguments do not match tool schema')
            await self._ensure_scheduler()
            operation = name.removeprefix('bba_task_')
            if operation in ('submit', 'cancel', 'resume') and not self.settings.fixture_authorized:
                return self.error('PERMISSION_DENIED','startup fixture mutation authority absent')
            if operation == 'submit':
                if not self.settings.fixture_authorized:
                    return self.error('PERMISSION_DENIED','OBSERVE permits no protected repository writes')
                self.worktrees.validate_write_set(arguments.get('write_set'))
                snapshot = self.models.snapshot(arguments.get('model_policy_ref','simulation-default@1'))
                data = {'task':self.store.submit(arguments,policy_snapshot=snapshot)}
            elif operation == 'list':
                limit, offset = arguments.get('limit',50), arguments.get('offset',0)
                tasks = self.store.list(limit+1, offset)
                data = {'tasks':tasks[:limit], 'next_cursor':offset+limit if len(tasks)>limit else None}
            else:
                tid = arguments['task_id']
                task = self.store.get(tid)
                if operation == 'status':
                    data = {'task':task}
                elif operation == 'wait':
                    deadline = time.monotonic()+arguments.get('timeout_ms',0)/1000
                    def complete():
                        return task['state'] in TERMINAL | {'UNKNOWN','PAUSED'} and not self.store.cleanup_pending(tid)
                    while not complete() and time.monotonic()<deadline:
                        await asyncio.sleep(.01)
                        task = self.store.get(tid)
                    data = {'task':task,'timed_out':not complete()}
                elif operation == 'evidence':
                    data = self.store.evidence(tid, arguments.get('after',0), arguments.get('limit',50))
                    data['model'] = self.models.evidence(tid)
                    data['next_cursor'] = data['events'][-1]['seq'] if data['events'] else None
                elif operation == 'cancel':
                    if task['state'] in ('QUEUED','PAUSED'):
                        task = self.store.transition(tid, task['version'], 'CANCELLED')
                    elif task['state'] == 'RUNNING':
                        peer = self.peers.get(tid)
                        session = self.store.latest_session(tid)
                        prompt_dispatched = any(e['kind']=='rpc_send' and e['data'].get('method')=='session/prompt' for e in self.store.events(tid,limit=1000))
                        if peer and session and prompt_dispatched:
                            await asyncio.to_thread(peer.cancel, session['session_id'])
                        else:
                            self._unknown(tid, 'cancel before authoritative session receipt')
                        task = self.store.get(tid)
                    else:
                        raise Conflict('cancel cannot assert unresolved or terminal outcome')
                    data = {'task':task}
                elif operation == 'resume':
                    data = await self._resume(tid, task, arguments)
            result = {'ok':True,'data':data}
            OUTPUT_VALIDATORS[name].validate(result)
            if len(json.dumps(result).encode()) > 131072:
                return self.error('LIMIT_EXCEEDED','result exceeds output byte limit; request smaller page')
            return result
        except KeyError:
            return self.error('NOT_FOUND','task not found')
        except BoundaryError:
            return self.error('PERMISSION_DENIED','repository or path boundary refused')
        except ModelDenied:
            return self.error('PERMISSION_DENIED','model policy, authorization or budget refused')
        except Conflict:
            return self.error('CONFLICT','state, version, queue, ownership or budget conflict')
        except TransportUnknown:
            return self.error('UNKNOWN','transport uncertain; no automatic retry')
        except Exception:
            return self.error('INTERNAL_ERROR','controller operation failed; raw payload omitted')

    async def _resume(self, tid, task, arguments):
        if not self.settings.fixture_authorized or task['spec']['kind'] != 'simulation':
            raise BoundaryError('resume authority absent')
        if task['state'] in TERMINAL | {'PAUSED'} and self.store.cleanup_pending(tid):
            if tid in self.peers or tid in self.running:
                raise Conflict('active execution must finish cleanup first')
            self.store.reconcile_cleanup(tid)
            if self.store.cleanup_pending(tid):
                raise Conflict('process group cleanup unconfirmed')
            return {'task':self.store.get(tid),'restored':False}
        if task['state'] != 'UNKNOWN':
            raise Conflict('only UNKNOWN is inspectable; no automatic retry')
        if tid not in self.peers and tid not in self.running:
            self.store.reconcile_cleanup(tid)
        if tid in self.peers or tid in self.running or self.store.cleanup_pending(tid):
            raise Conflict('active execution must finish cleanup first')
        if 'simulation_receipt' in arguments:
            self.store.event(tid, 'authorization', {'source':'explicit simulation receipt','real_effect':False}, critical=True)
            task = self.store.reconcile(tid, arguments['simulation_receipt'], 'explicit local simulation operator receipt; never live authority')
            return {'task':task,'restored':False}
        if tid in self.peers:
            raise Conflict('active peer must finish cleanup first')
        session = self.store.latest_session(tid)
        if not session:
            raise Conflict('session receipt absent')
        self.store.require_event_capacity(tid)
        self.store.cleanup_started(tid)
        peer = None
        try:
            peer = Supervisor([sys.executable,'-m','bba_controller.sim_peer'], self.root/'worktrees'/tid, self.root/'agents'/tid,
                              lambda event:self._record(tid,event), simulation=True)
            self.peers[tid] = peer
            await asyncio.to_thread(peer.initialize)
            await asyncio.to_thread(peer.restore, session['session_id'], 'load')
            await asyncio.to_thread(peer.restore, session['session_id'], 'resume')
            self.store.event(tid, 'restored_readonly', {'session_id':session['session_id'],'replayed_prompt':False})
        except Exception as error:
            if getattr(error, 'supervisor', None) is not None:
                peer = getattr(error, 'supervisor')
                self.peers[tid] = peer
                self.store.process_identity(tid, process_identity(peer.proc))
            raise
        finally:
            if peer:
                await asyncio.to_thread(peer.close)
            self.store.cleaned(tid)
            self.peers.pop(tid, None)
        return {'task':self.store.get(tid),'restored':True}

    @staticmethod
    def error(code, message):
        return {'ok':False,'error':{'code':code,'message':message}}

    async def close(self):
        # closed fences dispatch; owner.closed means resource shutdown completed.
        # A failed attempt with live resources must remain retryable.
        async with self.close_lock:
            if self.owner.closed:
                return
            self.closed = True
            errors = []
            workers = list(self.running.values())

            async def reap(tid, peer):
                try:
                    await asyncio.to_thread(peer.close)
                except Exception as error:
                    errors.append(error)
                    try:
                        self.store.event(tid, 'cleanup_error', {'reason':type(error).__name__, 'retained':True}, critical=True)
                    except Exception as audit_error:
                        errors.append(audit_error)
                    return
                try:
                    self.store.cleaned(tid)
                except Exception as error:
                    errors.append(error)
                else:
                    self.peers.pop(tid, None)

            try:
                if self.scheduler:
                    self.scheduler.cancel()
                    results = await asyncio.gather(self.scheduler, return_exceptions=True)
                    errors.extend(r for r in results if isinstance(r, Exception))
                for tid, peer in list(self.peers.items()):
                    await reap(tid, peer)
            finally:
                # Audit failures cannot bypass joining workers or owner release.
                workers = list(set(workers) | set(self.running.values()))
                results = await asyncio.gather(*workers, return_exceptions=True)
                errors.extend(r for r in results if isinstance(r, Exception))
                # Workers may have registered a constructor-failure peer meanwhile.
                for tid, peer in list(self.peers.items()):
                    if not peer.closed:
                        await reap(tid, peer)
                if all(p.cleanup_confirmed for p in self.peers.values()) and all(w.done() for w in workers):
                    self.owner.close()
                else:
                    errors.append(TransportUnknown('owned resources remain; close must be retried'))
            if errors:
                errors[0].cleanup_errors = tuple(errors)
                errors[0].add_note('shutdown errors: ' + ', '.join(type(e).__name__ for e in errors))
                raise errors[0]
