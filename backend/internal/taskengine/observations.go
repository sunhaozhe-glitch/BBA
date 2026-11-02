package taskengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// P0-10/P0-11: observation inbox, trusted result reduction, persistent
// outbox delivery, finite no_effect retry, and read-only reconciliation.
//
// Trust is never carried by the observation payload. Membership credit comes
// only from the runtime credential binding (membership_authority), the current
// owner epoch, and a matching consumed dispatch permit. Late facts are saved
// and queued for reconciliation; they never mutate the old execution.

const (
	reconcileKind      = "manual_reconciliation"
	outboxLeaseSeconds = 30
	maxOutbox          = 1000
)

// ---------------------------------------------------------------- validation

func normalizeObservation(obs Observation) (Observation, error) {
	obs.EventID = strings.TrimSpace(obs.EventID)
	obs.OperationID = strings.TrimSpace(obs.OperationID)
	obs.AccountID = strings.TrimSpace(obs.AccountID)
	obs.TargetID = strings.TrimSpace(obs.TargetID)
	obs.Kind = strings.TrimSpace(obs.Kind)
	obs.State = strings.TrimSpace(obs.State)
	obs.EvidenceRef = strings.TrimSpace(obs.EvidenceRef)

	switch obs.Kind {
	case "membership.observed":
		switch obs.State {
		case "joined", "not_joined", "unknown":
		default:
			return Observation{}, problem("VALIDATION_FAILED")
		}
	case "execution.observed", "operation.observed":
		obs.Kind = "operation.observed"
		switch obs.State {
		case "submitted", "confirmed", "rejected", "failed_no_effect", "unknown", "pending":
		default:
			return Observation{}, problem("VALIDATION_FAILED")
		}
	case "join.request.observed", "join_request.observed":
		obs.Kind = "join_request.observed"
		switch obs.State {
		case "approved":
			obs.State = "submitted"
		case "cancelled":
			obs.State = "rejected"
		}
		switch obs.State {
		case "submitted", "confirmed", "pending", "rejected", "failed_no_effect", "unknown":
		default:
			return Observation{}, problem("VALIDATION_FAILED")
		}
	default:
		return Observation{}, problem("VALIDATION_FAILED")
	}

	if obs.EventID == "" || len(obs.EventID) > 128 {
		return Observation{}, problem("VALIDATION_FAILED")
	}
	if obs.AccountID == "" || len(obs.AccountID) > 128 {
		return Observation{}, problem("VALIDATION_FAILED")
	}
	if obs.TargetID == "" || len(obs.TargetID) > 128 {
		return Observation{}, problem("VALIDATION_FAILED")
	}
	if obs.OperationID != "" && len(obs.OperationID) > 128 {
		return Observation{}, problem("VALIDATION_FAILED")
	}
	if obs.EvidenceRef == "" || len(obs.EvidenceRef) > 128 {
		return Observation{}, problem("VALIDATION_FAILED")
	}
	if obs.SourceEpoch <= 0 || obs.SourceSequence <= 0 {
		return Observation{}, problem("VALIDATION_FAILED")
	}
	if obs.ObservedAt.IsZero() {
		return Observation{}, problem("VALIDATION_FAILED")
	}
	return obs, nil
}

// ---------------------------------------------------------------- P0-10 inbox

// ReceiveObservation durably accepts one bridge observation into the inbox.
// It validates and authorizes before duplicate replay. A duplicate event with
// the same digest is an idempotent success; a different digest or a reused
// source sequence is an OBSERVATION_DIGEST_CONFLICT. Acceptance is not apply.
func (s *Store) ReceiveObservation(ctx context.Context, actor RuntimeActor, obs Observation) error {
	normalized, err := normalizeObservation(obs)
	if err != nil {
		return err
	}
	if actor.WorkspaceID == "" || actor.RuntimeID == "" {
		return problem("UNAUTHENTICATED")
	}
	if len(actor.AccountIDs) == 0 {
		return problem("UNAUTHENTICATED")
	}
	if normalized.SourceEpoch > actor.OwnerEpoch {
		return problem("VALIDATION_FAILED")
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return problem("VALIDATION_FAILED")
	}
	dg, err := digest(normalized)
	if err != nil {
		return err
	}

	conflict := false
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRuntime(ctx, tx, actor, normalized.AccountID); err != nil {
			return err
		}
		var disabled bool
		err := tx.QueryRow(ctx,
			`SELECT disabled FROM core.target WHERE workspace_id=$1 AND id=$2`,
			actor.WorkspaceID, normalized.TargetID).Scan(&disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("RESOURCE_NOT_FOUND")
		}
		if err != nil {
			return err
		}
		if disabled {
			return problem("VALIDATION_FAILED")
		}
		if normalized.OperationID != "" {
			if err := lockObservationOperation(ctx, tx, actor.WorkspaceID, normalized.OperationID); err != nil {
				return err
			}
		}

		var operation any
		if normalized.OperationID != "" {
			operation = normalized.OperationID
		}
		inserted, err := tx.Exec(ctx,
			`INSERT INTO core.inbox(
               workspace_id,source_runtime_id,event_id,source_account_id,source_epoch,
               source_sequence,operation_id,target_id,kind,state,snapshot_complete,
               observed_at,evidence_ref,digest,payload)
             VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
             ON CONFLICT DO NOTHING`,
			actor.WorkspaceID, actor.RuntimeID, normalized.EventID, normalized.AccountID,
			normalized.SourceEpoch, normalized.SourceSequence, operation, normalized.TargetID,
			normalized.Kind, normalized.State, normalized.SnapshotComplete,
			normalized.ObservedAt, normalized.EvidenceRef, dg, payload)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() == 1 {
			return nil
		}
		// The unique indexes arbitrate concurrent writers. A separate READ
		// COMMITTED statement sees the winner after INSERT has waited for it.
		// Prefer the event-id collision if both unique keys collide.
		var storedEvent, storedDigest string
		var storedPayload []byte
		err = tx.QueryRow(ctx,
			`SELECT event_id,digest,payload FROM core.inbox
			  WHERE workspace_id=$1 AND source_runtime_id=$2
			    AND (event_id=$3 OR (source_account_id=$4 AND source_epoch=$5 AND source_sequence=$6))
			  ORDER BY (event_id=$3) DESC LIMIT 1`,
			actor.WorkspaceID, actor.RuntimeID, normalized.EventID, normalized.AccountID,
			normalized.SourceEpoch, normalized.SourceSequence).Scan(&storedEvent, &storedDigest, &storedPayload)
		if err != nil {
			return err
		}
		if storedEvent == normalized.EventID && storedDigest == dg {
			return nil
		}
		kind := "source_position"
		if storedEvent == normalized.EventID {
			kind = "event_digest"
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO core.observation_conflict(
			   workspace_id,id,source_runtime_id,incoming_event_id,stored_event_id,
			   conflict_kind,incoming_digest,stored_digest,incoming_payload,stored_payload)
			 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			actor.WorkspaceID, newID("obsconflict"), actor.RuntimeID, normalized.EventID,
			storedEvent, kind, dg, storedDigest, payload, storedPayload)
		if err != nil {
			return err
		}
		conflict = true
		return nil
	})
	if err != nil {
		return err
	}
	if conflict {
		// Return rejection only after its evidence is durably committed.
		return problem("OBSERVATION_DIGEST_CONFLICT")
	}
	return nil
}

// ------------------------------------------------------------- P0-10 reducer

type inboxFact struct {
	EventID          string
	SourceRuntimeID  string
	SourceAccountID  string
	SourceEpoch      int64
	SourceSequence   int64
	OperationID      sql.NullString
	TargetID         string
	Kind             string
	State            string
	SnapshotComplete bool
	ObservedAt       time.Time
	EvidenceRef      string
	ApplyState       string
}

type observationExecution struct {
	WorkspaceID   string
	ID            string
	TaskID        string
	SubtaskID     string
	OperationID   string
	AccountID     string
	TargetID      string
	RuntimeID     string
	OwnerEpoch    int64
	Fence         int64
	Scope         int
	Attempt       int
	State         string
	Disposition   sql.NullString
	Effect        string
	EvidenceRef   sql.NullString
	RequestDigest string
	StartedAt     sql.NullTime
	ObservedAt    sql.NullTime
}

