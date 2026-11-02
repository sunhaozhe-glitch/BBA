-- Additive Summary.v1 store for the collector -> backend -> Character chain.
-- One row is one immutable Summary version. Rows are never updated; expiry and
-- retention are enforced on read, and PurgeExpiredGroupSummaries removes rows
-- once their source retention window has passed.
CREATE TABLE core.group_summary(
 workspace_id core.id NOT NULL,
 account_id core.id NOT NULL,
 group_id core.qq_id NOT NULL,
 summary_id text NOT NULL CHECK(summary_id ~ '^[a-f0-9]{64}$'),
 summary_version integer NOT NULL CHECK(summary_version>0),
 window_id text NOT NULL CHECK(window_id ~ '^[a-f0-9]{64}$'),
 window_message_count integer NOT NULL CHECK(window_message_count>=0),
 window_start timestamptz NOT NULL,
 window_end timestamptz NOT NULL,
 generated_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 privacy_class text NOT NULL CHECK(privacy_class IN ('local_only','redacted_external_allowed')),
 retention_days integer NOT NULL CHECK(retention_days BETWEEN 1 AND 365),
 source_manifest_sha256 text NOT NULL CHECK(source_manifest_sha256 ~ '^[a-f0-9]{64}$'),
 plugin_version text NOT NULL CHECK(length(plugin_version) BETWEEN 1 AND 128),
 collector_account_id core.qq_id NOT NULL,
 topics jsonb NOT NULL CHECK(jsonb_typeof(topics)='array' AND jsonb_array_length(topics)<=20),
 limitations jsonb NOT NULL CHECK(jsonb_typeof(limitations)='array' AND jsonb_array_length(limitations)<=20),
 source_digest text NOT NULL CHECK(source_digest ~ '^[a-f0-9]{64}$'),
 ingested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,account_id,group_id,summary_version),
 UNIQUE(workspace_id,account_id,group_id,summary_id),
 FOREIGN KEY(workspace_id,account_id) REFERENCES core.account(workspace_id,id),
 CHECK(window_start<=window_end),
 CHECK(generated_at>=window_start),
 CHECK(expires_at>generated_at),
 CHECK(expires_at<=window_start+make_interval(days=>retention_days))
);
-- Reader index: the newest live version for one account/group.
CREATE INDEX group_summary_reader ON core.group_summary(workspace_id,account_id,group_id,expires_at DESC,summary_version DESC);
-- Retention sweep index.
CREATE INDEX group_summary_expiry ON core.group_summary(expires_at);
INSERT INTO core.schema_version(version) VALUES(16);
