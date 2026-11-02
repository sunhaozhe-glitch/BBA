package taskengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5"
)

// P0-15 chat send permits. These are deliberately independent of the task
// engine: issuing or consuming a chat permit never creates a task, subtask,
// goal slot, execution, target claim, or dispatch permit. The durable permit
// is the only authority for one text adapter call.
const (
	chatPermitTTL = 15 * time.Second

	chatPermitSegmentCountMax   = 32
	chatPermitTextDigestPattern = `^[a-f0-9]{64}$`
	chatPermitOutcomeSent       = "sent"
	chatPermitOutcomeUnknown    = "unknown"
)

var chatDigestPattern = regexp.MustCompile(chatPermitTextDigestPattern)

// ChatConsumeRequest is the strict body of the one-shot consume call. It
// names the exact segment and text digest the permit authorizes.
type ChatConsumeRequest struct {
	SendId       string `json:"send_id"`
	SegmentIndex int    `json:"segment_index"`
	TextDigest   string `json:"text_digest"`
}

// ChatSendOutcomeRequest records what happened after a consumed permit. A
// consumed permit with no outcome is an unresolved unknown and is never
// reissued; "unknown" is terminal and is never converted into a resend.
type ChatSendOutcomeRequest struct {
	SendId     string    `json:"send_id"`
	State      string    `json:"state"`
	MessageId  *string   `json:"message_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ChatSendOutcome is the durable delivery outcome for one consumed permit.
type ChatSendOutcome struct {
	PermitId   string    `json:"permit_id"`
	SendId     string    `json:"send_id"`
	State      string    `json:"state"`
	MessageId  *string   `json:"message_id"`
	OccurredAt time.Time `json:"occurred_at"`
	CreatedAt  time.Time `json:"created_at"`
}

// ChatSendOutcomeEnvelope is the wire envelope for an outcome receipt.
type ChatSendOutcomeEnvelope struct {
	Data ChatSendOutcome      `json:"data"`
	Meta contract.RequestMeta `json:"meta"`
}

type chatPermitRow struct {
	WorkspaceID, PermitID, RuntimeID                    string
	OwnerEpoch                                          int64
	AccountID, SendID, ConversationKind, ConversationID string
	TriggerRef, TextDigest                              string
	ExpectedBindingEpoch                                int64
	DeadlineAt, IssuedAt, ExpiresAt                     time.Time
	SegmentIndex, SegmentCount                          int
	State                                               string
}

const chatPermitColumns = `workspace_id,permit_id,runtime_id,owner_epoch,account_id,send_id,conversation_kind,conversation_id,trigger_ref,text_digest,expected_binding_epoch,deadline_at,issued_at,expires_at,segment_index,segment_count,state`

func scanChatPermit(row pgx.Row) (chatPermitRow, error) {
	var p chatPermitRow
	err := row.Scan(&p.WorkspaceID, &p.PermitID, &p.RuntimeID, &p.OwnerEpoch, &p.AccountID, &p.SendID,
		&p.ConversationKind, &p.ConversationID, &p.TriggerRef, &p.TextDigest, &p.ExpectedBindingEpoch,
		&p.DeadlineAt, &p.IssuedAt, &p.ExpiresAt, &p.SegmentIndex, &p.SegmentCount, &p.State)
	return p, err
}

func loadChatPermit(ctx context.Context, tx pgx.Tx, workspace, permitID string, forUpdate bool) (chatPermitRow, error) {
	query := `SELECT ` + chatPermitColumns + ` FROM core.chat_send_permit WHERE workspace_id=$1 AND permit_id=$2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanChatPermit(tx.QueryRow(ctx, query, workspace, permitID))
}

type chatTriggerRow struct {
	TriggerRef, RuntimeID                       string
	OwnerEpoch                                  int64
	AccountID, ConversationKind, ConversationID string
	SenderID                                    string
	DirectedAccountID                           *string
	DeadlineAt                                  time.Time
	SegmentCount, NextSegment                   int
	State                                       string
}

const chatTriggerColumns = `trigger_ref,runtime_id,owner_epoch,account_id,conversation_kind,conversation_id,sender_id,directed_account_id,deadline_at,segment_count,next_segment,state`

func scanChatTrigger(row pgx.Row) (chatTriggerRow, error) {
	var t chatTriggerRow
	err := row.Scan(&t.TriggerRef, &t.RuntimeID, &t.OwnerEpoch, &t.AccountID, &t.ConversationKind,
		&t.ConversationID, &t.SenderID, &t.DirectedAccountID, &t.DeadlineAt, &t.SegmentCount, &t.NextSegment, &t.State)
	return t, err
}

