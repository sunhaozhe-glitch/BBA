"""Startup-only model catalog contract. No credentials or endpoints accepted."""
from .contracts import obj

NAME = {'type':'string','pattern':'^[a-z0-9][a-z0-9-]{0,63}$'}
VERSION = {'type':'integer','minimum':1,'maximum':999999}
REF = {'type':'string','pattern':'^[a-z0-9][a-z0-9-]{0,63}@[1-9][0-9]{0,5}$'}
ROLES = ['builder','reviewer','advisor','planner','judge','summarizer','memory','commit','title','compaction']
ROLE = {'enum':ROLES}
PROFILE = obj({'id':NAME,'version':VERSION,'provider':NAME,'is_local':{'type':'boolean'},'simulation':{'type':'boolean'},'enabled':{'type':'boolean'},
               'model_role':{'type':'array','minItems':1,'maxItems':10,'uniqueItems':True,'items':ROLE}},
              ['id','version','provider','is_local','simulation','enabled','model_role'])
POLICY = obj({'id':NAME,'version':VERSION,'provider_allowlist':{'type':'array','maxItems':32,'uniqueItems':True,'items':NAME},
              'model_role':{'type':'object','propertyNames':ROLE,'additionalProperties':{'type':'array','minItems':1,'maxItems':32,'uniqueItems':True,'items':REF}},
              'default_profile':REF,'default_role':ROLE,
              'model_budget':obj({'limit_units':{'type':'integer','minimum':0,'maximum':1000000},'unit':{'const':'simulation-credit'}},['limit_units','unit']),
              'fallback_policy':obj({'mode':{'const':'deny'},'approved_edges':{'type':'array','maxItems':0}},['mode','approved_edges']),
              'switch_policy':{'const':'explicit-authorized-role-only'}},
             ['id','version','provider_allowlist','model_role','default_profile','default_role','model_budget','fallback_policy','switch_policy'])
CONFIG = {'$schema':'https://json-schema.org/draft/2020-12/schema',**obj({'version':{'const':1},'profiles':{'type':'array','minItems':1,'maxItems':32,'items':PROFILE},
          'policies':{'type':'array','minItems':1,'maxItems':32,'items':POLICY}}, ['version','profiles','policies'])}
