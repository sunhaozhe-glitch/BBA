#!/usr/bin/env python3
"""B7 principal scope: real isolated server + PostgreSQL, no external adapters.
Run with --output NEW_ABSOLUTE_DIRECTORY --legacy-source DEPENDENCY_WORKTREE.
All credentials are synthetic and remain in the untracked fixture directory.
"""
import argparse
import concurrent.futures
import datetime
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import time
import urllib.error
import urllib.request


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--output', type=Path, required=True)
    ap.add_argument('--legacy-source', type=Path, required=True)
    args = ap.parse_args()
    root = Path(__file__).resolve().parents[1]
    out = args.output
    if not out.is_absolute() or out.exists():
        raise SystemExit('output must be a new absolute directory')
    out.mkdir(parents=True)
    scratch = out / 'scratch'
    scratch.mkdir(); scratch.chmod(0o777)
    sockdir = out / 'socket'
    sockdir.mkdir(); sockdir.chmod(0o777)
    name = 'bba-b7-principal-' + secrets.token_hex(6)
    proc = None
    records = []
    def run(cmd, **kw):
        started = datetime.datetime.now().astimezone().isoformat()
        p = subprocess.run(cmd, capture_output=True, text=True, **kw)
        index = len(records)
        (out / f'command-{index}.log').write_text(p.stdout + p.stderr)
        records.append(dict(command=cmd, start=started, end=datetime.datetime.now().astimezone().isoformat(), exit_code=p.returncode, log=f'command-{index}.log'))
        (out / 'commands.json').write_text(json.dumps(records, indent=2))
        if p.returncode:
            raise RuntimeError(f'command {index} failed: {p.returncode}')
        return p.stdout
    def sql(query):
        return run(['docker', 'exec', name, 'psql', '-h', '/var/run/postgresql', '-U', 'bba_fixture', '-d', 'bba_fixture', '-v', 'ON_ERROR_STOP=1', '-Atc', query]).strip()
    def build(source, label):
        run(['docker','run','--rm','--network','none','--user',f'{os.getuid()}:{os.getgid()}',
             '-v',str(source)+':/work','-v',str(out)+':/out','-e','HOME=/out/scratch/home',
             '-e','TMPDIR=/out/scratch','-e','GOTMPDIR=/out/scratch','-e','GOPATH=/work/.cache/gopath',
             '-e','GOMODCACHE=/work/.cache/gomod','-e','GOCACHE=/work/.cache/gobuild',
             '-e','GOPROXY=off','-e','GOSUMDB=off','-e','GOFLAGS=-mod=readonly','-e','CGO_ENABLED=0',
             '-w','/work/backend','golang:1.26','go','build','-trimpath','-o',f'/out/{label}', './cmd/taskengine-server'])
    try:
        build(root, 'server')
        build(args.legacy_source, 'legacy-server')
        with socket.socket() as dbsock:
            dbsock.bind(('127.0.0.1', 0)); dbport = dbsock.getsockname()[1]
        run(['docker','run','-d','--rm','--network','bridge','--name',name,'--label','bba.stage1='+name,
             '--tmpfs','/var/lib/postgresql/data:rw','-p',f'127.0.0.1:{dbport}:5432',
             '-e','POSTGRES_USER=bba_fixture','-e','POSTGRES_DB=bba_fixture','-e','POSTGRES_HOST_AUTH_METHOD=trust',
             'postgres:16-bookworm'])
        for _ in range(150):
            p = subprocess.run(['docker','exec',name,'psql','-h','/var/run/postgresql','-U','bba_fixture','-d','bba_fixture','-Atc','SELECT 1'],capture_output=True)
            if p.returncode == 0: break
            time.sleep(.2)
        else: raise RuntimeError('PostgreSQL readiness timeout')
        for migration in sorted((root/'backend/internal/taskengine/migrations').glob('*.sql')):
            run(['docker','exec','-i',name,'psql','-h','/var/run/postgresql','-U','bba_fixture','-d','bba_fixture','-v','ON_ERROR_STOP=1'],input=migration.read_text())
        sql("INSERT INTO core.workspace(id) VALUES('scope_ws'); INSERT INTO core.principal(workspace_id,id) VALUES('scope_ws','alice'),('scope_ws','bob'); INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id) VALUES('scope_ws','scope_account','fake_runtime','fake_adapter')")
        tokens = {p: secrets.token_hex(32) for p in ['alice','bob']}
        resolver = out/'resolver.json'
        resolver.write_text(json.dumps({'credentials':[dict(token_sha256=hashlib.sha256(token.encode()).hexdigest(),workspace_id='scope_ws',principal_id=p,permissions=['conversation.read','conversation.write','notification.read']) for p,token in tokens.items()]}));resolver.chmod(0o600)
        with socket.socket() as s:
            s.bind(('127.0.0.1',0)); port=s.getsockname()[1]
        origin=f'http://127.0.0.1:{port}'
        env=dict(os.environ,BBA_TASKENGINE_DSN=f'postgres://bba_fixture@127.0.0.1:{dbport}/bba_fixture?sslmode=disable',TMPDIR=str(scratch))
        def stop():
            nonlocal proc
            if proc is not None:
                proc.terminate()
                try: proc.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    proc.kill();proc.wait(timeout=5)
                proc=None
        def start(binary):
            nonlocal proc
            proc=subprocess.Popen([str(out/binary),'-listen',f'127.0.0.1:{port}','-origin',origin,'-resolver-file',str(resolver)],env=env,stdout=(out/(binary+'-runtime.log')).open('a'),stderr=subprocess.STDOUT)
            for _ in range(150):
                if proc.poll() is not None: raise RuntimeError('server startup failed')
                try:
                    with socket.create_connection(('127.0.0.1',port),timeout=.2): return
                except OSError: time.sleep(.1)
            raise RuntimeError('server readiness timeout')
        def request(principal,path,key,body):
            data=json.dumps(body,separators=(',',':')).encode()
            req=urllib.request.Request(origin+path,data=data,headers={'Authorization':'Bearer '+tokens[principal],'Content-Type':'application/json','Idempotency-Key':key,'Origin':origin},method='POST')
            try:
                with urllib.request.urlopen(req,timeout=20) as res: return res.status,res.read().decode()
            except urllib.error.HTTPError as e: return e.code,e.read().decode()
        def counts():
            return json.loads(sql("SELECT json_build_object('messages',(SELECT count(*) FROM core.operator_message),'notifications',(SELECT count(*) FROM core.operator_notification),'drafts',(SELECT count(*) FROM core.qq_draft),'ledger',(SELECT count(*) FROM core.idempotency),'chat_permits',(SELECT count(*) FROM core.chat_send_permit),'dispatch_permits',(SELECT count(*) FROM core.dispatch_permit),'tasks',(SELECT count(*) FROM core.task),'model_requests',(SELECT count(*) FROM core.model_request))"))
        paths=[('/v1/operator/agent-threads/scope_http/messages',{'body':'synthetic HTTP scope'}),('/v1/operator/qq-drafts',{'account_id':'scope_account','conversation_kind':'private','conversation_id':'scope_chat','body':'synthetic HTTP draft','expected_version':1})]
        start('legacy-server')
        legacy=[]
        for path,body in paths:
            result=request('alice',path,'legacy-key',body);assert result[0]==201,result
            legacy.append(result)
        before=counts();stop();start('server')
        for (path,body),expected in zip(paths,legacy): assert request('alice',path,'legacy-key',body)==expected
        assert counts()==before
        assert sql("SELECT count(*) FROM core.operator_message WHERE event_id='legacy-key'")=='2'
        outcomes=[]
        for path,body in paths:
            a=request('alice',path,'scope-key',body);assert a[0]==201,a
            assert request('alice',path,'scope-key',body)==a
            changed=dict(body,body='different synthetic text');conflict=request('alice',path,'scope-key',changed)
            assert conflict[0]==409 and 'synthetic' not in conflict[1],conflict
            b=request('bob',path,'scope-key',body);assert b[0]==201 and b[1]!=a[1],b
            assert request('bob',path,'scope-key',body)==b
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                results=list(pool.map(lambda p:request(p,path,'parallel-key',body),['alice','bob']))
            assert all(v[0]==201 for v in results) and results[0][1]!=results[1][1],results
            # Server commits while the client deliberately never consumes the response.
            connection=http.client.HTTPConnection('127.0.0.1',port,timeout=10)
            connection.request('POST',path,json.dumps(body),{'Authorization':'Bearer '+tokens['alice'],'Origin':origin,'Content-Type':'application/json','Idempotency-Key':'lost-key'})
            for _ in range(100):
                if sql("SELECT count(*) FROM core.idempotency WHERE key='lost-key' AND path='"+path+"'")=='1': break
                time.sleep(.05)
            else: raise RuntimeError('lost-response commit not observed')
            connection.close()
            snapshot=counts();lost=request('alice',path,'lost-key',body);assert lost[0]==201 and counts()==snapshot
            stop();start('server')
            assert request('alice',path,'scope-key',body)==a
            assert request('bob',path,'scope-key',body)==b
            assert request('alice',path,'lost-key',body)==lost
            assert counts()==snapshot
            outcomes.append(dict(path=path,first=a,second=b,concurrent=results,lost_response_replay=lost))
        final=counts()
        assert final==dict(messages=12,notifications=12,drafts=6,ledger=12,chat_permits=0,dispatch_permits=0,tasks=0,model_requests=0),final
        # Remove Alice from resolver and restart: a ledger hit cannot bypass revocation.
        config=json.loads(resolver.read_text());config['credentials']=config['credentials'][1:];resolver.write_text(json.dumps(config));resolver.chmod(0o600)
        stop();start('server')
        for path,body in paths: assert request('alice',path,'scope-key',body)[0]==401
        assert counts()==final
        result=dict(status='PASS',executor='Hermes AUTHOR_SELF_TEST',injected_responses=False,sha=run(['git','rev-parse','HEAD']).strip(),schema_head=sql('SELECT max(version) FROM core.schema_version'),binary_sha256=hashlib.sha256((out/'server').read_bytes()).hexdigest(),legacy_binary_sha256=hashlib.sha256((out/'legacy-server').read_bytes()).hexdigest(),counts=final,outcomes=outcomes,fixture_boundary='network-none PostgreSQL, loopback server, no model/provider/writer configured')
        (out/'result.json').write_text(json.dumps(result,indent=2));print(json.dumps(result,indent=2))
    finally:
        if proc is not None:
            proc.terminate()
            try: proc.wait(timeout=15)
            except subprocess.TimeoutExpired: proc.kill();proc.wait(timeout=5)
        inspect=subprocess.run(['docker','inspect','--format','{{ index .Config.Labels "bba.stage1" }}',name],capture_output=True,text=True)
        if inspect.returncode==0 and inspect.stdout.strip()==name:
            run(['docker','rm','-f',name])
        absent=subprocess.run(['docker','inspect',name],capture_output=True)
        cleanup=dict(container=name,container_absent=absent.returncode!=0,server_exited=proc is None or proc.poll() is not None)
        (out/'cleanup.json').write_text(json.dumps(cleanup,indent=2))
        assert cleanup['container_absent'] and cleanup['server_exited'],cleanup


if __name__=='__main__':
    main()
