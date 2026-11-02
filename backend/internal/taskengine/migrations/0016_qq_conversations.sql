CREATE TABLE core.operator_conversation (
 workspace_id core.id NOT NULL REFERENCES core.workspace(id),
 id core.id NOT NULL,
 kind text NOT NULL CHECK (kind IN ('agent','qq')),
 account_id core.id,
 conversation_kind text NOT NULL CHECK (conversation_kind IN ('agent','group','private')),
 conversation_id core.id NOT NULL,
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (workspace_id,id),
 CHECK ((kind='agent' AND account_id IS NULL AND conversation_kind='agent') OR
        (kind='qq' AND account_id IS NOT NULL AND conversation_kind IN ('group','private'))),
 FOREIGN KEY (workspace_id,account_id) REFERENCES core.account(workspace_id,id)
);
CREATE UNIQUE INDEX operator_conversation_scope ON core.operator_conversation
 (workspace_id,kind,COALESCE(account_id,''),conversation_kind,conversation_id);
CREATE TABLE core.operator_message (
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 conversation_id core.id NOT NULL,
 source text NOT NULL CHECK (source IN ('agent','qq')),
 direction text NOT NULL CHECK (direction IN ('inbound','outbound_draft')),
 delivery_state text NOT NULL CHECK (delivery_state IN ('received','draft','blocked')),
 body text NOT NULL CHECK (length(body) BETWEEN 1 AND 8192),
 body_digest text NOT NULL CHECK (body_digest ~ '^[a-f0-9]{64}$'),
 event_id core.id NOT NULL DEFAULT '',
 seq bigint NOT NULL CHECK (seq > 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (workspace_id,id),
 UNIQUE (workspace_id,conversation_id,seq),
 FOREIGN KEY (workspace_id,conversation_id) REFERENCES core.operator_conversation(workspace_id,id),
 CHECK ((source='agent' AND direction='inbound' AND delivery_state='received') OR
        (source='qq' AND direction='inbound' AND delivery_state='received') OR
        (source='qq' AND direction='outbound_draft' AND delivery_state IN ('draft','blocked')))
);
CREATE UNIQUE INDEX operator_message_event ON core.operator_message(workspace_id,conversation_id,event_id) WHERE event_id<>'';
CREATE INDEX operator_message_page ON core.operator_message(workspace_id,conversation_id,created_at DESC,id DESC);
CREATE FUNCTION core.operator_message_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'operator messages are append-only' USING ERRCODE='23514'; END $$;
CREATE TRIGGER operator_message_append_only BEFORE UPDATE OR DELETE ON core.operator_message
 FOR EACH ROW EXECUTE FUNCTION core.operator_message_immutable();
CREATE TABLE core.qq_draft (
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 conversation_id core.id NOT NULL,
 account_id core.id NOT NULL,
 message_id core.id NOT NULL,
 payload_digest text NOT NULL CHECK (payload_digest ~ '^[a-f0-9]{64}$'),
 rules_version bigint NOT NULL CHECK (rules_version >= 0),
 decision text NOT NULL CHECK (decision IN ('draft','blocked','unsupported')),
 evidence jsonb NOT NULL,
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 created_by core.id NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (workspace_id,id),
 UNIQUE (workspace_id,message_id),
 FOREIGN KEY (workspace_id,conversation_id) REFERENCES core.operator_conversation(workspace_id,id),
 FOREIGN KEY (workspace_id,account_id) REFERENCES core.account(workspace_id,id),
 FOREIGN KEY (workspace_id,message_id) REFERENCES core.operator_message(workspace_id,id),
 FOREIGN KEY (workspace_id,created_by) REFERENCES core.principal(workspace_id,id)
);
CREATE TABLE core.operator_notification (
 workspace_id core.id NOT NULL REFERENCES core.workspace(id),
 id core.id NOT NULL,
 event_key text NOT NULL,
 event_type text NOT NULL,
 object_id core.id NOT NULL,
 account_id core.id,
 conversation_id core.id,
 summary text NOT NULL CHECK (length(summary)<=240),
 evidence_ref text NOT NULL DEFAULT '',
 delivery_state text NOT NULL DEFAULT 'local_only' CHECK (delivery_state='local_only'),
 read_at timestamptz,
 version bigint NOT NULL DEFAULT 1 CHECK (version>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (workspace_id,id),
 UNIQUE (workspace_id,event_key),
 FOREIGN KEY (workspace_id,account_id) REFERENCES core.account(workspace_id,id),
 FOREIGN KEY (workspace_id,conversation_id) REFERENCES core.operator_conversation(workspace_id,id)
);
CREATE INDEX operator_notification_page ON core.operator_notification(workspace_id,created_at DESC,id DESC);
INSERT INTO core.schema_version(version) VALUES(25);