func loadChatTrigger(ctx context.Context, tx pgx.Tx, workspace, triggerRef string, forUpdate bool) (chatTriggerRow, error) {
	query := `SELECT ` + chatTriggerColumns + ` FROM core.chat_trigger WHERE workspace_id=$1 AND trigger_ref=$2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanChatTrigger(tx.QueryRow(ctx, query, workspace, triggerRef))
}

type chatAccountRow struct {
	RuntimeID, SelfID                                      string
	OwnerEpoch                                             int64
	IdentityStatus, ConnectionState, LoginState, RiskState string
	ControlState                                           string
}

// chatAuthorizeAccount proves that the caller runtime currently owns this
// account at the actor's owner epoch. It separates "unknown account/runtime"
// (not found) from "known but no longer authorized at this epoch"
// (ACCOUNT_RESTRICTED) so a stale runtime cannot keep sending.
func chatAuthorizeAccount(ctx context.Context, tx pgx.Tx, actor RuntimeActor, accountID string) (chatAccountRow, error) {
	var a chatAccountRow
	if taskBlank(actor.WorkspaceID) || taskBlank(actor.RuntimeID) || actor.OwnerEpoch <= 0 {
		return a, problem(chatCodePermitUnknown)
	}
	if !contains(actor.AccountIDs, accountID) {
		return a, problem(chatCodeAccountRestricted)
	}
	err := tx.QueryRow(ctx, `SELECT runtime_id,COALESCE(self_id,''),owner_epoch,identity_status,connection_state,login_state,risk_state,control_state
		FROM core.account WHERE workspace_id=$1 AND id=$2 FOR SHARE`, actor.WorkspaceID, accountID).
		Scan(&a.RuntimeID, &a.SelfID, &a.OwnerEpoch, &a.IdentityStatus, &a.ConnectionState, &a.LoginState, &a.RiskState, &a.ControlState)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, problem(chatCodePermitUnknown)
	}
	if err != nil {
		return a, err
	}
	if a.RuntimeID != actor.RuntimeID {
		return a, problem(chatCodePermitUnknown)
	}
	var credentialEpoch int64
	var active bool
	err = tx.QueryRow(ctx, `SELECT owner_epoch,active FROM core.runtime_credentials
		WHERE workspace_id=$1 AND runtime_id=$2 AND account_id=$3 FOR SHARE`, actor.WorkspaceID, actor.RuntimeID, accountID).
		Scan(&credentialEpoch, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, problem(chatCodePermitUnknown)
	}
	if err != nil {
		return a, err
	}
	if !active || credentialEpoch != actor.OwnerEpoch || a.OwnerEpoch != actor.OwnerEpoch {
		return a, problem(chatCodeAccountRestricted)
	}
	return a, nil
}

func chatRequireHealthy(a chatAccountRow) error {
	if a.IdentityStatus != "verified" || a.ConnectionState != "online" || a.LoginState != "logged_in" ||
		a.RiskState != "clear" || a.ControlState != "active" {
		return problem(chatCodeAccountRestricted)
	}
	return nil
}

func chatParseDecimal(value string) (int64, error) { return hbParseDecimal(value) }

func chatValidateCommonConversation(kind, conversationID string) error {
	if kind != "group" && kind != "private" {
		return problem(taskCodeValidation)
	}
	if !taskValidQQID(conversationID) {
		return problem(taskCodeValidation)
	}
	return nil
}

func chatValidatePermitRequest(req contract.ChatPermitRequest) error {
	if !taskValidID(string(req.SendId)) || !taskValidID(string(req.AccountId)) || !taskValidID(string(req.TriggerRef)) {
		return problem(taskCodeValidation)
	}
	if err := chatValidateCommonConversation(string(req.ConversationKind), string(req.ConversationId)); err != nil {
		return err
	}
	if !chatDigestPattern.MatchString(string(req.TextDigest)) {
		return problem(taskCodeValidation)
	}
	if req.SegmentCount < 1 || req.SegmentCount > chatPermitSegmentCountMax || req.SegmentIndex < 0 || req.SegmentIndex >= req.SegmentCount {
		return problem(taskCodeValidation)
	}
	epoch, err := chatParseDecimal(string(req.ExpectedBindingEpoch))
	if err != nil || epoch <= 0 {
		return problem(taskCodeValidation)
	}
	if req.DeadlineAt.IsZero() {
		return problem(taskCodeValidation)
	}
	return nil
}

func chatValidateConsumeRequest(req ChatConsumeRequest) error {
	if !taskValidID(req.SendId) || req.SegmentIndex < 0 || req.SegmentIndex >= chatPermitSegmentCountMax {
		return problem(taskCodeValidation)
	}
	if !chatDigestPattern.MatchString(req.TextDigest) {
		return problem(taskCodeValidation)
	}
	return nil
}

func chatValidateOutcomeRequest(req ChatSendOutcomeRequest) error {
	if !taskValidID(req.SendId) {
		return problem(taskCodeValidation)
	}
	if req.MessageId != nil && !taskValidID(*req.MessageId) {
		return problem(taskCodeValidation)
	}
	switch req.State {
	case chatPermitOutcomeSent:
		// Adapters that return no receipt id still report a real send. A
		// missing message_id is therefore a successful send without a
		// receipt, not an unknown delivery.
	case chatPermitOutcomeUnknown:
		if req.MessageId != nil {
			return problem(taskCodeValidation)
		}
	default:
		return problem(taskCodeValidation)
	}
	if req.OccurredAt.IsZero() {
		return problem(taskCodeValidation)
	}
	return nil
}

func chatPermitContract(p chatPermitRow) contract.ChatPermit {
	return contract.ChatPermit{
		PermitId:             contract.Id(p.PermitID),
		SendId:               contract.Id(p.SendID),
		AccountId:            contract.Id(p.AccountID),
		ConversationKind:     contract.ChatPermitConversationKind(p.ConversationKind),
		ConversationId:       contract.QQId(p.ConversationID),
		TriggerRef:           contract.Id(p.TriggerRef),
		TextDigest:           contract.Digest(p.TextDigest),
		ExpectedBindingEpoch: contract.DecimalSequence(strconv.FormatInt(p.ExpectedBindingEpoch, 10)),
		IssuedAt:             p.IssuedAt,
		ExpiresAt:            p.ExpiresAt,
		SegmentIndex:         p.SegmentIndex,
		SegmentCount:         p.SegmentCount,
		State:                contract.ChatPermitState(p.State),
	}
}

func chatLoadBinding(ctx context.Context, tx pgx.Tx, workspace, groupID string, forUpdate bool) (speaker string, bindingEpoch int64, lease time.Time, err error) {
	query := `SELECT active_speaker_account_id,binding_epoch,lease_expires_at FROM core.group_binding WHERE workspace_id=$1 AND group_id=$2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	err = tx.QueryRow(ctx, query, workspace, groupID).Scan(&speaker, &bindingEpoch, &lease)
	return speaker, bindingEpoch, lease, err
}

