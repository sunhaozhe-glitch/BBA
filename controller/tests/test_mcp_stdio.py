import asyncio
import json
from pathlib import Path
import sys
import tempfile
import unittest

from jsonschema import validate
from mcp import ClientSession
from mcp.client.stdio import stdio_client, StdioServerParameters

from bba_controller.contracts import TOOLS, output_for
from test_worktrees import fixture


class MCPStdioTests(unittest.IsolatedAsyncioTestCase):
    async def test_actual_sdk_stdio_client_all_seven_tools_and_restart(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo, sha = fixture(root)
            args = ['-m','bba_controller','--state-dir',str(root/'state'),'--repository',str(repo),'--fixture-repository',str(repo),'--authorize-fixture']
            params = StdioServerParameters(command=sys.executable,args=args,env={'PYTHONPATH':str(Path(__file__).resolve().parents[1]/'src')})
            async with stdio_client(params) as (read,write):
                async with ClientSession(read,write) as session:
                    await session.initialize()
                    tools = (await session.list_tools()).tools
                    self.assertEqual({t.name for t in tools}, set(TOOLS))
                    for tool in tools:
                        self.assertEqual(tool.input_schema, TOOLS[tool.name])
                        self.assertEqual(tool.output_schema, output_for(tool.name))
                    async def call(name,args):
                        result = await session.call_tool('bba_task_'+name,args)
                        data = result.structured_content
                        validate(data,output_for('bba_task_'+name))
                        return data
                    tid = (await call('submit',{'kind':'simulation','base_sha':sha,'write_set':['src']}))['data']['task']['id']
                    result = await call('wait',{'task_id':tid,'timeout_ms':1000})
                    self.assertEqual(result['data']['task']['state'],'SUCCEEDED')
                    self.assertEqual((await call('status',{'task_id':tid}))['data']['task']['id'],tid)
                    self.assertEqual(len((await call('list',{}))['data']['tasks']),1)
                    self.assertTrue((await call('evidence',{'task_id':tid}))['data']['session'])
                    model=(await call('evidence',{'task_id':tid}))['data']['model']
                    self.assertEqual(model['calls'][0]['profile'],'mock-local-builder')
                    self.assertEqual(model['budget']['spent_units'],1)
                    self.assertEqual((await call('cancel',{'task_id':tid}))['error']['code'],'CONFLICT')
                    self.assertEqual((await call('resume',{'task_id':tid}))['error']['code'],'CONFLICT')
                    denied = await call('submit',{'kind':'real','prompt':'must not run'})
                    self.assertFalse(denied['ok'])
            async with stdio_client(params) as (read,write):
                async with ClientSession(read,write) as session:
                    await session.initialize()
                    result = await session.call_tool('bba_task_status',{'task_id':tid})
                    self.assertEqual(result.structured_content['data']['task']['state'],'SUCCEEDED')
