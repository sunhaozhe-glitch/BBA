package main

// Controlled chat send through the existing P0-15 ChatSend permits. The
// permit ledger is the only authority for one text adapter call. A consumed
// permit without an outcome is terminal UNKNOWN and is never resent.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
)

type chatTriggerFacts struct {
	TriggerRef       string
	RuntimeID        string
	OwnerEpoch       int64
	AccountID        string
	ConversationKind string
	ConversationID   string
	DeadlineAt       time.Time
	SegmentCount     int
	NextSegment      int
	State            string
}

type chatPermitFacts struct {
	PermitID         string
	State            string
	RuntimeID        string
	OwnerEpoch       int64
	AccountID        string
	ConversationKind string
	ConversationID   string
	TriggerRef       string
	TextDigest       string
	SendID           string
	Segment          int
	ExpiresAt        time.Time
}

type chatOutcomeFacts struct {
	State      string
	MessageID  string
	OccurredAt time.Time
}

func loadChatTriggerFacts(ctx context.Context, store *taskengine.Store, workspace, triggerRef string) (chatTriggerFacts, error) {
	var out chatTriggerFacts
	err := store.Pool.QueryRow(ctx, `
		SELECT trigger_ref,runtime_id,owner_epoch,account_id,conversation_kind,conversation_id,
		       deadline_at,segment_count,next_segment,state
		  FROM core.chat_trigger WHERE workspace_id=$1 AND trigger_ref=$2`, workspace, triggerRef).
		Scan(&out.TriggerRef, &out.RuntimeID, &out.OwnerEpoch, &out.AccountID, &out.ConversationKind,
			&out.ConversationID, &out.DeadlineAt, &out.SegmentCount, &out.NextSegment, &out.State)
	return out, err
}

func loadChatPermitFacts(ctx context.Context, store *taskengine.Store, workspace, sendID string, segment int) (chatPermitFacts, error) {
	var out chatPermitFacts
	err := store.Pool.QueryRow(ctx, `
		SELECT permit_id,state,runtime_id,owner_epoch,account_id,conversation_kind,conversation_id,
		       trigger_ref,text_digest,send_id,segment_index,expires_at
		  FROM core.chat_send_permit
		 WHERE workspace_id=$1 AND send_id=$2 AND segment_index=$3`, workspace, sendID, segment).
		Scan(&out.PermitID, &out.State, &out.RuntimeID, &out.OwnerEpoch, &out.AccountID,
			&out.ConversationKind, &out.ConversationID, &out.TriggerRef,
			&out.TextDigest, &out.SendID, &out.Segment, &out.ExpiresAt)
	return out, err
}

func loadChatOutcome(ctx context.Context, store *taskengine.Store, workspace, permitID string) (chatOutcomeFacts, error) {
	var out chatOutcomeFacts
	err := store.Pool.QueryRow(ctx, `
		SELECT state,COALESCE(message_id,''),occurred_at FROM core.chat_send_outcome
		 WHERE workspace_id=$1 AND permit_id=$2`, workspace, permitID).
		Scan(&out.State, &out.MessageID, &out.OccurredAt)
	return out, err
}

func chatIdempotencyKey(sendID string, segment int) string {
	sum := sha256.Sum256([]byte(sendID + "|" + strconv.Itoa(segment)))
	return "p020_chat_" + hex.EncodeToString(sum[:16])
}

