-- Proposal-driven execution: explicit orchestration mode, materialized work
-- ledger and its attempt/execution chain.
--
-- Boundaries this migration must keep:
--   * The existing apply route stays intent only. Nothing here enqueues work by
--     itself; only the explicit materialize command turns an approved proposal
--     into durable work items.
--   * A task is expanded by exactly one mechanism: orchestration_mode='legacy'
--     keeps the existing template/subtask scheduler, orchestration_mode='proposal'
--     leaves expansion to materialized work items. The legacy worker never
--     reserves a proposal-mode task and the proposal consumer never claims a
--     legacy task.
--   * authorization_revision is separate from the ordinary task version. Normal
--     events (import, control, observations, proposal application) bump version
--     only; a change of authorization inputs (orchestration mode today, approval
--     binding or scope revision later) bumps authorization_revision, so a
--     materialized work item records the exact authorization it was created under.
--   * Every work item references the application/proposal/task evidence it came
--     from through composite foreign keys, so a cross-workspace reference is
--     impossible in the schema.
SET lock_timeout = '5s';

ALTER TABLE core.task ADD COLUMN orchestration_mode text NOT NULL DEFAULT 'legacy'
 CHECK(orchestration_mode IN ('legacy','proposal'));
ALTER TABLE core.task ADD COLUMN authorization_revision integer NOT NULL DEFAULT 1
 CHECK(authorization_revision>0);

-- One materialization command per approved proposal. It records what was
-- materialized and under which authorization; it is not a send.
CREATE TABLE core.work_materialization(
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 task_id core.id NOT NULL,
 proposal_id core.id NOT NULL,
 proposal_digest text NOT NULL CHECK(proposal_digest ~ '^[a-f0-9]{64}$'),
 approval_id core.id NOT NULL,
 task_version integer NOT NULL CHECK(task_version>0),
 authorization_revision integer NOT NULL CHECK(authorization_revision>0),
 materialized_by core.id NOT NULL,
 materialized_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,id),
 UNIQUE(workspace_id,proposal_id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,proposal_id) REFERENCES core.plan_proposal(workspace_id,id),
 FOREIGN KEY(workspace_id,approval_id) REFERENCES core.approval(workspace_id,id)
);

-- One durable unit of approved work. state moves only along
-- BLOCKED->READY->CLAIMED->WAITING_RESULT->SUCCEEDED/FAILED/UNKNOWN, plus
-- CANCELLED before any external effect. lease_epoch is the fencing token: a
-- writer that does not present the current epoch cannot mutate the row.
CREATE TABLE core.work_item(
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 task_id core.id NOT NULL,
 materialization_id core.id NOT NULL,
 application_id core.id NOT NULL,
 proposal_id core.id NOT NULL,
 step_id text NOT NULL CHECK(length(step_id) BETWEEN 1 AND 128),
 step_no integer NOT NULL CHECK(step_no BETWEEN 1 AND 20),
 kind text NOT NULL CHECK(kind='request_join'),
 account_id core.id NOT NULL,
 target_id core.id NOT NULL,
 scope_digest text NOT NULL CHECK(scope_digest ~ '^[a-f0-9]{64}$'),
 authorization_version integer NOT NULL CHECK(authorization_version>0),
 state text NOT NULL DEFAULT 'BLOCKED'
  CHECK(state IN ('BLOCKED','READY','CLAIMED','WAITING_RESULT','SUCCEEDED','FAILED','UNKNOWN','CANCELLED')),
 blocked_reason text,
 depends_on jsonb NOT NULL DEFAULT '[]'::jsonb
  CHECK(jsonb_typeof(depends_on)='array' AND jsonb_array_length(depends_on)<=20),
 lease_owner text,
 lease_epoch bigint NOT NULL DEFAULT 0 CHECK(lease_epoch>=0),
 lease_until timestamptz,
 execution_id core.id,
 attempt_count integer NOT NULL DEFAULT 0 CHECK(attempt_count>=0),
 last_error text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,id),
 UNIQUE(workspace_id,application_id,step_id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,materialization_id) REFERENCES core.work_materialization(workspace_id,id),
 FOREIGN KEY(workspace_id,application_id) REFERENCES core.plan_application(workspace_id,id),
 FOREIGN KEY(workspace_id,proposal_id) REFERENCES core.plan_proposal(workspace_id,id),
 FOREIGN KEY(workspace_id,task_id,account_id) REFERENCES core.task_account(workspace_id,task_id,account_id),
 FOREIGN KEY(workspace_id,task_id,target_id) REFERENCES core.task_target(workspace_id,task_id,target_id),
 FOREIGN KEY(workspace_id,execution_id) REFERENCES core.execution(workspace_id,id),
 CHECK((state='CLAIMED' AND lease_owner IS NOT NULL AND lease_epoch>0 AND lease_until IS NOT NULL) OR
       (state<>'CLAIMED' AND lease_owner IS NULL AND lease_until IS NULL)),
 CHECK((state='BLOCKED' AND blocked_reason IS NOT NULL) OR (state<>'BLOCKED')),
 CHECK((state IN ('WAITING_RESULT','SUCCEEDED','FAILED','UNKNOWN') AND execution_id IS NOT NULL) OR
       (state NOT IN ('WAITING_RESULT','SUCCEEDED','FAILED','UNKNOWN'))),
 CHECK((state IN ('READY','BLOCKED','CANCELLED') AND execution_id IS NULL) OR state IN ('CLAIMED','WAITING_RESULT','SUCCEEDED','FAILED','UNKNOWN'))
);
CREATE INDEX work_item_ready ON core.work_item(workspace_id,task_id,created_at,id) WHERE state='READY';
CREATE INDEX work_item_task ON core.work_item(workspace_id,task_id,created_at,id);
CREATE INDEX work_item_lease ON core.work_item(state,lease_until) WHERE state='CLAIMED';
CREATE INDEX work_item_execution ON core.work_item(workspace_id,execution_id) WHERE execution_id IS NOT NULL;

