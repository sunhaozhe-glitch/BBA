"""Versioned JSON Schema contracts shared by MCP and local dispatch."""
ID = {'type': 'string', 'pattern': '^[a-f0-9]{32}$'}
SHA = {'type': 'string', 'pattern': '^[a-f0-9]{40}$'}
ERROR_CODES = ['INVALID_ARGUMENT','PERMISSION_DENIED','NOT_FOUND','CONFLICT','LIMIT_EXCEEDED','UNKNOWN','INTERNAL_ERROR']


def obj(properties, required=()):
    return {'type':'object','properties':properties,'required':list(required),'additionalProperties':False}


TASK = obj({'id':ID,'state':{'enum':['QUEUED','RUNNING','UNKNOWN','PAUSED','SUCCEEDED','FAILED','CANCELLED']},
            'version':{'type':'integer','minimum':1},'spec':{'type':'object'},'reworks':{'type':'integer','minimum':0,'maximum':3},'created':{'type':'number'}},
           ['id','state','version','spec','reworks','created'])
EVENT = obj({'seq':{'type':'integer'},'id':ID,'task_id':ID,'kind':{'type':'string'},'data':{'type':'object'},'created':{'type':'number'}}, ['seq','id','task_id','kind','data','created'])
ARTIFACT = obj({'id':ID,'task_id':ID,'kind':{'type':'string'},'data':{'type':'object'},'created':{'type':'number'}}, ['id','task_id','kind','data','created'])
SESSION = obj({'id':ID,'task_id':ID,'session_id':{'type':'string','maxLength':256},'peer':{'enum':['simulation','real-metadata-only']},'created':{'type':'number'}}, ['id','task_id','session_id','peer','created'])
DATA = obj({'task':TASK,'tasks':{'type':'array','maxItems':50,'items':TASK},'events':{'type':'array','maxItems':100,'items':EVENT},
            'model':obj({'snapshot':{'type':['object','null']},'budget':{'type':['object','null']},'calls':{'type':'array','maxItems':100,'items':{'type':'object'}}}, ['snapshot','budget','calls']),
            'artifacts':{'type':'array','maxItems':100,'items':ARTIFACT},'session':{'anyOf':[SESSION,{'type':'null'}]},
            'next_cursor':{'type':['integer','null']},'restored':{'type':'boolean'},'timed_out':{'type':'boolean'}})
OUTPUT = {'$schema':'https://json-schema.org/draft/2020-12/schema','type':'object', 'oneOf':[
    obj({'ok':{'const':True},'data':DATA}, ['ok','data']),
    obj({'ok':{'const':False},'error':obj({'code':{'enum':ERROR_CODES},'message':{'type':'string','maxLength':256}}, ['code','message'])}, ['ok','error'])]}

TOOLS = {
    'bba_task_submit': obj({'request_id':{'type':'string','pattern':'^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$','not':{'pattern':'[^A-Za-z0-9_.:-]'}},'kind':{'const':'simulation'},'base_sha':SHA,'write_set':{'anyOf':[{'type':'null'},{'type':'array','maxItems':32,'uniqueItems':True,'items':{'type':'string','maxLength':256}}]},
                            'dependencies':{'type':'array','maxItems':32,'uniqueItems':True,'items':ID},
                            'behavior':{'enum':['success','hang','exit','cancel','malformed','overflow']},'rework_failures':{'type':'integer','minimum':0,'maximum':3},
                            'model_policy_ref':{'type':'string','pattern':'^[a-z0-9][a-z0-9-]{0,63}@[1-9][0-9]{0,5}$'}}, ['request_id','kind','base_sha']),
    'bba_task_status':obj({'task_id':ID}, ['task_id']),
    'bba_task_wait':obj({'task_id':ID,'timeout_ms':{'type':'integer','minimum':0,'maximum':1000}}, ['task_id']),
    'bba_task_cancel':obj({'task_id':ID}, ['task_id']),
    'bba_task_resume':obj({'task_id':ID,'simulation_receipt':{'enum':['SUCCEEDED','FAILED','CANCELLED']}}, ['task_id']),
    'bba_task_list':obj({'limit':{'type':'integer','minimum':1,'maximum':50},'offset':{'type':'integer','minimum':0,'maximum':1000000}}),
    'bba_task_evidence':obj({'task_id':ID,'after':{'type':'integer','minimum':0},'limit':{'type':'integer','minimum':1,'maximum':100}}, ['task_id']),
}

# Per-tool result shapes keep discovery strict while allowing a shared error envelope.
RESULT_FIELDS = {
    'bba_task_submit': ['task'], 'bba_task_status':['task'], 'bba_task_wait':['task','timed_out'],
    'bba_task_cancel':['task'], 'bba_task_resume':['task','restored'], 'bba_task_list':['tasks','next_cursor'],
    'bba_task_evidence':['task','events','artifacts','session','next_cursor','model'],
}


def output_for(name):
    fields = RESULT_FIELDS[name]
    return {'$schema':OUTPUT['$schema'], 'type':'object','oneOf':[
        obj({'ok':{'const':True},'data':obj({k:DATA['properties'][k] for k in fields}, fields)}, ['ok','data']), OUTPUT['oneOf'][1]]}