// chatCheckPolicy returns the account's daily limit and, when requireConsumed
// is set, proves the current day's issued count has not exceeded it.
func chatCheckPolicy(ctx context.Context, tx pgx.Tx, workspace, account string, requireConsumed bool) (int, error) {
	var enabled bool
	var dailyLimit int
	err := tx.QueryRow(ctx, `SELECT enabled,daily_limit FROM core.chat_send_policy WHERE workspace_id=$1 AND account_id=$2 FOR SHARE`,
		workspace, account).Scan(&enabled, &dailyLimit)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !enabled) {
		return 0, problem(chatCodePolicyDisabled)
	}
	if err != nil {
		return 0, err
	}
	if requireConsumed {
		var issued int
		err = tx.QueryRow(ctx, `SELECT issued FROM core.chat_send_usage WHERE workspace_id=$1 AND account_id=$2 AND window_start=`+chatUsageDateExpr+` FOR SHARE`,
			workspace, account).Scan(&issued)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && issued > dailyLimit) {
			return 0, problem(chatCodeBudgetExhausted)
		}
		if err != nil {
			return 0, err
		}
	}
	return dailyLimit, nil
}

// GetChatSendPermit returns a permit only to its authenticated runtime/account owner.
func (s *Store) GetChatSendPermit(ctx context.Context, actor RuntimeActor, permitID string) (contract.ChatPermit, error) {
	if !taskValidID(permitID) {
		return contract.ChatPermit{}, problem(chatCodePermitUnknown)
	}
	permit, err := scanChatPermit(s.Pool.QueryRow(ctx,
		`SELECT `+chatPermitColumns+` FROM core.chat_send_permit WHERE workspace_id=$1 AND permit_id=$2`, actor.WorkspaceID, permitID))
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ChatPermit{}, problem(chatCodePermitUnknown)
	}
	if err != nil {
		return contract.ChatPermit{}, err
	}
	if permit.RuntimeID != actor.RuntimeID || !contains(actor.AccountIDs, permit.AccountID) {
		return contract.ChatPermit{}, problem(chatCodePermitUnknown)
	}
	if permit.OwnerEpoch != actor.OwnerEpoch {
		return contract.ChatPermit{}, problem(chatCodeAccountRestricted)
	}
	return chatPermitContract(permit), nil
}

// GetChatSendOutcome exposes the durable result to its owning runtime without permitting a resend.
func (s *Store) GetChatSendOutcome(ctx context.Context, actor RuntimeActor, permitID string) (ChatSendOutcome, error) {
	var out ChatSendOutcome
	permit, err := s.GetChatSendPermit(ctx, actor, permitID)
	if err != nil {
		return out, err
	}
	var messageID string
	err = s.Pool.QueryRow(ctx, `SELECT permit_id,send_id,state,COALESCE(message_id,''),occurred_at,created_at
		FROM core.chat_send_outcome WHERE workspace_id=$1 AND permit_id=$2`, actor.WorkspaceID, permitID).
		Scan(&out.PermitId, &out.SendId, &out.State, &messageID, &out.OccurredAt, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatSendOutcome{}, problem(chatCodePermitNotConsumed)
	}
	if err != nil {
		return ChatSendOutcome{}, err
	}
	if out.SendId != string(permit.SendId) {
		return ChatSendOutcome{}, problem(chatCodeOutcomeConflict)
	}
	if messageID != "" {
		out.MessageId = &messageID
	}
	return out, nil
}