func runChatSend(ctx context.Context, store *taskengine.Store, rt taskengine.RuntimeActor, loaded loadedConfig, live *liveClient, ev *evidence) (map[string]any, error) {
	summary := map[string]any{}
	cfg := loaded.cfg
	chat := cfg.Chat
	if chat == nil {
		return summary, errors.New("chat section is required")
	}
	account, err := loadAccountFacts(ctx, store, cfg.WorkspaceID, cfg.AccountID)
	if err != nil {
		return summary, fmt.Errorf("read persisted account: %w", err)
	}
	if blockers := validateAccount(cfg, account, false); len(blockers) > 0 {
		return summary, fmt.Errorf("chat account is not healthy: %v", blockers)
	}
	identity, err := live.observeIdentity(ctx, false)
	if err != nil {
		return summary, fmt.Errorf("authenticated chat identity readback: %w", err)
	}
	if identity.SelfID != account.SelfID {
		return summary, errors.New("authenticated chat identity does not match persisted self_id")
	}
	trigger, err := loadChatTriggerFacts(ctx, store, cfg.WorkspaceID, chat.TriggerRef)
	if err != nil {
		return summary, fmt.Errorf("read persisted chat trigger: %w", err)
	}
	if trigger.RuntimeID != cfg.RuntimeID || trigger.OwnerEpoch != cfg.OwnerEpoch || trigger.AccountID != cfg.AccountID {
		return summary, errors.New("chat trigger is not owned by this runtime/account/owner epoch")
	}
	if !trigger.DeadlineAt.After(time.Now().UTC()) {
		return summary, errors.New("chat trigger deadline already passed")
	}
	text, err := readChatText(chat.TextFile)
	if err != nil {
		return summary, err
	}
	textDigest := taskengine.ChatTextDigest(text)
	summary["trigger_ref"] = trigger.TriggerRef
	summary["conversation_kind"] = trigger.ConversationKind
	summary["segment_index"] = trigger.NextSegment
	summary["text_digest"] = textDigest
	summary["text_length"] = len(text)

	permit, err := loadChatPermitFacts(ctx, store, cfg.WorkspaceID, chat.SendID, chat.SegmentIndex)
	existing := err == nil
	if err != nil && taskengine.Code(err) != "RESOURCE_NOT_FOUND" {
		return summary, err
	}
	if existing {
		if permit.RuntimeID != cfg.RuntimeID || permit.OwnerEpoch != cfg.OwnerEpoch ||
			permit.AccountID != cfg.AccountID || permit.SendID != chat.SendID ||
			permit.Segment != chat.SegmentIndex || permit.TextDigest != textDigest ||
			permit.TriggerRef != trigger.TriggerRef || permit.ConversationKind != trigger.ConversationKind ||
			permit.ConversationID != trigger.ConversationID {
			return summary, errors.New("existing permit does not match the configured send scope and text digest")
		}
		switch permit.State {
		case "expired":
			return summary, errors.New("chat permit already expired")
		case "consumed":
			outcome, outcomeErr := loadChatOutcome(ctx, store, cfg.WorkspaceID, permit.PermitID)
			if outcomeErr == nil {
				summary["permit_id"] = permit.PermitID
				summary["outcome_state"] = outcome.State
				summary["message_id"] = outcome.MessageID
				summary["result"] = "ALREADY_HAS_OUTCOME"
				if outcome.State != "sent" {
					return summary, errors.New("EXTERNAL_OUTCOME_UNKNOWN")
				}
				return summary, nil
			}
			if taskengine.Code(outcomeErr) != "RESOURCE_NOT_FOUND" {
				return summary, outcomeErr
			}
			// A consumed permit without an outcome is terminal unknown. Record
			// the durable outcome and never resend.
			_, _, recordErr := store.RecordChatSendOutcome(ctx, rt, permit.PermitID, taskengine.ChatSendOutcomeRequest{
				SendId: chat.SendID, State: "unknown", OccurredAt: time.Now().UTC(),
			})
			summary["permit_id"] = permit.PermitID
			summary["outcome_state"] = "unknown"
			summary["result"] = "EXTERNAL_OUTCOME_UNKNOWN"
			if recordErr != nil {
				return summary, fmt.Errorf("record terminal unknown chat outcome: %w", recordErr)
			}
			return summary, errors.New("EXTERNAL_OUTCOME_UNKNOWN")
		case "issued":
			// Continue with this already-issued permit.
		default:
			return summary, fmt.Errorf("unsupported chat permit state %s", permit.State)
		}
	}
	if !existing {
		if trigger.State != "open" {
			return summary, fmt.Errorf("chat trigger state=%s, want open", trigger.State)
		}
		if trigger.NextSegment != chat.SegmentIndex || chat.SegmentIndex >= trigger.SegmentCount {
			return summary, errors.New("configured chat segment is not the next permitted segment")
		}
		contextFacts, err := store.ChatContextForRuntime(ctx, rt, cfg.AccountID, trigger.ConversationKind, trigger.ConversationID)
		if err != nil {
			return summary, fmt.Errorf("read chat context: %w", err)
		}
		if !contextFacts.SpeakerActive || contextFacts.BindingEpoch == nil || *contextFacts.BindingEpoch == "" {
			return summary, errors.New("chat binding is not active for this account")
		}
		request := contract.ChatPermitRequest{
			AccountId:            contract.Id(cfg.AccountID),
			ConversationId:       contract.QQId(trigger.ConversationID),
			ConversationKind:     contract.ChatPermitRequestConversationKind(trigger.ConversationKind),
			DeadlineAt:           trigger.DeadlineAt.UTC(),
			ExpectedBindingEpoch: *contextFacts.BindingEpoch,
			SegmentCount:         trigger.SegmentCount,
			SegmentIndex:         trigger.NextSegment,
			SendId:               contract.Id(chat.SendID),
			TextDigest:           textDigest,
			TriggerRef:           contract.Id(trigger.TriggerRef),
		}
		issued, err := store.IssueChatSendPermit(ctx, rt, chatIdempotencyKey(chat.SendID, chat.SegmentIndex), request)
		if err != nil {
			return summary, fmt.Errorf("issue chat send permit: %w", err)
		}
		permit = chatPermitFacts{PermitID: issued.PermitId, State: string(issued.State), RuntimeID: rt.RuntimeID,
			OwnerEpoch: rt.OwnerEpoch, AccountID: cfg.AccountID, ConversationKind: trigger.ConversationKind,
			ConversationID: trigger.ConversationID, TriggerRef: trigger.TriggerRef,
			TextDigest: issued.TextDigest, SendID: issued.SendId,
			Segment: issued.SegmentIndex, ExpiresAt: issued.ExpiresAt}
	}
	summary["permit_id"] = permit.PermitID

	messageID, sent, sendErr := live.sendChatText(ctx, permit.PermitID, chat.SendID, chat.SegmentIndex, text)
	summary["permit_state"] = "consumed_by_gateway"
	ev.step("chat_gateway_send", map[string]any{"permit_id": permit.PermitID, "segment_index": chat.SegmentIndex, "text_digest": textDigest})
	summary["outcome_state"] = "unknown"
	if sendErr != nil || !sent || messageID == "" {
		summary["result"] = "EXTERNAL_OUTCOME_UNKNOWN"
		return summary, errors.New("EXTERNAL_OUTCOME_UNKNOWN")
	}
	summary["outcome_state"] = "sent"
	summary["message_id"] = messageID
	summary["result"] = "SENT"
	return summary, nil
}

func nullableMessageIDForState(state, messageID string) *string {
	if state != "sent" {
		return nil
	}
	return nullableMessageID(messageID)
}

func nullableMessageID(messageID string) *string {
	if messageID == "" {
		return nil
	}
	value := messageID
	return &value
}
