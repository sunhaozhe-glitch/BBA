package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	chatPermPolicy       = "account.chat.manage"
	chatPermSpeakerLease = "group.binding.manage"
)

type ChatPolicyRequest struct {
	Enabled         bool `json:"enabled"`
	DailyLimit      int  `json:"daily_limit"`
	ExpectedVersion *int `json:"expected_version"`
}

type ChatPolicy struct {
	AccountID  string    `json:"account_id"`
	Enabled    bool      `json:"enabled"`
	DailyLimit int       `json:"daily_limit"`
	Version    int       `json:"version"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type GroupSpeakerLeaseRequest struct {
	AccountID       string    `json:"account_id"`
	BindingEpoch    string    `json:"binding_epoch"`
	LeaseExpiresAt  time.Time `json:"lease_expires_at"`
	ExpectedVersion *int      `json:"expected_version"`
}

type GroupSpeakerLease struct {
	GroupID                string    `json:"group_id"`
	ActiveSpeakerAccountID string    `json:"active_speaker_account_id"`
	BindingEpoch           string    `json:"binding_epoch"`
	LeaseExpiresAt         time.Time `json:"lease_expires_at"`
	Version                int       `json:"version"`
	UpdatedAt              time.Time `json:"updated_at"`
}

func chatDecimal(value int64) string { return strconv.FormatInt(value, 10) }

func chatPolicyPath(accountID string) string { return "/v1/accounts/" + accountID + "/chat-policy" }
func chatSpeakerLeasePath(groupID string) string {
	return "/v1/groups/" + groupID + "/speaker-lease"
}

// chatRequireAdmin validates the operator identity and permission. task.manage_all
// is accepted as the existing workspace-wide administration grant so operators
// do not need a second role solely for P0-15 chat limits.
func chatRequireAdmin(actor Actor, permission string) error {
	if err := actor.require(permission); err == nil {
		return nil
	}
	if actor.Permissions["task.manage_all"] {
		return actor.require("task.manage_all")
	}
	return problem(taskCodeForbidden)
}

func (s *Store) chatAccountExists(ctx context.Context, tx pgx.Tx, workspace, accountID string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.account WHERE workspace_id=$1 AND id=$2)`,
		workspace, accountID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return problem(taskCodeNotFound)
	}
	return nil
}

// SetChatPolicy is the trusted operator switch for daily chat text. A missing
// row is disabled; expected_version makes concurrent operator edits explicit.
func (s *Store) SetChatPolicy(ctx context.Context, actor Actor, accountID, idempotencyKey string, req ChatPolicyRequest) (ChatPolicy, error) {
	var out ChatPolicy
	if err := chatRequireAdmin(actor, chatPermPolicy); err != nil {
		return out, err
	}
	if !chatValidID(accountID) || taskCheckIdempotencyKey(idempotencyKey) != nil ||
		req.DailyLimit < 1 || req.DailyLimit > 10000 || (req.ExpectedVersion != nil && *req.ExpectedVersion < 0) {
		return out, problem(taskCodeValidation)
	}
	path := chatPolicyPath(accountID)
	reqDigest, err := digest(req)
	if err != nil {
		return out, problem(taskCodeValidation)
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := s.chatAccountExists(ctx, tx, actor.WorkspaceID, accountID); err != nil {
			return err
		}
		rec, ok, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, http.MethodPut, path, idempotencyKey)
		if err != nil {
			return err
		}
		if ok {
			if rec.digest != reqDigest {
				return problem(taskCodeIdempotency)
			}
			return json.Unmarshal(rec.response, &out)
		}

		currentVersion := 0
		var enabled bool
		var limit int
		var updated time.Time
		err = tx.QueryRow(ctx, `SELECT enabled,daily_limit,version,updated_at FROM core.chat_send_policy
			WHERE workspace_id=$1 AND account_id=$2 FOR UPDATE`,
			actor.WorkspaceID, accountID).Scan(&enabled, &limit, &currentVersion, &updated)
		found := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if req.ExpectedVersion != nil && *req.ExpectedVersion != currentVersion {
			return problem(chatCodePolicyConflict)
		}
		if !found {
			err = tx.QueryRow(ctx, `INSERT INTO core.chat_send_policy(workspace_id,account_id,enabled,daily_limit)
				VALUES($1,$2,$3,$4) RETURNING account_id,enabled,daily_limit,version,updated_at`,
				actor.WorkspaceID, accountID, req.Enabled, req.DailyLimit).
				Scan(&out.AccountID, &out.Enabled, &out.DailyLimit, &out.Version, &out.UpdatedAt)
		} else {
			err = tx.QueryRow(ctx, `UPDATE core.chat_send_policy
				SET enabled=$3,daily_limit=$4,version=version+1,updated_at=clock_timestamp()
				WHERE workspace_id=$1 AND account_id=$2
				RETURNING account_id,enabled,daily_limit,version,updated_at`,
				actor.WorkspaceID, accountID, req.Enabled, req.DailyLimit).
				Scan(&out.AccountID, &out.Enabled, &out.DailyLimit, &out.Version, &out.UpdatedAt)
		}
		if err != nil {
			return err
		}
		out.UpdatedAt = out.UpdatedAt.UTC()
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, http.MethodPut, path,
			idempotencyKey, reqDigest, accountID, out)
	})
	if err != nil {
		return ChatPolicy{}, err
	}
	return out, nil
}