-- One work item may legitimately be attempted more than once (bounded by the
-- existing subtask retry model). Each attempt records the exact execution it
-- produced, so an observation correlates to execution -> attempt -> work item
-- instead of "same account and group".
CREATE TABLE core.work_item_attempt(
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 work_item_id core.id NOT NULL,
 attempt_no integer NOT NULL CHECK(attempt_no>0),
 lease_epoch bigint NOT NULL CHECK(lease_epoch>0),
 execution_id core.id,
 outcome text CHECK(outcome IN ('SUCCEEDED','FAILED','UNKNOWN','CANCELLED')),
 evidence_ref core.id,
 observation_event_id core.id,
 error_code text,
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 PRIMARY KEY(workspace_id,id),
 UNIQUE(workspace_id,work_item_id,attempt_no),
 UNIQUE(workspace_id,execution_id),
 FOREIGN KEY(workspace_id,work_item_id) REFERENCES core.work_item(workspace_id,id),
 FOREIGN KEY(workspace_id,execution_id) REFERENCES core.execution(workspace_id,id),
 CHECK((outcome IS NULL AND finished_at IS NULL) OR (outcome IS NOT NULL AND finished_at IS NOT NULL))
);
CREATE FUNCTION core.work_item_attempt_guard() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
 IF TG_OP='DELETE' THEN
  RAISE EXCEPTION 'work item attempt evidence is append only' USING ERRCODE='23514';
 END IF;
 IF OLD.outcome IS NOT NULL AND (NEW.outcome IS DISTINCT FROM OLD.outcome OR
    NEW.evidence_ref IS DISTINCT FROM OLD.evidence_ref OR
    NEW.observation_event_id IS DISTINCT FROM OLD.observation_event_id OR
    NEW.finished_at IS DISTINCT FROM OLD.finished_at) THEN
  RAISE EXCEPTION 'work item attempt outcome is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER work_item_attempt_guard BEFORE UPDATE OR DELETE ON core.work_item_attempt
 FOR EACH ROW EXECUTE FUNCTION core.work_item_attempt_guard();

CREATE FUNCTION core.work_item_guard() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
 IF TG_OP='DELETE' THEN
  RAISE EXCEPTION 'work item evidence is append only' USING ERRCODE='23514';
 END IF;
 IF OLD.execution_id IS NOT NULL AND NEW.execution_id IS DISTINCT FROM OLD.execution_id THEN
  RAISE EXCEPTION 'work item execution binding is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.state IN ('SUCCEEDED','FAILED','UNKNOWN','CANCELLED') AND NEW.state<>OLD.state THEN
  RAISE EXCEPTION 'terminal work item state is immutable' USING ERRCODE='23514';
 END IF;
 IF NEW.state<>OLD.state AND NOT (
   (OLD.state='BLOCKED' AND NEW.state IN ('READY','CANCELLED')) OR
   (OLD.state='READY' AND NEW.state IN ('CLAIMED','CANCELLED')) OR
   (OLD.state='CLAIMED' AND NEW.state IN ('WAITING_RESULT','READY','CANCELLED')) OR
   (OLD.state='WAITING_RESULT' AND NEW.state IN ('SUCCEEDED','FAILED','UNKNOWN'))
 ) THEN
  RAISE EXCEPTION 'invalid work item transition % -> %', OLD.state, NEW.state USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $fn$;
CREATE TRIGGER work_item_guard BEFORE UPDATE OR DELETE ON core.work_item
 FOR EACH ROW EXECUTE FUNCTION core.work_item_guard();

-- A work item is one step of an approved application. The legacy single-step
-- apply path keeps writing step_no=1; the widened keys make a bounded multi-step
-- application (step order is the dependency order) expressible without a second
-- application model.
ALTER TABLE core.plan_application DROP CONSTRAINT plan_application_step_no_check;
ALTER TABLE core.plan_application ADD CONSTRAINT plan_application_step_no_range CHECK(step_no BETWEEN 1 AND 20);
ALTER TABLE core.plan_application DROP CONSTRAINT plan_application_workspace_id_proposal_id_key;
ALTER TABLE core.plan_application ADD CONSTRAINT plan_application_proposal_step_key UNIQUE(workspace_id,proposal_id,step_no);
ALTER TABLE core.plan_application DROP CONSTRAINT plan_application_workspace_id_task_id_task_version_key;
ALTER TABLE core.plan_application ADD CONSTRAINT plan_application_task_version_step_key UNIQUE(workspace_id,task_id,task_version,step_no);

-- A proposal may describe up to 20 approved steps. Generation still emits the
-- deterministic single step; multi-step input is bounded and dependency ordered.
ALTER TABLE core.plan_proposal DROP CONSTRAINT plan_proposal_steps_check;
ALTER TABLE core.plan_proposal ADD CONSTRAINT plan_proposal_steps_bounded
 CHECK(jsonb_typeof(steps)='array' AND jsonb_array_length(steps) BETWEEN 0 AND 20);

-- A proposal may be generated from one bounded ARTEX planning snapshot. The
-- snapshot table lives in the S2 migration (0011), so this column carries the
-- reference without a cross-migration foreign key; the snapshot validator
-- rejects a missing or stale reference before any work is materialized.
ALTER TABLE core.plan_proposal ADD COLUMN graph_snapshot_id core.id;

INSERT INTO core.schema_version(version) VALUES(19);
