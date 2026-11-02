-- Fixture/explicit migrations only. Existing tasks retain compatible serial behavior.
ALTER TABLE core.task ADD COLUMN work_slots integer CHECK(work_slots BETWEEN 1 AND 8);
UPDATE core.task SET work_slots=1;
-- A snapshot is immutable once assigned. NULL is reserved for never-started tasks.
CREATE FUNCTION core.guard_work_slots() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.work_slots IS NOT NULL AND NEW.work_slots IS DISTINCT FROM OLD.work_slots THEN
  RAISE EXCEPTION 'task work slots snapshot is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.work_slots IS NULL AND NEW.state='RUNNING' AND OLD.state<>'RUNNING' THEN
  NEW.work_slots=COALESCE(NULLIF(current_setting('bba.task_work_slots',true),''),'1')::integer;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_work_slots_guard BEFORE UPDATE ON core.task FOR EACH ROW EXECUTE FUNCTION core.guard_work_slots();
INSERT INTO core.schema_version(version) VALUES(23);
