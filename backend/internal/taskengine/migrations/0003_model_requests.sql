CREATE TABLE core.model_request(
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 task_id core.id NOT NULL,
 principal_id core.id NOT NULL,
 idempotency_key core.id NOT NULL,
 request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
 operation_kind text NOT NULL CHECK(operation_kind='complex_plan'),
 schema_id text NOT NULL CHECK(schema_id='PlanProposal.v1'),
 prompt_version text NOT NULL CHECK(length(prompt_version) BETWEEN 1 AND 128),
 input_ref core.id NOT NULL,
 deadline_at timestamptz NOT NULL,
 max_provider_attempts integer NOT NULL CHECK(max_provider_attempts BETWEEN 1 AND 3),
 attempts integer NOT NULL DEFAULT 1 CHECK(attempts=1),
 state text NOT NULL DEFAULT 'started' CHECK(state IN ('started','completed','failed')),
 error_code text,
 response jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 completed_at timestamptz,
 PRIMARY KEY(workspace_id,id),
 UNIQUE(workspace_id,principal_id,idempotency_key),
 UNIQUE(workspace_id,task_id,id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,principal_id) REFERENCES core.principal(workspace_id,id),
 CHECK((state='started' AND error_code IS NULL AND response IS NULL AND completed_at IS NULL) OR
       (state='completed' AND error_code IS NULL AND jsonb_typeof(response)='object' AND completed_at IS NOT NULL) OR
       (state='failed' AND error_code IS NOT NULL AND response IS NULL AND completed_at IS NOT NULL))
);
CREATE INDEX model_request_task_budget ON core.model_request(workspace_id,task_id);
CREATE FUNCTION core.model_request_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'model request evidence is append only' USING ERRCODE='23514'; END IF;
 IF (to_jsonb(NEW)-ARRAY['state','error_code','response','completed_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['state','error_code','response','completed_at']) THEN
  RAISE EXCEPTION 'model request identity is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.state<>'started' OR NEW.state NOT IN ('completed','failed') THEN
  RAISE EXCEPTION 'model request has one terminal transition' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER model_request_guard BEFORE UPDATE OR DELETE ON core.model_request FOR EACH ROW EXECUTE FUNCTION core.model_request_guard();
INSERT INTO core.schema_version(version) VALUES(12);
