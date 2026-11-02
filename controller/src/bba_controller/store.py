"""SQLite WAL fact store. Transactions fence projections, events and slots."""
from contextlib import contextmanager
import json
from pathlib import Path
import sqlite3
import time
import uuid

from .evidence import encoded, summary, redact

TERMINAL = {'SUCCEEDED', 'FAILED', 'CANCELLED'}
EDGES = {
    'QUEUED': {'RUNNING', 'CANCELLED', 'PAUSED'},
    'RUNNING': {'SUCCEEDED', 'FAILED', 'CANCELLED', 'UNKNOWN', 'PAUSED'},
    'UNKNOWN': set(), 'PAUSED': {'QUEUED', 'CANCELLED'},
    'SUCCEEDED': set(), 'FAILED': {'PAUSED'}, 'CANCELLED': set(),
}


class Conflict(Exception):
    pass


def uid():
    return uuid.uuid4().hex


class Connection(sqlite3.Connection):
    def __exit__(self, *args):
        try:
            return super().__exit__(*args)
        finally:
            self.close()


class Store:
    def __init__(self, path, slots=1):
        if slots not in (1, 2):
            raise ValueError('slots must be 1 or 2')
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with self.connection() as c:
            c.execute('pragma journal_mode=WAL')
            c.executescript('''
                CREATE TABLE IF NOT EXISTS config(version INTEGER PRIMARY KEY, slots INTEGER NOT NULL CHECK(slots BETWEEN 1 AND 2));
                CREATE TABLE IF NOT EXISTS repository_binding(singleton INTEGER PRIMARY KEY CHECK(singleton=1), identity TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY, state TEXT NOT NULL, version INTEGER NOT NULL, spec TEXT NOT NULL, reworks INTEGER NOT NULL DEFAULT 0, created REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS task_events(seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, task_id TEXT NOT NULL REFERENCES tasks(id), kind TEXT NOT NULL, data TEXT NOT NULL, created REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS execution_sessions(id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), session_id TEXT NOT NULL, peer TEXT NOT NULL, created REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS task_artifacts(id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), kind TEXT NOT NULL, data TEXT NOT NULL, created REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS scheduler_slots(task_id TEXT PRIMARY KEY REFERENCES tasks(id), acquired REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS execution_cleanup(task_id TEXT PRIMARY KEY REFERENCES tasks(id), pending INTEGER NOT NULL, process TEXT);
            ''')
            if 'critical' not in [r[1] for r in c.execute('pragma table_info(task_events)')]:
                c.execute('ALTER TABLE task_events ADD COLUMN critical INTEGER NOT NULL DEFAULT 0')
            c.execute('CREATE TABLE IF NOT EXISTS safety_retention(task_id TEXT PRIMARY KEY, dropped INTEGER NOT NULL DEFAULT 0)')
            c.execute('INSERT OR IGNORE INTO config VALUES(1, ?)', (slots,))
            if c.execute('SELECT version, slots FROM config').fetchall() != [(1, slots)]:
                raise Conflict('unsupported config version or slot configuration mismatch')
        self.path.chmod(0o600)

    def connection(self):
        c = sqlite3.connect(self.path, timeout=10, factory=Connection)
        c.execute('pragma foreign_keys=ON')
        c.execute('pragma busy_timeout=10000')
        return c

    def bind_repository(self, identity):
        with self.tx() as c:
            c.execute('INSERT OR IGNORE INTO repository_binding VALUES(1,?)', (identity,))
            if c.execute('SELECT identity FROM repository_binding WHERE singleton=1').fetchone()[0] != identity:
                raise Conflict('state repository binding mismatch')

    @contextmanager
    def tx(self):
        c = self.connection()
        try:
            c.execute('BEGIN IMMEDIATE')
            yield c
            c.commit()
        except BaseException:
            c.rollback()
            raise
        finally:
            c.close()

    def _event(self, c, task, kind, data, *, critical=False):
        if not critical and c.execute('SELECT count(*) FROM task_events WHERE task_id=? AND critical=0', (task,)).fetchone()[0] >= 2000:
            raise Conflict('event quota reached')
        if critical:
            rows = c.execute('SELECT seq FROM task_events WHERE task_id=? AND critical=1 ORDER BY seq', (task,)).fetchall()
            if len(rows) >= 128:
                c.execute('DELETE FROM task_events WHERE seq=?', (rows[0][0],))
                c.execute('INSERT INTO safety_retention VALUES(?,1) ON CONFLICT(task_id) DO UPDATE SET dropped=dropped+1', (task,))
            dropped = c.execute('SELECT dropped FROM safety_retention WHERE task_id=?', (task,)).fetchone()
            data = {**data, 'safety_events_dropped':dropped[0] if dropped else 0}
        c.execute('INSERT INTO task_events(id, task_id, kind, data, created, critical) VALUES(?,?,?,?,?,?)',
                  (uid(), task, kind, encoded(data), time.time(), int(critical)))

    def require_event_capacity(self, task, reserve=32):
        with self.connection() as c:
            count = c.execute('SELECT count(*) FROM task_events WHERE task_id=? AND critical=0', (task,)).fetchone()[0]
            if count + reserve > 2000:
                raise Conflict('insufficient ordinary audit capacity before process launch')

    def submit(self, spec, policy_snapshot=None):
        with self.tx() as c:
            if c.execute("SELECT count(*) FROM tasks WHERE state NOT IN ('SUCCEEDED','FAILED','CANCELLED')").fetchone()[0] >= 100:
                raise Conflict('queue full')
            for dep in spec.get('dependencies', []):
                if not c.execute('SELECT 1 FROM tasks WHERE id=?', (dep,)).fetchone():
                    raise Conflict('dependency missing')
            task = uid()
            c.execute('INSERT INTO tasks VALUES(?,?,?,?,0,?)', (task, 'QUEUED', 1, json.dumps(redact(spec)), time.time()))
            if policy_snapshot is not None:
                c.execute('INSERT INTO task_model_policy VALUES(?,?)',(task,json.dumps(policy_snapshot,sort_keys=True)))
                c.execute('INSERT INTO model_budget VALUES(?,?,0,0)',(task,policy_snapshot['policy']['model_budget']['limit_units']))
                self._event(c,task,'model_policy_snapshot',{'policy':policy_snapshot['policy']['id'],'version':policy_snapshot['policy']['version'],'summary':summary(policy_snapshot)})
            self._event(c, task, 'submitted', {'input': summary(spec), 'authorization': 'simulation-only'})
        return self.get(task)

    def _get(self, c, task):
        c.row_factory = sqlite3.Row
        row = c.execute('SELECT * FROM tasks WHERE id=?', (task,)).fetchone()
        if row is None:
            raise KeyError(task)
        result = dict(row)
        result['spec'] = json.loads(result['spec'])
        return result

    def get(self, task):
        with self.connection() as c:
            return self._get(c, task)

    def list(self, limit=50, offset=0):
        with self.connection() as c:
            ids = [r[0] for r in c.execute('SELECT id FROM tasks ORDER BY created,id LIMIT ? OFFSET ?', (limit, offset))]
            return [self._get(c, t) for t in ids]

    def queued(self, after=None, limit=25):
        """Keyset pages exclude history; failed claims cannot hide later work."""
        with self.connection() as c:
            ids = [r[0] for r in c.execute(
                "SELECT id FROM tasks WHERE state='QUEUED' AND (created,id)>(?,?) ORDER BY created,id LIMIT ?",
                (*(after or (-1, '')), limit))]
            return [self._get(c, t) for t in ids]

    def _transition(self, c, task, version, state, data=None, reconcile=False):
        old = self._get(c, task)
        if old['version'] != version or (not reconcile and state not in EDGES[old['state']]):
            raise Conflict('stale version or illegal transition')
        n = c.execute('UPDATE tasks SET state=?,version=version+1 WHERE id=? AND version=?', (state, task, version)).rowcount
        if n != 1:
            raise Conflict('CAS failed')
        self._event(c, task, 'state', {'from': old['state'], 'to': state, **(data or {})}, critical=state!='RUNNING')
        if (state in TERMINAL or state == 'PAUSED') and not self._cleanup_pending(c, task):
            c.execute('DELETE FROM scheduler_slots WHERE task_id=?', (task,))

    def transition(self, task, version, state, data=None):
        with self.tx() as c:
            self._transition(c, task, version, state, data)
        return self.get(task)

    def _cleanup_pending(self, c, task):
        return bool(c.execute('SELECT 1 FROM execution_cleanup WHERE task_id=? AND pending=1', (task,)).fetchone())

    def cleanup_pending(self, task):
        with self.connection() as c:
            return self._cleanup_pending(c, task)

    def cleanup_started(self, task):
        with self.tx() as c:
            if self._cleanup_pending(c, task):
                raise Conflict('cleanup already pending')
            c.execute('INSERT OR REPLACE INTO execution_cleanup VALUES(?,1,NULL)', (task,))

    def process_started(self, task, data):
        with self.tx() as c:
            c.execute('UPDATE execution_cleanup SET process=? WHERE task_id=? AND pending=1', (json.dumps(data['identity']), task))
            self._event(c, task, 'process_start', data)

    def process_identity(self, task, identity):
        with self.tx() as c:
            c.execute('UPDATE execution_cleanup SET process=? WHERE task_id=? AND pending=1', (json.dumps(identity), task))
            self._event(c, task, 'audit_error', {'reason':'process construction/cleanup failed','identity':identity}, critical=True)

    def reconcile_cleanup(self, task):
        from .acp import group_alive
        with self.connection() as c:
            row = c.execute('SELECT process FROM execution_cleanup WHERE task_id=? AND pending=1', (task,)).fetchone()
        if row and row[0] and not group_alive(json.loads(row[0])):
            self.cleaned(task)

    def cleaned(self, task):
        with self.tx() as c:
            c.execute('UPDATE execution_cleanup SET pending=0 WHERE task_id=?', (task,))
            self._event(c, task, 'cleanup_confirmed', {'process_group_terminated':True}, critical=True)
            if self._get(c, task)['state'] in TERMINAL | {'PAUSED'}:
                c.execute('DELETE FROM scheduler_slots WHERE task_id=?', (task,))

    def claim(self, task, cleanup_required=False):
        with self.tx() as c:
            t = self._get(c, task)
            if t['state'] != 'QUEUED':
                return False
            active = [self._get(c, r[0]) for r in c.execute('SELECT task_id FROM scheduler_slots').fetchall()]
            slots = c.execute('SELECT slots FROM config WHERE version=1').fetchone()[0]
            if len(active) >= slots:
                return False
            for dep in t['spec'].get('dependencies', []):
                if self._get(c, dep)['state'] != 'SUCCEEDED' or self._cleanup_pending(c, dep):
                    return False
            writes = t['spec'].get('write_set')
            for other in active:
                theirs = other['spec'].get('write_set')
                if writes is None or theirs is None:
                    return False
                if any(a == b or a.startswith(b + '/') or b.startswith(a + '/') for a in writes for b in theirs):
                    return False
            c.execute('INSERT INTO scheduler_slots VALUES(?,?)', (task, time.time()))
            if cleanup_required:
                c.execute('INSERT OR REPLACE INTO execution_cleanup VALUES(?,1,NULL)', (task,))
            self._transition(c, task, t['version'], 'RUNNING')
        return True

    def slot_count(self):
        with self.connection() as c:
            return c.execute('SELECT count(*) FROM scheduler_slots').fetchone()[0]

    def recover(self):
        with self.tx() as c:
            rows = c.execute("SELECT id,version FROM tasks WHERE state='RUNNING'").fetchall()
            for task, version in rows:
                self._transition(c, task, version, 'UNKNOWN', {'reason': 'controller restart; no automatic replay'})

    def reconcile(self, task, state, authority):
        if state not in TERMINAL or not authority:
            raise Conflict('authoritative terminal receipt required')
        with self.tx() as c:
            t = self._get(c, task)
            if t['state'] != 'UNKNOWN':
                raise Conflict('reconcile only UNKNOWN')
            if self._cleanup_pending(c, task):
                raise Conflict('execution cleanup not confirmed')
            self._transition(c, task, t['version'], state, {'authority': authority}, reconcile=True)
        return self.get(task)

    def event(self, task, kind, data, *, critical=False):
        with self.tx() as c:
            self._get(c, task)
            self._event(c, task, kind, data, critical=critical)

    def events(self, task, after=0, limit=100):
        with self.connection() as c:
            c.row_factory = sqlite3.Row
            return [{**dict(r), 'data': json.loads(r['data'])} for r in c.execute('SELECT seq,id,task_id,kind,data,created FROM task_events WHERE task_id=? AND seq>? ORDER BY seq LIMIT ?', (task, after, limit))]

    def session(self, task, session_id, peer):
        with self.tx() as c:
            c.execute('INSERT INTO execution_sessions VALUES(?,?,?,?,?)', (uid(), task, session_id, peer, time.time()))
            self._event(c, task, 'session', {'session_id': session_id, 'peer': peer})

    def latest_session(self, task):
        with self.connection() as c:
            c.row_factory = sqlite3.Row
            row = c.execute('SELECT * FROM execution_sessions WHERE task_id=? ORDER BY created DESC LIMIT 1', (task,)).fetchone()
            return dict(row) if row else None

    def artifact(self, task, kind, data):
        with self.tx() as c:
            if c.execute('SELECT count(*) FROM task_artifacts WHERE task_id=?', (task,)).fetchone()[0] >= 100:
                raise Conflict('artifact quota reached')
            c.execute('INSERT INTO task_artifacts VALUES(?,?,?,?,?)', (uid(), task, kind, encoded(data), time.time()))
            self._event(c, task, 'artifact', {'kind': kind, 'summary': summary(data)}, critical=kind=='acceptance')

    def evidence(self, task, after=0, limit=50):
        with self.connection() as c:
            c.row_factory = sqlite3.Row
            artifacts = [{**dict(r), 'data': json.loads(r['data'])} for r in c.execute('SELECT * FROM task_artifacts WHERE task_id=? ORDER BY created LIMIT 100', (task,))]
        return {'task': self.get(task), 'events': self.events(task, after, limit), 'artifacts': artifacts, 'session': self.latest_session(task)}

    def rework(self, task, reason, change, result):
        with self.tx() as c:
            t = self._get(c, task)
            if t['reworks'] >= 3:
                raise Conflict('rework budget exhausted')
            n = t['reworks'] + 1
            c.execute('UPDATE tasks SET reworks=? WHERE id=?', (n, task))
            self._event(c, task, 'rework', {'round': n, 'reason': reason, 'change': change, 'result': result})
            if n == 3 and result != 'passed':
                if t['state'] == 'UNKNOWN':
                    self._event(c, task, 'pause_required', {'reason': 'UNKNOWN retains slot pending reconciliation'})
                else:
                    self._transition(c, task, t['version'], 'PAUSED')
