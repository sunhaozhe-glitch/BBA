-- P0 bounded proposal lifecycle: durable proposal, durable approval binding and
-- idempotent task-kernel application. A proposal is not a dispatch and an
-- application never writes an execution, permit, claim or bridge operation.
SET lock_timeout = '5s';

CREATE TABLE core.plan_proposal(
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 task_id core.id NOT NULL,
 principal_id core.id NOT NULL,
 expected_task_version integer NOT NULL CHECK(expected_task_version>0),
 scope_revision integer NOT NULL CHECK(scope_revision>0),
 template text NOT NULL CHECK(template='controlled_participation_v1'),
 proposal_kind text NOT NULL CHECK(proposal_kind IN ('use_template','adjust_order','request_clarification','diagnose')),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','approved','applied','rejected','expired')),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 steps jsonb NOT NULL CHECK(jsonb_typeof(steps)='array' AND jsonb_array_length(steps) BETWEEN 0 AND 1),
 questions jsonb NOT NULL CHECK(jsonb_typeof(questions)='array' AND jsonb_array_length(questions)<=20),
 rationale_summary text NOT NULL CHECK(length(rationale_summary) BETWEEN 1 AND 2000),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 500),
 approval_id core.id,
 approved_by core.id,
 approved_at timestamptz,
 applied_by core.id,
 applied_at timestamptz,
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,principal_id) REFERENCES core.principal(workspace_id,id),
 FOREIGN KEY(workspace_id,approval_id) REFERENCES core.approval(workspace_id,id),
 CHECK(expires_at>created_at),
 CHECK((state='pending' AND approval_id IS NULL AND approved_by IS NULL AND approved_at IS NULL AND applied_by IS NULL AND applied_at IS NULL) OR
       (state='approved' AND approval_id IS NOT NULL AND approved_by IS NOT NULL AND approved_at IS NOT NULL AND applied_by IS NULL AND applied_at IS NULL) OR
       (state='applied' AND approval_id IS NOT NULL AND approved_by IS NOT NULL AND approved_at IS NOT NULL AND applied_by IS NOT NULL AND applied_at IS NOT NULL) OR
       (state IN ('rejected','expired')))
);
CREATE INDEX plan_proposal_task_state ON core.plan_proposal(workspace_id,task_id,state,created_at);
CREATE UNIQUE INDEX plan_proposal_active_content ON core.plan_proposal(
 workspace_id,task_id,expected_task_version,scope_revision,template,digest)
 WHERE state IN ('pending','approved','applied');

CREATE TABLE core.plan_application(
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 task_id core.id NOT NULL,
 proposal_id core.id NOT NULL,
 proposal_digest text NOT NULL CHECK(proposal_digest ~ '^[a-f0-9]{64}$'),
 task_version integer NOT NULL CHECK(task_version>0),
 scope_revision integer NOT NULL CHECK(scope_revision>0),
 approval_id core.id NOT NULL,
 step_no integer NOT NULL CHECK(step_no=1),
 action text NOT NULL CHECK(action='request_join'),
 account_id core.id NOT NULL,
 target_id core.id NOT NULL,
 group_external_id core.qq_id NOT NULL,
 receipt_id core.id NOT NULL,
 applied_by core.id NOT NULL,
 applied_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,id),
 UNIQUE(workspace_id,proposal_id),
 UNIQUE(workspace_id,task_id,task_version),
 UNIQUE(workspace_id,receipt_id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,proposal_id) REFERENCES core.plan_proposal(workspace_id,id),
 FOREIGN KEY(workspace_id,approval_id) REFERENCES core.approval(workspace_id,id),
 FOREIGN KEY(workspace_id,task_id,account_id) REFERENCES core.task_account(workspace_id,task_id,account_id),
 FOREIGN KEY(workspace_id,task_id,target_id) REFERENCES core.task_target(workspace_id,task_id,target_id)
);

-- Proposal identity and operator intent are immutable. Only the lifecycle
-- columns move, and an applied/rejected/expired proposal is terminal.
CREATE FUNCTION core.plan_proposal_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'plan proposal evidence is append only' USING ERRCODE='23514'; END IF;
 IF (to_jsonb(NEW)-ARRAY['state','approval_id','approved_by','approved_at','applied_by','applied_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['state','approval_id','approved_by','approved_at','applied_by','applied_at']) THEN
  RAISE EXCEPTION 'plan proposal identity is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.state IN ('applied','rejected','expired') AND NEW.state<>OLD.state THEN
  RAISE EXCEPTION 'terminal plan proposal state is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.state='pending' AND NEW.state NOT IN ('pending','approved','rejected','expired') THEN
  RAISE EXCEPTION 'invalid plan proposal transition' USING ERRCODE='23514';
 END IF;
 IF OLD.state='approved' AND NEW.state NOT IN ('approved','applied','rejected','expired') THEN
  RAISE EXCEPTION 'invalid plan proposal transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER plan_proposal_guard BEFORE UPDATE OR DELETE ON core.plan_proposal FOR EACH ROW EXECUTE FUNCTION core.plan_proposal_guard();

CREATE TRIGGER plan_application_append_only BEFORE UPDATE OR DELETE ON core.plan_application FOR EACH ROW EXECUTE FUNCTION core.append_only();

INSERT INTO core.schema_version(version) VALUES(15);
