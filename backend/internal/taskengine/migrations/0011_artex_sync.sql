-- ARTEX dual-graph sync: durable root bindings, provenance anchors, external node
-- mappings, the local graph outbox, per-partition checkpoints and bounded planning
-- snapshots.
--
-- Boundaries this schema encodes:
--   - PostgreSQL stays the execution fact authority. These tables never carry a
--     QQ action, an execution or a permit; ARTEX is an external reference graph.
--   - A task only syncs after an explicit root binding. There is no default root,
--     no shared root between two tasks of the same provider+kind, and an unbound
--     task stays unbound rather than falling back to a local graph.
--   - External writes are queued in core.graph_sync_outbox inside the same
--     transaction as the decision that caused them, and are performed outside
--     any transaction. An operation whose external outcome was not answered
--     unambiguously can only move to needs_reconcile; there is no auto-resend
--     column and no retry counter that resets it.
--   - Fact and inference are separate columns. A trusted local fact anchor is
--     never overwritten by an inference anchor: the two are distinct rows with
--     distinct provenance, and only the trusted provenance satisfies the fact
--     CHECK.
--   - Planning snapshots are bounded and immutable; rounds and deadlines cap a
--     planning loop so a changed graph cannot spin forever.
SET lock_timeout = '5s';

-- One external root per (task, graph kind, provider). graph_revision advances on
-- every accepted external pull; it is the version a planning snapshot binds.
CREATE TABLE core.task_graph_roots(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 graph_kind text NOT NULL CHECK(graph_kind IN ('fact','goal')),
 provider text NOT NULL CHECK(provider='artex'),
 external_root text NOT NULL CHECK(length(external_root) BETWEEN 1 AND 128 AND btrim(external_root)<>'' AND external_root<>'active'),
 graph_revision bigint NOT NULL DEFAULT 0 CHECK(graph_revision>=0),
 last_sync_at timestamptz,
 last_error_code text,
 last_error text,
 last_error_at timestamptz,
 bound_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,task_id,graph_kind,provider),
 UNIQUE(workspace_id,provider,graph_kind,external_root),
 CHECK((last_error_code IS NULL AND last_error IS NULL) OR last_error_code IS NOT NULL),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id)
);

-- Provenance anchors: one row binds a task graph node to an account/group and,
-- for a trusted fact, to the exact Summary version + manifest + projection
-- sequence it was derived from. group_id is the QQ group id (core.qq_id); it must
-- be a target of the same task, enforced by the store transaction.
CREATE TABLE core.task_graph_anchors(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 anchor_id core.id NOT NULL,
 graph_kind text NOT NULL CHECK(graph_kind IN ('fact','goal')),
 provider text NOT NULL CHECK(provider='artex'),
 account_id core.id NOT NULL,
 group_id core.qq_id NOT NULL,
 summary_id text CHECK(summary_id IS NULL OR summary_id ~ '^[a-f0-9]{64}$'),
 summary_version integer CHECK(summary_version IS NULL OR summary_version>0),
 manifest_sha256 text CHECK(manifest_sha256 IS NULL OR manifest_sha256 ~ '^[a-f0-9]{64}$'),
 projection_sequence bigint NOT NULL DEFAULT 0 CHECK(projection_sequence>=0),
 external_node_id text CHECK(external_node_id IS NULL OR (length(external_node_id) BETWEEN 1 AND 64 AND btrim(external_node_id)<>'')),
 external_provenance text CHECK(external_provenance IS NULL OR external_provenance IN ('fact','inference')),
 provenance text NOT NULL CHECK(provenance IN ('fact','inference')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,task_id,anchor_id),
 UNIQUE NULLS NOT DISTINCT (workspace_id,task_id,graph_kind,provider,account_id,group_id,summary_version,provenance),
 CHECK(provenance<>'fact' OR (summary_id IS NOT NULL AND summary_version IS NOT NULL AND manifest_sha256 IS NOT NULL)),
 CHECK((external_node_id IS NULL)=(external_provenance IS NULL)),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,task_id,account_id) REFERENCES core.task_account(workspace_id,task_id,account_id)
);
CREATE INDEX task_graph_anchors_summary ON core.task_graph_anchors(workspace_id,task_id,graph_kind,account_id,group_id,summary_version);
CREATE INDEX task_graph_anchors_external ON core.task_graph_anchors(workspace_id,task_id,graph_kind,provider,external_node_id) WHERE external_node_id IS NOT NULL;

