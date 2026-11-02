import assert from 'node:assert/strict'
import { mkdtemp, rm } from 'node:fs/promises'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { createRequire } from 'node:module'
const packageRequire = createRequire(__filename)
const { CharacterAccountRuntimeManager, ChatBackendClient } = packageRequire('..') as typeof import('../src/index.js')
assert.equal(packageRequire.resolve('bba-character-runtime'), packageRequire.resolve('..'))
assert.equal(packageRequire('bba-character-runtime').CharacterAccountRuntimeManager, CharacterAccountRuntimeManager)
console.log(JSON.stringify({default_package_entry: packageRequire.resolve('..'), exports_entry: packageRequire.resolve('bba-character-runtime')}))
import { ACCOUNT_A } from '../test/helpers/fixtures.js'
import { FakeTransportBot } from '../test/helpers/fake-transport.js'
import { simulatedCharacter } from '../test/helpers/simulated-character.js'
import { h } from '../src/koishi.js'

async function main() {
 const [issueURL, gatewayURL, controlURL] = process.argv.slice(2) as [string,string,string]
 const root = await mkdtemp(join(tmpdir(),'b3-real-koishi-'))
 const account = {...ACCOUNT_A, maxSegmentCount:1, runtimeToken:'b3-issue-only',gateway:{baseUrl:gatewayURL,token:'b3-gateway-only'}}
 const options = {stateDir:root,backendBaseUrl:issueURL,accounts:[account],startApps:true,chatlunaPlugin:null,characterPlugin:simulatedCharacter,characterConfig:{replies:{}}}
 let manager = new CharacterAccountRuntimeManager(options)
 let seq=0
 const control = async (mode:string) => {const r=await fetch(controlURL+'/'+mode); assert.equal(r.status,200);return await r.json() as { writer: number }}
 const open = async (kind:'group'|'private'='group', target='777', segments=1) => {
  const gate=manager.runtimes.get('acc_a')!.gate
  assert.ok(await gate.openTriggerForEvent({kind,conversationId:target,senderId:'20001',sourceEventId:'b3-real-'+(++seq),segmentCount:segments}))
  return gate
 }
 try {
  await manager.start()
  let bot=manager.runtimes.get('acc_a')!.bot! as FakeTransportBot
  const before = await control('counts')
  for (const entry of ['sendMessage','sendPrivateMessage','createMessage','gate.sendText','gate.send']) {
   const entryBefore=(await control('counts')).writer
   const privateEntry=entry==='sendPrivateMessage'
   const gate=await open(privateEntry?'private':'group',privateEntry?'9001':'777')
   const text='allowed '+entry
   let result:unknown
   if(entry==='sendMessage') result=await bot.sendMessage('group:777',text)
   else if(entry==='sendPrivateMessage') result=await bot.sendPrivateMessage('9001',text)
   else if(entry==='createMessage') result=await bot.createMessage('group:777',text)
   else result=await gate[entry==='gate.send'?'send':'sendText']({kind:'group',conversationId:'777',text,transport:async()=>{throw new Error('LOCAL TRANSPORT CALLED')}})
   assert.ok(Array.isArray(result)&&result.length===1)
   assert.equal(bot.transportCalls.length,0)
   assert.equal((await control('counts')).writer-entryBefore,1)
   await open(privateEntry?'private':'group',privateEntry?'9001':'777')
   if(entry==='sendMessage') await assert.rejects(bot.sendMessage('group:777','BLOCK_B3'))
   else if(entry==='sendPrivateMessage') await assert.rejects(bot.sendPrivateMessage('9001','BLOCK_B3'))
   else if(entry==='createMessage') await assert.rejects(bot.createMessage('group:777','BLOCK_B3'))
   else await assert.rejects(gate[entry==='gate.send'?'send':'sendText']({kind:'group',conversationId:'777',text:'BLOCK_B3',transport:async()=>{throw new Error('LOCAL TRANSPORT CALLED')}}))
   assert.equal((await control('counts')).writer-entryBefore,1)
   assert.equal(bot.transportCalls.length,0)
   console.log(JSON.stringify({entry,result,allowed_writer_delta:1,blocked_writer_delta:0,local_transport:0}))
  }
  let count=(await control('counts')).writer
  assert.equal(count-before.writer,5)
  let gate=await open(); await assert.rejects(bot.sendMessage('group:777','BLOCK_B3'));assert.equal((await control('counts')).writer,count)
  for(const [kind,target] of [['group','778'],['private','777']] as const){await open();await assert.rejects(gate.sendText({kind,conversationId:target,text:'wrong target'}))}
  await open();await assert.rejects(bot.sendMessage('group:777',h.image('file:///no')));
  const internals=bot.internal as unknown as Record<string,(...a:unknown[])=>unknown>
  for(const method of ['send_msg','send_group_msg','send_private_msg','send_forward_msg','send_packet','_request']) assert.throws(()=>internals[method]!({}))
  assert.equal((await control('counts')).writer,count)
  // Drop gateway receipt AFTER durable send. Local state must be unknown and poison trigger.
  await control('drop');gate=await open();await assert.rejects(bot.sendMessage('group:777','lost receipt'));
  let snapshot=await gate.snapshot(); const lost=Object.values(snapshot.sends).find(s=>s.state==='unknown')!;assert.ok(lost)
  assert.equal((await control('counts')).writer,++count)
  await assert.rejects(bot.sendMessage('group:777','no resend'));assert.equal(bot.transportCalls.length,0)
  await manager.stop();manager=new CharacterAccountRuntimeManager(options);await manager.start();
  gate=manager.runtimes.get('acc_a')!.gate;bot=manager.runtimes.get('acc_a')!.bot! as FakeTransportBot
  await assert.rejects(bot.sendMessage('group:777','restart no resend'));
  // Explicit identical replay gets ONLY server terminal receipt; no new send/permit.
  const client=new ChatBackendClient({baseUrl:issueURL,runtimeToken:'b3-issue-only',gateway:account.gateway})
  const receipt=await client.gatewaySend(lost.permit_id!,{send_id:lost.send_id,segment_index:lost.segment_index,text:'lost receipt',account_id:'acc_a',conversation_kind:'group',conversation_id:'777'})
  assert.equal(receipt.state,'sent');assert.equal((await control('counts')).writer,count)
  await assert.rejects(client.gatewaySend(lost.permit_id!,{send_id:lost.send_id,segment_index:0,text:'changed',account_id:'acc_a',conversation_kind:'group',conversation_id:'777'}))
  // Old service capability / POST route failures never reach the local writer.
  for(const mode of ['cap404','cap405','capmissing','send404','send405','timeout']) {
   await control(mode);gate=await open();await assert.rejects(bot.sendMessage('group:777','mode '+mode));
   snapshot=await gate.snapshot();const last=Object.values(snapshot.sends).at(-1)!
   assert.equal(last.state, mode==='timeout'?'unknown':'rejected');assert.equal(bot.transportCalls.length,0)
   console.log(JSON.stringify({mode,state:last.state,local_transport:0}))
  }
  await control('normal');assert.equal((await control('counts')).writer,count)
  console.log(JSON.stringify({result:'PASS',real_go_gateway:true,writer:count-before.writer,local_transport:0,restart_replay:'no additional writer',lost_receipt:'unknown'}))
 } finally {await manager.stop();await rm(root,{recursive:true,force:true})}
}
main().catch(e=>{console.error(e);process.exitCode=1})