// IssueChatSendPermit issues at most one durable permit per
// (send_id, segment_index). It never consumes the permit and never calls an
// adapter. Exact duplicate requests return the stored permit without
// extending expiry or charging budget twice. The whole decision is made in one
// transaction so quota, trigger order, speaker lease and idempotency agree.
func (s *Store) IssueChatSendPermit(ctx context.Context, actor RuntimeActor, idempotencyKey string, req contract.ChatPermitRequest) (contract.ChatPermit, error) {
	var out contract.ChatPermit
	if err := chatValidatePermitRequest(req); err != nil {
		return out, err
	}
	if taskBlank(idempotencyKey) || len(idempotencyKey) > 128 {
		return out, problem(taskCodeValidation)
	}
	requestDigest, err := digest(req)
	if err != nil {
		return out, problem(taskCodeValidation)
	}
	expectedEpoch, _ := chatParseDecimal(string(req.ExpectedBindingEpoch))
	var domainErr error
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		account, err := chatAuthorizeAccount(ctx, tx, actor, string(req.AccountId))
		if err != nil {
			return err
		}
		if err := chatRequireHealthy(account); err != nil {
			return err
		}
		dailyLimit, err := chatCheckPolicy(ctx, tx, actor.WorkspaceID, string(req.AccountId), false)
		if err != nil {
			return err
		}
		trigger, err := loadChatTrigger(ctx, tx, actor.WorkspaceID, string(req.TriggerRef), true)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodeTriggerUnknown)
		}
		if err != nil {
			return err
		}
		if trigger.RuntimeID != actor.RuntimeID || trigger.OwnerEpoch != actor.OwnerEpoch || trigger.AccountID != string(req.AccountId) {
			return problem(chatCodeTriggerUnknown)
		}

		// Exact replays return the already issued permit even if the trigger
		// has since advanced, expired or been closed. Consumption, not issue,
		// is the send authorization, so no replay can authorize a send.
		var storedDigest, storedPermit string
		err = tx.QueryRow(ctx, `SELECT request_digest,permit_id FROM core.chat_send_idempotency
			WHERE workspace_id=$1 AND runtime_id=$2 AND idempotency_key=$3 FOR UPDATE`,
			actor.WorkspaceID, actor.RuntimeID, idempotencyKey).Scan(&storedDigest, &storedPermit)
		switch {
		case err == nil:
			if storedDigest != requestDigest {
				return problem(chatCodeSendDigestConflict)
			}
			existing, err := loadChatPermit(ctx, tx, actor.WorkspaceID, storedPermit, false)
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(chatCodePermitUnknown)
			}
			if err != nil {
				return err
			}
			if existing.RuntimeID != actor.RuntimeID || existing.AccountID != string(req.AccountId) ||
				existing.TriggerRef != string(req.TriggerRef) || existing.SendID != string(req.SendId) ||
				existing.SegmentIndex != req.SegmentIndex || existing.TextDigest != string(req.TextDigest) {
				return problem(chatCodeSendDigestConflict)
			}
			out = chatPermitContract(existing)
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		existing, err := scanChatPermit(tx.QueryRow(ctx, `SELECT `+chatPermitColumns+` FROM core.chat_send_permit
			WHERE workspace_id=$1 AND send_id=$2 AND segment_index=$3 FOR UPDATE`,
			actor.WorkspaceID, string(req.SendId), req.SegmentIndex))
		if err == nil {
			if existing.RuntimeID != actor.RuntimeID || existing.AccountID != string(req.AccountId) ||
				existing.TriggerRef != string(req.TriggerRef) || existing.TextDigest != string(req.TextDigest) ||
				existing.ConversationKind != string(req.ConversationKind) || existing.ConversationID != string(req.ConversationId) ||
				existing.ExpectedBindingEpoch != expectedEpoch || existing.SegmentCount != req.SegmentCount ||
				!existing.DeadlineAt.Equal(req.DeadlineAt) {
				return problem(chatCodeSendDigestConflict)
			}
			if err := chatBindIdempotency(ctx, tx, actor, idempotencyKey, requestDigest, existing.PermitID); err != nil {
				return err
			}
			out = chatPermitContract(existing)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		if trigger.State == "expired" || !now.Before(trigger.DeadlineAt) {
			if trigger.State == "open" {
				if _, err := tx.Exec(ctx, `UPDATE core.chat_trigger SET state='expired' WHERE workspace_id=$1 AND trigger_ref=$2 AND state='open'`,
					actor.WorkspaceID, trigger.TriggerRef); err != nil {
					return err
				}
			}
			// Commit the terminal trigger state, then report the refusal
			// outside the transaction. Returning the domain error here would
			// roll the expiry back.
			domainErr = problem(chatCodeTriggerExpired)
			return nil
		}
		if trigger.State != "open" {
			return problem(chatCodeTriggerConflict)
		}
		if trigger.ConversationKind != string(req.ConversationKind) || trigger.ConversationID != string(req.ConversationId) ||
			trigger.SegmentCount != req.SegmentCount || !trigger.DeadlineAt.Equal(req.DeadlineAt) {
			return problem(chatCodeTriggerConflict)
		}
		if trigger.NextSegment != req.SegmentIndex || req.SegmentIndex >= req.SegmentCount {
			return problem(chatCodeSegmentOutOfOrder)
		}
		if string(req.ConversationKind) == "group" {
			speaker, bindingEpoch, lease, err := chatLoadBinding(ctx, tx, actor.WorkspaceID, string(req.ConversationId), true)
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(chatCodeSpeakerNotActive)
			}
			if err != nil {
				return err
			}
			if speaker != string(req.AccountId) || !now.Before(lease) {
				return problem(chatCodeSpeakerNotActive)
			}
			if bindingEpoch != expectedEpoch {
				return problem(chatCodeBindingConflict)
			}
		} else {
			if trigger.DirectedAccountID != nil && *trigger.DirectedAccountID != string(req.AccountId) {
				return problem(chatCodeBindingConflict)
			}
			if account.OwnerEpoch != expectedEpoch {
				return problem(chatCodeBindingConflict)
			}
		}

		expires := now.Add(chatPermitTTL)
		if expires.After(trigger.DeadlineAt) {
			expires = trigger.DeadlineAt
		}
		if !expires.After(now) {
			if trigger.State == "open" {
				if _, err := tx.Exec(ctx, `UPDATE core.chat_trigger SET state='expired' WHERE workspace_id=$1 AND trigger_ref=$2 AND state='open'`,
					actor.WorkspaceID, trigger.TriggerRef); err != nil {
					return err
				}
			}
			domainErr = problem(chatCodeTriggerExpired)
			return nil
		}
		permitID := newID("permit")
		inserted, err := scanChatPermit(tx.QueryRow(ctx, `INSERT INTO core.chat_send_permit
			(workspace_id,permit_id,runtime_id,owner_epoch,account_id,send_id,conversation_kind,conversation_id,trigger_ref,
			 text_digest,expected_binding_epoch,deadline_at,segment_index,segment_count,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT (workspace_id,send_id,segment_index) DO NOTHING
			RETURNING `+chatPermitColumns,
			actor.WorkspaceID, permitID, actor.RuntimeID, actor.OwnerEpoch, string(req.AccountId), string(req.SendId),
			string(req.ConversationKind), string(req.ConversationId), string(req.TriggerRef), string(req.TextDigest),
			expectedEpoch, req.DeadlineAt, req.SegmentIndex, req.SegmentCount, expires))
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent issue won the (send_id, segment_index) race. Return
			// its permit only when every request field agrees, and do not
			// charge the daily budget a second time.
			existing, err := scanChatPermit(tx.QueryRow(ctx, `SELECT `+chatPermitColumns+` FROM core.chat_send_permit
				WHERE workspace_id=$1 AND send_id=$2 AND segment_index=$3 FOR UPDATE`,
				actor.WorkspaceID, string(req.SendId), req.SegmentIndex))
			if err != nil {
				return err
			}
			if !chatPermitMatchesIssue(existing, actor, req, expectedEpoch) {
				return problem(chatCodeSendDigestConflict)
			}
			if err := chatBindIdempotency(ctx, tx, actor, idempotencyKey, requestDigest, existing.PermitID); err != nil {
				return err
			}
			out = chatPermitContract(existing)
			return nil
		}
		if err != nil {
			return err
		}
		var issued int
		err = tx.QueryRow(ctx, `INSERT INTO core.chat_send_usage(workspace_id,account_id,window_start,issued)
			VALUES($1,$2,`+chatUsageDateExpr+`,1)
			ON CONFLICT (workspace_id,account_id,window_start) DO UPDATE SET issued=core.chat_send_usage.issued+1
			WHERE core.chat_send_usage.issued < $3
			RETURNING issued`, actor.WorkspaceID, string(req.AccountId), dailyLimit).Scan(&issued)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodeBudgetExhausted)
		}
		if err != nil {
			return err
		}
		if issued > dailyLimit {
			return problem(chatCodeBudgetExhausted)
		}
		if err := chatBindIdempotency(ctx, tx, actor, idempotencyKey, requestDigest, inserted.PermitID); err != nil {
			return err
		}
		out = chatPermitContract(inserted)
		return nil
	})
	if err != nil {
		return out, err
	}
	if domainErr != nil {
		return out, domainErr
	}
	return out, nil
}

