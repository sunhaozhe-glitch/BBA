-- Explicit migration only: no implicit startup writes or changes to task snapshots.
CREATE TABLE core.workspace_work_settings (
 workspace_id core.id PRIMARY KEY REFERENCES core.workspace(id),
 work_slots integer NOT NULL CHECK (work_slots BETWEEN 1 AND 8),
 version bigint NOT NULL CHECK (version > 0),
 updated_by core.id NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY (workspace_id,updated_by) REFERENCES core.principal(workspace_id,id)
);
CREATE TABLE core.workspace_work_settings_audit (
 workspace_id core.id NOT NULL REFERENCES core.workspace(id),
 version bigint NOT NULL CHECK (version > 0),
 principal_id core.id NOT NULL,
 old_work_slots integer NOT NULL CHECK (old_work_slots BETWEEN 1 AND 8),
 new_work_slots integer NOT NULL CHECK (new_work_slots BETWEEN 1 AND 8),
 old_version bigint NOT NULL CHECK (old_version >= 0),
 old_source text NOT NULL CHECK (old_source IN ('cli_fallback','workspace')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (workspace_id,version),
 CHECK (version=old_version+1),
 FOREIGN KEY (workspace_id,principal_id) REFERENCES core.principal(workspace_id,id)
);
CREATE TRIGGER workspace_work_settings_audit_immutable BEFORE UPDATE OR DELETE ON core.workspace_work_settings_audit
 FOR EACH ROW EXECUTE FUNCTION core.append_only();
INSERT INTO core.schema_version(version) VALUES(24);
