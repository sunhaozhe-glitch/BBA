"""MCP SDK stdio transport. stdout carries protocol messages only."""
import asyncio
from contextlib import asynccontextmanager
import json
import sys
import anyio

from mcp.server.lowlevel import Server
from mcp.server.stdio import stdio_server
import mcp.types as types

from .contracts import TOOLS, output_for
from .controller import Controller


class BoundedInput:
    """Bound before the SDK JSON decoder allocates an arbitrary line."""
    def __init__(self, stream, limit=65536):
        self.stream, self.limit = stream, limit

    def __aiter__(self):
        return self

    async def __anext__(self):
        line = await self.stream.readline(self.limit + 1)
        if not line:
            raise StopAsyncIteration
        if len(line.encode('utf-8')) > self.limit or not line.endswith('\n'):
            raise ValueError('MCP input frame limit exceeded')
        return line


async def serve(settings):
    controller = Controller(settings)
    semaphore = asyncio.Semaphore(8)

    async def list_tools(context, params):
        return types.ListToolsResult(tools=[types.Tool(name=name, description='BBA fixture development controller phase 1',
                                                        inputSchema=schema, outputSchema=output_for(name)) for name,schema in TOOLS.items()])

    async def call_tool(context, params):
        if semaphore.locked():
            result = controller.error('LIMIT_EXCEEDED','concurrent request limit reached')
        else:
            async with semaphore:
                result = await controller.call(params.name, params.arguments or {})
        return types.CallToolResult(content=[types.TextContent(type='text',text=json.dumps(result))],
                                    structuredContent=result,isError=not result['ok'])

    # The SDK validates RPC envelopes; controller validates exact tool input contracts
    # to guarantee the documented application error envelope for invalid arguments.
    server = Server('bba-controller',version='0.1.0',on_list_tools=list_tools,on_call_tool=call_tool)
    try:
        class Input:
            async def readline(self, size):
                return await anyio.to_thread.run_sync(lambda:sys.stdin.readline(size))
        async with stdio_server(stdin=BoundedInput(Input())) as (read,write):
            await server.run(read,write,server.create_initialization_options())
    finally:
        await controller.close()
