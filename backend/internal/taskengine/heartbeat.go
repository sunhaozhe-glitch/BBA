package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
)

// heartbeatMaxCapabilities mirrors the wire contract bound. The receiver never
// infers capability support from the declaration; it only stores the bounded
// declaration as evidence for later verification.
const heartbeatMaxCapabilities = 8

// hbParseDecimal accepts the contract's positive decimal string. The database
// stores bigint; a value that does not fit is rejected instead of truncated.
func hbParseDecimal(value string) (int64, error) {
	if value == "" || value[0] < '1' || value[0] > '9' {
		return 0, errors.New("invalid decimal sequence")
	}
	for _, r := range value[1:] {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid decimal sequence")
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("invalid decimal sequence")
	}
	return parsed, nil
}

func hbConnectionValid(state string) bool {
	switch contract.HeartbeatRequestConnectionState(state) {
	case contract.HeartbeatRequestConnectionStateOnline, contract.HeartbeatRequestConnectionStateOffline,
		contract.HeartbeatRequestConnectionStateDegraded, contract.HeartbeatRequestConnectionStateUnknown:
		return true
	}
	return false
}

func hbLoginValid(state string) bool {
	switch contract.HeartbeatRequestLoginState(state) {
	case contract.HeartbeatRequestLoginStateLoggedIn, contract.HeartbeatRequestLoginStateLoggedOut,
		contract.HeartbeatRequestLoginStateChallengeRequired, contract.HeartbeatRequestLoginStateUnknown:
		return true
	}
	return false
}

