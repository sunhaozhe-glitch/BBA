-- Append-only migration: notification dedup follows the persisted event scope.
-- The workspace-wide unique key made one event id a workspace-wide identity, so
-- the same id in a second account or a second conversation lost its projection.
-- The scope below matches the persisted inbox event identity: workspace, the
-- conversation row that carries account + conversation kind + external id, and
-- the event key. Historical rows are unchanged and stay valid.
ALTER TABLE core.operator_notification
 DROP CONSTRAINT operator_notification_workspace_id_event_key_key;
CREATE UNIQUE INDEX operator_notification_event_scope ON core.operator_notification
 (workspace_id,COALESCE(conversation_id,''),event_key);
INSERT INTO core.schema_version(version) VALUES(26);
