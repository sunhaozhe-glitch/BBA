#!/usr/bin/env python3
"""Bounded, recorded isolated fixture commands. No production DSN."""
import sys,json,subprocess,time,re
from pathlib import Path
ROOT=Path(__file__).resolve().parent.parent
OUT=Path((ROOT/'.content-hardening-path').read_text())
label=sys.argv[1];cmd=sys.argv[2:];start=time.time()
p=subprocess.run(cmd,cwd=ROOT,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True)
(OUT/(label+'.log')).write_text(p.stdout)
record={'command':cmd,'exit':p.returncode,'elapsed_seconds':time.time()-start,'log':str(OUT/(label+'.log'))}
with (OUT/'commands.jsonl').open('a') as f:f.write(json.dumps(record)+'\n')
print(json.dumps(record));print(p.stdout[-14000:])
names=sorted(set(re.findall(r'^\+ FIXTURE_NAME=(bba-p004-011-[0-9]+-[0-9]+)$',p.stdout,re.M)))
checks=[]
for name in names:
 for kind,target in [('container',name),('volume',name+'-socket')]:
  q=subprocess.run(['docker',kind,'inspect',target],capture_output=True,text=True)
  expected=f'Error response from daemon: No such container: {target}' if kind=='container' else f'Error response from daemon: get {target}: no such volume'
  checks.append({'kind':kind,'target':target,'exit':q.returncode,'stdout':q.stdout,'stderr':q.stderr,'absent':q.returncode==1 and q.stdout.strip()=='[]' and q.stderr.strip()==expected})
(OUT/(label+'-cleanup.json')).write_text(json.dumps(checks,indent=2))
if names:print('EXACT_CLEANUP',all(x['absent'] for x in checks))
sys.exit(p.returncode if p.returncode else (1 if checks and not all(x['absent'] for x in checks) else 0))
