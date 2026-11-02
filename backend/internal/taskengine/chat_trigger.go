package taskengine

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	chatConversationGroup   = "group"
	chatConversationPrivate = "private"

	// chatUsageDateExpr is the sole daily-budget day key. current_date follows
	// the database session TimeZone, so a non-UTC server would roll the budget
	// at local midnight. The explicit UTC conversion keeps issuance, budget
	// reads, and the context projection on the same UTC day.
	chatUsageDateExpr = `(clock_timestamp() AT TIME ZONE 'UTC')::date`
)

// chatValidID mirrors the contract's 128-character identifier bound using
// Unicode characters, matching core.id's character-length domain rather than
// Go's byte length.
func chatValidID(value string) bool {
	return !taskBlank(value) && !strings.ContainsRune(value, '\x00') && utf8.RuneCountInString(value) <= 128
}

// ChatTriggerRequest is the durable registration of one authenticated inbound
// event. trigger_ref is caller-stable; segment_count is the exact number of
// text segments the permitted reply will consume.
type ChatTriggerRequest struct {
	TriggerRef        string    `json:"trigger_ref"`
	AccountID         string    `json:"account_id"`
	ConversationKind  string    `json:"conversation_kind"`
	ConversationID    string    `json:"conversation_id"`
	SourceEventID     string    `json:"source_event_id"`
	SenderID          string    `json:"sender_id"`
	DirectedAccountID *string   `json:"directed_account_id"`
	DeadlineAt        time.Time `json:"deadline_at"`
	SegmentCount      int       `json:"segment_count"`
}