// ReceiveHeartbeat records one runtime heartbeat and applies it only when it is
// strictly newer than the stored sequence for the same owner epoch. The audit
// rows are appended for applied and stale deliveries alike; replaying the same
// (epoch, sequence) with the same payload is idempotent, while a different
// payload is a business conflict rather than a bare unique violation.
func (s *Store) ReceiveHeartbeat(ctx context.Context, actor RuntimeActor, runtimeID, requestID string, req contract.HeartbeatRequest) (contract.HeartbeatReceipt, error) {
	var out contract.HeartbeatReceipt
	if taskBlank(runtimeID) || runtimeID != actor.RuntimeID || req.RuntimeId != actor.RuntimeID ||
		req.WorkspaceId != actor.WorkspaceID || taskBlank(req.AccountId) {
		return out, problem(taskCodeNotFound)
	}
	if taskBlank(requestID) || len(requestID) > 128 {
		return out, problem(taskCodeValidation)
	}
	epoch, err := hbParseDecimal(req.OwnerEpoch)
	if err != nil {
		return out, problem(taskCodeValidation)
	}
	sequence, err := hbParseDecimal(req.Sequence)
	if err != nil {
		return out, problem(taskCodeValidation)
	}
	if epoch != actor.OwnerEpoch {
		return out, problem("ACCOUNT_RESTRICTED")
	}
	if !hbConnectionValid(string(req.ConnectionState)) || !hbLoginValid(string(req.LoginState)) ||
		req.HeartbeatAt.IsZero() || len(req.Capabilities) > heartbeatMaxCapabilities {
		return out, problem(taskCodeValidation)
	}
	capabilityJSON, err := json.Marshal(req.Capabilities)
	if err != nil || hbValidateCapabilities(capabilityJSON) != nil {
		return out, problem(taskCodeValidation)
	}
	payload, err := digest(req)
	if err != nil {
		return out, problem(taskCodeValidation)
	}
	capabilities, err := json.Marshal(req.Capabilities)
	if err != nil {
		return out, problem(taskCodeValidation)
	}

	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRuntime(ctx, tx, actor, req.AccountId); err != nil {
			return err
		}
		var version int
		var connection, login, control string
		var nextAction time.Time
		if err := tx.QueryRow(ctx, `SELECT version,connection_state,login_state,control_state,next_action_at FROM core.account
			WHERE workspace_id=$1 AND id=$2 AND runtime_id=$3 FOR UPDATE`,
			actor.WorkspaceID, req.AccountId, actor.RuntimeID).Scan(&version, &connection, &login, &control, &nextAction); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(taskCodeNotFound)
			}
			return err
		}

		// Idempotency-Key replay returns the receipt stored by the original
		// request; the same key with a different request is a conflict. Both
		// the original key and every successful alias are bound to exactly
		// one heartbeat fact.
		existing, storedDigest, err := hbAliasReceipt(ctx, tx, actor, req.AccountId, requestID)
		switch {
		case err == nil:
			if storedDigest != payload {
				return problem(taskCodeIdempotency)
			}
			out = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		var storedEpoch, storedSequence int64
		err = tx.QueryRow(ctx, `SELECT owner_epoch,sequence FROM core.runtime_heartbeat
			WHERE workspace_id=$1 AND runtime_id=$2 AND account_id=$3 ORDER BY owner_epoch DESC,sequence DESC LIMIT 1 FOR UPDATE`,
			actor.WorkspaceID, actor.RuntimeID, req.AccountId).Scan(&storedEpoch, &storedSequence)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var sameSequenceDigest string
		err = tx.QueryRow(ctx, `SELECT digest FROM core.runtime_heartbeat
			WHERE workspace_id=$1 AND runtime_id=$2 AND account_id=$3 AND owner_epoch=$4 AND sequence=$5`,
			actor.WorkspaceID, actor.RuntimeID, req.AccountId, epoch, sequence).Scan(&sameSequenceDigest)
		switch {
		case err == nil:
			if sameSequenceDigest != payload {
				return problem("HEARTBEAT_SEQUENCE_CONFLICT")
			}
			// Same event under a new idempotency key: bind the alias to the
			// existing fact and reuse the original receipt.
			if err := hbBindAlias(ctx, tx, actor, req.AccountId, requestID, epoch, sequence, payload); err != nil {
				return err
			}
			existing, _, err := hbStoredReceiptBySequence(ctx, tx, actor, req.AccountId, epoch, sequence)
			if err != nil {
				return err
			}
			out = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		// A newer owner epoch starts a new sequence space; only then is a low
		// sequence newer than the previous epoch's high sequence.
		disposition := "stale"
		if epoch > storedEpoch || (epoch == storedEpoch && sequence > storedSequence) {
			disposition = "applied"
		}
		acceptedAt := time.Now().UTC().Truncate(time.Microsecond)
		if disposition == "applied" {
			if err := tx.QueryRow(ctx, `UPDATE core.account
				SET connection_state=$4,login_state=$5,
					heartbeat_at=GREATEST(COALESCE(heartbeat_at,$6),$6),
					version=version+1
				WHERE workspace_id=$1 AND id=$2 AND runtime_id=$3
				RETURNING version,control_state,next_action_at`,
				actor.WorkspaceID, req.AccountId, actor.RuntimeID,
				string(req.ConnectionState), string(req.LoginState), req.HeartbeatAt.UTC()).
				Scan(&version, &control, &nextAction); err != nil {
				return err
			}
		}
		var nextActionValue *time.Time
		if control != "active" {
			next := nextAction.UTC()
			nextActionValue = &next
		}
		if _, err := tx.Exec(ctx, `INSERT INTO core.runtime_heartbeat(
			workspace_id,runtime_id,account_id,id,owner_epoch,sequence,connection_state,login_state,heartbeat_at,capabilities,request_id,digest,disposition,accepted_at,account_version,control_state,next_action_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			actor.WorkspaceID, actor.RuntimeID, req.AccountId, newID("hb"), epoch, sequence,
			string(req.ConnectionState), string(req.LoginState), req.HeartbeatAt.UTC(), capabilities, requestID, payload, disposition,
			acceptedAt, version, control, nextActionValue); err != nil {
			return err
		}
		if err := hbBindAlias(ctx, tx, actor, req.AccountId, requestID, epoch, sequence, payload); err != nil {
			return err
		}

		// The receipt always echoes this request, never an older stored sequence.
		out = contract.HeartbeatReceipt{
			AcceptedAt:     acceptedAt.UTC(),
			AccountId:      req.AccountId,
			AccountVersion: version,
			ControlState:   contract.HeartbeatReceiptControlState(control),
			OwnerEpoch:     strconv.FormatInt(epoch, 10),
			RuntimeId:      actor.RuntimeID,
			Sequence:       strconv.FormatInt(sequence, 10),
			NextActionAt:   nullable.NewNullNullable[time.Time](),
		}
		if nextActionValue != nil {
			out.NextActionAt.Set(*nextActionValue)
		}
		return nil
	})
	return out, err
}

func hbAliasReceipt(ctx context.Context, tx pgx.Tx, actor RuntimeActor, accountID, requestID string) (contract.HeartbeatReceipt, string, error) {
	var out contract.HeartbeatReceipt
	var digest string
	var acceptedAt time.Time
	var version int
	var control string
	var nextAction *time.Time
	err := tx.QueryRow(ctx, `SELECT h.digest,h.accepted_at,h.account_version,h.control_state,h.next_action_at,h.owner_epoch,h.sequence
		FROM core.heartbeat_idempotency i
		JOIN core.runtime_heartbeat h ON (h.workspace_id,h.runtime_id,h.account_id,h.owner_epoch,h.sequence)=
			(i.workspace_id,i.runtime_id,i.account_id,i.owner_epoch,i.sequence)
		WHERE i.workspace_id=$1 AND i.runtime_id=$2 AND i.account_id=$3 AND i.request_id=$4`,
		actor.WorkspaceID, actor.RuntimeID, accountID, requestID).
		Scan(&digest, &acceptedAt, &version, &control, &nextAction, &out.OwnerEpoch, &out.Sequence)
	if err != nil {
		return out, "", err
	}
	out = hbReceiptValues(out.OwnerEpoch, out.Sequence, acceptedAt, version, control, nextAction, accountID, actor.RuntimeID)
	return out, digest, nil
}

func hbBindAlias(ctx context.Context, tx pgx.Tx, actor RuntimeActor, accountID, requestID string, epoch, sequence int64, payload string) error {
	_, err := tx.Exec(ctx, `INSERT INTO core.heartbeat_idempotency(
		workspace_id,runtime_id,account_id,request_id,owner_epoch,sequence,digest)
		VALUES($1,$2,$3,$4,$5,$6,$7)`,
		actor.WorkspaceID, actor.RuntimeID, accountID, requestID, epoch, sequence, payload)
	return err
}

func hbStoredReceiptBySequence(ctx context.Context, tx pgx.Tx, actor RuntimeActor, accountID string, epoch, sequence int64) (contract.HeartbeatReceipt, string, error) {
	var out contract.HeartbeatReceipt
	var digest string
	var acceptedAt time.Time
	var version int
	var control string
	var nextAction *time.Time
	err := tx.QueryRow(ctx, `SELECT digest,accepted_at,account_version,control_state,next_action_at
		FROM core.runtime_heartbeat
		WHERE workspace_id=$1 AND runtime_id=$2 AND account_id=$3 AND owner_epoch=$4 AND sequence=$5`,
		actor.WorkspaceID, actor.RuntimeID, accountID, epoch, sequence).
		Scan(&digest, &acceptedAt, &version, &control, &nextAction)
	if err != nil {
		return out, "", err
	}
	out = hbReceiptValues(strconv.FormatInt(epoch, 10), strconv.FormatInt(sequence, 10), acceptedAt, version, control, nextAction, accountID, actor.RuntimeID)
	return out, digest, nil
}

func hbReceiptValues(epoch, sequence string, acceptedAt time.Time, version int, control string, nextAction *time.Time, accountID, runtimeID string) contract.HeartbeatReceipt {
	out := contract.HeartbeatReceipt{
		AcceptedAt:     acceptedAt.UTC(),
		AccountId:      accountID,
		AccountVersion: version,
		ControlState:   contract.HeartbeatReceiptControlState(control),
		OwnerEpoch:     epoch,
		RuntimeId:      runtimeID,
		Sequence:       sequence,
		NextActionAt:   nullable.NewNullNullable[time.Time](),
	}
	if nextAction != nil {
		out.NextActionAt.Set(nextAction.UTC())
	}
	return out
}

// hbValidateCapabilities enforces common.json#/Capability for each declared
// item. The generated types would otherwise accept zero values, so a bare {}
// must fail here instead of being stored as a plausible capability.
func hbValidateCapabilities(raw json.RawMessage) error {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return errors.New("capabilities must be an array")
	}
	if len(items) > heartbeatMaxCapabilities {
		return errors.New("too many capabilities")
	}
	for _, item := range items {
		if err := hbValidateObject(item, []string{"account_id", "adapter_id", "adapter_version", "verification_state", "verified_at", "evidence_ref", "actions"}); err != nil {
			return err
		}
		if err := hbRejectNull(item, "account_id", "adapter_id", "adapter_version", "verification_state", "actions"); err != nil {
			return err
		}
		if err := hbValidateID(item["account_id"]); err != nil {
			return err
		}
		for _, field := range []string{"adapter_id", "adapter_version"} {
			var value string
			if err := json.Unmarshal(item[field], &value); err != nil || !hbTextWithin(value, 128) {
				return errors.New("invalid adapter identity")
			}
		}
		var verification string
		if err := json.Unmarshal(item["verification_state"], &verification); err != nil ||
			(verification != "unverified" && verification != "verified") {
			return errors.New("invalid verification state")
		}
		if err := hbNullableUTC(item["verified_at"]); err != nil {
			return err
		}
		if err := hbNullableID(item["evidence_ref"]); err != nil {
			return err
		}
		var actions map[string]json.RawMessage
		if err := json.Unmarshal(item["actions"], &actions); err != nil || actions == nil {
			return errors.New("invalid actions")
		}
		if err := hbValidateObject(actions, []string{"search_groups", "request_join", "observe_membership", "observe_join_request", "cancel_join_request", "send_text"}); err != nil {
			return err
		}
		for _, action := range actions {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(action, &fields); err != nil || fields == nil {
				return errors.New("invalid capability action")
			}
			if err := hbValidateObject(fields, []string{"supported", "effect", "pagination", "fresh_read", "idempotency"}); err != nil {
				return err
			}
			if err := hbRejectNull(fields, "supported", "effect", "pagination", "fresh_read", "idempotency"); err != nil {
				return err
			}
			var supported bool
			if err := json.Unmarshal(fields["supported"], &supported); err != nil {
				return errors.New("invalid supported flag")
			}
			var fresh bool
			if err := json.Unmarshal(fields["fresh_read"], &fresh); err != nil {
				return errors.New("invalid fresh_read flag")
			}
			for field, allowed := range map[string][]string{
				"effect":      {"read", "write", "none"},
				"pagination":  {"none", "cursor"},
				"idempotency": {"none", "operation_id"},
			} {
				var value string
				if err := json.Unmarshal(fields[field], &value); err != nil || !contains(allowed, value) {
					return errors.New("invalid capability action enum")
				}
			}
		}
	}
	return nil
}

func hbRejectNull(object map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		if string(object[field]) == "null" {
			return errors.New("required capability field is null")
		}
	}
	return nil
}

func hbValidateObject(object map[string]json.RawMessage, required []string) error {
	if len(object) != len(required) {
		return errors.New("unexpected or missing capability fields")
	}
	for _, field := range required {
		if _, ok := object[field]; !ok {
			return errors.New("missing capability field")
		}
	}
	return nil
}

func hbTextWithin(value string, max int) bool {
	n := utf8.RuneCountInString(value)
	return n >= 1 && n <= max
}

func hbValidateID(raw json.RawMessage) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !hbTextWithin(value, 128) {
		return errors.New("invalid id")
	}
	return nil
}

func hbNullableID(raw json.RawMessage) error {
	if string(raw) == "null" {
		return nil
	}
	return hbValidateID(raw)
}

func hbNullableUTC(raw json.RawMessage) error {
	if string(raw) == "null" {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || len(value) == 0 || value[len(value)-1] != 'Z' {
		return errors.New("invalid UTC timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC {
		return errors.New("invalid UTC timestamp")
	}
	return nil
}