// chatPermitMatchesIssue reports whether a stored permit was issued for
// exactly this request. An idempotency key replay never broadens a permit.
func chatPermitMatchesIssue(p chatPermitRow, actor RuntimeActor, req contract.ChatPermitRequest, expectedEpoch int64) bool {
	return p.OwnerEpoch == actor.OwnerEpoch && p.RuntimeID == actor.RuntimeID && p.AccountID == string(req.AccountId) &&
		p.TriggerRef == string(req.TriggerRef) && p.SendID == string(req.SendId) &&
		p.ConversationKind == string(req.ConversationKind) && p.ConversationID == string(req.ConversationId) &&
		p.TextDigest == string(req.TextDigest) && p.ExpectedBindingEpoch == expectedEpoch &&
		p.SegmentIndex == req.SegmentIndex && p.SegmentCount == req.SegmentCount &&
		p.DeadlineAt.Equal(req.DeadlineAt)
}

func chatBindIdempotency(ctx context.Context, tx pgx.Tx, actor RuntimeActor, key, requestDigest, permitID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO core.chat_send_idempotency(workspace_id,runtime_id,idempotency_key,request_digest,permit_id)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT (workspace_id,runtime_id,idempotency_key) DO NOTHING`,
		actor.WorkspaceID, actor.RuntimeID, key, requestDigest, permitID)
	if err != nil {
		return err
	}
	var storedDigest, storedPermit string
	if err := tx.QueryRow(ctx, `SELECT request_digest,permit_id FROM core.chat_send_idempotency
		WHERE workspace_id=$1 AND runtime_id=$2 AND idempotency_key=$3`,
		actor.WorkspaceID, actor.RuntimeID, key).Scan(&storedDigest, &storedPermit); err != nil {
		return err
	}
	if storedDigest != requestDigest || storedPermit != permitID {
		return problem(chatCodeSendDigestConflict)
	}
	return nil
}

// ConsumeChatSendPermit atomically proves every send-time invariant and marks
// the permit consumed. A second consume returns CHAT_PERMIT_CONSUMED and the
// caller must not call the adapter; an expired permit is marked expired and
// returns CHAT_PERMIT_EXPIRED. The trigger advances exactly one segment.
func (s *Store) ConsumeChatSendPermit(ctx context.Context, actor RuntimeActor, permitID string, req ChatConsumeRequest) (contract.ChatPermit, error) {
	// Retired even when rules are empty: no digest-only external-send grant.
	if err := chatValidateConsumeRequest(req); err != nil {
		return contract.ChatPermit{}, err
	}
	p, err := s.GetChatSendPermit(ctx, actor, permitID)
	if err != nil {
		return contract.ChatPermit{}, err
	}
	if string(p.SendId) != req.SendId || p.SegmentIndex != req.SegmentIndex || string(p.TextDigest) != req.TextDigest {
		return contract.ChatPermit{}, problem(chatCodeSendDigestConflict)
	}
	return contract.ChatPermit{}, problem("CONTENT_FINAL_TEXT_REQUIRED")
}
func (s *Store) consumeChatSendPermit(ctx context.Context, actor RuntimeActor, permitID string, req ChatConsumeRequest) (contract.ChatPermit, error) {
	var out contract.ChatPermit
	if !taskValidID(permitID) {
		return out, problem(chatCodePermitUnknown)
	}
	if err := chatValidateConsumeRequest(req); err != nil {
		return out, err
	}
	var domainErr error
	err := s.tx(ctx, func(tx pgx.Tx) error {

		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		permit, err := loadChatPermit(ctx, tx, actor.WorkspaceID, permitID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodePermitUnknown)
		}
		if err != nil {
			return err
		}
		if permit.RuntimeID != actor.RuntimeID || !contains(actor.AccountIDs, permit.AccountID) {
			return problem(chatCodePermitUnknown)
		}
		if permit.OwnerEpoch != actor.OwnerEpoch {
			return problem(chatCodeAccountRestricted)
		}
		// Terminal/expired state wins over every other refusal so the caller
		// sees "do not send" for the real reason. The checks are repeated
		// under the permit row lock below.
		if permit.State == "consumed" {
			return problem(chatCodePermitConsumed)
		}
		if permit.State == "expired" {
			return problem(chatCodePermitExpired)
		}
		if permit.State != "issued" {
			return problem(chatCodePermitConsumed)
		}
		if !now.Before(permit.ExpiresAt) {
			tag, err := tx.Exec(ctx, `UPDATE core.chat_send_permit SET state='expired' WHERE workspace_id=$1 AND permit_id=$2 AND state='issued'`,
				actor.WorkspaceID, permit.PermitID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				// A concurrent consume may have won after the unlocked read.
				latest, err := loadChatPermit(ctx, tx, actor.WorkspaceID, permitID, false)
				if errors.Is(err, pgx.ErrNoRows) {
					return problem(chatCodePermitUnknown)
				}
				if err != nil {
					return err
				}
				if latest.State == "consumed" {
					return problem(chatCodePermitConsumed)
				}
				return problem(chatCodePermitExpired)
			}
			domainErr = problem(chatCodePermitExpired)
			return nil
		}
		if permit.SendID != req.SendId || permit.SegmentIndex != req.SegmentIndex || permit.TextDigest != req.TextDigest {
			return problem(chatCodeSendDigestConflict)
		}
		// Lock order is trigger -> group binding -> permit, matching issue.
		trigger, err := loadChatTrigger(ctx, tx, actor.WorkspaceID, permit.TriggerRef, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodeTriggerUnknown)
		}
		if err != nil {
			return err
		}
		if permit.ConversationKind == "group" {
			speaker, bindingEpoch, lease, err := chatLoadBinding(ctx, tx, actor.WorkspaceID, permit.ConversationID, true)
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(chatCodeSpeakerNotActive)
			}
			if err != nil {
				return err
			}
			if speaker != permit.AccountID || !now.Before(lease) {
				return problem(chatCodeSpeakerNotActive)
			}
			if bindingEpoch != permit.ExpectedBindingEpoch {
				return problem(chatCodeBindingConflict)
			}
		} else if trigger.DirectedAccountID != nil && *trigger.DirectedAccountID != permit.AccountID {
			return problem(chatCodeBindingConflict)
		}
		permit, err = loadChatPermit(ctx, tx, actor.WorkspaceID, permitID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodePermitUnknown)
		}
		if err != nil {
			return err
		}
		if permit.TriggerRef != trigger.TriggerRef {
			return problem(chatCodePermitUnknown)
		}
		if permit.State == "consumed" {
			return problem(chatCodePermitConsumed)
		}
		if permit.State == "expired" {
			return problem(chatCodePermitExpired)
		}
		if permit.State != "issued" {
			return problem(chatCodePermitConsumed)
		}
		if !now.Before(permit.ExpiresAt) {
			if _, err := tx.Exec(ctx, `UPDATE core.chat_send_permit SET state='expired' WHERE workspace_id=$1 AND permit_id=$2 AND state='issued'`,
				actor.WorkspaceID, permit.PermitID); err != nil {
				return err
			}
			domainErr = problem(chatCodePermitExpired)
			return nil
		}
		if permit.SendID != req.SendId || permit.SegmentIndex != req.SegmentIndex || permit.TextDigest != req.TextDigest {
			return problem(chatCodeSendDigestConflict)
		}
		if permit.RuntimeID != actor.RuntimeID || !contains(actor.AccountIDs, permit.AccountID) {
			return problem(chatCodePermitUnknown)
		}
		if permit.OwnerEpoch != actor.OwnerEpoch || trigger.OwnerEpoch != actor.OwnerEpoch ||
			trigger.RuntimeID != actor.RuntimeID || trigger.AccountID != permit.AccountID {
			return problem(chatCodeAccountRestricted)
		}
		account, err := chatAuthorizeAccount(ctx, tx, actor, permit.AccountID)
		if err != nil {
			return err
		}
		if err := chatRequireHealthy(account); err != nil {
			return err
		}
		if permit.ConversationKind == "private" && permit.ExpectedBindingEpoch != account.OwnerEpoch {
			return problem(chatCodeBindingConflict)
		}
		if trigger.State == "expired" || !now.Before(trigger.DeadlineAt) {
			if trigger.State == "open" {
				if _, err := tx.Exec(ctx, `UPDATE core.chat_trigger SET state='expired' WHERE workspace_id=$1 AND trigger_ref=$2 AND state='open'`,
					actor.WorkspaceID, trigger.TriggerRef); err != nil {
					return err
				}
			}
			domainErr = problem(chatCodeTriggerExpired)
			return nil
		}
		if trigger.State != "open" {
			return problem(chatCodeTriggerConflict)
		}
		if trigger.ConversationKind != permit.ConversationKind || trigger.ConversationID != permit.ConversationID ||
			trigger.SegmentCount != permit.SegmentCount {
			return problem(chatCodeBindingConflict)
		}
		if trigger.NextSegment != permit.SegmentIndex || trigger.NextSegment >= trigger.SegmentCount {
			return problem(chatCodeSegmentOutOfOrder)
		}
		if _, err := chatCheckPolicy(ctx, tx, actor.WorkspaceID, permit.AccountID, true); err != nil {
			return err
		}
		var consumedAt time.Time
		err = tx.QueryRow(ctx, `UPDATE core.chat_send_permit SET state='consumed',consumed_at=clock_timestamp()
			WHERE workspace_id=$1 AND permit_id=$2 AND state='issued' RETURNING consumed_at`,
			actor.WorkspaceID, permit.PermitID).Scan(&consumedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodePermitConsumed)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE core.chat_trigger SET next_segment=next_segment+1,
			state=CASE WHEN next_segment+1=segment_count THEN 'complete' ELSE state END
			WHERE workspace_id=$1 AND trigger_ref=$2`, actor.WorkspaceID, trigger.TriggerRef)
		if err != nil {
			return err
		}
		permit.State = "consumed"
		out = chatPermitContract(permit)
		return nil
	})
	if err != nil {
		return out, err
	}
	if domainErr != nil {
		return out, domainErr
	}
	return out, nil
}

