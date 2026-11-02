"""Versioned multi-profile model policy; only explicit local simulations execute.

Hermes owns policy selection. This router never invokes a provider or grants tools.
"""
import copy
import json
import re
import time
from jsonschema import Draft202012Validator, ValidationError
from .model_contracts import CONFIG

from .evidence import encoded
from .store import uid


class ModelDenied(Exception):
    pass


def default_config():
    config = {
        'version': 1,
        'profiles': [
            {'id':'mock-local-builder','version':1,'provider':'mock-local','is_local':True,'simulation':True,'enabled':True,'model_role':['builder']},
            {'id':'mock-local-reviewer','version':1,'provider':'mock-local','is_local':True,'simulation':True,'enabled':True,'model_role':['reviewer']},
            {'id':'reserved-advanced','version':1,'provider':'reserved-external','is_local':False,'simulation':False,'enabled':False,'model_role':['advisor']},
        ],
        'policies': [
            {'id':'simulation-default','version':1,'provider_allowlist':['mock-local'],
             'model_role':{'builder':['mock-local-builder@1'],'reviewer':['mock-local-reviewer@1']},
             'default_profile':'mock-local-builder@1','default_role':'builder',
             'model_budget':{'limit_units':10,'unit':'simulation-credit'},
             'fallback_policy':{'mode':'deny','approved_edges':[]}, 'switch_policy':'explicit-authorized-role-only'},
        ],
    }
    config['policies'].append({**copy.deepcopy(config['policies'][0]),'id':'simulation-review',
                               'default_profile':'mock-local-reviewer@1','default_role':'reviewer'})
    return config


def canonical(value):
    return json.dumps(value,sort_keys=True,separators=(',',':'))