type ChatTrigger struct {
	TriggerRef        string    `json:"trigger_ref"`
	AccountID         string    `json:"account_id"`
	ConversationKind  string    `json:"conversation_kind"`
	ConversationID    string    `json:"conversation_id"`
	SourceEventID     string    `json:"source_event_id"`
	SenderID          string    `json:"sender_id"`
	DirectedAccountID *string   `json:"directed_account_id"`
	DeadlineAt        time.Time `json:"deadline_at"`
	SegmentCount      int       `json:"segment_count"`
	NextSegment       int       `json:"next_segment"`
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"created_at"`
}

// ChatContext is the read-only runtime projection used before replying. It
// exposes the active speaker and effective chat budget without exposing other
// accounts' policies.
type ChatContext struct {
	AccountID              string     `json:"account_id"`
	QQSelfID               string     `json:"qq_self_id"`
	ConversationKind       string     `json:"conversation_kind"`
	ConversationID         string     `json:"conversation_id"`
	ActiveSpeakerAccountID *string    `json:"active_speaker_account_id"`
	BindingEpoch           *string    `json:"binding_epoch"`
	LeaseExpiresAt         *time.Time `json:"lease_expires_at"`
	SpeakerActive          bool       `json:"speaker_active"`
	ChatEnabled            bool       `json:"chat_enabled"`
	DailyLimit             int        `json:"daily_limit"`
	IssuedToday            int        `json:"issued_today"`
	ManagedSelfIDs         []string   `json:"managed_self_ids"`
}

type chatTriggerRecord struct {
	ChatTrigger
	WorkspaceID string
	RuntimeID   string
	OwnerEpoch  int64
	RequestHash string
}

const chatTriggerSelect = `workspace_id,trigger_ref,runtime_id,owner_epoch,account_id,source_event_id,conversation_kind,conversation_id,sender_id,directed_account_id,deadline_at,segment_count,next_segment,state,request_digest,created_at`

func scanChatTriggerRecord(row pgx.Row) (chatTriggerRecord, error) {
	var t chatTriggerRecord
	err := row.Scan(&t.WorkspaceID, &t.TriggerRef, &t.RuntimeID, &t.OwnerEpoch, &t.AccountID, &t.SourceEventID,
		&t.ConversationKind, &t.ConversationID, &t.SenderID, &t.DirectedAccountID, &t.DeadlineAt,
		&t.SegmentCount, &t.NextSegment, &t.State, &t.RequestHash, &t.CreatedAt)
	t.DeadlineAt = t.DeadlineAt.UTC()
	t.CreatedAt = t.CreatedAt.UTC()
	return t, err
}

func (s *Store) loadChatTriggerRecord(ctx context.Context, tx pgx.Tx, workspace, triggerRef string, forUpdate bool) (chatTriggerRecord, error) {
	q := `SELECT ` + chatTriggerSelect + ` FROM core.chat_trigger WHERE workspace_id=$1 AND trigger_ref=$2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	t, err := scanChatTriggerRecord(tx.QueryRow(ctx, q, workspace, triggerRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, problem(chatCodeTriggerUnknown)
	}
	return t, err
}

func (s *Store) chatTriggerDigest(actor RuntimeActor, req ChatTriggerRequest) (string, error) {
	return digest(struct {
		WorkspaceID       string
		RuntimeID         string
		OwnerEpoch        int64
		TriggerRef        string
		AccountID         string
		ConversationKind  string
		ConversationID    string
		SourceEventID     string
		SenderID          string
		DirectedAccountID *string
		DeadlineAt        time.Time
		SegmentCount      int
	}{
		WorkspaceID: actor.WorkspaceID, RuntimeID: actor.RuntimeID, OwnerEpoch: actor.OwnerEpoch,
		TriggerRef: req.TriggerRef, AccountID: req.AccountID, ConversationKind: req.ConversationKind,
		ConversationID: req.ConversationID, SourceEventID: req.SourceEventID, SenderID: req.SenderID,
		DirectedAccountID: req.DirectedAccountID, DeadlineAt: req.DeadlineAt.UTC(), SegmentCount: req.SegmentCount,
	})
}

func chatManagedSelfID(ctx context.Context, tx pgx.Tx, workspace, selfID string) (bool, error) {
	var managed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.account WHERE workspace_id=$1 AND self_id=$2)`, workspace, selfID).Scan(&managed)
	return managed, err
}

func (s *Store) validateChatTriggerTarget(ctx context.Context, tx pgx.Tx, workspace string, req ChatTriggerRequest) error {
	if req.DirectedAccountID != nil {
		if err := s.chatAccountExists(ctx, tx, workspace, *req.DirectedAccountID); err != nil {
			return err
		}
	}
	managed, err := chatManagedSelfID(ctx, tx, workspace, req.SenderID)
	if err != nil {
		return err
	}
	if managed {
		return problem(chatCodeManagedSelfID)
	}
	if req.ConversationKind == chatConversationPrivate {
		managed, err = chatManagedSelfID(ctx, tx, workspace, req.ConversationID)
		if err != nil {
			return err
		}
		if managed {
			return problem(chatCodeManagedSelfID)
		}
	}
	return nil
}

// RegisterChatTrigger durably records a trigger for one inbound event. A
// replay with the same idempotency key or the same trigger_ref/source event
// returns the original row; any differing payload is a conflict. Every
// managed bot self-id is rejected as a sender (and as a private peer) so two
// Character runtimes can never start a reply loop.
func (s *Store) RegisterChatTrigger(ctx context.Context, actor RuntimeActor, idempotencyKey string, req ChatTriggerRequest) (ChatTrigger, bool, error) {
	var out ChatTrigger
	created := false
	if taskCheckIdempotencyKey(idempotencyKey) != nil {
		return out, false, problem(taskCodeValidation)
	}
	if !chatValidID(req.TriggerRef) || !chatValidID(req.AccountID) || !chatValidID(req.SourceEventID) ||
		(req.DirectedAccountID != nil && !chatValidID(*req.DirectedAccountID)) ||
		!taskValidQQID(req.ConversationID) || !taskValidQQID(req.SenderID) ||
		(req.ConversationKind != chatConversationGroup && req.ConversationKind != chatConversationPrivate) ||
		req.SegmentCount < 1 || req.SegmentCount > 32 || req.DeadlineAt.IsZero() {
		return out, false, problem(taskCodeValidation)
	}
	req.DeadlineAt = req.DeadlineAt.UTC()
	reqDigest, err := s.chatTriggerDigest(actor, req)
	if err != nil {
		return out, false, problem(taskCodeValidation)
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRuntime(ctx, tx, actor, req.AccountID); err != nil {
			return err
		}
		if err := s.validateChatTriggerTarget(ctx, tx, actor.WorkspaceID, req); err != nil {
			return err
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if !req.DeadlineAt.After(now) {
			return problem(chatCodeTriggerExpired)
		}

		var storedDigest, storedRef string
		err = tx.QueryRow(ctx, `SELECT request_digest,trigger_ref FROM core.chat_trigger_idempotency
			WHERE workspace_id=$1 AND runtime_id=$2 AND idempotency_key=$3`,
			actor.WorkspaceID, actor.RuntimeID, idempotencyKey).Scan(&storedDigest, &storedRef)
		if err == nil {
			if storedDigest != reqDigest {
				return problem(chatCodeTriggerConflict)
			}
			row, err := s.loadChatTriggerRecord(ctx, tx, actor.WorkspaceID, storedRef, false)
			if err != nil {
				return err
			}
			out = row.ChatTrigger
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		row, err := s.loadChatTriggerRecord(ctx, tx, actor.WorkspaceID, req.TriggerRef, false)
		switch {
		case err == nil:
			if row.RequestHash != reqDigest || row.RuntimeID != actor.RuntimeID || row.AccountID != req.AccountID ||
				row.OwnerEpoch != actor.OwnerEpoch {
				return problem(chatCodeTriggerConflict)
			}
			out = row.ChatTrigger
			return nil
		case Code(err) != chatCodeTriggerUnknown:
			return err
		}

		var existingRef string
		err = tx.QueryRow(ctx, `SELECT trigger_ref FROM core.chat_trigger
			WHERE workspace_id=$1 AND runtime_id=$2 AND account_id=$3 AND source_event_id=$4`,
			actor.WorkspaceID, actor.RuntimeID, req.AccountID, req.SourceEventID).Scan(&existingRef)
		switch {
		case err == nil:
			return problem(chatCodeTriggerConflict)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		var inserted chatTriggerRecord
		inserted, err = scanChatTriggerRecord(tx.QueryRow(ctx, `INSERT INTO core.chat_trigger(
			workspace_id,trigger_ref,runtime_id,owner_epoch,account_id,source_event_id,conversation_kind,conversation_id,
			sender_id,directed_account_id,deadline_at,segment_count,next_segment,state,request_digest)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,0,'open',$13)
			ON CONFLICT DO NOTHING RETURNING `+chatTriggerSelect,
			actor.WorkspaceID, req.TriggerRef, actor.RuntimeID, actor.OwnerEpoch, req.AccountID, req.SourceEventID,
			req.ConversationKind, req.ConversationID, req.SenderID, req.DirectedAccountID, req.DeadlineAt,
			req.SegmentCount, reqDigest))
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent registration won one of the unique keys. Re-read
			// both identities and fail closed unless it is the exact request.
			row, loadErr := s.loadChatTriggerRecord(ctx, tx, actor.WorkspaceID, req.TriggerRef, false)
			if loadErr == nil {
				if row.RequestHash != reqDigest || row.OwnerEpoch != actor.OwnerEpoch {
					return problem(chatCodeTriggerConflict)
				}
				out = row.ChatTrigger
				return nil
			}
			var ref string
			if err := tx.QueryRow(ctx, `SELECT trigger_ref FROM core.chat_trigger
				WHERE workspace_id=$1 AND runtime_id=$2 AND account_id=$3 AND source_event_id=$4`,
				actor.WorkspaceID, actor.RuntimeID, req.AccountID, req.SourceEventID).Scan(&ref); err != nil {
				return err
			}
			return problem(chatCodeTriggerConflict)
		}
		if err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `INSERT INTO core.chat_trigger_idempotency(
			workspace_id,runtime_id,idempotency_key,request_digest,trigger_ref)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
			actor.WorkspaceID, actor.RuntimeID, idempotencyKey, reqDigest, req.TriggerRef)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var idemDigest, idemRef string
			if err := tx.QueryRow(ctx, `SELECT request_digest,trigger_ref FROM core.chat_trigger_idempotency
				WHERE workspace_id=$1 AND runtime_id=$2 AND idempotency_key=$3`,
				actor.WorkspaceID, actor.RuntimeID, idempotencyKey).Scan(&idemDigest, &idemRef); err != nil {
				return err
			}
			if idemDigest != reqDigest || idemRef != req.TriggerRef {
				return problem(chatCodeTriggerConflict)
			}
		}
		out = inserted.ChatTrigger
		created = true
		return nil
	})
	if err != nil {
		return ChatTrigger{}, false, err
	}
	return out, created, nil
}

