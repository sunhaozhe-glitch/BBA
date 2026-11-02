#!/usr/bin/env python3
"""Record bounded isolated commands and exact owned Docker exec cleanup."""
import json, os, re, subprocess, sys, time
from pathlib import Path
ROOT=Path(__file__).resolve().parent.parent
OUT=Path((ROOT/'.content-hardening-fix-path').read_text())
label=sys.argv[1];cmd=sys.argv[2:];start=time.time()
# Docker events identify ephemeral execution containers without renaming scripts.
events=OUT/(label+'-docker-events.jsonl')
f=events.open('w');ev=subprocess.Popen(['docker','events','--filter','type=container','--format','{{json .}}'],stdout=f,stderr=subprocess.DEVNULL)
p=subprocess.Popen(cmd,cwd=ROOT,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,start_new_session=True)
try: output,_=p.communicate(timeout=540)
except subprocess.TimeoutExpired:
 import signal
 os.killpg(p.pid,signal.SIGTERM);output,_=p.communicate(timeout=10)
finally:
 ev.terminate();ev.wait(timeout=5);f.close()
(OUT/(label+'.log')).write_text(output)
rec={'command':cmd,'exit':p.returncode,'pid':p.pid,'elapsed_seconds':time.time()-start,'log':str(OUT/(label+'.log'))}
with (OUT/'commands.jsonl').open('a') as f:f.write(json.dumps(rec)+'\n')
checks=[]
names=sorted(set(re.findall(r'^\+ FIXTURE_NAME=(bba-p004-011-[0-9]+-[0-9]+)$',output,re.M)))
for name in names:
 for kind,target in [('container',name),('volume',name+'-socket')]:
  q=subprocess.run(['docker',kind,'inspect',target],capture_output=True,text=True)
  expected=f'Error response from daemon: No such container: {target}' if kind=='container' else f'Error response from daemon: get {target}: no such volume'
  checks.append({'kind':kind,'target':target,'exit':q.returncode,'stdout':q.stdout,'stderr':q.stderr,'absent':q.returncode==1 and q.stdout.strip()=='[]' and q.stderr.strip()==expected})
# Only inspect containers whose mount set proves this exact repository and one
# of this command's fixture volumes; no broad cleanup of other runs.
for line in events.read_text().splitlines():
 try:e=json.loads(line)
 except json.JSONDecodeError:continue
 if e.get('Action')!='create':continue
 cid=e.get('Actor',{}).get('ID','');attrs=e.get('Actor',{}).get('Attributes',{})
 if attrs.get('image')!='golang:1.26':continue
 # Event ID readback is observational; never delete an unproven container.
 q=subprocess.run(['docker','container','inspect',cid],capture_output=True,text=True)
 checks.append({'kind':'execution_candidate','target':cid,'exit':q.returncode,'stdout':q.stdout,'stderr':q.stderr,'absent':q.returncode==1 and 'No such container' in q.stderr})
checks.append({'kind':'execution_process','target':p.pid,'absent':not Path('/proc/'+str(p.pid)).exists()})
(OUT/(label+'-cleanup.json')).write_text(json.dumps(checks,indent=2))
print(json.dumps(rec));print(output[-18000:]);print('CLEANUP',json.dumps(checks))
sys.exit(p.returncode if p.returncode else (1 if any(not x['absent'] for x in checks) else 0))
