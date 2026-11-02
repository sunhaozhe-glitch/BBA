ALTER TABLE core.approval
 ADD COLUMN join_reason text NOT NULL DEFAULT '',
 ADD COLUMN verification_message text NOT NULL DEFAULT '';

INSERT INTO core.schema_version(version) VALUES(13);
