#!/usr/bin/env python3
"""Real binary + real resolver + browser search save, inside network-none.
Only an owned Unix-socket/host-loopback HTTP relay is exposed. No host DNS/CA edits.
"""
from pathlib import Path
import json,os,secrets,shlex,socket,subprocess,sys,threading,time,select
ROOT=Path(__file__).resolve().parents[1]
def checked_output(raw):
 p=Path(raw)
 if '..' in p.parts:raise ValueError('traversal refused')
 p=p.absolute()
 base=ROOT/'evals/results'
 if not p.is_relative_to(base) or p==base:raise ValueError('output must be below evals/results')
 if p!=p.resolve() or p.exists() or p.is_symlink():raise ValueError('existing/root/symlink output refused')
 # Every existing directory below the trusted implementation root must be owned,
 # non-symlink, and not writable by another user; never use author-name prefixes.
 for parent in [p.parent,*p.parent.parents]:
  _trusted_parent(parent)
  if parent==ROOT:break
 return p

def _trusted_parent(p):
 import stat
 st=p.lstat()
 if not stat.S_ISDIR(st.st_mode) or stat.S_ISLNK(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o002 or st.st_mode&0o020 and not _private_group(st.st_gid) or not os.access(p,os.W_OK):raise ValueError('untrusted output parent')

def _private_group(gid):
 import grp,pwd
 members=set(grp.getgrgid(gid).gr_mem)|{p.pw_name for p in pwd.getpwall() if p.pw_gid==gid}
 return members=={pwd.getpwuid(os.getuid()).pw_name}

def checked_nonce(value, expected):
 if not isinstance(expected,str) or len(expected)!=64 or any(c not in '0123456789abcdef' for c in expected) or not isinstance(value,dict) or value.get('nonce')!=expected:raise ValueError('fixture ownership nonce mismatch')
 return value

def main():
 if os.environ.get('BBA_TASKENGINE_DSN') or os.environ.get('BBA_FIXTURE_DSN'):raise ValueError('inherited DSN refused')
 out=checked_output(sys.argv[1]);out.mkdir(mode=0o700);private=out/'private';private.mkdir(mode=0o700)
 nonce=secrets.token_hex(32);(private/'owner.json').write_text(json.dumps({'nonce':nonce}));os.chmod(private/'owner.json',0o600)
 name='bba-b6-'+secrets.token_hex(8);db=name+'-db';volume=db+'-socket';test=name+'-test'
 manifest={'nonce':nonce,'containers':[db,test],'volumes':[volume],'network':'none','output':str(out)};(out/'resources.json').write_text(json.dumps(manifest,indent=2))
 exits=[];process=None;bridge=None;log=None;stop=threading.Event();clients=[]
 def run(label,cmd):
  with (out/(label+'.log')).open('w') as log:p=subprocess.run(cmd,cwd=ROOT,stdout=log,stderr=subprocess.STDOUT)
  exits.append({'label':label,'command':cmd,'exit_code':p.returncode});(out/'exits.json').write_text(json.dumps(exits,indent=2));p.check_returncode()
 def relay(c):
  d=socket.socket(socket.AF_UNIX);clients.extend([c,d])
  try:
   fd=os.open(private,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
   try:d.connect('/proc/self/fd/'+str(fd)+'/relay.sock')
   finally:os.close(fd)
   while not stop.is_set():
    ready,_,_=select.select([c,d],[],[],.2)
    for a in ready:
     data=a.recv(65536)
     if not data:return
     (d if a is c else c).sendall(data)
  except (ConnectionError,OSError):
   if not stop.is_set():raise
  finally:c.close();d.close()
 def accept():
  while not stop.is_set():
   try:c,_=bridge.accept()
   except socket.timeout:continue
   except OSError:return
   threading.Thread(target=relay,args=(c,),daemon=True).start()
 try:
  run('embed',['python3','tools/build-product.py'])
  build=['docker','run','--rm','--network','none','--user',f'{os.getuid()}:{os.getgid()}','-e','HOME=/scratch','-e','TMPDIR=/scratch','-v',os.environ['TMPDIR']+':/scratch','-e','GOPATH=/work/.cache/gopath','-e','GOMODCACHE=/work/.cache/gomod','-e','GOCACHE=/work/.cache/gobuild','-e','GOPROXY=off','-e','GOSUMDB=off','-e','GOFLAGS=-mod=readonly','-v',str(ROOT)+':/work','-v',str(out)+':/evidence','-w','/work/backend','golang:1.26','go','build','-o','/evidence/private/product','./cmd/taskengine-server']
  run('build',build)
  bridge=socket.socket();bridge.bind(('127.0.0.1',0));port=bridge.getsockname()[1];bridge.listen();bridge.settimeout(.2);accept_thread=threading.Thread(target=accept,daemon=True);accept_thread.start()
  manifest['loopback_port']=port;(out/'resources.json').write_text(json.dumps(manifest,indent=2))
  source=(ROOT/'tools/kernel.sh').read_text().replace('ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"','ROOT='+shlex.quote(str(ROOT)))
  source=source.replace('FIXTURE_NAME="bba-p004-011-$(date +%s)-$$"','FIXTURE_NAME='+shlex.quote(db))
  source=source.replace('GO_COMMAND=(go test -race -count=1 ./internal/taskengine)','GO_COMMAND=(go test -race -count=1 ./internal/taskengine -run ^TestB6ContinuousProduct$ -v -timeout=110s)')
  source=source.replace('docker run --rm --network none --user "$(id -u):$(id -g)" \\',f'docker run --rm --name {test} --network none --cap-add NET_ADMIN --user "$(id -u):$(id -g)" --add-host api.search.brave.com:8.8.8.8 --add-host api.tavily.com:8.8.8.8 --add-host model.bba.example:8.8.8.8 -v '+shlex.quote(str(out)+':/evidence')+f' -e BBA_B6_PRODUCT=1 -e BBA_B6_NONCE={nonce} -e BBA_B6_OUT=/evidence -e BBA_B6_BINARY=/evidence/private/product -e BBA_B6_PORT={port} \\')
  # NET_ADMIN is granted solely to this owned network-none namespace.
  alias="import socket,fcntl,struct; s=socket.socket(); fcntl.ioctl(s,0x8916,struct.pack('16sH2s4s8s',b'lo:b6',socket.AF_INET,b'\\x00'*2,socket.inet_aton('8.8.8.8'),b'\\x00'*8))"
  # Network setup needs root; Go and product then drop to the invoking UID/GID.
  source=source.replace('--cap-add NET_ADMIN --user "$(id -u):$(id -g)"','--cap-add NET_ADMIN')
  wrapper='python3 -c '+shlex.quote(alias)+'; exec setpriv --reuid='+str(os.getuid())+' --regid='+str(os.getgid())+' --clear-groups "$@"'
  source=source.replace('golang:1.26 "${GO_COMMAND[@]}"','golang:1.26 sh -c '+shlex.quote(wrapper)+' b6-product "${GO_COMMAND[@]}"')
  log=(out/'runner.log').open('w');process=subprocess.Popen(['bash','-x','-c',source,'b6-product','test'],cwd=ROOT,stdout=log,stderr=subprocess.STDOUT)
  end=time.monotonic()+160
  while not (out/'fixture-config.json').exists():
   if process.poll() is not None:raise RuntimeError('fixture exited before ready: '+str(process.returncode))
   if time.monotonic()>end:raise TimeoutError('fixture startup')
   time.sleep(.1)
  cfg=checked_nonce(json.loads((out/'fixture-config.json').read_text()),nonce);assert cfg['production_binary'] is True
  run('browser',['node','tools/b6-product-browser.cjs',str(out)])
  (out/'browser-done.json').write_text(json.dumps(checked_nonce({'nonce':cfg['nonce']},nonce)))
  checked_nonce(json.loads((out/'browser-done.json').read_text()),nonce)
  rc=process.wait(timeout=25);log.close();exits.append({'label':'continuous','command':['bash','-x','generated-network-none-kernel'],'exit_code':rc});(out/'exits.json').write_text(json.dumps(exits,indent=2));assert rc==0,rc
  consumer=json.loads((out/'consumer.json').read_text());assert consumer['same_product_pid']==cfg['pid'] and consumer['search_count']==1 and consumer['model_count']==1
 finally:
  if process and process.poll() is None:
   subprocess.run(['docker','rm','-f',test],capture_output=True);process.wait(timeout=20)
  if log:log.close()
  stop.set()
  if bridge:
   bridge.close();accept_thread.join(timeout=2)
  for c in clients:
   try:c.close()
   except OSError:pass
  cleanup=[]
  for kind,target in [('container',test),('container',db),('volume',volume)]:
   cmd=['docker','rm','-f',target] if kind=='container' else ['docker','volume','rm',target]
   q=subprocess.run(cmd,capture_output=True,text=True)
   inspect=subprocess.run(['docker',kind,'inspect',target],capture_output=True,text=True)
   cleanup.append({'kind':kind,'target':target,'remove_exit':q.returncode,'inspect_exit':inspect.returncode,'stdout':inspect.stdout,'stderr':inspect.stderr,'absent':inspect.returncode==1 and 'no such' in inspect.stderr.lower()})
  closed=True
  if bridge:
   with socket.socket() as s:closed=s.connect_ex(('127.0.0.1',port))!=0
  result={'resources':cleanup,'loopback_closed':closed,'runner_exit':None if process is None else process.poll()};(out/'cleanup-readback.json').write_text(json.dumps(result,indent=2))
  assert all(x['absent'] for x in cleanup) and closed,result
if __name__=='__main__':main()