type runtimeCredential struct {
	Active              bool
	MembershipAuthority bool
	OwnerEpoch          int64
}

func (e observationExecution) terminalOutcome() bool {
	if e.State == "REJECTED_BEFORE_SEND" {
		return true
	}
	return e.Disposition.Valid &&
		(e.Disposition.String == "rejected" || e.Disposition.String == "failed_no_effect")
}

func lockObservationOperation(ctx context.Context, tx pgx.Tx, workspace, operationID string) error {
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2, 0))`,
		workspace, operationID)
	return err
}

func terminalTaskState(state string) bool {
	switch state {
	case "CANCELLED", "SUCCEEDED", "PARTIAL", "FAILED":
		return true
	}
	return false
}

// reconcileTaskProgress closes a non-terminal task only when durable evidence
// proves either the goal or that no safe local work remains. Unknown, pending,
// in-flight and reconciling work deliberately keep the task open before its
// deadline.
func reconcileTaskProgress(ctx context.Context, tx pgx.Tx, task TaskRow) error {
	if terminalTaskState(task.State) {
		return nil
	}
	var validResults, pendingExternal, actionable, retryScheduled int
	if err := tx.QueryRow(ctx, `
 		SELECT
 			(SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid),
 			(SELECT count(*) FROM core.execution
 			  WHERE workspace_id=$1 AND task_id=$2
 			    AND (state IN ('DISPATCHING','UNKNOWN')
 			         OR effect_certainty IN ('unknown','confirmed_submitted'))),
 			(SELECT count(*) FROM core.subtask s
 			  JOIN core.task_target tt ON (tt.workspace_id,tt.task_id,tt.target_id)=(s.workspace_id,s.task_id,s.target_id)
 			  WHERE s.workspace_id=$1 AND s.task_id=$2
 			    AND s.state NOT IN ('SUCCEEDED','FAILED','CANCELLED')
 			    AND tt.eligibility='eligible' AND NOT tt.baseline_covered),
 			(SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2 AND state='scheduled')`,
		task.WorkspaceID, task.ID).Scan(&validResults, &pendingExternal, &actionable, &retryScheduled); err != nil {
		return err
	}
	if validResults >= task.GoalCount && pendingExternal == 0 {
		return transitionTaskTerminal(ctx, tx, task, "SUCCEEDED", "goal_reached", validResults, pendingExternal)
	}
	if time.Now().Before(task.Deadline) {
		if !task.ImportDone || pendingExternal != 0 || actionable != 0 || retryScheduled != 0 || task.State == "BLOCKED" || task.State == "PAUSED" || task.State == "PAUSING" || task.State == "CANCELLING" {
			return nil
		}
		return transitionTaskTerminal(ctx, tx, task, taskPartialOrFailed(validResults), "candidates_exhausted", validResults, pendingExternal)
	}
	return transitionTaskTerminal(ctx, tx, task, taskPartialOrFailed(validResults), "deadline_reached", validResults, pendingExternal)
}

func taskPartialOrFailed(validResults int) string {
	if validResults > 0 {
		return "PARTIAL"
	}
	return "FAILED"
}

func transitionTaskTerminal(ctx context.Context, tx pgx.Tx, task TaskRow, state, reason string, validResults, pendingExternal int) error {
	if !terminalTaskState(state) || terminalTaskState(task.State) {
		return nil
	}
	ct, err := tx.Exec(ctx, `UPDATE core.task SET state=$3,version=version+1 WHERE workspace_id=$1 AND id=$2 AND version=$4 AND state NOT IN ('CANCELLED','SUCCEEDED','PARTIAL','FAILED')`, task.WorkspaceID, task.ID, state, task.Version)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return nil
	}
	return appendEvent(ctx, tx, task.WorkspaceID, task.ID, "task.terminal", "reconciler", map[string]any{
		"state":            state,
		"reason":           reason,
		"valid_results":    validResults,
		"pending_external": pendingExternal,
		"goal_count":       task.GoalCount,
	})
}

func loadInboxFact(ctx context.Context, tx pgx.Tx, workspace, runtime, eventID string, lock bool) (inboxFact, error) {
	var f inboxFact
	query := `SELECT event_id,source_runtime_id,source_account_id,source_epoch,source_sequence,
		        operation_id,target_id,kind,state,snapshot_complete,observed_at,
		        evidence_ref,apply_state
		   FROM core.inbox
		  WHERE workspace_id=$1 AND source_runtime_id=$2 AND event_id=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, query,
		workspace, runtime, eventID).Scan(
		&f.EventID, &f.SourceRuntimeID, &f.SourceAccountID, &f.SourceEpoch,
		&f.SourceSequence, &f.OperationID, &f.TargetID, &f.Kind, &f.State,
		&f.SnapshotComplete, &f.ObservedAt, &f.EvidenceRef, &f.ApplyState)
	if err != nil {
		return inboxFact{}, err
	}
	return f, nil
}

func loadRuntimeCredential(ctx context.Context, tx pgx.Tx, workspace, runtime, account string) (runtimeCredential, error) {
	var c runtimeCredential
	err := tx.QueryRow(ctx,
		`SELECT active,membership_authority,owner_epoch
		   FROM core.runtime_credentials
		  WHERE workspace_id=$1 AND runtime_id=$2 AND account_id=$3`,
		workspace, runtime, account).Scan(&c.Active, &c.MembershipAuthority, &c.OwnerEpoch)
	if err != nil {
		return runtimeCredential{}, err
	}
	return c, nil
}

func executionTaskID(ctx context.Context, tx pgx.Tx, workspace, operationID string) (string, error) {
	var taskID string
	err := tx.QueryRow(ctx,
		`SELECT task_id FROM core.execution
		  WHERE workspace_id=$1 AND operation_id=$2`,
		workspace, operationID).Scan(&taskID)
	return taskID, err
}

func loadExecutionForUpdate(ctx context.Context, tx pgx.Tx, workspace, operationID string) (observationExecution, error) {
	var e observationExecution
	err := tx.QueryRow(ctx,
		`SELECT workspace_id,id,task_id,subtask_id,operation_id,account_id,target_id,
		        runtime_id,owner_epoch,fence_token,scope_revision,attempt_no,state,
		        disposition,effect_certainty,evidence_ref,request_digest,started_at,observed_at
		   FROM core.execution
		  WHERE workspace_id=$1 AND operation_id=$2
		  FOR UPDATE`,
		workspace, operationID).Scan(
		&e.WorkspaceID, &e.ID, &e.TaskID, &e.SubtaskID, &e.OperationID, &e.AccountID,
		&e.TargetID, &e.RuntimeID, &e.OwnerEpoch, &e.Fence, &e.Scope, &e.Attempt,
		&e.State, &e.Disposition, &e.Effect, &e.EvidenceRef, &e.RequestDigest,
		&e.StartedAt, &e.ObservedAt)
	if err != nil {
		return observationExecution{}, err
	}
	return e, nil
}

func newerMembershipFact(ctx context.Context, tx pgx.Tx, workspace string, f inboxFact) (string, bool, error) {
	var state string
	err := tx.QueryRow(ctx,
		`SELECT state
		   FROM core.inbox
		  WHERE workspace_id=$1 AND source_account_id=$2 AND target_id=$3
		    AND kind='membership.observed'
		    AND (source_epoch > $4 OR (source_epoch=$4 AND source_sequence > $5))
		  ORDER BY source_epoch DESC, source_sequence DESC
		  LIMIT 1`,
		workspace, f.SourceAccountID, f.TargetID, f.SourceEpoch, f.SourceSequence).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return state, true, nil
}

