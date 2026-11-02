-- B7 manual acceptance fixture seed (fixture-only; never applied to a live DB).
-- Reuses the round-1 schema: core.operator_conversation / operator_message /
-- qq_draft / operator_notification (migration 0016, schema 25) and
-- core.content_rule so the keyword-block path exercises the real rule engine.
BEGIN;
INSERT INTO core.workspace(id) VALUES ('b7fix-ws');
-- operator + reader + runtime-anchor principals (handler checks principal existence)
INSERT INTO core.principal(workspace_id,id) VALUES
  ('b7fix-ws','op-b7fix'), ('b7fix-ws','op-b7fix-reader'), ('b7fix-ws','rt-b7fix');
-- two QQ accounts: verifiable account isolation
INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,
  connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,
  quota_limit,heartbeat_at)
VALUES
  ('b7fix-ws','acct-qq-1','rt-b7fix-1','b7fix-adapter-1','100001','unverified',
   'unknown','unknown','paused','unknown',false,false,1,clock_timestamp()),
  ('b7fix-ws','acct-qq-2','rt-b7fix-2','b7fix-adapter-2','100002','unverified',
   'unknown','unknown','paused','unknown',false,false,1,clock_timestamp());
-- enabled keyword rule so a POSTed draft containing TEST_BLOCK is really blocked
INSERT INTO core.content_revision(workspace_id,version) VALUES ('b7fix-ws',1);
INSERT INTO core.content_rule(workspace_id,id,version,document) VALUES
  ('b7fix-ws','rule-b7fix-keyword',1,'{"id":"rule-b7fix-keyword","version":1,"name":"B7 关键词阻断","kind":"keyword","expression":"TEST_BLOCK","case_sensitive":false,"group":"","priority":10,"enabled":true,"deleted":false,"updated_by":"op-b7fix","updated_at":"2026-11-01T00:00:00Z"}'::jsonb);
-- dual views: one agent thread, one QQ group (acct-qq-1), one QQ private (acct-qq-2)
INSERT INTO core.operator_conversation(workspace_id,id,kind,conversation_kind,conversation_id) VALUES
  ('b7fix-ws','conv-b7fix-agent','agent','agent','agent-thread-1');
INSERT INTO core.operator_conversation(workspace_id,id,kind,account_id,conversation_kind,conversation_id) VALUES
  ('b7fix-ws','conv-b7fix-group','qq','acct-qq-1','group','qq-group-101'),
  ('b7fix-ws','conv-b7fix-private','qq','acct-qq-2','private','qq-peer-202');
-- messages: agent, QQ inbound (both conversations), seeded draft, keyword-blocked draft
INSERT INTO core.operator_message(workspace_id,id,conversation_id,source,direction,delivery_state,body,body_digest,event_id,seq) VALUES
  ('b7fix-ws','msg-b7fix-agent-1','conv-b7fix-agent','agent','inbound','received','种子消息：Agent 线程内往返，不会进入 QQ 出站','e78962d77059e96eb8af75ef62818eca60609421020534a2e6a0042d5ff87467','seed-evt-01',1),
  ('b7fix-ws','msg-b7fix-group-in-1','conv-b7fix-group','qq','inbound','received','QQ 群入站种子：客户咨询（不含关键词）','24b8700809e9ab7fdc8780a623dbef1e6c00521c4f6920f69c0f2cba791ea40f','seed-evt-02',1),
  ('b7fix-ws','msg-b7fix-group-in-2','conv-b7fix-group','qq','inbound','received','QQ 群入站种子第二条：明确意向','3ea87c13236d4a5f83c932bcaef13fb861c5e52038ba8c013db8c9be15955a06','seed-evt-03',2),
  ('b7fix-ws','msg-b7fix-private-in-1','conv-b7fix-private','qq','inbound','received','QQ 私聊入站种子（acct-qq-2 隔离视图）','4d9b5734f9a44215a4bcf9d00ba856eaa9c7f406f8e222973adf3ac1c7bef64b','seed-evt-04',1),
  ('b7fix-ws','msg-b7fix-draft-ok','conv-b7fix-group','qq','outbound_draft','draft','出站草稿种子：普通草稿（未发送）','05e4272e809d8d70697b5ebc7b1fd69247ab74802404d656cde6404d8645bfc5','seed-evt-05',3),
  ('b7fix-ws','msg-b7fix-draft-blocked','conv-b7fix-group','qq','outbound_draft','blocked','出站草稿种子：命中关键词 TEST_BLOCK 被规则阻断','3606a3ccfc8b63c54d138bb6088ae52d99a4365f83c46d4c964c45de385d2155','seed-evt-06',4);
INSERT INTO core.qq_draft(workspace_id,id,conversation_id,account_id,message_id,payload_digest,rules_version,decision,evidence,created_by) VALUES
  ('b7fix-ws','draft-b7fix-ok','conv-b7fix-group','acct-qq-1','msg-b7fix-draft-ok',
   '05e4272e809d8d70697b5ebc7b1fd69247ab74802404d656cde6404d8645bfc5',1,'draft',
   '{"hits":[],"seeded":true,"send_permit":"absent"}'::jsonb,'op-b7fix'),
  ('b7fix-ws','draft-b7fix-blocked','conv-b7fix-group','acct-qq-1','msg-b7fix-draft-blocked',
   '3606a3ccfc8b63c54d138bb6088ae52d99a4365f83c46d4c964c45de385d2155',1,'blocked',
   '{"hits":[{"rule_id":"rule-b7fix-keyword","rule_version":1,"seeded":true}],"seeded":true,"send_permit":"absent"}'::jsonb,'op-b7fix');
-- notifications: one unread, one already read
INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary,read_at) VALUES
  ('b7fix-ws','notice-b7fix-unread','seed:unread','seed.qq.message','msg-b7fix-group-in-1','acct-qq-1','conv-b7fix-group',
   '种子通知：QQ 群入站（保持未读至页面验收）',NULL),
  ('b7fix-ws','notice-b7fix-read','seed:read','seed.agent.message','msg-b7fix-agent-1',NULL,'conv-b7fix-agent',
   '种子通知：Agent 线程消息（示例已读）',clock_timestamp());
COMMIT;