// ConsumeChatPermitText is the Character gate path: the permit row already
// fixes send_id and segment_index, so only its ID and the exact text digest
// are needed. The full consume validation still runs.
func (s *Store) ConsumeChatPermitText(ctx context.Context, actor RuntimeActor, permitID, textDigest string) error {
	permit, err := scanChatPermit(s.Pool.QueryRow(ctx,
		`SELECT `+chatPermitColumns+` FROM core.chat_send_permit WHERE workspace_id=$1 AND permit_id=$2`,
		actor.WorkspaceID, permitID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodePermitUnknown)
		}
		return err
	}
	_, err = s.ConsumeChatSendPermit(ctx, actor, permitID, ChatConsumeRequest{
		SendId:       permit.SendID,
		SegmentIndex: permit.SegmentIndex,
		TextDigest:   textDigest,
	})
	return err
}

// RecordChatSendOutcome stores the adapter result for one consumed permit.
// Exactly one outcome is accepted. An identical replay returns the stored
// row; a different outcome is a conflict and never rewrites history.
func (s *Store) RecordChatSendOutcome(ctx context.Context, actor RuntimeActor, permitID string, req ChatSendOutcomeRequest) (ChatSendOutcome, bool, error) {
	var out ChatSendOutcome
	if !taskValidID(permitID) {
		return out, false, problem(chatCodePermitUnknown)
	}
	if err := chatValidateOutcomeRequest(req); err != nil {
		return out, false, err
	}
	created := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		permit, err := loadChatPermit(ctx, tx, actor.WorkspaceID, permitID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodePermitUnknown)
		}
		if err != nil {
			return err
		}
		if permit.RuntimeID != actor.RuntimeID || !contains(actor.AccountIDs, permit.AccountID) {
			return problem(chatCodePermitUnknown)
		}
		if permit.OwnerEpoch != actor.OwnerEpoch {
			return problem(chatCodeAccountRestricted)
		}
		if permit.State != "consumed" {
			return problem(chatCodePermitNotConsumed)
		}
		if permit.SendID != req.SendId {
			return problem(chatCodeOutcomeConflict)
		}
		// Lock order matches issue/consume: trigger before permit. The
		// initial permit read is unlocked only to learn trigger_ref; the
		// authoritative state and trigger are re-read below.
		trigger, err := loadChatTrigger(ctx, tx, actor.WorkspaceID, permit.TriggerRef, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodeTriggerUnknown)
		}
		if err != nil {
			return err
		}
		if trigger.RuntimeID != actor.RuntimeID || trigger.AccountID != permit.AccountID {
			return problem(chatCodePermitUnknown)
		}
		if trigger.OwnerEpoch != actor.OwnerEpoch {
			return problem(chatCodeAccountRestricted)
		}
		permit, err = loadChatPermit(ctx, tx, actor.WorkspaceID, permitID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(chatCodePermitUnknown)
		}
		if err != nil {
			return err
		}
		if permit.TriggerRef != trigger.TriggerRef {
			return problem(chatCodePermitUnknown)
		}
		if permit.State != "consumed" {
			return problem(chatCodePermitNotConsumed)
		}
		if permit.SendID != req.SendId {
			return problem(chatCodeOutcomeConflict)
		}
		if permit.RuntimeID != actor.RuntimeID || !contains(actor.AccountIDs, permit.AccountID) {
			return problem(chatCodePermitUnknown)
		}
		if permit.OwnerEpoch != actor.OwnerEpoch {
			return problem(chatCodeAccountRestricted)
		}
		var messageID string
		var occurredAt, createdAt time.Time
		err = tx.QueryRow(ctx, `SELECT permit_id,send_id,state,COALESCE(message_id,''),occurred_at,created_at
			FROM core.chat_send_outcome WHERE workspace_id=$1 AND permit_id=$2`,
			actor.WorkspaceID, permitID).Scan(&out.PermitId, &out.SendId, &out.State, &messageID, &occurredAt, &createdAt)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO core.chat_send_outcome(workspace_id,permit_id,send_id,state,message_id,occurred_at)
				VALUES($1,$2,$3,$4,$5,$6)
				ON CONFLICT (workspace_id,permit_id) DO NOTHING
				RETURNING permit_id,send_id,state,COALESCE(message_id,''),occurred_at,created_at`,
				actor.WorkspaceID, permitID, req.SendId, req.State, req.MessageId, req.OccurredAt).
				Scan(&out.PermitId, &out.SendId, &out.State, &messageID, &out.OccurredAt, &out.CreatedAt)
			if errors.Is(err, pgx.ErrNoRows) {
				// A concurrent transaction won the append; compare its row.
				err = tx.QueryRow(ctx, `SELECT permit_id,send_id,state,COALESCE(message_id,''),occurred_at,created_at
					FROM core.chat_send_outcome WHERE workspace_id=$1 AND permit_id=$2`,
					actor.WorkspaceID, permitID).Scan(&out.PermitId, &out.SendId, &out.State, &messageID, &out.OccurredAt, &out.CreatedAt)
			} else if err == nil {
				created = true
			}
		} else if err == nil {
			out.OccurredAt, out.CreatedAt = occurredAt, createdAt
		}
		if err != nil {
			return err
		}
		if messageID != "" {
			out.MessageId = new(messageID)
		}
		if out.State != req.State || !chatSameMessageID(out.MessageId, req.MessageId) || !chatSameOutcomeTime(out.OccurredAt, req.OccurredAt) {
			return problem(chatCodeOutcomeConflict)
		}
		if req.State == chatPermitOutcomeUnknown {
			// An unknown segment delivery is terminal for the whole trigger.
			// The trigger is already locked, and the open->suppressed update
			// commits with the append-only outcome. A permit for a later
			// segment, even one issued before this outcome, will fail its
			// trigger-state recheck at consume time.
			if trigger.State == "open" {
				if _, err := tx.Exec(ctx, `UPDATE core.chat_trigger SET state='suppressed'
					WHERE workspace_id=$1 AND trigger_ref=$2 AND state='open'`,
					actor.WorkspaceID, trigger.TriggerRef); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, created, err
}

func chatSameMessageID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// chatSameOutcomeTime treats a timestamp as the same instant when the
// difference is below PostgreSQL timestamptz's one-microsecond resolution.
// The first append and an exact HTTP replay can therefore carry nanosecond
// precision in the request while the stored row is microsecond-truncated.
func chatSameOutcomeTime(a, b time.Time) bool {
	diff := a.Sub(b)
	if diff < 0 {
		diff = -diff
	}
	return diff < time.Microsecond
}

// ChatTextDigest is the contract digest: lowercase hex SHA-256 over the exact
// UTF-8 bytes of the text, with no prefix, suffix or newline. The Character
// gate must hash exactly the bytes it is about to hand to the adapter.
func ChatTextDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