func hasPendingNewerMembershipEvidence(ctx context.Context, tx pgx.Tx, workspace, taskID, targetID, executionID string) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(
			   SELECT 1
			     FROM core.result r
			     JOIN core.inbox i
			       ON i.workspace_id=r.workspace_id
			      AND i.source_runtime_id=r.source_runtime_id
			      AND i.event_id=r.source_event_id
			    WHERE r.workspace_id=$1 AND r.task_id=$2 AND r.target_id=$3
			      AND r.execution_id=$4 AND r.valid
			      AND i.kind='membership.observed'
			      AND EXISTS(
				   SELECT 1 FROM core.inbox newer
				    WHERE newer.workspace_id=i.workspace_id
				      AND newer.source_account_id=i.source_account_id
				      AND newer.target_id=i.target_id
				      AND newer.kind='membership.observed'
				      AND (newer.source_epoch>i.source_epoch
				           OR (newer.source_epoch=i.source_epoch
				               AND newer.source_sequence>i.source_sequence))
				      AND newer.apply_state<>'applied')
			 )`,
		workspace, taskID, targetID, executionID).Scan(&pending)
	return pending, err
}

func upsertMembershipFact(ctx context.Context, tx pgx.Tx, workspace string, f inboxFact) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO core.membership AS m(
		   workspace_id,account_id,target_id,state,observed_at,evidence_ref,
		   source_runtime_id,source_event_id)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (workspace_id,account_id,target_id) DO UPDATE
		   SET state=EXCLUDED.state,
		       observed_at=EXCLUDED.observed_at,
		       evidence_ref=EXCLUDED.evidence_ref,
		       source_runtime_id=EXCLUDED.source_runtime_id,
		       source_event_id=EXCLUDED.source_event_id
		 WHERE (
		   (SELECT i.source_epoch FROM core.inbox i
		     WHERE i.workspace_id=m.workspace_id
		       AND i.source_runtime_id=m.source_runtime_id
		       AND i.event_id=m.source_event_id) < $9
		   OR (
		     (SELECT i.source_epoch FROM core.inbox i
		       WHERE i.workspace_id=m.workspace_id
		         AND i.source_runtime_id=m.source_runtime_id
		         AND i.event_id=m.source_event_id) = $9
		     AND (
		       m.source_runtime_id <> $7
		       OR (SELECT i.source_sequence FROM core.inbox i
		            WHERE i.workspace_id=m.workspace_id
		              AND i.source_runtime_id=m.source_runtime_id
		              AND i.event_id=m.source_event_id) <= $10
		     )
		   )
		 )`,
		workspace, f.SourceAccountID, f.TargetID, f.State, f.ObservedAt,
		f.EvidenceRef, f.SourceRuntimeID, f.EventID, f.SourceEpoch, f.SourceSequence)
	return err
}

func appendReconcileOnce(ctx context.Context, tx pgx.Tx, task TaskRow, e observationExecution, eventID, reason string) error {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM core.task_event
		    WHERE workspace_id=$1 AND task_id=$2 AND kind=$3
		      AND detail->>'execution_id'=$4)`,
		task.WorkspaceID, task.ID, reconcileKind, e.ID).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return appendEvent(ctx, tx, task.WorkspaceID, task.ID, reconcileKind,
		"reconciler", map[string]any{
			"execution_id": e.ID,
			"operation_id": e.OperationID,
			"event_id":     eventID,
			"reason":       reason,
			"state":        e.State,
			"disposition":  e.Disposition.String,
		})
}

func markInboxApplied(ctx context.Context, tx pgx.Tx, workspace, runtime, eventID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE core.inbox
		    SET apply_state='applied', applied_at=clock_timestamp(), error_code=NULL
		  WHERE workspace_id=$1 AND source_runtime_id=$2 AND event_id=$3`,
		workspace, runtime, eventID)
	return err
}

// ApplyObservation is the trusted internal reducer. It must be called with
// service-internal arguments after ReceiveObservation; callers cannot mark an
// observation trusted. The whole reduction, including inbox apply state,
// result, slot, execution fact and audit event, commits atomically.
func (s *Store) ApplyObservation(ctx context.Context, workspace, runtime, eventID string) error {
	if workspace == "" || runtime == "" || eventID == "" {
		return problem("VALIDATION_FAILED")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		// Pre-read without locking to learn the correlated task. The lock
		// order for the reducer is task -> inbox -> execution/subtask/slot/
		// claim/account so two events for one task cannot deadlock with a
		// task holder that later has to lock an inbox row.
		pre, err := loadInboxFact(ctx, tx, workspace, runtime, eventID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("RESOURCE_NOT_FOUND")
		}
		if err != nil {
			return err
		}
		if pre.SourceRuntimeID != runtime {
			return problem("RESOURCE_NOT_FOUND")
		}
		if pre.ApplyState == "applied" {
			return nil
		}

		var task TaskRow
		haveTask := false
		if pre.OperationID.Valid && pre.OperationID.String != "" {
			taskID, err := executionTaskID(ctx, tx, workspace, pre.OperationID.String)
			if err == nil {
				task, err = lockTask(ctx, tx, workspace, taskID)
				if err != nil {
					return err
				}
				haveTask = true
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}

		f, err := loadInboxFact(ctx, tx, workspace, runtime, eventID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("RESOURCE_NOT_FOUND")
		}
		if err != nil {
			return err
		}
		if f.SourceRuntimeID != runtime {
			return problem("RESOURCE_NOT_FOUND")
		}
		if f.ApplyState == "applied" {
			return nil
		}
		if pre.OperationID != f.OperationID {
			return problem("OBSERVATION_DIGEST_CONFLICT")
		}

		var e observationExecution
		haveExecution := false
		if haveTask {
			e, err = loadExecutionForUpdate(ctx, tx, workspace, f.OperationID.String)
			if errors.Is(err, pgx.ErrNoRows) {
				haveExecution = false
			} else if err != nil {
				return err
			} else {
				haveExecution = true
			}
		}

		cred, credErr := loadRuntimeCredential(ctx, tx, workspace, f.SourceRuntimeID, f.SourceAccountID)
		if credErr != nil && !errors.Is(credErr, pgx.ErrNoRows) {
			return credErr
		}
		trustedFresh := credErr == nil && cred.Active && cred.MembershipAuthority &&
			cred.OwnerEpoch == f.SourceEpoch
		matched := haveExecution && e.AccountID == f.SourceAccountID &&
			e.TargetID == f.TargetID &&
			e.OperationID == f.OperationID.String &&
			e.RuntimeID == f.SourceRuntimeID &&
			e.OwnerEpoch == f.SourceEpoch

		// Facts are never last-write-wins. Authoritative membership only
		// moves forward when this observation is current, complete and, when
		// correlated to an execution, matches that execution. An older event
		// whose newer membership event already exists is retained in inbox
		// but cannot overwrite the projection or credit a result.
		if f.Kind == "membership.observed" {
			_, hasNewer, err := newerMembershipFact(ctx, tx, workspace, f)
			if err != nil {
				return err
			}
			if hasNewer {
				return markInboxApplied(ctx, tx, workspace, runtime, eventID)
			}
			if !f.SnapshotComplete {
				if haveExecution {
					if err := appendReconcileOnce(ctx, tx, task, e, eventID, "incomplete_membership_snapshot"); err != nil {
						return err
					}
				}
				return markInboxApplied(ctx, tx, workspace, runtime, eventID)
			}
			if trustedFresh && (!haveExecution || matched) {
				if err := upsertMembershipFact(ctx, tx, workspace, f); err != nil {
					return err
				}
			}
		}

		if !haveExecution {
			// Passive facts may have no operation correlation. P0 never
			// adopts an uncorrelated join as task success, but an authoritative
			// leave is the same external truth as a correlated one: every
			// credited result for that account and target must stop counting so
			// progress can never exceed reality.
			if f.Kind == "membership.observed" && f.State == "not_joined" && trustedFresh {
				if err := invalidatePassiveLeave(ctx, tx, workspace, f); err != nil {
					return err
				}
			}
			return markInboxApplied(ctx, tx, workspace, runtime, eventID)
		}

		if !matched || !trustedFresh {
			if f.OperationID.Valid && f.OperationID.String != "" {
				if err := lockObservationOperation(ctx, tx, workspace, f.OperationID.String); err != nil {
					return err
				}
			}
			pendingNewer, err := hasPendingNewerMembershipEvidence(ctx, tx, workspace, task.ID, f.TargetID, e.ID)
			if err != nil {
				return err
			}
			if pendingNewer {
				return markInboxApplied(ctx, tx, workspace, runtime, eventID)
			}
			// Stale epoch or mismatched operation/account/target: preserve the
			// fact and enqueue read-only reconciliation. Never overwrite the old
			// execution or credit a result.
			if err := appendReconcileOnce(ctx, tx, task, e, eventID, "stale_or_mismatched_source"); err != nil {
				return err
			}
			return markInboxApplied(ctx, tx, workspace, runtime, eventID)
		}

		if err := markInboxApplied(ctx, tx, workspace, runtime, eventID); err != nil {
			return err
		}
		terminalTask := terminalTaskState(task.State)
		switch f.Kind {
		case "membership.observed":
			if err := applyMembershipObservation(ctx, tx, task, e, f, terminalTask); err != nil {
				return err
			}
		case "operation.observed", "join_request.observed":
			if err := applyOperationObservation(ctx, tx, task, e, f); err != nil {
				return err
			}
		default:
			return problem("VALIDATION_FAILED")
		}
		// The trusted fact and its credited result are now durable; project the
		// exact execution/attempt onto its work item in the same transaction.
		if err := s.projectWorkItemExecution(ctx, tx, task, e.ID, "observation", eventID); err != nil {
			return err
		}
		if err := reconcileTaskProgress(ctx, tx, task); err != nil {
			return err
		}
		return settlePausingTask(ctx, tx, workspace, task.ID)
	})
}

