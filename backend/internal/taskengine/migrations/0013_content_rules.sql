-- Additive, transactional; no defaults enable content rules or sending.
CREATE TABLE core.content_revision (
 workspace_id text PRIMARY KEY REFERENCES core.workspace(id), version bigint NOT NULL DEFAULT 0
);
CREATE TABLE core.content_rule (
 workspace_id text NOT NULL REFERENCES core.workspace(id), id text NOT NULL,
 version integer NOT NULL CHECK(version>0), document jsonb NOT NULL,
 PRIMARY KEY(workspace_id,id)
);
CREATE TABLE core.content_rule_history (
 workspace_id text NOT NULL, id text NOT NULL, version integer NOT NULL,
 document jsonb NOT NULL, updated_by text NOT NULL, updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,id,version)
);
CREATE TABLE core.content_review (
 workspace_id text NOT NULL REFERENCES core.workspace(id), id text NOT NULL,
 account_id text NOT NULL, conversation_kind text NOT NULL, conversation_id text NOT NULL,
 send_id text NOT NULL, task_id text NOT NULL DEFAULT '', payload_digest text NOT NULL,
 rules_version bigint NOT NULL, decision text NOT NULL, evidence jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(workspace_id,id)
);
CREATE INDEX content_review_page ON core.content_review(workspace_id,created_at DESC,id);
CREATE TABLE core.content_review_note (
 workspace_id text NOT NULL, review_id text NOT NULL, id text NOT NULL,
 note text NOT NULL, updated_by text NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,review_id,id), FOREIGN KEY(workspace_id,review_id) REFERENCES core.content_review(workspace_id,id)
);
INSERT INTO core.schema_version(version) VALUES(22);
