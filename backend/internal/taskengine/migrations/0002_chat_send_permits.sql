-- P0-15 additive chat text permit state. This migration is applied only by
-- explicit fixture/approved Migrate calls; existing schema 10 data is kept.
CREATE TABLE core.chat_send_policy(
 workspace_id core.id NOT NULL,
 account_id core.id NOT NULL,
 enabled boolean NOT NULL DEFAULT false,
 daily_limit integer NOT NULL CHECK(daily_limit BETWEEN 1 AND 10000),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,account_id),
 FOREIGN KEY(workspace_id,account_id) REFERENCES core.account(workspace_id,id)
);
CREATE TABLE core.group_binding(
 workspace_id core.id NOT NULL,
 group_id core.qq_id NOT NULL,
 active_speaker_account_id core.id NOT NULL,
 binding_epoch bigint NOT NULL CHECK(binding_epoch>0),
 lease_expires_at timestamptz NOT NULL,
 version integer NOT NULL DEFAULT 1 CHECK(version>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,group_id),
 FOREIGN KEY(workspace_id,active_speaker_account_id) REFERENCES core.account(workspace_id,id)
);
CREATE FUNCTION core.group_binding_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.lease_expires_at<=clock_timestamp() THEN RAISE EXCEPTION 'speaker lease must be future dated' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' THEN
  IF NEW.binding_epoch<OLD.binding_epoch OR NEW.version<=OLD.version THEN RAISE EXCEPTION 'speaker binding epoch/version must advance' USING ERRCODE='23514'; END IF;
  IF NEW.active_speaker_account_id<>OLD.active_speaker_account_id AND NEW.binding_epoch<=OLD.binding_epoch THEN RAISE EXCEPTION 'speaker change must advance epoch' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW; END $$;
CREATE TRIGGER group_binding_guard BEFORE INSERT OR UPDATE ON core.group_binding FOR EACH ROW EXECUTE FUNCTION core.group_binding_guard();