func applyMembershipObservation(ctx context.Context, tx pgx.Tx, task TaskRow, e observationExecution, f inboxFact, terminalTask bool) error {
	if terminalTask {
		// Membership facts and inbox were advanced by the caller. Closed task
		// accounting is historical for every membership state, including leave.
		return appendEvent(ctx, tx, task.WorkspaceID, task.ID,
			"membership.late_observed", "runtime:"+e.RuntimeID, map[string]any{
				"execution_id": e.ID,
				"event_id":     f.EventID,
				"account_id":   f.SourceAccountID,
				"target_id":    f.TargetID,
				"task_state":   task.State,
				"state":        f.State,
			})
	}
	switch f.State {
	case "joined":
		if !task.BaselineComplete {
			return problem("EXTERNAL_OUTCOME_UNKNOWN")
		}
		if e.Scope != task.ScopeRevision {
			if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "scope_revision_mismatch"); err != nil {
				return err
			}
			return nil
		}
		if !e.StartedAt.Valid {
			if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "execution_never_started"); err != nil {
				return err
			}
			return nil
		}
		if f.ObservedAt.Before(e.StartedAt.Time) {
			if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "observation_before_dispatch"); err != nil {
				return err
			}
			return nil
		}
		var dbNow time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
			return err
		}
		if f.ObservedAt.After(dbNow.Add(5 * time.Second)) {
			if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "observation_in_future"); err != nil {
				return err
			}
			return nil
		}
		if e.terminalOutcome() {
			if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "joined_after_rejected_or_no_effect"); err != nil {
				return err
			}
			return nil
		}

		var baselineCovered bool
		var eligibility string
		err := tx.QueryRow(ctx,
			`SELECT baseline_covered,eligibility FROM core.task_target
			  WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3`,
			task.WorkspaceID, task.ID, f.TargetID).Scan(&baselineCovered, &eligibility)
		if errors.Is(err, pgx.ErrNoRows) {
			return appendReconcileOnce(ctx, tx, task, e, f.EventID, "target_not_in_task")
		}
		if err != nil {
			return err
		}
		if baselineCovered || eligibility != "eligible" {
			// Baseline membership and ineligible targets never count as new.
			return appendEvent(ctx, tx, task.WorkspaceID, task.ID,
				"membership.baseline_ignored", "runtime:"+e.RuntimeID, map[string]any{
					"execution_id": e.ID,
					"event_id":     f.EventID,
					"target_id":    f.TargetID,
				})
		}

		var permitOK bool
		err = tx.QueryRow(ctx,
			`SELECT EXISTS(
			   SELECT 1 FROM core.dispatch_permit
			    WHERE workspace_id=$1 AND execution_id=$2 AND operation_id=$3
			      AND account_id=$4 AND target_id=$5 AND scope_revision=$6
			      AND owner_epoch=$7 AND state='consumed')`,
			task.WorkspaceID, e.ID, e.OperationID, e.AccountID, e.TargetID,
			e.Scope, e.OwnerEpoch).Scan(&permitOK)
		if err != nil {
			return err
		}
		if !permitOK {
			if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "missing_consumed_permit"); err != nil {
				return err
			}
			return nil
		}

		var slotNo int
		err = tx.QueryRow(ctx,
			`SELECT slot_no FROM core.goal_slot
			  WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'
			  ORDER BY slot_no LIMIT 1 FOR UPDATE`,
			task.WorkspaceID, task.ID, f.TargetID).Scan(&slotNo)
		slotFound := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !slotFound {
			if f.OperationID.Valid && f.OperationID.String != "" {
				if err := lockObservationOperation(ctx, tx, task.WorkspaceID, f.OperationID.String); err != nil {
					return err
				}
			}
			var hasResult bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM core.result
				  WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND valid)`,
				task.WorkspaceID, task.ID, f.TargetID).Scan(&hasResult); err != nil {
				return err
			}
			if hasResult {
				if _, err := insertResultOnce(ctx, tx, task, e, f); err != nil {
					return err
				}
				return nil
			}
			return appendReconcileOnce(ctx, tx, task, e, f.EventID, "missing_reserved_slot")
		}

		if f.OperationID.Valid && f.OperationID.String != "" {
			if err := lockObservationOperation(ctx, tx, task.WorkspaceID, f.OperationID.String); err != nil {
				return err
			}
		}

		_, hasNewer, err := newerMembershipFact(ctx, tx, task.WorkspaceID, f)
		if err != nil {
			return err
		}
		if hasNewer {
			return nil
		}
		inserted, err := insertResultOnce(ctx, tx, task, e, f)
		if err != nil {
			return err
		}
		if !inserted {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.goal_slot
			    SET state='fulfilled',fulfilled_at=$4
			  WHERE workspace_id=$1 AND task_id=$2 AND slot_no=$3 AND state='reserved'`,
			task.WorkspaceID, task.ID, slotNo, f.ObservedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.subtask
			    SET state='SUCCEEDED',version=version+1
			  WHERE workspace_id=$1 AND id=$2 AND state<>'CANCELLED'`,
			task.WorkspaceID, e.SubtaskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.execution
			    SET state='OBSERVED',disposition='confirmed',effect_certainty='confirmed',
			        evidence_ref=$3,observed_at=$4
			  WHERE workspace_id=$1 AND id=$2`,
			task.WorkspaceID, e.ID, f.EvidenceRef, f.ObservedAt); err != nil {
			return err
		}
		// The send is authoritatively finished; no managed send remains in
		// flight for this execution.
		if _, err := tx.Exec(ctx,
			`UPDATE core.account_action_lock SET held=false
			  WHERE workspace_id=$1 AND execution_id=$2 AND held`,
			task.WorkspaceID, e.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.target_claim SET state='resolved',resolved_at=clock_timestamp()
			  WHERE workspace_id=$1 AND task_id=$2 AND subtask_id=$3 AND target_id=$4
			    AND state='active'
			    AND NOT EXISTS(
			      SELECT 1 FROM core.execution x
			       WHERE x.workspace_id=$1 AND x.task_id=$2 AND x.target_id=$4
			         AND x.id<>$5
			         AND (x.state IN ('DISPATCHING','UNKNOWN')
			              OR x.effect_certainty IN ('unknown','confirmed_submitted')))`,
			task.WorkspaceID, task.ID, e.SubtaskID, f.TargetID, e.ID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, task.WorkspaceID, task.ID,
			"result.confirmed", "runtime:"+e.RuntimeID, map[string]any{
				"execution_id": e.ID,
				"operation_id": e.OperationID,
				"account_id":   e.AccountID,
				"target_id":    e.TargetID,
				"event_id":     f.EventID,
				"evidence_ref": f.EvidenceRef,
				"observed_at":  f.ObservedAt,
			}); err != nil {
			return err
		}
		return reconcileTaskProgress(ctx, tx, task)
	case "not_joined":
		// not_joined never proves that no join request is pending, so it must
		// never release a hold that an unresolved request still needs. It is
		// authoritative about current membership, though: an account that is
		// no longer in the group cannot keep counting as a new member, so a
		// previously credited result stops counting and its slot returns to
		// the pool. Nothing is resent, retried or reassigned here.
		invalidated, err := invalidateTargetResult(ctx, tx, task, e, f)
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.WorkspaceID, task.ID,
			"membership.not_joined", "runtime:"+e.RuntimeID, map[string]any{
				"execution_id": e.ID,
				"event_id":     f.EventID,
				"invalidated":  invalidated,
			})
	case "unknown":
		// Unknown membership requires read-only external query. It does not
		// change the execution outcome or release holds.
		if _, err := tx.Exec(ctx,
			`UPDATE core.subtask SET state='RECONCILING',version=version+1
			  WHERE workspace_id=$1 AND id=$2
			    AND state NOT IN ('SUCCEEDED','FAILED','CANCELLED')`,
			task.WorkspaceID, e.SubtaskID); err != nil {
			return err
		}
		return appendReconcileOnce(ctx, tx, task, e, f.EventID, "membership_unknown")
	default:
		return problem("VALIDATION_FAILED")
	}
}

// invalidatePassiveLeave applies one uncorrelated authoritative not_joined to
// every task that still credits that account in that target. Tasks are locked
// in task_id order, the same order the reducer and recovery use, so a passive
// fact cannot deadlock with a task holder. Terminal tasks keep their historical
// counts: their success is a closed record, and retroactively rewriting it
// would be a business decision, not an observation.
func invalidatePassiveLeave(ctx context.Context, tx pgx.Tx, workspace string, f inboxFact) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT task_id FROM core.result
		WHERE workspace_id=$1 AND target_id=$2 AND account_id=$3 AND valid
		ORDER BY task_id`, workspace, f.TargetID, f.SourceAccountID)
	if err != nil {
		return err
	}
	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		taskIDs = append(taskIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, taskID := range taskIDs {
		task, err := lockTask(ctx, tx, workspace, taskID)
		if err != nil {
			return err
		}
		var executionID string
		err = tx.QueryRow(ctx, `SELECT execution_id FROM core.result
			WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND account_id=$4 AND valid`,
			workspace, taskID, f.TargetID, f.SourceAccountID).Scan(&executionID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		e := observationExecution{
			WorkspaceID: workspace, ID: executionID, TaskID: taskID,
			AccountID: f.SourceAccountID, TargetID: f.TargetID,
		}
		if err := tx.QueryRow(ctx, `SELECT subtask_id,operation_id,runtime_id
			FROM core.execution WHERE workspace_id=$1 AND id=$2`, workspace, executionID).
			Scan(&e.SubtaskID, &e.OperationID, &e.RuntimeID); err != nil {
			return err
		}
		if terminalTaskState(task.State) {
			if err := appendEvent(ctx, tx, workspace, taskID, "membership.passive_leave_terminal",
				"runtime:"+e.RuntimeID, map[string]any{
					"execution_id": executionID,
					"event_id":     f.EventID,
					"target_id":    f.TargetID,
					"account_id":   f.SourceAccountID,
					"task_state":   task.State,
					"read_only":    true,
				}); err != nil {
				return err
			}
			continue
		}
		invalidated, err := invalidateTargetResult(ctx, tx, task, e, f)
		if err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, workspace, taskID, "membership.not_joined_passive",
			"runtime:"+e.RuntimeID, map[string]any{
				"execution_id": executionID,
				"event_id":     f.EventID,
				"target_id":    f.TargetID,
				"account_id":   f.SourceAccountID,
				"invalidated":  invalidated,
			}); err != nil {
			return err
		}
	}
	return nil
}

