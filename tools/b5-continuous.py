#!/usr/bin/env python3
"""Network-none product proxy integration. Alias exists only inside owned container."""
from pathlib import Path
import json,os,re,shlex,subprocess,sys
ROOT=Path(__file__).resolve().parents[1]
def main():
 out=Path(sys.argv[1]).absolute();assert out.resolve()==out and out.is_relative_to(ROOT/'evals/results') and out!=ROOT/'evals/results';out.mkdir(exist_ok=False)
 assert not os.environ.get('BBA_TASKENGINE_DSN')
 private=out/'private';private.mkdir(mode=0o700)
 relative=private.relative_to(ROOT)
 build=['docker','run','--rm','--network','none','--user',f'{os.getuid()}:{os.getgid()}','-e','HOME=/scratch','-e','TMPDIR=/scratch','-v',os.environ['TMPDIR']+':/scratch','-e','GOPATH=/work/.cache/gopath','-e','GOMODCACHE=/work/.cache/gomod','-e','GOCACHE=/work/.cache/gobuild','-e','GOPROXY=off','-e','GOSUMDB=off','-v',str(ROOT)+':/work','-w','/work/backend','golang:1.26','go','build','-o','/work/'+str(relative)+'/product','./cmd/taskengine-server']
 exits=[]
 with (out/'build.log').open('w') as f:p=subprocess.run(build,cwd=ROOT,stdout=f,stderr=subprocess.STDOUT)
 exits.append({'label':'build','command':build,'exit_code':p.returncode});(out/'exits.json').write_text(json.dumps(exits,indent=2));p.check_returncode()
 alias="import socket,fcntl,struct; s=socket.socket(); fcntl.ioctl(s,0x8916,struct.pack('16sH2s4s8s',b'lo:b5',socket.AF_INET,b'\\x00'*2,socket.inet_aton('8.8.8.8'),b'\\x00'*8))"
 source=(ROOT/'tools/kernel.sh').read_text().replace('ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"','ROOT='+shlex.quote(str(ROOT)))
 source=source.replace('GO_COMMAND=(go test -race -count=1 ./internal/taskengine)','GO_COMMAND=(go test -race -count=1 ./internal/taskengine -run ^TestB5ContinuousProductProxy$ -v -timeout=90s)')
 source=source.replace('docker run --rm --network none --user "$(id -u):$(id -g)" \\', 'docker run --rm --network none --cap-add NET_ADMIN --add-host model.bba.example:8.8.8.8 -e BBA_B5_CONTINUOUS=1 -e BBA_B5_PRODUCT_BINARY=/work/'+str(relative)+'/product -e BBA_B5_CONTINUOUS_OUT=/work/'+str(out.relative_to(ROOT))+' \\')
 wrapper='python3 -c '+shlex.quote(alias)+'; exec "$@"'
 source=source.replace('golang:1.26 "${GO_COMMAND[@]}"','golang:1.26 sh -c '+shlex.quote(wrapper)+' b5-continuous "${GO_COMMAND[@]}"')
 with (out/'runner.log').open('w') as f:p=subprocess.run(['bash','-x','-c',source,'b5-continuous','test'],cwd=ROOT,stdout=f,stderr=subprocess.STDOUT)
 exits.append({'label':'continuous','exit_code':p.returncode});(out/'exits.json').write_text(json.dumps(exits,indent=2))
 names=sorted(set(re.findall(r'^\+ FIXTURE_NAME=(\S+)$',(out/'runner.log').read_text(),re.M)));assert names
 checks=[]
 for name in names:
  for kind,target in [('container',name),('volume',name+'-socket')]:
   q=subprocess.run(['docker',kind,'inspect',target],capture_output=True,text=True);checks.append({'kind':kind,'target':target,'exit_code':q.returncode,'stdout':q.stdout,'stderr':q.stderr,'absent':q.returncode==1 and 'no such' in q.stderr.lower()})
 (out/'cleanup-readback.json').write_text(json.dumps(checks,indent=2));assert all(x['absent'] for x in checks)
 p.check_returncode()
 assert (out/'consumer.json').exists()
 return 0
if __name__=='__main__':sys.exit(main())
