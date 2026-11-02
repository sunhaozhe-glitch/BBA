"""Explicit simulation ACP peer; never invokes OMP, tools, or a model."""
import argparse
import json
import os
from pathlib import Path
import sys
import uuid


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--behavior', choices=['success','reverse','hang','exit','malformed','overflow','cancel'], default='success')
    behavior = parser.parse_args().behavior
    root = Path(os.environ['PI_CODING_AGENT_DIR'])
    sessions = root / 'sim-sessions'
    sessions.mkdir(parents=True, exist_ok=True)
    pending = None
    reversed_request = None
    catalog = json.loads(os.environ.get('BBA_FIXTURE_CATALOG', '{}'))

    def send(value):
        print(json.dumps(value), flush=True)

    def reply(request, result):
        send({'jsonrpc': '2.0', 'id': request['id'], 'result': result})

    for line in sys.stdin:
        message = json.loads(line)
        method = message.get('method')
        params = message.get('params', {})
        if method == 'initialize':
            reply(message, {'protocolVersion':1, 'agentInfo':{'name':'bba-simulation'}, 'agentCapabilities':{'loadSession':True, 'sessionCapabilities':{'resume':{}}}})
        elif method == 'session/new':
            sid = 'sim-' + uuid.uuid4().hex
            (sessions / sid).write_text('ready')
            reply(message, {'sessionId':sid})
        elif method in ('session/load', 'session/resume'):
            sid = params['sessionId']
            if not sid.startswith('sim-') or not (sessions / sid).is_file():
                send({'jsonrpc':'2.0', 'id':message['id'], 'error':{'code':-32000,'message':'simulation session absent'}})
            else:
                reply(message, {'sessionId':sid})
        elif method == 'echo':
            if behavior == 'reverse' and reversed_request is None:
                reversed_request = message
            else:
                reply(message, params)
                if reversed_request:
                    reply(reversed_request, reversed_request['params'])
                    reversed_request = None
        elif method == 'simulation/select_model':
            if params.get('role') not in catalog.get(params.get('profile'), []):
                send({'jsonrpc':'2.0','id':message['id'],'error':{'code':-32000,'message':'simulation profile not registered'}})
            else:
                reply(message,{'selected':params['profile'],'role':params['role'],'simulation':True})
        elif method == 'session/prompt':
            if behavior == 'exit':
                return
            if behavior == 'malformed':
                print('not json', flush=True)
            elif behavior == 'overflow':
                print('x'*70000, flush=True)
            elif behavior in ('hang', 'cancel'):
                pending = message
            else:
                print('token=fake-stderr-secret', file=sys.stderr, flush=True)
                for _ in range(2):
                    send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':params['sessionId'],'update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'simulation'}}}})
                send({'jsonrpc':'2.0','id':'permission-1','method':'session/request_permission','params':{'sessionId':params['sessionId'],'toolCall':{'kind':'execute','rawInput':{'command':'must never execute'}},'options':[]}})
                pending = message
        elif method == 'session/cancel':
            if pending and behavior == 'cancel':
                reply(pending, {'stopReason':'cancelled'})
                pending = None
        elif 'method' not in message and message.get('id') == 'permission-1':
            if message.get('result', {}).get('outcome', {}).get('outcome') != 'cancelled':
                return
            reply(pending, {'stopReason':'end_turn'})
            pending = None


if __name__ == '__main__':
    main()