// invalidateTargetResult revokes a previously credited result after an
// authoritative membership fact proves the account is no longer in the target.
// The execution, permit and inbox evidence stay immutable; only the derived
// result flag, its slot and the target claim move so progress counts cannot
// exceed reality or go negative.
func invalidateTargetResult(ctx context.Context, tx pgx.Tx, task TaskRow, e observationExecution, f inboxFact) (bool, error) {
	ct, err := tx.Exec(ctx, `UPDATE core.result SET valid=false,version=version+1
		WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND account_id=$4 AND valid`,
		task.WorkspaceID, task.ID, f.TargetID, f.SourceAccountID)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE core.goal_slot
		SET state='free',target_id=NULL,reserved_at=NULL,fulfilled_at=NULL
		WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state IN ('reserved','fulfilled')`,
		task.WorkspaceID, task.ID, f.TargetID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE core.target_claim SET state='resolved',resolved_at=clock_timestamp()
		WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='active'`,
		task.WorkspaceID, task.ID, f.TargetID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE core.subtask SET state='RECONCILING',version=version+1
		WHERE workspace_id=$1 AND id=$2 AND state<>'CANCELLED'`,
		task.WorkspaceID, e.SubtaskID); err != nil {
		return false, err
	}
	if err := appendEvent(ctx, tx, task.WorkspaceID, task.ID, "result.invalidated",
		"runtime:"+e.RuntimeID, map[string]any{
			"execution_id": e.ID,
			"event_id":     f.EventID,
			"target_id":    f.TargetID,
			"reason":       "authoritative_not_joined",
			"read_only":    true,
		}); err != nil {
		return false, err
	}
	if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "membership_exited_after_result"); err != nil {
		return false, err
	}
	return true, nil
}

func insertResultOnce(ctx context.Context, tx pgx.Tx, task TaskRow, e observationExecution, f inboxFact) (bool, error) {
	ct, err := tx.Exec(ctx,
		`INSERT INTO core.result(
		   workspace_id,id,task_id,target_id,account_id,execution_id,
		   source_runtime_id,source_event_id,valid,evidence_ref,confirmed_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,true,$9,$10)
		 ON CONFLICT (workspace_id,task_id,target_id) DO NOTHING`,
		task.WorkspaceID, newID("res"), task.ID, f.TargetID, e.AccountID, e.ID,
		f.SourceRuntimeID, f.EventID, f.EvidenceRef, f.ObservedAt)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 1 {
		return true, nil
	}

	// A previous invalid result may be revived by new trusted evidence. A
	// valid result for this execution is refreshed to the current evidence so
	// the deferred closure sees the result's source as the newest applied fact.
	var valid bool
	var executionID string
	err = tx.QueryRow(ctx,
		`SELECT valid,execution_id FROM core.result
		  WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 FOR UPDATE`,
		task.WorkspaceID, task.ID, f.TargetID).Scan(&valid, &executionID)
	if err != nil {
		return false, err
	}
	if valid {
		if executionID != e.ID {
			return false, nil
		}
		_, err = tx.Exec(ctx,
			`UPDATE core.result
			    SET source_runtime_id=$4,source_event_id=$5,evidence_ref=$6,
			        confirmed_at=$7,version=version+1
			  WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND valid`,
			task.WorkspaceID, task.ID, f.TargetID, f.SourceRuntimeID, f.EventID,
			f.EvidenceRef, f.ObservedAt)
		return false, err
	}
	_, err = tx.Exec(ctx,
		`UPDATE core.result
		    SET valid=true,account_id=$4,execution_id=$5,source_runtime_id=$6,
		        source_event_id=$7,evidence_ref=$8,confirmed_at=$9,version=version+1
		  WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND valid=false`,
		task.WorkspaceID, task.ID, f.TargetID, e.AccountID, e.ID,
		f.SourceRuntimeID, f.EventID, f.EvidenceRef, f.ObservedAt)
	if err != nil {
		return false, err
	}
	return true, nil
}

func applyOperationObservation(ctx context.Context, tx pgx.Tx, task TaskRow, e observationExecution, f inboxFact) error {
	terminalTask := terminalTaskState(task.State)
	switch f.State {
	case "submitted", "pending", "confirmed":
		// A confirmed adapter response is still not membership success.
		disposition := f.State
		effect := "confirmed_submitted"
		if f.State == "confirmed" {
			effect = "confirmed"
		}
		if e.State == "OBSERVED" {
			// Never regress a stronger terminal observation with a later,
			// weaker in-flight fact.
			if e.Disposition.Valid && e.Disposition.String != "unknown" &&
				e.Disposition.String != disposition {
				return nil
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.execution
			    SET state='OBSERVED',disposition=$3,effect_certainty=$4,
			        evidence_ref=$5,observed_at=$6
			  WHERE workspace_id=$1 AND id=$2`,
			task.WorkspaceID, e.ID, disposition, effect, f.EvidenceRef, f.ObservedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.subtask SET state='WAITING_EXTERNAL',version=version+1
			  WHERE workspace_id=$1 AND id=$2
			    AND state NOT IN ('SUCCEEDED','FAILED','CANCELLED')`,
			task.WorkspaceID, e.SubtaskID); err != nil {
			return err
		}
		if terminalTask {
			return nil
		}
		return appendEvent(ctx, tx, task.WorkspaceID, task.ID,
			"execution."+disposition, "runtime:"+e.RuntimeID, map[string]any{
				"execution_id":  e.ID,
				"operation_id":  e.OperationID,
				"event_id":      f.EventID,
				"outcome":       disposition,
				"slot_retained": true,
			})
	case "rejected":
		if _, err := tx.Exec(ctx,
			`UPDATE core.execution
			    SET state='OBSERVED',disposition='rejected',effect_certainty='confirmed_rejected',
			        evidence_ref=$3,observed_at=$4
			  WHERE workspace_id=$1 AND id=$2`,
			task.WorkspaceID, e.ID, f.EvidenceRef, f.ObservedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.subtask SET state='FAILED',version=version+1
			  WHERE workspace_id=$1 AND id=$2 AND state<>'SUCCEEDED'`,
			task.WorkspaceID, e.SubtaskID); err != nil {
			return err
		}
		if err := releaseAfterNoEffect(ctx, tx, task, e); err != nil {
			return err
		}
		if err := releaseExecutionSendLock(ctx, tx, task.WorkspaceID, e.ID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.WorkspaceID, task.ID,
			"execution.rejected", "runtime:"+e.RuntimeID, map[string]any{
				"execution_id": e.ID,
				"operation_id": e.OperationID,
				"event_id":     f.EventID,
				"retry":        false,
			})
	case "failed_no_effect":
		proven, err := bridgeNoEffectProof(ctx, tx, task.WorkspaceID, e.OperationID)
		if err != nil {
			return err
		}
		if !proven {
			// A caller cannot self-declare no_effect. Leave the inbox pending
			// and the execution untouched until the dispatcher persists proof.
			return problem("RETRY_NOT_ALLOWED")
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.execution
			    SET state='OBSERVED',disposition='failed_no_effect',effect_certainty='no_effect',
			        evidence_ref=$3,observed_at=$4
			  WHERE workspace_id=$1 AND id=$2`,
			task.WorkspaceID, e.ID, f.EvidenceRef, f.ObservedAt); err != nil {
			return err
		}
		if err := releaseAfterNoEffect(ctx, tx, task, e); err != nil {
			return err
		}
		if err := releaseExecutionSendLock(ctx, tx, task.WorkspaceID, e.ID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.WorkspaceID, task.ID,
			"execution.failed_no_effect", "runtime:"+e.RuntimeID, map[string]any{
				"execution_id": e.ID,
				"operation_id": e.OperationID,
				"event_id":     f.EventID,
				"retryable":    true,
			})
	case "unknown":
		if e.State == "OBSERVED" && e.Disposition.Valid &&
			(e.Disposition.String == "confirmed" || e.Disposition.String == "rejected" ||
				e.Disposition.String == "failed_no_effect" || e.Disposition.String == "submitted" ||
				e.Disposition.String == "pending") {
			// A weaker unknown fact must not erase a stronger observation, but
			// it still asks for a read-only external query.
			if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "execution_unknown_after_observed"); err != nil {
				return err
			}
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.execution
			    SET state='UNKNOWN',disposition='unknown',effect_certainty='unknown',
			        observed_at=$3
			  WHERE workspace_id=$1 AND id=$2`,
			task.WorkspaceID, e.ID, f.ObservedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.subtask SET state='RECONCILING',version=version+1
			  WHERE workspace_id=$1 AND id=$2
			    AND state NOT IN ('SUCCEEDED','FAILED','CANCELLED')`,
			task.WorkspaceID, e.SubtaskID); err != nil {
			return err
		}
		if err := appendReconcileOnce(ctx, tx, task, e, f.EventID, "execution_unknown"); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.WorkspaceID, task.ID,
			"execution.unknown", "runtime:"+e.RuntimeID, map[string]any{
				"execution_id": e.ID,
				"operation_id": e.OperationID,
				"event_id":     f.EventID,
			})
	default:
		return problem("VALIDATION_FAILED")
	}
}

func releaseExecutionSendLock(ctx context.Context, tx pgx.Tx, workspace, executionID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE core.account_action_lock SET held=false
		  WHERE workspace_id=$1 AND execution_id=$2 AND held`,
		workspace, executionID)
	return err
}