// SetGroupSpeakerLease installs or renews the single active speaker for a
// group. A speaker change must advance binding_epoch; version always advances.
func (s *Store) SetGroupSpeakerLease(ctx context.Context, actor Actor, groupID, idempotencyKey string, req GroupSpeakerLeaseRequest) (GroupSpeakerLease, error) {
	var out GroupSpeakerLease
	if err := chatRequireAdmin(actor, chatPermSpeakerLease); err != nil {
		return out, err
	}
	epoch, err := hbParseDecimal(req.BindingEpoch)
	if err != nil || !taskValidQQID(groupID) || !chatValidID(req.AccountID) ||
		taskCheckIdempotencyKey(idempotencyKey) != nil || req.LeaseExpiresAt.IsZero() ||
		(req.ExpectedVersion != nil && *req.ExpectedVersion < 0) {
		return out, problem(taskCodeValidation)
	}
	req.LeaseExpiresAt = req.LeaseExpiresAt.UTC()
	path := chatSpeakerLeasePath(groupID)
	reqDigest, err := digest(req)
	if err != nil {
		return out, problem(taskCodeValidation)
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := s.chatAccountExists(ctx, tx, actor.WorkspaceID, req.AccountID); err != nil {
			return err
		}
		rec, ok, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, http.MethodPut, path, idempotencyKey)
		if err != nil {
			return err
		}
		if ok {
			if rec.digest != reqDigest {
				return problem(taskCodeIdempotency)
			}
			return json.Unmarshal(rec.response, &out)
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if !req.LeaseExpiresAt.After(now) {
			return problem(chatCodeBindingConflict)
		}

		currentVersion := 0
		var speaker string
		var currentEpoch int64
		var lease time.Time
		var updated time.Time
		err = tx.QueryRow(ctx, `SELECT active_speaker_account_id,binding_epoch,lease_expires_at,version,updated_at
			FROM core.group_binding WHERE workspace_id=$1 AND group_id=$2 FOR UPDATE`,
			actor.WorkspaceID, groupID).Scan(&speaker, &currentEpoch, &lease, &currentVersion, &updated)
		found := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if req.ExpectedVersion != nil && *req.ExpectedVersion != currentVersion {
			return problem(chatCodeBindingConflict)
		}
		var epochOut int64
		if found {
			if epoch < currentEpoch || (speaker != req.AccountID && epoch <= currentEpoch) {
				return problem(chatCodeBindingConflict)
			}
			err = tx.QueryRow(ctx, `UPDATE core.group_binding
				SET active_speaker_account_id=$3,binding_epoch=$4,lease_expires_at=$5,version=version+1,updated_at=clock_timestamp()
				WHERE workspace_id=$1 AND group_id=$2
				RETURNING group_id,active_speaker_account_id,binding_epoch,lease_expires_at,version,updated_at`,
				actor.WorkspaceID, groupID, req.AccountID, epoch, req.LeaseExpiresAt).
				Scan(&out.GroupID, &out.ActiveSpeakerAccountID, &epochOut, &out.LeaseExpiresAt, &out.Version, &out.UpdatedAt)
		} else {
			err = tx.QueryRow(ctx, `INSERT INTO core.group_binding(
				workspace_id,group_id,active_speaker_account_id,binding_epoch,lease_expires_at)
				VALUES($1,$2,$3,$4,$5)
				RETURNING group_id,active_speaker_account_id,binding_epoch,lease_expires_at,version,updated_at`,
				actor.WorkspaceID, groupID, req.AccountID, epoch, req.LeaseExpiresAt).
				Scan(&out.GroupID, &out.ActiveSpeakerAccountID, &epochOut, &out.LeaseExpiresAt, &out.Version, &out.UpdatedAt)
		}
		if err != nil {
			return err
		}
		out.BindingEpoch = chatDecimal(epochOut)
		out.LeaseExpiresAt = out.LeaseExpiresAt.UTC()
		out.UpdatedAt = out.UpdatedAt.UTC()
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, http.MethodPut, path,
			idempotencyKey, reqDigest, groupID, out)
	})
	if err != nil {
		return GroupSpeakerLease{}, err
	}
	return out, nil
}
