import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, rm, mkdir, readFile, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { CharacterSendLedger } from '../src/ledger.js'
import { CharacterSendGate } from '../src/gate.js'
import { ChatBackendClient } from '../src/backend.js'
import { textDigest, sendIdFor } from '../src/digest.js'

async function fixture(t: { after: (fn: () => Promise<void>) => void }, mutate?: (file: string) => Promise<void>) {
 const dir = await mkdtemp(join(tmpdir(), 'b3-failure-')); t.after(() => rm(dir, {recursive:true,force:true}))
 const file=join(dir,'ledger.json'), ledger=new CharacterSendLedger(file,'a','r');await ledger.load()
 const send=sendIdFor('trigger',0), text='plain allowed'
 await ledger.recordTrigger({trigger_ref:'trigger',kind:'group',conversation_id:'777',sender_id:'30001',source_event_id:'event',deadline_at:new Date(Date.now()+60000).toISOString(),segment_count:1,next_segment:0,binding_epoch:'1',state:'open',pending_send_id:null})
 await ledger.beginSend({send_id:send,trigger_ref:'trigger',segment_index:0,text_digest:textDigest(text),permit_id:'permit',state:'issued',message_ids:[],updated_at:new Date().toISOString()})
 let writer=0,local=0,posts=0
 const backend=new ChatBackendClient({baseUrl:'http://127.0.0.1:1',runtimeToken:'issue-fixture',gateway:{baseUrl:'http://127.0.0.1:2',token:'gateway-fixture'},fetchImpl:async (url,init)=>{
  assert.equal(new URL(String(url)).port,'2');assert.equal((init?.headers as Record<string,string>).authorization,'Bearer gateway-fixture')
  if(String(url).endsWith('/capabilities'))return Response.json({data:{api_version:'character-gateway-v1',single_text:true}})
  assert.ok(String(url).endsWith('/gateway-send'));posts++;writer++;await mutate?.(file)
  return Response.json({data:{api_version:'character-gateway-v1',permit_id:'permit',send_id:send,segment_index:0,account_id:'a',conversation_kind:'group',conversation_id:'777',state:'sent',message_id:'one'}})
 }})
 const gate=new CharacterSendGate({account:{accountId:'a',runtimeId:'r',qqSelfId:'10001',persona:'fixture'} as never,backend,ledger})
 const call=()=>gate.sendText({kind:'group',conversationId:'777',text,transport:async()=>{local++;return ['BAD']}})
 return {file,ledger,gate,backend,send,call,counts:()=>({writer,local,posts})}
}

test('concurrent duplicate gateway calls claim the durable send once',async t=>{
 const f=await fixture(t);const results=await Promise.allSettled([f.call(),f.call()]);assert.equal(results.filter(r=>r.status==='fulfilled').length,1);assert.deepEqual(f.counts(),{writer:1,local:0,posts:1});assert.equal((await f.ledger.snapshotNow()).sends[f.send]?.state,'sent')
 await assert.rejects(f.call());assert.equal(f.counts().writer,1)
})

test('pre-network ledger persistence failure blocks every writer and stays fail-stop',async t=>{
 const f=await fixture(t);const saved=await readFile(f.file,'utf8');await rm(f.file);await mkdir(f.file)
 await assert.rejects(f.call());await assert.rejects(f.call());assert.deepEqual(f.counts(),{writer:0,local:0,posts:0})
 await rm(f.file,{recursive:true});await writeFile(f.file,saved);const restarted=new CharacterSendLedger(f.file,'a','r');await restarted.load();assert.equal((await restarted.snapshotNow()).sends[f.send]?.state,'unknown')
})

test('sent receipt persistence failure cannot retry; restart recovers durable claim as unknown',async t=>{
 let saved='';const f=await fixture(t,async file=>{saved=await readFile(file,'utf8');await rm(file);await mkdir(file)})
 await assert.rejects(f.call());await assert.rejects(f.call());assert.deepEqual(f.counts(),{writer:1,local:0,posts:1})
 await rm(f.file,{recursive:true});await writeFile(f.file,saved);const restarted=new CharacterSendLedger(f.file,'a','r');await restarted.load();const snapshot=await restarted.snapshotNow();assert.equal(snapshot.sends[f.send]?.state,'unknown');assert.equal(snapshot.triggers.trigger?.state,'poisoned')
})

test('multi-segment trigger is rejected before permit or gateway requests',async t=>{
 const f=await fixture(t);await assert.rejects(f.gate.openTriggerForEvent({kind:'group',conversationId:'777',senderId:'20001',sourceEventId:'new',segmentCount:2}));assert.deepEqual(f.counts(),{writer:0,local:0,posts:0})
})
