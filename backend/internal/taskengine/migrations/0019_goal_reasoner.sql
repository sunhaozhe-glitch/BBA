-- Versioned bounded planning context retained beside each logical step.
ALTER TABLE core.goal_step
 ADD COLUMN context_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
 ADD COLUMN context_version integer NOT NULL DEFAULT 0 CHECK(context_version >= 0),
 ADD COLUMN context_digest text NOT NULL DEFAULT '' CHECK(context_digest='' OR context_digest ~ '^[a-f0-9]{64}$'),
 ADD COLUMN model_request_id text NOT NULL DEFAULT '' CHECK(model_request_id='' OR model_request_id ~ '^[a-zA-Z0-9_.:-]{1,160}$'),
 ADD CONSTRAINT goal_step_context_size CHECK(pg_column_size(context_snapshot) <= 131072),
 ADD CONSTRAINT goal_step_input_size CHECK(pg_column_size(input) <= 262144);
ALTER TABLE core.goal_result
 ADD COLUMN model_request_id text NOT NULL DEFAULT '' CHECK(model_request_id='' OR model_request_id ~ '^[a-zA-Z0-9_.:-]{1,160}$');
INSERT INTO core.schema_version(version) VALUES(28);