-- Read model of the external graph: every node an accepted pull returned, with
-- its provenance. local_key maps a locally created goal back to the outbox
-- operation that created it (empty for nodes merely read from ARTEX). An
-- inference row never replaces a fact row: provenance is part of the identity,
-- and fact rows come only from the trusted local truth, not from ARTEX.
CREATE TABLE core.graph_sync_nodes(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 graph_kind text NOT NULL CHECK(graph_kind IN ('fact','goal')),
 provider text NOT NULL CHECK(provider='artex'),
 external_node_id text NOT NULL CHECK(length(external_node_id) BETWEEN 1 AND 64 AND btrim(external_node_id)<>''),
 external_kind text NOT NULL CHECK(length(external_kind) BETWEEN 1 AND 32),
 external_state text NOT NULL CHECK(length(external_state) BETWEEN 1 AND 64),
 origin text NOT NULL DEFAULT '',
 local_key text NOT NULL DEFAULT '',
 payload jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(payload)='object'),
 provenance text NOT NULL CHECK(provenance IN ('fact','inference')),
 observed_revision bigint NOT NULL CHECK(observed_revision>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,task_id,graph_kind,provider,external_node_id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id)
);
CREATE UNIQUE INDEX graph_sync_nodes_local_key ON core.graph_sync_nodes(workspace_id,task_id,graph_kind,provider,local_key) WHERE local_key<>'';

-- Local, transactional intent to call ARTEX. payload_digest binds the exact body
-- that was reviewed, idempotency_key de-duplicates the same decision, sequence
-- orders one partition, and state records how far the external effect is known.
CREATE TABLE core.graph_sync_outbox(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 graph_kind text NOT NULL CHECK(graph_kind IN ('fact','goal')),
 provider text NOT NULL CHECK(provider='artex'),
 id core.id NOT NULL,
 operation text NOT NULL CHECK(operation IN ('graph.pull','goal.create','goal.update','goal.delete')),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 200),
 payload jsonb NOT NULL CHECK(jsonb_typeof(payload)='object'),
 payload_digest text NOT NULL CHECK(payload_digest ~ '^[a-f0-9]{64}$'),
 scope text NOT NULL CHECK(length(scope) BETWEEN 1 AND 200),
 sequence bigint NOT NULL CHECK(sequence>0),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','sending','sent','failed','needs_reconcile','canceled')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 max_attempts integer NOT NULL DEFAULT 3 CHECK(max_attempts BETWEEN 1 AND 10),
 external_node_id text CHECK(external_node_id IS NULL OR (length(external_node_id) BETWEEN 1 AND 64 AND btrim(external_node_id)<>'')),
 last_error_code text,
 last_error text,
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_owner text,
 lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,task_id,id),
 UNIQUE(workspace_id,task_id,graph_kind,provider,idempotency_key),
 CHECK((state='sending')=(lease_owner IS NOT NULL AND lease_until IS NOT NULL)),
 CHECK(state NOT IN ('failed','needs_reconcile') OR last_error_code IS NOT NULL),
 CHECK(state<>'sent' OR operation='graph.pull' OR external_node_id IS NOT NULL),
 CHECK(attempts<=max_attempts),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id)
);
-- The head of one partition is the smallest non-terminal sequence; this index
-- makes that a bounded range read per task/graph instead of a scan.
CREATE INDEX graph_sync_outbox_head ON core.graph_sync_outbox(workspace_id,task_id,graph_kind,provider,sequence)
 WHERE state IN ('pending','sending');
