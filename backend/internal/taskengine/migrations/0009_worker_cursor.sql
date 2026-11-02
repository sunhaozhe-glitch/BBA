-- Durable worker rotation cursors.
--
-- One row per worker consumer ("recovery", "bridge_outbox"). The triple
-- (workspace_id,runtime_id,owner_epoch) is a keyset position inside the active
-- runtime-credential population: a worker walks the groups strictly after it and
-- writes the empty position ('','',0) back when it reaches the end of the
-- population, so the next tick starts a new lap at the head again. A page is
-- therefore a bounded range read and a restarted process continues where the
-- previous one stopped instead of re-reading the same head page forever.
--
-- The cursor is a rotation hint, not a constraint: it holds no foreign key, so
-- retiring, superseding or re-adding a runtime credential can never make the
-- cursor unusable, and an inserted group with a key before the cursor is picked
-- up by the next lap.
--
-- lap_incomplete marks the current lap as not eligible to close a recovery pass.
-- It is durable on purpose and is raised by exactly two events: a group of the
-- lap failed, so a later clean page must not close the pass over it; or a process
-- started while the stored position was mid-lap, so the lap was inherited and
-- the new process owes a complete head-to-tail lap before it may close the pass
-- (a group may have started DISPATCHING after the inherited lap's last committed
-- page). Closing a lap clears the flag and only completes the pass when it was
-- clear.
SET lock_timeout = '5s';

CREATE TABLE core.worker_cursor(
 consumer core.id NOT NULL,
 workspace_id text NOT NULL DEFAULT '' CHECK(workspace_id='' OR (length(workspace_id) BETWEEN 1 AND 128 AND btrim(workspace_id)<>'')),
 runtime_id text NOT NULL DEFAULT '' CHECK(runtime_id='' OR (length(runtime_id) BETWEEN 1 AND 128 AND btrim(runtime_id)<>'')),
 owner_epoch bigint NOT NULL DEFAULT 0 CHECK(owner_epoch>=0),
 lap_incomplete boolean NOT NULL DEFAULT false,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(consumer),
 CHECK((workspace_id='' AND runtime_id='' AND owner_epoch=0) OR
       (workspace_id<>'' AND runtime_id<>'' AND owner_epoch>0))
);

-- The rotation reads active groups in keyset order, one page at a time. This
-- partial index keeps a page a range scan of the active population instead of a
-- scan of every credential row. The keyset tuple comparison uses exactly these
-- columns in this order.
CREATE INDEX runtime_credentials_active_rotation ON core.runtime_credentials(workspace_id,runtime_id,owner_epoch) INCLUDE (account_id) WHERE active;

-- The active population can change while a rotation lap is in flight: a new
-- group may appear behind the position the lap already passed, a group may gain
-- an account, or one may be retired. A lap may only close a recovery pass when it
-- swept a population that did not change underneath it, so every credential
-- change bumps a single counter. Comparing the counter across a lap costs one
-- row read per segment instead of a scan, and a changed population only costs the
-- next lap, not a skipped group.
CREATE TABLE core.runtime_credential_population(
 only_row boolean PRIMARY KEY DEFAULT true CHECK(only_row),
 generation bigint NOT NULL DEFAULT 1 CHECK(generation>0),
 changed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO core.runtime_credential_population(only_row) VALUES(true);

CREATE FUNCTION core.runtime_credential_population_bump() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  UPDATE core.runtime_credential_population
     SET generation=generation+1, changed_at=clock_timestamp()
   WHERE only_row;
  RETURN NULL;
END $fn$;

CREATE TRIGGER runtime_credential_population_bump
  AFTER INSERT OR UPDATE OR DELETE ON core.runtime_credentials
  FOR EACH STATEMENT EXECUTE FUNCTION core.runtime_credential_population_bump();

INSERT INTO core.schema_version(version) VALUES(18);