func bridgeNoEffectProof(ctx context.Context, tx pgx.Tx, workspace, operationID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM bridge.gateway_operation
		    WHERE workspace_id=$1 AND operation_id=$2
		      AND state='OBSERVED' AND disposition='failed_no_effect'
		      AND effect_certainty='no_effect'
		      AND evidence_ref IS NOT NULL AND btrim(evidence_ref)<>'')`,
		workspace, operationID).Scan(&ok)
	return ok, err
}

// releaseAfterNoEffect releases a reserved goal slot and the target claim only
// when the execution is terminally proven to have no external effect and no
// other execution for the target may still be in flight.
func releaseAfterNoEffect(ctx context.Context, tx pgx.Tx, task TaskRow, e observationExecution) error {
	if _, err := tx.Exec(ctx,
		`UPDATE core.goal_slot
		    SET state='free',target_id=NULL,reserved_at=NULL,fulfilled_at=NULL
		  WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'
		    AND NOT EXISTS(
		      SELECT 1 FROM core.execution x
		       WHERE x.workspace_id=$1 AND x.task_id=$2 AND x.target_id=$3
		         AND x.id<>$4
		         AND (x.state IN ('DISPATCHING','UNKNOWN')
		              OR x.effect_certainty IN ('unknown','confirmed_submitted')))`,
		task.WorkspaceID, task.ID, e.TargetID, e.ID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx,
		`UPDATE core.target_claim SET state='resolved',resolved_at=clock_timestamp()
		  WHERE workspace_id=$1 AND task_id=$2 AND subtask_id=$3 AND target_id=$4
		    AND state='active'
		    AND NOT EXISTS(
		      SELECT 1 FROM core.execution x
		       WHERE x.workspace_id=$1 AND x.task_id=$2 AND x.target_id=$4
		         AND x.id<>$5
		         AND (x.state IN ('DISPATCHING','UNKNOWN')
		              OR x.effect_certainty IN ('unknown','confirmed_submitted')))`,
		task.WorkspaceID, task.ID, e.SubtaskID, e.TargetID, e.ID)
	return err
}

// ------------------------------------------------------------- P0-10 outbox

// DeliverOutbox leases persistent core outbox rows and CAS-acks each delivery.
// A transport failure keeps the same event id pending for retry; it never
// creates a second business event or a new write action.
func (s *Store) DeliverOutbox(ctx context.Context, worker string, limit int, deliver func(context.Context, string, json.RawMessage) error) (int, error) {
	if worker == "" || limit <= 0 || deliver == nil {
		return 0, problem("VALIDATION_FAILED")
	}
	if limit > maxOutbox {
		limit = maxOutbox
	}
	type leased struct {
		workspace string
		id        string
		eventType string
		payload   []byte
	}
	var batch []leased
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`WITH picked AS (
			   SELECT workspace_id,id FROM core.outbox
			    WHERE (state='pending' AND next_attempt_at<=clock_timestamp())
			       OR (state='delivering' AND lease_until<clock_timestamp())
			    ORDER BY next_attempt_at,id
			    LIMIT $1
			    FOR UPDATE SKIP LOCKED)
			 UPDATE core.outbox o
			    SET state='delivering',lease_owner=$2,
			        lease_until=clock_timestamp()+make_interval(secs => $3::double precision)
			   FROM picked p
			  WHERE o.workspace_id=p.workspace_id AND o.id=p.id
			  RETURNING o.workspace_id,o.id,o.event_type,o.payload`,
			limit, worker, outboxLeaseSeconds)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l leased
			if err := rows.Scan(&l.workspace, &l.id, &l.eventType, &l.payload); err != nil {
				return err
			}
			batch = append(batch, l)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}

	delivered := 0
	var firstErr error
	for _, item := range batch {
		// The stable outbox/event id is the transport idempotency key. The
		// payload carries the event type and detail for routing.
		dErr := deliver(ctx, item.id, json.RawMessage(item.payload))
		if dErr == nil {
			var n int64
			err := s.tx(ctx, func(tx pgx.Tx) error {
				ct, err := tx.Exec(ctx,
					`UPDATE core.outbox
					    SET state='delivered',attempts=attempts+1,
					        lease_owner=NULL,lease_until=NULL
					  WHERE workspace_id=$1 AND id=$2
					    AND state='delivering' AND lease_owner=$3`,
					item.workspace, item.id, worker)
				if err != nil {
					return err
				}
				n = ct.RowsAffected()
				return nil
			})
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if n == 1 {
				delivered++
			}
			continue
		}
		if firstErr == nil {
			firstErr = dErr
		}
		_ = s.tx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE core.outbox
				    SET state='pending',attempts=attempts+1,
				        next_attempt_at=clock_timestamp()+$4::interval,
				        lease_owner=NULL,lease_until=NULL
				  WHERE workspace_id=$1 AND id=$2
				    AND state='delivering' AND lease_owner=$3`,
				item.workspace, item.id, worker, outboxBackoff(0))
			return err
		})
	}
	return delivered, firstErr
}

