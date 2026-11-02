#!/usr/bin/env python3
from pathlib import Path
import subprocess,sys,json,time
r=Path(__file__).resolve().parents[1]
out=Path(sys.argv[1]);out.mkdir(parents=True,exist_ok=True)
commands=[['python3','tools/build-product.py'],['node','--check','frontend/app.js'],['bash','tools/kernel.sh','test-all','-race=false']]
results=[]
for i,cmd in enumerate(commands):
 start=time.time()
 with (out/f'verify-{i}.log').open('w') as log:
  p=subprocess.run(cmd,cwd=r,stdout=log,stderr=subprocess.STDOUT)
  log.write(f'\nEXIT_CODE={p.returncode}\n')
 results.append(dict(command=cmd,exit_code=p.returncode,seconds=round(time.time()-start,2)))
 (out/'test-exits.json').write_text(json.dumps(results,ensure_ascii=False,indent=2))
 if p.returncode:sys.exit(p.returncode)
