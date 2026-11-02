CREATE TABLE core.task_graph_root(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 root_id core.id NOT NULL,
 graph_revision bigint NOT NULL DEFAULT 0 CHECK(graph_revision>=0),
 task_version integer NOT NULL CHECK(task_version>0),
 scope_revision integer NOT NULL CHECK(scope_revision>0),
 snapshot_digest text NOT NULL DEFAULT '' CHECK(snapshot_digest='' OR snapshot_digest ~ '^[a-f0-9]{64}$'),
 projected_at timestamptz,
 PRIMARY KEY(workspace_id,task_id),
 UNIQUE(workspace_id,root_id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id)
);
CREATE TABLE core.task_graph_anchor(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 anchor_id core.id NOT NULL,
 anchor_kind text NOT NULL CHECK(anchor_kind IN ('account','target')),
 account_id core.id,
 target_id core.id,
 source_ref core.id,
 eligibility text,
 baseline_covered boolean,
 PRIMARY KEY(workspace_id,task_id,anchor_id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,task_id,account_id) REFERENCES core.task_account(workspace_id,task_id,account_id),
 FOREIGN KEY(workspace_id,task_id,target_id) REFERENCES core.task_target(workspace_id,task_id,target_id),
 CHECK((anchor_kind='account' AND account_id IS NOT NULL AND target_id IS NULL AND source_ref IS NULL AND eligibility IS NULL AND baseline_covered IS NULL) OR
       (anchor_kind='target' AND account_id IS NULL AND target_id IS NOT NULL AND source_ref IS NOT NULL AND eligibility IN ('eligible','already_covered','excluded','rejected') AND baseline_covered IS NOT NULL))
);
CREATE TABLE core.task_graph_node(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 node_id core.id NOT NULL,
 node_kind text NOT NULL CHECK(node_kind IN ('goal','task','account','target','fact')),
 anchor_id core.id,
 state text NOT NULL,
 node_json jsonb NOT NULL CHECK(jsonb_typeof(node_json)='object'),
 updated_revision bigint NOT NULL CHECK(updated_revision>0),
 PRIMARY KEY(workspace_id,task_id,node_id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,task_id,anchor_id) REFERENCES core.task_graph_anchor(workspace_id,task_id,anchor_id)
);
CREATE TABLE core.task_graph_fact(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 fact_id core.id NOT NULL,
 node_id core.id NOT NULL,
 fact_kind text NOT NULL CHECK(fact_kind IN ('membership','task_result')),
 account_id core.id NOT NULL,
 target_id core.id NOT NULL,
 source_runtime_id core.id NOT NULL,
 source_event_id core.id NOT NULL,
 evidence_ref core.id NOT NULL,
 observed_at timestamptz NOT NULL,
 fact_json jsonb NOT NULL CHECK(jsonb_typeof(fact_json)='object'),
 updated_revision bigint NOT NULL CHECK(updated_revision>0),
 PRIMARY KEY(workspace_id,task_id,fact_id),
 UNIQUE(workspace_id,task_id,source_runtime_id,source_event_id,fact_kind,account_id,target_id),
 FOREIGN KEY(workspace_id,task_id,node_id) REFERENCES core.task_graph_node(workspace_id,task_id,node_id),
 FOREIGN KEY(workspace_id,task_id,account_id) REFERENCES core.task_account(workspace_id,task_id,account_id),
 FOREIGN KEY(workspace_id,task_id,target_id) REFERENCES core.task_target(workspace_id,task_id,target_id),
 FOREIGN KEY(workspace_id,source_runtime_id,source_event_id) REFERENCES core.inbox(workspace_id,source_runtime_id,event_id)
);
CREATE TABLE core.task_graph_edge(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 edge_id core.id NOT NULL,
 from_node_id core.id NOT NULL,
 to_node_id core.id NOT NULL,
 edge_kind text NOT NULL CHECK(edge_kind IN ('has_goal','has_account','has_target','supports_result','observes_membership')),
 edge_json jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(edge_json)='object'),
 updated_revision bigint NOT NULL CHECK(updated_revision>0),
 PRIMARY KEY(workspace_id,task_id,edge_id),
 UNIQUE(workspace_id,task_id,from_node_id,to_node_id,edge_kind),
 FOREIGN KEY(workspace_id,task_id,from_node_id) REFERENCES core.task_graph_node(workspace_id,task_id,node_id),
 FOREIGN KEY(workspace_id,task_id,to_node_id) REFERENCES core.task_graph_node(workspace_id,task_id,node_id)
);
CREATE INDEX task_graph_edge_from ON core.task_graph_edge(workspace_id,task_id,from_node_id);
CREATE FUNCTION core.task_graph_edge_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'task graph edges are append only' USING ERRCODE='23514'; END $$;
CREATE TRIGGER task_graph_edge_append_only BEFORE UPDATE OR DELETE ON core.task_graph_edge FOR EACH ROW EXECUTE FUNCTION core.task_graph_edge_append_only();
CREATE TABLE core.task_graph_snapshot(
 workspace_id core.id NOT NULL,
 task_id core.id NOT NULL,
 graph_revision bigint NOT NULL CHECK(graph_revision>0),
 task_version integer NOT NULL CHECK(task_version>0),
 scope_revision integer NOT NULL CHECK(scope_revision>0),
 snapshot_digest text NOT NULL CHECK(snapshot_digest ~ '^[a-f0-9]{64}$'),
 snapshot_json jsonb NOT NULL CHECK(jsonb_typeof(snapshot_json)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,task_id,graph_revision),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id)
);
CREATE FUNCTION core.task_graph_fact_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'task graph facts are append only' USING ERRCODE='23514'; END $$;
CREATE TRIGGER task_graph_fact_append_only BEFORE UPDATE OR DELETE ON core.task_graph_fact FOR EACH ROW EXECUTE FUNCTION core.task_graph_fact_append_only();
CREATE FUNCTION core.task_graph_snapshot_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'task graph snapshots are append only' USING ERRCODE='23514'; END $$;
CREATE TRIGGER task_graph_snapshot_append_only BEFORE UPDATE OR DELETE ON core.task_graph_snapshot FOR EACH ROW EXECUTE FUNCTION core.task_graph_snapshot_append_only();
INSERT INTO core.schema_version(version) VALUES(14);
