import assert from 'node:assert/strict'
import { mkdtemp, rm, readFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { ACCOUNT_A, ACCOUNT_B } from '../test/helpers/fixtures.js'
import { FakeTransportBot } from '../test/helpers/fake-transport.js'
import { simulatedCharacter } from '../test/helpers/simulated-character.js'
import { h } from '../src/koishi.js'
const require=createRequire(__filename)
const {CharacterAccountRuntimeManager,ChatBackendClient}=require('..') as typeof import('../src/index.js')
async function main(){
 const cfg=JSON.parse(await readFile(process.argv[2]!, 'utf8'))
 const dir=await mkdtemp(join(tmpdir(),'b3-product-online-'))
 const options={stateDir:dir,backendBaseUrl:cfg.origin,accounts:[{...ACCOUNT_A,maxSegmentCount:1,runtimeToken:'b3-issue-only',gateway:{baseUrl:cfg.gateway,token:'b3-gateway-only'}},{...ACCOUNT_B,maxSegmentCount:1,runtimeToken:'b3-issue-b',gateway:{baseUrl:cfg.gateway,token:'b3-gateway-blocked'}}],startApps:true,chatlunaPlugin:null,characterPlugin:simulatedCharacter,characterConfig:{replies:{}}}
 let manager=new CharacterAccountRuntimeManager(options);let seq=0
 const count=async()=>{const r=await fetch(cfg.writer+'/counts');assert.equal(r.status,200);return (await r.json() as {writer:number}).writer}
 const open=async()=>{const gate=manager.runtimes.get('acc_a')!.gate;assert.ok(await gate.openTriggerForEvent({kind:'group',conversationId:'777',senderId:'20001',sourceEventId:'product-online-'+(++seq),segmentCount:1}));return gate}
 try{await manager.start();let bot=manager.runtimes.get('acc_a')!.bot! as FakeTransportBot
 const before=await count();await open();await assert.rejects(bot.sendMessage('group:777','BLOCK_B3'));assert.equal(await count(),before)
 let gate=await open();const result=await bot.sendMessage('group:777','allowed product online');assert.equal(result.length,1);assert.equal(await count(),before+1)
 const snap=await gate.snapshot();const sent=Object.values(snap.sends).find(x=>x.state==='sent')!;assert(sent)
 const client=new ChatBackendClient({baseUrl:cfg.origin,runtimeToken:'b3-issue-only',gateway:options.accounts[0]!.gateway})
 const replay=await client.gatewaySend(sent.permit_id!,{send_id:sent.send_id,segment_index:0,text:'allowed product online',account_id:'acc_a',conversation_kind:'group',conversation_id:'777'});assert.equal(replay.state,'sent');assert.equal(await count(),before+1)
 for(let i=0;i<21;i++){await open();await assert.rejects(bot.sendMessage('group:777','BLOCK_B3 '+i))};assert.equal(await count(),before+1)
 for(const text of [h.image('file:///not-real'),'[CQ:image,file=no]']){await open();await assert.rejects(bot.sendMessage('group:777',text))}
 const internal=bot.internal as unknown as Record<string,(...a:unknown[])=>unknown>;for(const key of ['send_packet','send_msg','send_forward_msg','_request'])assert.throws(()=>internal[key]!({}))
 assert.equal(bot.transportCalls.length,0);assert.equal(await count(),before+1)
 await manager.stop();manager=new CharacterAccountRuntimeManager(options);await manager.start();bot=manager.runtimes.get('acc_a')!.bot! as FakeTransportBot
 const again=await client.gatewaySend(sent.permit_id!,{send_id:sent.send_id,segment_index:0,text:'allowed product online',account_id:'acc_a',conversation_kind:'group',conversation_id:'777'});assert.equal(again.state,'sent');assert.equal(await count(),before+1);assert.equal(bot.transportCalls.length,0)

 const negativeEvidence:unknown[]=[]
 // Both auth scopes are real service resolver/gateway entries. A permit is never transferable.
 const wrongAccount=new ChatBackendClient({baseUrl:cfg.origin,runtimeToken:'b3-issue-b',gateway:{baseUrl:cfg.gateway,token:'b3-gateway-blocked'}})
 const wrongWorkspace=new ChatBackendClient({baseUrl:cfg.origin,runtimeToken:'b3-issue-other',gateway:{baseUrl:cfg.gateway,token:'b3-gateway-other'}})
 const bound={send_id:sent.send_id,segment_index:0,text:'allowed product online',account_id:'acc_a',conversation_kind:'group' as const,conversation_id:'777'}
 for(const [name,who,body] of [
  ['cross-account-token',wrongAccount,bound],['cross-workspace-token',wrongWorkspace,bound],
  ['changed-text',client,{...bound,text:'changed'}],['changed-account',client,{...bound,account_id:'acc_b'}],
  ['changed-group',client,{...bound,conversation_id:'778'}],['changed-kind',client,{...bound,conversation_kind:'private' as const}]
 ] as const){const n=await count();await assert.rejects(who.gatewaySend(sent.permit_id!,body));assert.equal(await count(),n);negativeEvidence.push({name,writer_delta:0})}
 // Exercise real default-package group AND private sends for each partition.
 for(const [account,kind,target] of [['acc_a','private','9001'],['acc_b','group','778'],['acc_b','private','9002']] as const){
  const runtime=manager.runtimes.get(account)!;const n=await count()
  const context=await new ChatBackendClient({baseUrl:cfg.origin,runtimeToken:account==='acc_a'?'b3-issue-only':'b3-issue-b'}).getContext({accountId:account,kind,conversationId:target})
  assert.equal(context.qq_self_id,account==='acc_a'?'10001':'10002');assert.equal(context.chat_enabled,true);assert.equal(context.speaker_active,true)
  assert.ok(await runtime.gate.openTriggerForEvent({kind,conversationId:target,senderId:'20001',sourceEventId:'valid-'+(++seq),segmentCount:1}))
  const transport=runtime.bot! as FakeTransportBot
  const receipt=kind==='private'?await transport.sendPrivateMessage(target,'valid '+account+' '+kind):await transport.sendMessage('group:'+target,'valid '+account+' '+kind)
  assert.equal(receipt.length,1);assert.equal(await count(),n+1);assert.equal(transport.transportCalls.length,0)
  assert.ok(await runtime.gate.openTriggerForEvent({kind,conversationId:target,senderId:'20001',sourceEventId:'block-'+(++seq),segmentCount:1}))
  if(kind==='private')await assert.rejects(transport.sendPrivateMessage(target,'BLOCK_B3'));else await assert.rejects(transport.sendMessage('group:'+target,'BLOCK_B3'))
  assert.equal(await count(),n+1)
  console.log(JSON.stringify({account,kind,target,self_id:context.qq_self_id,allowed_writer_delta:1,blocked_writer_delta:0,local_transport:0}))
 }
 console.log(JSON.stringify({negativeEvidence,writer_delta:3,negative_writer_delta:0}))
 console.log(JSON.stringify({result:'PASS',default_package_entry:require.resolve('..'),actual_product_server:cfg.origin,actual_gateway:cfg.gateway,blocked_writer_delta:0,allowed_writer_delta:1,replay_writer_delta:0,restart_replay_writer_delta:0,local_transport:0,paid_model_calls:0}))
 }finally{await manager.stop();await rm(dir,{recursive:true,force:true})}
}
main().catch(e=>{console.error(e);process.exitCode=1})