CREATE TABLE core.chat_trigger(
 workspace_id core.id NOT NULL,
 trigger_ref core.id NOT NULL,
 runtime_id core.id NOT NULL,
 owner_epoch bigint NOT NULL CHECK(owner_epoch>0),
 account_id core.id NOT NULL,
 source_event_id core.id NOT NULL,
 conversation_kind text NOT NULL CHECK(conversation_kind IN ('group','private')),
 conversation_id core.qq_id NOT NULL,
 sender_id core.qq_id NOT NULL,
 directed_account_id core.id,
 deadline_at timestamptz NOT NULL,
 segment_count integer NOT NULL CHECK(segment_count BETWEEN 1 AND 32),
 next_segment integer NOT NULL DEFAULT 0 CHECK(next_segment BETWEEN 0 AND 32),
 state text NOT NULL DEFAULT 'open' CHECK(state IN ('open','complete','expired','suppressed')),
 request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,trigger_ref),
 UNIQUE(workspace_id,runtime_id,account_id,source_event_id),
 FOREIGN KEY(workspace_id,account_id) REFERENCES core.account(workspace_id,id),
 FOREIGN KEY(workspace_id,directed_account_id) REFERENCES core.account(workspace_id,id),
 CHECK((segment_count IS NULL AND next_segment=0) OR (segment_count IS NOT NULL AND next_segment<=segment_count)),
 CHECK((state='complete' AND segment_count=next_segment) OR state<>'complete')
);
CREATE TABLE core.chat_send_usage(
 workspace_id core.id NOT NULL,
 account_id core.id NOT NULL,
 window_start date NOT NULL,
 issued integer NOT NULL DEFAULT 0 CHECK(issued>=0),
 PRIMARY KEY(workspace_id,account_id,window_start),
 FOREIGN KEY(workspace_id,account_id) REFERENCES core.account(workspace_id,id)
);
CREATE TABLE core.chat_send_permit(
 workspace_id core.id NOT NULL,
 permit_id core.id NOT NULL,
 runtime_id core.id NOT NULL,
 owner_epoch bigint NOT NULL CHECK(owner_epoch>0),
 account_id core.id NOT NULL,
 send_id core.id NOT NULL,
 conversation_kind text NOT NULL CHECK(conversation_kind IN ('group','private')),
 conversation_id core.qq_id NOT NULL,
 trigger_ref core.id NOT NULL,
 text_digest text NOT NULL CHECK(text_digest ~ '^[a-f0-9]{64}$'),
 expected_binding_epoch bigint NOT NULL CHECK(expected_binding_epoch>0),
 deadline_at timestamptz NOT NULL,
 segment_index integer NOT NULL CHECK(segment_index BETWEEN 0 AND 31),
 segment_count integer NOT NULL CHECK(segment_count BETWEEN 1 AND 32 AND segment_index<segment_count),
 issued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'issued' CHECK(state IN ('issued','consumed','expired')),
 consumed_at timestamptz,
 PRIMARY KEY(workspace_id,permit_id),
 UNIQUE(workspace_id,send_id,segment_index),
 UNIQUE(workspace_id,runtime_id,send_id,segment_index),
 FOREIGN KEY(workspace_id,account_id) REFERENCES core.account(workspace_id,id),
 FOREIGN KEY(workspace_id,trigger_ref) REFERENCES core.chat_trigger(workspace_id,trigger_ref),
 CHECK(expires_at>issued_at AND expires_at<=deadline_at),
 CHECK((state='consumed' AND consumed_at IS NOT NULL) OR (state<>'consumed' AND consumed_at IS NULL))
);
CREATE TABLE core.chat_send_idempotency(
 workspace_id core.id NOT NULL,
 runtime_id core.id NOT NULL,
 idempotency_key core.id NOT NULL,
 request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
 permit_id core.id NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,runtime_id,idempotency_key),
 FOREIGN KEY(workspace_id,permit_id) REFERENCES core.chat_send_permit(workspace_id,permit_id)
);
CREATE TABLE core.chat_trigger_idempotency(
 workspace_id core.id NOT NULL,
 runtime_id core.id NOT NULL,
 idempotency_key core.id NOT NULL,
 request_digest text NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),
 trigger_ref core.id NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,runtime_id,idempotency_key),
 FOREIGN KEY(workspace_id,trigger_ref) REFERENCES core.chat_trigger(workspace_id,trigger_ref)
);
CREATE TABLE core.chat_send_outcome(
 workspace_id core.id NOT NULL,
 permit_id core.id NOT NULL,
 send_id core.id NOT NULL,
 state text NOT NULL CHECK(state IN ('sent','unknown')),
 message_id core.id,
 occurred_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,permit_id),
 FOREIGN KEY(workspace_id,permit_id) REFERENCES core.chat_send_permit(workspace_id,permit_id),
 CHECK(state='sent' OR message_id IS NULL)
);
CREATE INDEX chat_trigger_deadline ON core.chat_trigger(state,deadline_at);
CREATE INDEX chat_permit_expiry ON core.chat_send_permit(state,expires_at);
CREATE FUNCTION core.immutable_chat_send_permit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF TG_OP='DELETE' OR (to_jsonb(NEW)-ARRAY['state','consumed_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['state','consumed_at']) THEN RAISE EXCEPTION 'chat permit identity is immutable' USING ERRCODE='23514'; END IF;
 IF NEW.state=OLD.state AND NEW.consumed_at IS DISTINCT FROM OLD.consumed_at THEN RAISE EXCEPTION 'chat consumed timestamp requires consumed state' USING ERRCODE='23514'; END IF;
 IF OLD.state<>'issued' AND NEW.state<>OLD.state THEN RAISE EXCEPTION 'terminal chat permit state is immutable' USING ERRCODE='23514'; END IF;
 IF OLD.state='issued' AND NEW.state NOT IN ('issued','consumed','expired') THEN RAISE EXCEPTION 'invalid chat permit transition' USING ERRCODE='23514'; END IF;
 IF NEW.state='consumed' AND NEW.consumed_at IS NULL THEN RAISE EXCEPTION 'consumed chat permit requires consumed_at' USING ERRCODE='23514'; END IF;
 IF NEW.state<>'consumed' AND NEW.consumed_at IS NOT NULL THEN RAISE EXCEPTION 'only consumed chat permit carries consumed_at' USING ERRCODE='23514'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER chat_send_permit_immutable BEFORE UPDATE OR DELETE ON core.chat_send_permit FOR EACH ROW EXECUTE FUNCTION core.immutable_chat_send_permit();
CREATE FUNCTION core.append_only_chat_trigger() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'chat trigger evidence is append only' USING ERRCODE='23514'; END $$;
CREATE TRIGGER chat_trigger_append_only BEFORE DELETE ON core.chat_trigger FOR EACH ROW EXECUTE FUNCTION core.append_only_chat_trigger();
CREATE FUNCTION core.chat_trigger_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.next_segment<>0 OR NEW.state<>'open' THEN RAISE EXCEPTION 'chat trigger must start open at segment zero' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF (to_jsonb(NEW)-ARRAY['state','next_segment']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['state','next_segment']) THEN RAISE EXCEPTION 'chat trigger identity is immutable' USING ERRCODE='23514'; END IF;
 IF OLD.state<>'open' AND NEW.state<>OLD.state THEN RAISE EXCEPTION 'terminal chat trigger state is immutable' USING ERRCODE='23514'; END IF;
 IF OLD.state<>'open' AND NEW.next_segment<>OLD.next_segment THEN RAISE EXCEPTION 'terminal chat trigger segment is immutable' USING ERRCODE='23514'; END IF;
 IF NEW.state NOT IN ('open','complete','expired','suppressed') THEN RAISE EXCEPTION 'invalid chat trigger state' USING ERRCODE='23514'; END IF;
 IF NEW.next_segment<>OLD.next_segment AND NEW.next_segment<>OLD.next_segment+1 THEN RAISE EXCEPTION 'chat trigger advances one segment' USING ERRCODE='23514'; END IF;
 IF NEW.next_segment=NEW.segment_count AND NEW.state<>'complete' THEN RAISE EXCEPTION 'final chat trigger segment completes' USING ERRCODE='23514'; END IF;
 RETURN NEW; END $$;
CREATE TRIGGER chat_trigger_guard BEFORE INSERT OR UPDATE ON core.chat_trigger FOR EACH ROW EXECUTE FUNCTION core.chat_trigger_guard();
CREATE TRIGGER chat_send_idempotency_append_only BEFORE UPDATE OR DELETE ON core.chat_send_idempotency FOR EACH ROW EXECUTE FUNCTION core.append_only();
CREATE TRIGGER chat_trigger_idempotency_append_only BEFORE UPDATE OR DELETE ON core.chat_trigger_idempotency FOR EACH ROW EXECUTE FUNCTION core.append_only();
CREATE TRIGGER chat_send_outcome_append_only BEFORE UPDATE OR DELETE ON core.chat_send_outcome FOR EACH ROW EXECUTE FUNCTION core.append_only();
INSERT INTO core.schema_version(version) VALUES(11);