func outboxBackoff(attempts int) string {
	seconds := 5 << attempts
	if seconds > 300 || seconds <= 0 {
		seconds = 300
	}
	return fmt.Sprintf("%d seconds", seconds)
}

// -------------------------------------------------------- P0-11 retry budget

// ScheduleRetry creates at most one scheduled retry for a logical subtask.
// Eligibility comes only from persisted dispatcher proof plus core execution
// fact; caller booleans are not consulted. Attempt budgets belong to the
// logical subtask and task, so changing account or execution cannot reset them.
func (s *Store) ScheduleRetry(ctx context.Context, workspace, executionID, strategy string, due time.Time) error {
	if workspace == "" || executionID == "" {
		return problem("VALIDATION_FAILED")
	}
	if strategy != "same_account" && strategy != "reselect" {
		return problem("VALIDATION_FAILED")
	}
	if due.IsZero() {
		return problem("VALIDATION_FAILED")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var taskID string
		err := tx.QueryRow(ctx,
			`SELECT task_id FROM core.execution WHERE workspace_id=$1 AND id=$2`,
			workspace, executionID).Scan(&taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("RESOURCE_NOT_FOUND")
		}
		if err != nil {
			return err
		}
		task, err := lockTask(ctx, tx, workspace, taskID)
		if err != nil {
			return err
		}
		if terminalTaskState(task.State) {
			return problem("ACTION_FORBIDDEN")
		}
		e, err := loadExecutionByIDForUpdate(ctx, tx, workspace, executionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("RESOURCE_NOT_FOUND")
		}
		if err != nil {
			return err
		}
		if e.RefusedButUncertain() {
			return problem("EXTERNAL_OUTCOME_UNKNOWN")
		}
		if e.Disposition.Valid && e.Disposition.String == "rejected" {
			return problem("ACTION_FORBIDDEN")
		}
		if e.State != "OBSERVED" || !e.Disposition.Valid ||
			e.Disposition.String != "failed_no_effect" || e.Effect != "no_effect" {
			return problem("RETRY_NOT_ALLOWED")
		}

		proven, err := bridgeNoEffectProof(ctx, tx, workspace, e.OperationID)
		if err != nil {
			return err
		}
		if !proven {
			return problem("RETRY_NOT_ALLOWED")
		}
		var pendingPermit bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM core.dispatch_permit
			  WHERE workspace_id=$1 AND execution_id=$2 AND state='issued')`,
			workspace, e.ID).Scan(&pendingPermit); err != nil {
			return err
		}
		if pendingPermit {
			return problem("RETRY_NOT_ALLOWED")
		}
		var heldLock bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM core.account_action_lock
			  WHERE workspace_id=$1 AND execution_id=$2 AND held)`,
			workspace, e.ID).Scan(&heldLock); err != nil {
			return err
		}
		if heldLock {
			return problem("RETRY_NOT_ALLOWED")
		}
		var otherLive bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM core.execution
			  WHERE workspace_id=$1 AND subtask_id=$2 AND id<>$3
			    AND (state IN ('CREATED','ACCEPTED','DISPATCHING','UNKNOWN')
			         OR effect_certainty IN ('unknown','confirmed_submitted')))`,
			workspace, e.SubtaskID, e.ID).Scan(&otherLive); err != nil {
			return err
		}
		if otherLive {
			return problem("EXTERNAL_OUTCOME_UNKNOWN")
		}

		var attemptCount, maxAttempts int
		err = tx.QueryRow(ctx,
			`SELECT attempt_count,max_attempts FROM core.subtask
			  WHERE workspace_id=$1 AND id=$2 AND task_id=$3 FOR UPDATE`,
			workspace, e.SubtaskID, taskID).Scan(&attemptCount, &maxAttempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("RESOURCE_NOT_FOUND")
		}
		if err != nil {
			return err
		}
		if attemptCount >= maxAttempts || attemptCount >= task.MaxAttempts {
			return problem("LOCAL_BUDGET_EXHAUSTED")
		}
		var totalUsed int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM core.execution
			  WHERE workspace_id=$1 AND task_id=$2`,
			workspace, taskID).Scan(&totalUsed); err != nil {
			return err
		}
		if totalUsed >= task.MaxTotalAttempts {
			return problem("LOCAL_BUDGET_EXHAUSTED")
		}

		var assigned any
		if strategy == "same_account" {
			assigned = e.AccountID
		}
		retryID := newID("retry")
		ct, err := tx.Exec(ctx,
			`INSERT INTO core.retry(
			   workspace_id,id,task_id,subtask_id,previous_execution_id,
			   strategy,reason_code,due_at,state)
			 VALUES($1,$2,$3,$4,$5,$6,'ADAPTER_TRANSIENT_NO_EFFECT',$7,'scheduled')
			 ON CONFLICT (workspace_id,subtask_id) WHERE state='scheduled' DO NOTHING`,
			workspace, retryID, taskID, e.SubtaskID, e.ID, strategy, due)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return problem("RETRY_ALREADY_SCHEDULED")
		}
		if _, err := tx.Exec(ctx,
			`UPDATE core.subtask
			    SET state='RETRY_WAIT',next_run_at=$3,assigned_account_id=$4,
			        current_execution_id=NULL,version=version+1
			  WHERE workspace_id=$1 AND id=$2`,
			workspace, e.SubtaskID, due, assigned); err != nil {
			return err
		}
		return appendEvent(ctx, tx, workspace, task.ID, "retry.scheduled",
			"reconciler", map[string]any{
				"retry_id":             retryID,
				"execution_id":         e.ID,
				"operation_id":         e.OperationID,
				"subtask_id":           e.SubtaskID,
				"strategy":             strategy,
				"attempt_count":        attemptCount,
				"max_attempts":         maxAttempts,
				"task_total_used":      totalUsed,
				"task_total_remaining": task.MaxTotalAttempts - totalUsed,
				"due_at":               due,
			})
	})
}

type executionForRetry struct {
	ID          string
	TaskID      string
	SubtaskID   string
	OperationID string
	AccountID   string
	TargetID    string
	State       string
	Disposition sql.NullString
	Effect      string
}

func (e executionForRetry) RefusedButUncertain() bool {
	if e.State == "DISPATCHING" || e.State == "UNKNOWN" {
		return true
	}
	if e.Disposition.Valid {
		switch e.Disposition.String {
		case "submitted", "pending", "unknown":
			return true
		}
	}
	return false
}