class ModelRouter:
    def __init__(self, store, config=None, register_existing=False):
        self.store = store
        config = copy.deepcopy(config if config is not None else default_config())
        try:
            Draft202012Validator(CONFIG).validate(config)
        except ValidationError as error:
            raise ModelDenied('invalid versioned model configuration') from error
        with self.store.connection() as c:
            c.executescript('''
                CREATE TABLE IF NOT EXISTS model_profiles(profile TEXT NOT NULL, version INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(profile,version));
                CREATE TABLE IF NOT EXISTS model_policies(policy TEXT NOT NULL, version INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(policy,version));
                CREATE TABLE IF NOT EXISTS task_model_policy(task_id TEXT PRIMARY KEY REFERENCES tasks(id), snapshot TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS model_budget(task_id TEXT PRIMARY KEY REFERENCES tasks(id), limit_units INTEGER NOT NULL CHECK(limit_units>=0), reserved_units INTEGER NOT NULL DEFAULT 0 CHECK(reserved_units>=0), spent_units INTEGER NOT NULL DEFAULT 0 CHECK(spent_units>=0));
                CREATE TABLE IF NOT EXISTS model_call_events(id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), session_id TEXT NOT NULL, profile TEXT NOT NULL, profile_version INTEGER NOT NULL, role TEXT NOT NULL, start REAL NOT NULL, end REAL, outcome TEXT NOT NULL, failure_reason TEXT, reserved_units INTEGER NOT NULL, actual_units INTEGER);
            ''')
        with self.store.tx() as c:
            for profile in config.get('profiles',[]):
                self._register(c,'model_profiles','profile',profile)
            for policy in config.get('policies',[]):
                self._register(c,'model_policies','policy',policy)

    def _register(self,c,table,key,value):
        if not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,63}',value.get('id','')) or type(value.get('version')) is not int or value['version'] < 1:
            raise ModelDenied('invalid model registry identifier')
        if len(canonical(value).encode())>16384:
            raise ModelDenied('model registry entry too large')
        raw=canonical(value)
        row=c.execute(f'SELECT data FROM {table} WHERE {key}=? AND version=?',(value['id'],value['version'])).fetchone()
        if row and row[0]!=raw:
            raise ModelDenied('immutable model registry version changed')
        c.execute(f'INSERT OR IGNORE INTO {table} VALUES(?,?,?)',(value['id'],value['version'],raw))

    def snapshot(self, policy_ref='simulation-default@1'):
        try:
            name,version=policy_ref.rsplit('@',1)
            version=int(version)
        except (ValueError,AttributeError) as e:
            raise ModelDenied('invalid policy reference') from e
        with self.store.connection() as c:
            row=c.execute('SELECT data FROM model_policies WHERE policy=? AND version=?',(name,version)).fetchone()
            if not row:
                raise ModelDenied('unregistered policy')
            policy=json.loads(row[0])
            profiles={}
            for references in policy.get('model_role',{}).values():
                for reference in references:
                    profile,pversion=reference.rsplit('@',1)
                    record=c.execute('SELECT data FROM model_profiles WHERE profile=? AND version=?',(profile,int(pversion))).fetchone()
                    if not record:
                        raise ModelDenied('unregistered policy profile')
                    profiles[reference]=json.loads(record[0])
        budget=policy.get('model_budget',{})
        if policy['default_profile'] not in policy['model_role'].get(policy['default_role'],[]):
            raise ModelDenied('default profile is not authorized for default role')
        if type(budget.get('limit_units')) is not int or budget['limit_units']<0 or budget.get('unit')!='simulation-credit':
            raise ModelDenied('unsupported model budget')
        snapshot={'version':1,'policy':policy,'profiles':profiles,'environment':'SIMULATION_ONLY'}
        if len(canonical(snapshot).encode())>32768:
            raise ModelDenied('policy snapshot limit')
        return snapshot

    def begin(self, task_id, session_id, profile, role, reserve_units, *, fallback=False):
        reason=None
        call_id=None
        with self.store.tx() as c:
            task=self.store._get(c,task_id)
            row=c.execute('SELECT snapshot FROM task_model_policy WHERE task_id=?',(task_id,)).fetchone()
            snapshot=json.loads(row[0]) if row else None
            if not snapshot:
                reason='task policy snapshot missing'
            elif task['state']!='RUNNING' or task['spec'].get('kind')!='simulation':
                reason='only running simulation tasks may select a model'
            elif not c.execute("SELECT 1 FROM execution_sessions WHERE task_id=? AND session_id=? AND peer='simulation'",(task_id,session_id)).fetchone():
                reason='unconfirmed simulation session'
            else:
                policy=snapshot['policy']
                references=policy.get('model_role',{}).get(role,[])
                matches = [ref for ref in references if ref == profile or ('@' not in profile and ref.split('@')[0] == profile)]
                selected = matches[0] if len(matches) == 1 else None
                definition=snapshot['profiles'].get(selected) if selected else None
                if not definition:
                    reason='profile or role unauthorized'
                elif definition.get('enabled') is not True or definition.get('is_local') is not True or definition.get('simulation') is not True:
                    reason='phase 1 permits explicit local simulation profiles only'
                elif definition.get('provider') not in policy.get('provider_allowlist',[]):
                    reason='provider outside task allowlist'
                elif role not in definition.get('model_role',[]):
                    reason='profile role unauthorized'
                elif fallback:
                    # No automatic fallback path exists in phase 1, even if malformed config asks for it.
                    reason='fallback denied; separate explicit selection required'
                elif type(reserve_units) is not int or reserve_units <= 0:
                    reason='invalid model reservation'
                else:
                    budget=c.execute('SELECT limit_units,reserved_units,spent_units FROM model_budget WHERE task_id=?',(task_id,)).fetchone()
                    if not budget or budget[1]+budget[2]+reserve_units>budget[0]:
                        reason='model budget exhausted'
                    elif c.execute("SELECT 1 FROM model_call_events WHERE task_id=? AND outcome IN ('STARTED','UNKNOWN')",(task_id,)).fetchone():
                        reason='prior model call unresolved'
                    elif c.execute('SELECT count(*) FROM model_call_events WHERE task_id=?',(task_id,)).fetchone()[0]>=100:
                        reason='model call quota reached'
                    else:
                        previous=c.execute('SELECT profile,role,profile_version FROM model_call_events WHERE task_id=? ORDER BY start DESC LIMIT 1',(task_id,)).fetchone()
                        profile = definition['id']
                        call_id=uid()
                        c.execute('UPDATE model_budget SET reserved_units=reserved_units+? WHERE task_id=?',(reserve_units,task_id))
                        c.execute('INSERT INTO model_call_events VALUES(?,?,?,?,?,?,?,?,?,?,?,NULL)',
                                  (call_id,task_id,session_id,profile,definition['version'],role,time.time(),None,'STARTED',None,reserve_units))
                        self.store._event(c,task_id,'model_call_start',{'call_id':call_id,'session_id':session_id,'profile':profile,'profile_version':definition['version'],'model_role':role,'reserve_units':reserve_units,'policy':policy['id'],'policy_version':policy['version']})
                        if previous and (previous[0],previous[2])!=(profile,definition['version']):
                            self.store._event(c,task_id,'model_switch',{'from':previous[0],'to':profile,'from_ref':f'{previous[0]}@{previous[2]}','to_ref':selected,'from_role':previous[1],'to_role':role,'call_id':call_id,'authorization':'immutable task policy; explicit selection','fallback':False})
            if reason:
                self.store._event(c,task_id,'model_selection_denied',{'profile':profile,'model_role':role,'session_id':session_id,'reason':reason,'fallback':fallback})
        if reason:
            raise ModelDenied(reason)
        return call_id

    def finish(self,call_id,actual_units,outcome,failure_reason=None):
        if outcome not in ('SUCCEEDED','FAILED','CANCELLED','UNKNOWN'):
            raise ModelDenied('invalid model outcome')
        with self.store.tx() as c:
            c.row_factory=__import__('sqlite3').Row
            record=c.execute('SELECT * FROM model_call_events WHERE id=?',(call_id,)).fetchone()
            if not record or record['outcome']!='STARTED':
                raise ModelDenied('model call already settled or unresolved')
            if outcome!='UNKNOWN' and (type(actual_units) is not int or actual_units<0 or actual_units>record['reserved_units']):
                raise ModelDenied('invalid settlement exceeds reservation')
            c.execute('UPDATE model_call_events SET end=?,outcome=?,failure_reason=?,actual_units=? WHERE id=?',
                      (time.time(),outcome,encoded({'reason':failure_reason}) if failure_reason else None,actual_units,call_id))
            if outcome!='UNKNOWN':
                c.execute('UPDATE model_budget SET reserved_units=reserved_units-?,spent_units=spent_units+? WHERE task_id=?',(record['reserved_units'],actual_units,record['task_id']))
            self.store._event(c,record['task_id'],'model_call_end',{'call_id':call_id,'session_id':record['session_id'],'profile':record['profile'],'model_role':record['role'],'outcome':outcome,'failure_reason':failure_reason,'actual_units':actual_units,'reservation_retained':outcome=='UNKNOWN'}, critical=True)

    def recover(self):
        with self.store.tx() as c:
            rows=c.execute("SELECT id,task_id FROM model_call_events WHERE outcome='STARTED'").fetchall()
            for call_id,task in rows:
                c.execute("UPDATE model_call_events SET outcome='UNKNOWN',end=?,failure_reason=? WHERE id=?",(time.time(),encoded({'reason':'controller restart; no replay'}),call_id))
                self.store._event(c,task,'model_call_recovered_unknown',{'call_id':call_id,'reservation_retained':True}, critical=True)

    def evidence(self,task_id):
        with self.store.connection() as c:
            c.row_factory=__import__('sqlite3').Row
            row=c.execute('SELECT snapshot FROM task_model_policy WHERE task_id=?',(task_id,)).fetchone()
            budget=c.execute('SELECT limit_units,reserved_units,spent_units FROM model_budget WHERE task_id=?',(task_id,)).fetchone()
            calls=[]
            for record in c.execute('SELECT * FROM model_call_events WHERE task_id=? ORDER BY start LIMIT 100',(task_id,)):
                value=dict(record)
                if value['failure_reason']:
                    value['failure_reason']=json.loads(value['failure_reason'])['reason']
                calls.append(value)
            return {'snapshot':json.loads(row[0]) if row else None,'budget':dict(budget) if budget else None,'calls':calls}
