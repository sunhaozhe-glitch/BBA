-- P0 console account login: durable, workspace/account/instance-scoped login
-- session with an explicit generation fence.
--
-- The console never talks to NapCat/QQ directly and never receives a provider
-- credential. A session is a server-side state machine:
--   CREATED -> QR_READY -> SCANNED -> AUTHENTICATING -> ONLINE
-- with terminal EXPIRED, CANCELLED and FAILED. Only ONLINE is projected into
-- core.account, and only after the provider readback matched the account
-- binding; a scanned QR by itself changes no readiness flag.
--
-- generation is the fence: issuing/refreshing a QR, cancelling and terminal
-- transitions increment it; intermediate observation polls (SCANNED,
-- AUTHENTICATING) verify it without moving it, so a client can poll repeatedly
-- with one expected_generation. A stale poll response therefore cannot
-- overwrite a newer QR or reopen a cancelled session even when its HTTP
-- response is delivered late.
SET lock_timeout = '5s';

CREATE TABLE core.login_session(
 workspace_id core.id NOT NULL,
 id core.id NOT NULL,
 account_id core.id NOT NULL,
 adapter_instance_id core.id NOT NULL,
 runtime_id core.id NOT NULL,
 provider text NOT NULL CHECK(provider IN ('fixture','napcat')),
 state text NOT NULL DEFAULT 'CREATED' CHECK(state IN ('CREATED','QR_READY','SCANNED','AUTHENTICATING','ONLINE','EXPIRED','CANCELLED','FAILED')),
 generation bigint NOT NULL DEFAULT 0 CHECK(generation>=0),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),
 qr_expires_at timestamptz,
 identity_self_id core.qq_id,
 identity_nickname text CHECK(identity_nickname IS NULL OR (length(identity_nickname) BETWEEN 1 AND 128 AND btrim(identity_nickname)<>'')),
 verified_at timestamptz,
 binding_checked boolean NOT NULL DEFAULT false,
 projected boolean NOT NULL DEFAULT false,
 failure_reason text CHECK(failure_reason IS NULL OR failure_reason IN ('PROVIDER_ERROR','QR_EXPIRED','TIMEOUT','IDENTITY_MISMATCH','IDENTITY_ALREADY_BOUND')),
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,id),
 FOREIGN KEY(workspace_id,account_id) REFERENCES core.account(workspace_id,id),
 CHECK(expires_at>created_at),
 -- A session starts at generation 0 without a QR. Any later state was reached
 -- through a fenced write that issued a QR or bumped the fence on cancel.
 CHECK((state='CREATED' AND generation=0 AND qr_expires_at IS NULL) OR
       (state<>'CREATED' AND generation>=1)),
 CHECK(qr_expires_at IS NULL OR (qr_expires_at>created_at AND qr_expires_at<=expires_at)),
 -- ONLINE is only reachable with a verified identity readback and a bound
 -- account projection, in the same transaction.
 CHECK(state<>'ONLINE' OR (identity_self_id IS NOT NULL AND identity_nickname IS NOT NULL AND verified_at IS NOT NULL AND binding_checked AND projected)),
 CHECK(state<>'FAILED' OR failure_reason IS NOT NULL),
 -- QR-bearing states always carry the QR deadline; the exempt states are the
 -- ones a session can enter before any QR existed.
 CHECK(state IN ('CREATED','CANCELLED','FAILED','EXPIRED') OR qr_expires_at IS NOT NULL)
);

-- A second open session for one account is rejected by the unique guard, so
-- concurrent creates resolve to one durable session instead of two QR flows.
CREATE UNIQUE INDEX login_session_open_account ON core.login_session(workspace_id,account_id)
 WHERE state IN ('CREATED','QR_READY','SCANNED','AUTHENTICATING');

-- Newest-first bounded list per account.
CREATE INDEX login_session_account_created ON core.login_session(workspace_id,account_id,created_at DESC,id DESC);

-- Durable audit trail for the login chain. detail is a bounded object with
-- provider phase names only: never a QR image, token, URL or provider body.
CREATE TABLE core.login_session_event(
 workspace_id core.id NOT NULL,
 session_id core.id NOT NULL,
 seq bigint NOT NULL CHECK(seq>0),
 id core.id NOT NULL,
 kind text NOT NULL CHECK(kind IN ('created','qr_issued','qr_refreshed','scanned','authenticating','online','expired','cancelled','failed')),
 generation bigint NOT NULL CHECK(generation>=0),
 detail jsonb NOT NULL CHECK(jsonb_typeof(detail)='object'),
 actor text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,session_id,seq),
 UNIQUE(workspace_id,id),
 FOREIGN KEY(workspace_id,session_id) REFERENCES core.login_session(workspace_id,id) ON DELETE RESTRICT
);
CREATE INDEX login_session_event_created ON core.login_session_event(workspace_id,created_at,id);

CREATE FUNCTION core.immutable_login_session_event() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'login session events are immutable' USING ERRCODE='23514';
END $fn$;
CREATE TRIGGER login_session_event_immutable
 BEFORE UPDATE OR DELETE ON core.login_session_event
 FOR EACH ROW EXECUTE FUNCTION core.immutable_login_session_event();

-- Terminal states are terminal, and the fence never moves backwards. The Go
-- transaction already re-reads the row under FOR UPDATE; this trigger keeps a
-- direct or future writer from reopening a cancelled/expired session.
CREATE FUNCTION core.guard_login_session_transition() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.workspace_id<>OLD.workspace_id OR NEW.id<>OLD.id OR NEW.account_id<>OLD.account_id OR
     NEW.adapter_instance_id<>OLD.adapter_instance_id OR NEW.runtime_id<>OLD.runtime_id OR
     NEW.provider<>OLD.provider OR NEW.created_at<>OLD.created_at THEN
    RAISE EXCEPTION 'login session identity is immutable' USING ERRCODE='23514';
  END IF;
  IF NEW.generation<OLD.generation THEN
    RAISE EXCEPTION 'login session generation cannot decrease' USING ERRCODE='23514';
  END IF;
  IF OLD.state IN ('ONLINE','EXPIRED','CANCELLED','FAILED') AND NEW.state<>OLD.state THEN
    RAISE EXCEPTION 'terminal login session state cannot transition' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
END $fn$;
CREATE TRIGGER login_session_transition_guard
 BEFORE UPDATE ON core.login_session
 FOR EACH ROW EXECUTE FUNCTION core.guard_login_session_transition();

INSERT INTO core.schema_version(version) VALUES(21);
