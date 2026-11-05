-- Offline orchestration facts; no Task, dispatch or Chat Permit ownership.
CREATE TABLE core.goal_run (
 workspace_id core.id NOT NULL REFERENCES core.workspace(id), id core.id NOT NULL,
 principal_id core.id NOT NULL, thread_id core.id NOT NULL, group_id text NOT NULL CHECK(group_id ~ '^[0-9]{1,20}$'),
 goal text NOT NULL CHECK(length(goal) BETWEEN 1 AND 1000),
 state text NOT NULL CHECK(state IN ('WAITING_INPUT','RUNNING','AWAITING_CONFIRMATION','CONFIRMED','STOPPED','TIMED_OUT','LIMIT_REACHED')),
 version integer NOT NULL DEFAULT 1 CHECK(version>0), round integer NOT NULL DEFAULT 0 CHECK(round BETWEEN 0 AND 4),
 reason text NOT NULL, configuration jsonb NOT NULL, memory jsonb NOT NULL DEFAULT '{}'::jsonb,
 graph jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(), deadline_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,id), FOREIGN KEY(workspace_id,principal_id) REFERENCES core.principal(workspace_id,id),
 FOREIGN KEY(workspace_id,thread_id) REFERENCES core.operator_conversation(workspace_id,id), CHECK(deadline_at>created_at)
);
CREATE TABLE core.goal_command (
 workspace_id core.id NOT NULL, principal_id core.id NOT NULL, key text NOT NULL, digest text NOT NULL,
 run_id core.id NOT NULL, PRIMARY KEY(workspace_id,principal_id,key),
 FOREIGN KEY(workspace_id,run_id) REFERENCES core.goal_run(workspace_id,id)
);
-- Transactional wakeup outbox. Publication and round creation commit together.
CREATE TABLE core.goal_event (
 workspace_id core.id NOT NULL, run_id core.id NOT NULL, id core.id NOT NULL,
 digest text NOT NULL, payload jsonb NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','published','rejected','ignored')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), reason text NOT NULL DEFAULT '',
 PRIMARY KEY(workspace_id,run_id,id), FOREIGN KEY(workspace_id,run_id) REFERENCES core.goal_run(workspace_id,id)
);
CREATE TABLE core.goal_round (
 workspace_id core.id NOT NULL, run_id core.id NOT NULL, number integer NOT NULL CHECK(number BETWEEN 1 AND 4),
 event_id core.id NOT NULL, state text NOT NULL CHECK(state IN ('RUNNING','COMPLETED','FAILED','STOPPED')),
 change_reason text NOT NULL DEFAULT '', consumed_evidence_ids jsonb NOT NULL DEFAULT '[]'::jsonb,
 PRIMARY KEY(workspace_id,run_id,number), UNIQUE(workspace_id,run_id,event_id),
 FOREIGN KEY(workspace_id,run_id,event_id) REFERENCES core.goal_event(workspace_id,run_id,id)
);
CREATE TABLE core.goal_step (
 workspace_id core.id NOT NULL, run_id core.id NOT NULL, round integer NOT NULL, id core.id NOT NULL,
 role text NOT NULL CHECK(role IN ('planner','analyst')),
 state text NOT NULL CHECK(state IN ('READY','BLOCKED','RUNNING','COMPLETED','FAILED','STOPPED')),
 attempt integer NOT NULL DEFAULT 0 CHECK(attempt BETWEEN 0 AND 2), lease_until timestamptz,
 input jsonb NOT NULL, output jsonb NOT NULL DEFAULT '{}'::jsonb,
 PRIMARY KEY(workspace_id,id), UNIQUE(workspace_id,run_id,round,role),
 FOREIGN KEY(workspace_id,run_id,round) REFERENCES core.goal_round(workspace_id,run_id,number)
);
-- Result inbox is immutable per fenced attempt, separate from its reducer.
CREATE TABLE core.goal_result (
 workspace_id core.id NOT NULL, step_id core.id NOT NULL, attempt integer NOT NULL CHECK(attempt BETWEEN 1 AND 2),
 run_id core.id NOT NULL, output jsonb NOT NULL, digest text NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','applied','stale')),
 PRIMARY KEY(workspace_id,step_id,attempt), FOREIGN KEY(workspace_id,step_id) REFERENCES core.goal_step(workspace_id,id),
 FOREIGN KEY(workspace_id,run_id) REFERENCES core.goal_run(workspace_id,id)
);
CREATE TABLE core.goal_evidence (
 workspace_id core.id NOT NULL, run_id core.id NOT NULL, id core.id NOT NULL, round integer NOT NULL,
 kind text NOT NULL CHECK(kind IN ('raw','summary','model_judgment','hint')), text text NOT NULL,
 refs jsonb NOT NULL, expires_at timestamptz NOT NULL, PRIMARY KEY(workspace_id,run_id,id),
 FOREIGN KEY(workspace_id,run_id,round) REFERENCES core.goal_round(workspace_id,run_id,number)
);
CREATE INDEX goal_event_pending ON core.goal_event(created_at) WHERE state='pending';
CREATE INDEX goal_step_pending ON core.goal_step(run_id,round) WHERE state IN ('READY','RUNNING');
INSERT INTO core.schema_version(version) VALUES(27);