// ChatContextForRuntime returns the active speaker/lease and effective chat
// policy for one conversation. A missing policy is disabled; a missing or
// expired group lease is not speaker-active. Any read failure is fail-closed
// for the caller.
func (s *Store) ChatContextForRuntime(ctx context.Context, actor RuntimeActor, accountID, kind, conversationID string) (ChatContext, error) {
	var out ChatContext
	if !chatValidID(accountID) || !taskValidQQID(conversationID) ||
		(kind != chatConversationGroup && kind != chatConversationPrivate) {
		return out, problem(taskCodeValidation)
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRuntime(ctx, tx, actor, accountID); err != nil {
			return err
		}
		out = ChatContext{AccountID: accountID, ConversationKind: kind, ConversationID: conversationID}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(self_id,'') FROM core.account WHERE workspace_id=$1 AND id=$2`,
			actor.WorkspaceID, accountID).Scan(&out.QQSelfID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(taskCodeNotFound)
			}
			return err
		}
		if taskBlank(out.QQSelfID) {
			return problem(chatCodeAccountRestricted)
		}
		var enabled bool
		limit := 1
		err := tx.QueryRow(ctx, `SELECT enabled,daily_limit FROM core.chat_send_policy WHERE workspace_id=$1 AND account_id=$2`,
			actor.WorkspaceID, accountID).Scan(&enabled, &limit)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out.ChatEnabled, out.DailyLimit = enabled, limit
		err = tx.QueryRow(ctx, `SELECT issued FROM core.chat_send_usage
			WHERE workspace_id=$1 AND account_id=$2 AND window_start=`+chatUsageDateExpr,
			actor.WorkspaceID, accountID).Scan(&out.IssuedToday)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if kind == chatConversationGroup {
			var speaker string
			var epoch int64
			var lease time.Time
			err = tx.QueryRow(ctx, `SELECT active_speaker_account_id,binding_epoch,lease_expires_at
				FROM core.group_binding WHERE workspace_id=$1 AND group_id=$2`,
				actor.WorkspaceID, conversationID).Scan(&speaker, &epoch, &lease)
			switch {
			case err == nil:
				epochText := chatDecimal(epoch)
				lease = lease.UTC()
				out.ActiveSpeakerAccountID = &speaker
				out.BindingEpoch = &epochText
				out.LeaseExpiresAt = &lease
				now, err := s.dbNow(ctx, tx)
				if err != nil {
					return err
				}
				out.SpeakerActive = speaker == accountID && lease.After(now)
			case errors.Is(err, pgx.ErrNoRows):
				out.SpeakerActive = false
			default:
				return err
			}
		} else {
			// Private conversations have no group speaker lease. The account's
			// current owner epoch is the binding value a permit must echo, and
			// there is no speaker restriction to satisfy.
			ownerEpoch := chatDecimal(actor.OwnerEpoch)
			out.BindingEpoch = &ownerEpoch
			out.SpeakerActive = true
		}
		rows, err := tx.Query(ctx, `SELECT self_id FROM core.account
			WHERE workspace_id=$1 AND self_id IS NOT NULL ORDER BY self_id`, actor.WorkspaceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		out.ManagedSelfIDs = []string{}
		for rows.Next() {
			var selfID string
			if err := rows.Scan(&selfID); err != nil {
				return err
			}
			out.ManagedSelfIDs = append(out.ManagedSelfIDs, selfID)
		}
		return rows.Err()
	})
	if err != nil {
		return ChatContext{}, err
	}
	return out, nil
}