func loadExecutionByIDForUpdate(ctx context.Context, tx pgx.Tx, workspace, executionID string) (executionForRetry, error) {
	var e executionForRetry
	err := tx.QueryRow(ctx,
		`SELECT id,task_id,subtask_id,operation_id,account_id,target_id,state,disposition,effect_certainty
		   FROM core.execution
		  WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
		workspace, executionID).Scan(
		&e.ID, &e.TaskID, &e.SubtaskID, &e.OperationID, &e.AccountID,
		&e.TargetID, &e.State, &e.Disposition, &e.Effect)
	if err != nil {
		return executionForRetry{}, err
	}
	return e, nil
}

// ---------------------------------------------------- P0-11 reconciliation

// ReconcileTimeouts writes one deduplicated read-only reconciliation audit
// event per unresolved execution older than 24 hours. It never retries,
// releases, reassigns, or weakens an unknown outcome.
func (s *Store) ReconcileTimeouts(ctx context.Context, workspace string) (int, error) {
	if workspace == "" {
		return 0, problem("VALIDATION_FAILED")
	}
	count := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT x.task_id,x.id,x.operation_id,x.target_id,x.state,COALESCE(x.disposition,'')
			   FROM core.execution x
			  WHERE x.workspace_id=$1
			    AND (x.state IN ('DISPATCHING','UNKNOWN')
			         OR COALESCE(x.disposition,'') IN ('submitted','pending','unknown')
			         OR (x.state='OBSERVED' AND x.disposition='confirmed'
			             AND NOT EXISTS(SELECT 1 FROM core.result r
			                             WHERE r.workspace_id=x.workspace_id AND r.task_id=x.task_id
			                               AND r.target_id=x.target_id AND r.valid)))
			    AND COALESCE(x.observed_at,x.started_at,x.created_at)
			        <= clock_timestamp()-interval '24 hours'
			  ORDER BY x.task_id,x.id`,
			workspace)
		if err != nil {
			return err
		}
		type candidate struct {
			taskID      string
			id          string
			operationID string
			targetID    string
			state       string
			disposition string
		}
		var cands []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.taskID, &c.id, &c.operationID, &c.targetID, &c.state, &c.disposition); err != nil {
				rows.Close()
				return err
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, c := range cands {
			task, err := lockTask(ctx, tx, workspace, c.taskID)
			if err != nil {
				return err
			}
			var state, disposition string
			err = tx.QueryRow(ctx,
				`SELECT state,COALESCE(disposition,'') FROM core.execution
				  WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
				workspace, c.id).Scan(&state, &disposition)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			var hasValidResult bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM core.result
				  WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND valid)`,
				workspace, c.taskID, c.targetID).Scan(&hasValidResult); err != nil {
				return err
			}
			confirmedWithoutResult := state == "OBSERVED" && disposition == "confirmed" && !hasValidResult
			unresolved := state == "DISPATCHING" || state == "UNKNOWN" ||
				disposition == "submitted" || disposition == "pending" || disposition == "unknown" ||
				confirmedWithoutResult
			if !unresolved {
				continue
			}
			reason := "timeout_24h"
			if confirmedWithoutResult {
				// The receipt is confirmed but no trusted membership result
				// exists. This is a read-only lookup request: the slot, claim
				// and account lock stay exactly as they are.
				reason = "membership_result_missing_24h"
			}
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(
				   SELECT 1 FROM core.task_event
				    WHERE workspace_id=$1 AND task_id=$2 AND kind=$3
				      AND detail->>'execution_id'=$4)`,
				workspace, task.ID, reconcileKind, c.id).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			if err := appendEvent(ctx, tx, workspace, task.ID, reconcileKind,
				"reconciler", map[string]any{
					"execution_id": c.id,
					"operation_id": c.operationID,
					"state":        state,
					"disposition":  disposition,
					"reason":       reason,
					"read_only":    true,
				}); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

// ------------------------------------------------- bridge outbox -> inbox

// bridgeObservationPayload is the canonical bridge Observation JSON. Epoch
// and sequence are decimal strings on the wire but integers in core.
type bridgeObservationPayload struct {
	SchemaVersion   string          `json:"schema_version"`
	EventID         string          `json:"event_id"`
	WorkspaceID     string          `json:"workspace_id"`
	SourceRuntimeID string          `json:"source_runtime_id"`
	SourceAccountID string          `json:"source_account_id"`
	SourceEpoch     json.RawMessage `json:"source_epoch"`
	SourceSequence  json.RawMessage `json:"source_sequence"`
	OperationID     *string         `json:"operation_id"`
	Target          struct {
		ExternalID string `json:"external_id"`
	} `json:"target"`
	Kind    string `json:"kind"`
	Payload struct {
		State            string  `json:"state"`
		SnapshotComplete bool    `json:"snapshot_complete"`
		Outcome          string  `json:"outcome"`
		EffectCertainty  string  `json:"effect_certainty"`
		RequestRef       *string `json:"request_ref"`
		MessageRef       *string `json:"message_ref"`
	} `json:"payload"`
	ObservedAt  time.Time `json:"observed_at"`
	EvidenceRef *string   `json:"evidence_ref"`
}

func parseDecimal(raw json.RawMessage) (int64, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" {
		return 0, problem("VALIDATION_FAILED")
	}
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		return 0, problem("VALIDATION_FAILED")
	}
	return n, nil
}

// PumpBridgeOutbox feeds persisted bridge observation outbox rows into the
// core inbox using the supplied RuntimeActor. It CAS-acks bridge rows after
// durable inbox acceptance, so a crash merely replays the same event id.
func (s *Store) PumpBridgeOutbox(ctx context.Context, actor RuntimeActor) (int, error) {
	return s.PumpBridgeOutboxBatch(ctx, actor, 100)
}

// PumpBridgeOutboxBatch is PumpBridgeOutbox with an explicit bounded batch.
func (s *Store) PumpBridgeOutboxBatch(ctx context.Context, actor RuntimeActor, limit int) (int, error) {
	if actor.WorkspaceID == "" || actor.RuntimeID == "" || len(actor.AccountIDs) == 0 {
		return 0, problem("UNAUTHENTICATED")
	}
	if limit <= 0 {
		return 0, problem("VALIDATION_FAILED")
	}
	if limit > maxOutbox {
		limit = maxOutbox
	}
	type row struct {
		id      string
		payload []byte
	}
	var rowsOut []row
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT bo.id,bo.payload
			   FROM bridge.outbox bo
			   JOIN bridge.gateway_operation g
			     ON g.workspace_id=bo.workspace_id AND g.operation_id=bo.operation_id
			  WHERE bo.workspace_id=$1 AND bo.state='pending'
			    AND bo.next_attempt_at<=clock_timestamp()
			    AND g.runtime_id=$2 AND g.account_id=ANY($3::text[])
			  ORDER BY bo.id LIMIT $4`,
			actor.WorkspaceID, actor.RuntimeID, actor.AccountIDs, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.payload); err != nil {
				return err
			}
			rowsOut = append(rowsOut, r)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}

	accepted := 0
	var firstErr error
	for _, r := range rowsOut {
		var p bridgeObservationPayload
		if err := json.Unmarshal(r.payload, &p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		epoch, err := parseDecimal(p.SourceEpoch)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		seq, err := parseDecimal(p.SourceSequence)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var targetID string
		if err := s.Pool.QueryRow(ctx,
			`SELECT id FROM core.target WHERE workspace_id=$1 AND external_id=$2`,
			actor.WorkspaceID, p.Target.ExternalID).Scan(&targetID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		state := p.Payload.Outcome
		if state == "" {
			state = p.Payload.State
		}
		evidence := p.EventID
		if p.EvidenceRef != nil && strings.TrimSpace(*p.EvidenceRef) != "" {
			evidence = *p.EvidenceRef
		}
		op := ""
		if p.OperationID != nil {
			op = *p.OperationID
		}
		obs := Observation{
			EventID:          p.EventID,
			OperationID:      op,
			AccountID:        p.SourceAccountID,
			TargetID:         targetID,
			Kind:             p.Kind,
			State:            state,
			EvidenceRef:      evidence,
			SourceEpoch:      epoch,
			SourceSequence:   seq,
			ObservedAt:       p.ObservedAt,
			SnapshotComplete: p.Payload.SnapshotComplete,
		}
		if err := s.ReceiveObservation(ctx, actor, obs); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			_ = s.tx(ctx, func(tx pgx.Tx) error {
				_, e := tx.Exec(ctx,
					`UPDATE bridge.outbox SET attempts=attempts+1,
					        next_attempt_at=clock_timestamp()+$3::interval
					  WHERE workspace_id=$1 AND id=$2 AND state='pending'`,
					actor.WorkspaceID, r.id, outboxBackoff(0))
				return e
			})
			continue
		}
		ct, err := s.ackBridgeOutbox(ctx, actor.WorkspaceID, r.id)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ct == 1 {
			accepted++
		}
	}
	return accepted, firstErr
}

func (s *Store) ackBridgeOutbox(ctx context.Context, workspace, id string) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE bridge.outbox
			    SET state='delivered',attempts=attempts+1
			  WHERE workspace_id=$1 AND id=$2 AND state='pending'`,
			workspace, id)
		if err != nil {
			return err
		}
		n = ct.RowsAffected()
		return nil
	})
	return n, err
}