CREATE INDEX graph_sync_outbox_due ON core.graph_sync_outbox(next_attempt_at)
 WHERE state='pending';
CREATE INDEX graph_sync_outbox_attention ON core.graph_sync_outbox(workspace_id,task_id,state)
 WHERE state IN ('needs_reconcile','failed');

-- Durable consumption offsets. There is one row per (task, graph, source,
-- scope): core.outbox is ordered by core.task_event.seq (appendEvent writes both
-- in one transaction), and one Summary scope is one account/group pair whose
-- offset is the highest summary_version consumed. A failed partition keeps its
-- own offset, so it cannot hold another task back.
CREATE TABLE core.graph_sync_checkpoints(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 graph_kind text NOT NULL CHECK(graph_kind IN ('fact','goal')),
 provider text NOT NULL CHECK(provider='artex'),
 source text NOT NULL CHECK(source IN ('core.outbox','group_summary')),
 scope_key text NOT NULL CHECK(length(scope_key) BETWEEN 1 AND 200),
 offset_value bigint NOT NULL CHECK(offset_value>=0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,task_id,graph_kind,provider,source,scope_key),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id)
);

-- Bounded, immutable planning basis. candidates is an array of
-- {candidate_id,external_node_id,target_external_id,text,provenance,evidence_refs}
-- and summary_refs an array of {account_id,group_id,summary_id,summary_version,
-- manifest_sha256}. state is open|consumed|stale|expired; only open snapshots can
-- back a proposal, and any dependency change (graph revision, summary version,
-- task version, scope revision, authorization revision) makes one stale.
CREATE TABLE core.graph_planning_snapshots(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 snapshot_id core.id NOT NULL,
 graph_kind text NOT NULL CHECK(graph_kind IN ('fact','goal')),
 provider text NOT NULL CHECK(provider='artex'),
 graph_revision bigint NOT NULL CHECK(graph_revision>=0),
 task_version integer NOT NULL CHECK(task_version>0),
 scope_revision integer NOT NULL CHECK(scope_revision>0),
 authorization_revision integer NOT NULL CHECK(authorization_revision>0),
 model_version text NOT NULL CHECK(length(model_version) BETWEEN 1 AND 128),
 template_version text NOT NULL CHECK(length(template_version) BETWEEN 1 AND 128),
 summary_refs jsonb NOT NULL CHECK(jsonb_typeof(summary_refs)='array' AND jsonb_array_length(summary_refs)<=64),
 anchor_ids jsonb NOT NULL CHECK(jsonb_typeof(anchor_ids)='array' AND jsonb_array_length(anchor_ids)<=256),
 candidates jsonb NOT NULL CHECK(jsonb_typeof(candidates)='array' AND jsonb_array_length(candidates)<=20),
 candidate_count integer NOT NULL CHECK(candidate_count BETWEEN 0 AND 20),
 round integer NOT NULL CHECK(round>0),
 max_rounds integer NOT NULL CHECK(max_rounds BETWEEN 1 AND 100),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 state text NOT NULL DEFAULT 'open' CHECK(state IN ('open','consumed','stale','expired')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 CHECK(expires_at>created_at),
 CHECK(candidate_count=jsonb_array_length(candidates)),
 PRIMARY KEY(workspace_id,task_id,snapshot_id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id)
);
CREATE INDEX graph_planning_snapshots_latest ON core.graph_planning_snapshots(workspace_id,task_id,created_at DESC);
CREATE INDEX graph_planning_snapshots_rounds ON core.graph_planning_snapshots(workspace_id,task_id,created_at DESC) WHERE state IN ('open','consumed');

INSERT INTO core.schema_version(version) VALUES(20);
