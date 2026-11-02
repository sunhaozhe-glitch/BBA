package taskengine

// Bounded pipeline steps of the deterministic worker. Each step selects at most
// WorkerConfig.BatchSize items per page, holds no transaction across an external
// call and is safe to repeat: after a settled pass every step reports zero.
// Runtime-group enumeration walks a durable per-consumer rotation cursor, so a
// group past this tick's bounded page window is served on a later tick and a
// restarted worker continues where the previous one stopped.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	workerManualReconcileRequestKind = "manual_reconciliation_requested"
	workerManualReconcileAckKind     = "manual_reconciliation_observed"
)

// fixtureTransport is the default join adapter. It is deterministic, performs no
// network call and returns a submitted receipt whose evidence reference is
// derived from the operation id, so a fixture run can be replayed exactly.
type fixtureTransport struct{}

func (fixtureTransport) RequestJoin(_ context.Context, r Reservation) (SendOutcome, error) {
	return SendOutcome{
		Disposition:     "submitted",
		EffectCertainty: "confirmed_submitted",
		EvidenceRef:     "fixture_send_" + r.OperationID,
		RequestRef:      "fixture_request_" + r.OperationID,
	}, nil
}

// fixtureOutboxSink acknowledges a task event locally. There is no external
// subscriber in the development composition, so the sink only exists to keep the
// outbox state machine exercised.
func fixtureOutboxSink(context.Context, string, []byte) error { return nil }

// stepRecover runs the instance's recovery pass before the first dispatch, so a
// restarted process quarantines its leftover DISPATCHING operations as UNKNOWN
// instead of sending again. Never-expired permits are expired separately; both
// keep goal slots, claims and account windows exactly as they are.
//
// The pass walks the durable recovery rotation cursor: every tick covers at most
// workerMaxRuntimePages bounded pages of active runtime groups and remembers its
// position, so a group past the page cap is served on a later tick instead of
// never. Reaching the end of the population closes the lap, and a closed lap
// always resumes at the head, so a non-zero stored position means "everything up
// to here has already been acted on".
//
// The position is advanced only after the walk's work has been attempted, so a
// tick that is cancelled or crashes mid-window repeats that window next time
// instead of stepping over it. A failed group never blocks the rest of the lap:
// its work is left pending, the rotation still advances, and the failure is
// recorded durably (lap_incomplete) before the position moves. A closing lap
// therefore cannot complete the pass over a group that failed on an earlier page,
// in this process or a previous one: the pass stays pending and the next lap,
// which starts at the head, retries it. Each group's work is its own transaction,
// so groups that already committed are settled and only the unfinished work runs
// again.
//
// A restarted worker resumes the stored position, so the tail is served first,
// but it never inherits a partial lap as complete: before its first segment it
// durably marks a lap that does not start at the head as incomplete. A group that
// started DISPATCHING after the previous process committed its last page is
// therefore still covered by a full head-to-tail lap before this instance may
// close the pass. The cost after a restart is at most one extra lap, i.e.
// ceil(groups/(pages*BatchSize)) further ticks.
//
// The same applies to a group that appears behind the position while the lap is
// still running in this process: every credential insert, update and delete bumps
// the population generation, and a lap that closes after the generation moved
// cannot complete the pass. It resets to the head and the next lap, which starts
// there, serves the new group. The check costs one row read per segment, and a
// changing population only costs further laps, never a skipped group.
//
// One session holds the recovery rotation lease for the whole segment (walk,
// actor calls, position, lap close), and every rotation read and control write
// runs on that session, so two instances can never interleave a failed early page
// with a clean tail close, and a session that died (dropping the lock) cannot
// write the cursor or the lap marker any more. The durable lap marker can only be
// set or cleared by the instance that owns the lap. The second instance skips its
// recovery step for that tick; the owner is making the progress.
func (w *Worker) stepRecover(ctx context.Context, tick *WorkerTick) error {
	if !w.recoveryPending() {
		return nil
	}
	lease, err := acquireSessionLease(ctx, w.store.Pool, "taskengine.worker.rotation."+workerCursorRecovery)
	if err != nil {
		return fmt.Errorf("recovery: rotation lease: %w", err)
	}
	if lease == nil {
		// Another worker owns the recovery rotation for this segment; it is the one
		// moving the pass forward, and this tick must not close a lap it does not own.
		return nil
	}
	defer lease.release()
	// Every rotation read and control write runs on the lease session, so a session
	// that died and lost the lock can neither read nor write a cursor another
	// instance now owns.
	db := workerDB(lease.direct)

	if w.recoveryNeedsStart() {
		if err := w.inheritRecoveryLap(ctx, db); err != nil {
			return fmt.Errorf("recovery: mark inherited lap: %w", err)
		}
		w.markRecoveryStarted()
	}
	generation, err := w.runtimePopulationGeneration(ctx, db)
	if err != nil {
		return fmt.Errorf("recovery: read population generation: %w", err)
	}
	w.recoveryLapGeneration(generation)
	rotation, err := w.runtimeActors(ctx, db, workerCursorRecovery, w.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("recovery: enumerate runtime actors: %w", err)
	}
	var errs []error
	for _, actor := range rotation.Actors {
		recovered, err := w.store.RecoverDispatching(ctx, actor)
		if err != nil {
			errs = append(errs, fmt.Errorf("recovery: %s/%s dispatching: %w", actor.WorkspaceID, actor.RuntimeID, err))
			continue
		}
		tick.RecoveredDispatching += recovered
		expired, err := w.store.ExpirePermits(ctx, actor)
		if err != nil {
			errs = append(errs, fmt.Errorf("recovery: %s/%s permits: %w", actor.WorkspaceID, actor.RuntimeID, err))
			continue
		}
		tick.ExpiredPermits += expired
	}
	if len(errs) > 0 {
		// The obligation is durable before the position moves, so a crash here
		// cannot let a later lap close over the failed group.
		if err := w.markRecoveryLapIncomplete(ctx, db); err != nil {
			return errors.Join(append(errs, fmt.Errorf("recovery: record lap failure: %w", err))...)
		}
	}
	// The segment may have outlived its session (server restart, network loss). A
	// dead session has already dropped the lock, so the position and the lap marker
	// must not be written by this instance any more; the window simply repeats.
	if err := lease.valid(ctx); err != nil {
		return errors.Join(append(errs, fmt.Errorf("recovery: rotation lease: %w", err))...)
	}
	if err := w.saveRuntimeCursor(ctx, db, workerCursorRecovery, rotation.From, rotation.To); err != nil {
		// The position is unchanged, so the next tick repeats this window instead of
		// closing the lap over work that was never attempted.
		return errors.Join(append(errs, fmt.Errorf("recovery: advance rotation cursor: %w", err))...)
	}
	if rotation.LapClosed {
		closed, err := w.runtimePopulationGeneration(ctx, db)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("recovery: read population generation: %w", err))...)
		}
		changed := w.closeRecoveryLap(closed)
		clean, err := w.claimRecoveryLap(ctx, db)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("recovery: close lap: %w", err))
		case clean && !changed:
			w.completeRecovery()
		}
	}
	return errors.Join(errs...)
}

// stepExpand imports one page of the fixed candidate template for each draft
// task. It is pure template expansion: no model call, no search provider and no
// external request.
func (w *Worker) stepExpand(ctx context.Context, tick *WorkerTick) error {
	rows, err := w.store.Pool.Query(ctx, `
		SELECT workspace_id,id,principal_id
		  FROM core.task
		 WHERE state IN ('DRAFT','VALIDATING') AND NOT import_done
		 ORDER BY created_at,workspace_id,id
		 LIMIT $1`, w.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("expand: select drafts: %w", err)
	}
	type ref struct{ workspace, taskID, principal string }
	var refs []ref
	for rows.Next() {
		var item ref
		if err := rows.Scan(&item.workspace, &item.taskID, &item.principal); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var errs []error
	for _, item := range refs {
		processed, err := w.withTaskLease(ctx, item.workspace, item.taskID, func(lease *taskLease) error {
			actor := Actor{WorkspaceID: item.workspace, PrincipalID: item.principal, Permissions: map[string]bool{taskPermControl: true}}
			for range workerPagesPerTaskPerTick {
				if err := lease.valid(ctx); err != nil {
					return err
				}
				done, err := w.store.ExpandFixedPage(ctx, actor, item.taskID, w.cfg.PageSize)
				if err != nil {
					if Code(err) == taskCodeStateConflict {
						// Another actor started the task or already imported the
						// whole list; this is settled, not a failure.
						return nil
					}
					return err
				}
				tick.ExpandedPages++
				if done {
					tick.ExpandedTasks++
					return nil
				}
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("expand %s: %w", item.taskID, err))
		}
		if !processed {
			tick.Skipped++
		}
	}
	return errors.Join(errs...)
}

// stepDispatch advances at most one authorized attempt per running task. A task
// whose reservation already exists but was never accepted (a crash between
// Reserve and AcceptOperation) is resumed from its immutable execution row; that
// is the same attempt, not a retry.
func (w *Worker) stepDispatch(ctx context.Context, tick *WorkerTick) error {
	rows, err := w.store.Pool.Query(ctx, `
		SELECT workspace_id,id
		  FROM core.task
		 WHERE state='RUNNING' AND mode='live' AND import_done AND baseline_complete
		   AND orchestration_mode='legacy'
		   AND deadline_at > clock_timestamp()
		 ORDER BY created_at,workspace_id,id
		 LIMIT $1`, w.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("dispatch: select running tasks: %w", err)
	}
	type ref struct{ workspace, taskID string }
	var refs []ref
	for rows.Next() {
		var item ref
		if err := rows.Scan(&item.workspace, &item.taskID); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var errs []error
	for _, item := range refs {
		processed, err := w.withTaskLease(ctx, item.workspace, item.taskID, func(lease *taskLease) error {
			return w.dispatchTask(ctx, lease, item.workspace, item.taskID, tick)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("dispatch %s: %w", item.taskID, err))
		}
		if !processed {
			tick.Skipped++
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) dispatchTask(ctx context.Context, lease *taskLease, workspace, taskID string, tick *WorkerTick) error {
	if err := lease.valid(ctx); err != nil {
		return err
	}
	res, resumed, err := w.pendingCreatedReservation(ctx, workspace, taskID)
	if err != nil {
		return err
	}
	if !resumed {
		res, err = w.store.Reserve(ctx, workspace, taskID, "")
		if err != nil {
			if w.nothingToReserve(err) {
				tick.Skipped++
				return nil
			}
			return err
		}
		tick.Reservations++
	} else {
		tick.ResumedReservations++
	}
	rt := runtimeActorFor(res)
	if err := lease.valid(ctx); err != nil {
		return err
	}
	if _, err := w.store.AcceptOperation(ctx, rt, res); err != nil {
		return fmt.Errorf("accept operation %s: %w", res.OperationID, err)
	}
	if err := lease.valid(ctx); err != nil {
		return err
	}
	if _, err := w.store.IssuePermit(ctx, rt, res.OperationID, w.cfg.PermitTTL); err != nil {
		return fmt.Errorf("issue permit %s: %w", res.OperationID, err)
	}
	if err := lease.valid(ctx); err != nil {
		return err
	}
	if _, err := w.store.Dispatch(ctx, rt, res.OperationID, w.identityCheck, w.adapter); err != nil {
		// Dispatch persists the authoritative outcome before returning. An
		// adapter error is recorded as an unknown effect and is never retried
		// by the worker.
		tick.Dispatched++
		return fmt.Errorf("dispatch operation %s: %w", res.OperationID, err)
	}
	tick.Dispatched++
	return nil
}

// pendingCreatedReservation reconstructs the canonical reservation of a
// persisted but not yet accepted attempt. Only state CREATED is resumed: every
// later state is owned by the bridge fencing rules or by crash recovery.
func (w *Worker) pendingCreatedReservation(ctx context.Context, workspace, taskID string) (Reservation, bool, error) {
	var operationID string
	err := w.store.Pool.QueryRow(ctx, `
		SELECT operation_id FROM core.execution
		 WHERE workspace_id=$1 AND task_id=$2 AND state='CREATED'
		 ORDER BY attempt_no DESC, created_at DESC, id DESC
		 LIMIT 1`, workspace, taskID).Scan(&operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, err
	}
	var res Reservation
	err = w.store.tx(ctx, func(tx pgx.Tx) error {
		e, err := w.store.loadExecutionByOperation(ctx, tx, workspace, operationID)
		if err != nil {
			return err
		}
		p, err := decodeExecutionPayload(e)
		if err != nil {
			return err
		}
		r, err := reservationFromPayload(p)
		if err != nil {
			return err
		}
		r.Digest = e.RequestDigest
		res = r
		return nil
	})
	if err != nil {
		return Reservation{}, false, err
	}
	return res, true, nil
}

// nothingToReserve reports the scheduler codes that mean "no dispatchable work
// now". They are ordinary idle outcomes, not worker failures.
func (w *Worker) nothingToReserve(err error) bool {
	switch Code(err) {
	case schedCodeNoWork, schedCodeNoCapacity, schedCodeLocalBudget, schedCodeCapabilityUnavail:
		return true
	}
	return false
}

// identityCheck re-reads the persisted writer facts. It is deterministic and
// makes no network call; an unproven capability or a stale session rejects the
// send before the adapter is reached.
func (w *Worker) identityCheck(ctx context.Context, r Reservation) error {
	var ok bool
	err := w.store.Pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			  FROM core.account a
			  JOIN core.runtime_credentials c
			    ON (c.workspace_id,c.runtime_id,c.account_id)=(a.workspace_id,a.runtime_id,a.id)
			 WHERE a.workspace_id=$1 AND a.id=$2 AND a.runtime_id=$3
			   AND a.owner_epoch=$4 AND c.owner_epoch=$4 AND c.active
			   AND a.identity_status='verified'
			   AND a.connection_state='online'
			   AND a.login_state='logged_in'
			   AND a.control_state='active'
			   AND a.risk_state='clear'
			   AND a.request_join_verified)`,
		r.WorkspaceID, r.AccountID, r.RuntimeID, r.OwnerEpoch).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return problem("IDENTITY_CHECK_FAILED")
	}
	return nil
}

func (w *Worker) stepPumpBridgeOutbox(ctx context.Context, tick *WorkerTick) error {
	_, err := w.pumpBridgeOutbox(ctx, tick)
	return err
}

// pumpBridgeOutbox moves bridge receipts into core.inbox for every active
// runtime group of one bounded rotation page. It never acknowledges an
// unparsable or foreign fact.
//
// The pump keeps its own durable cursor, so a receipt belonging to a group past
// the page cap is delivered on a later tick instead of never, and a group whose
// pump failed does not hold the others back: the cursor advances and the group is
// revisited on the next lap with the same pending receipt, which is the same
// event id, so re-application stays idempotent.
func (w *Worker) pumpBridgeOutbox(ctx context.Context, tick *WorkerTick) (int, error) {
	rotation, err := w.runtimeActors(ctx, w.store.Pool, workerCursorBridgeOutbox, w.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("bridge outbox: enumerate runtime actors: %w", err)
	}
	var errs []error
	total := 0
	for _, actor := range rotation.Actors {
		accepted, err := w.store.PumpBridgeOutboxBatch(ctx, actor, w.cfg.BatchSize)
		total += accepted
		tick.BridgeAccepted += accepted
		if err != nil {
			errs = append(errs, fmt.Errorf("bridge outbox %s/%s: %w", actor.WorkspaceID, actor.RuntimeID, err))
		}
	}
	if err := w.saveRuntimeCursor(ctx, w.store.Pool, workerCursorBridgeOutbox, rotation.From, rotation.To); err != nil {
		// The receipts of this window stay pending and are pumped again next tick
		// under the same event ids, so a repeat is idempotent.
		errs = append(errs, fmt.Errorf("bridge outbox: advance rotation cursor: %w", err))
	}
	return total, errors.Join(errs...)
}

func (w *Worker) stepApplyInbox(ctx context.Context, tick *WorkerTick) error {
	_, err := w.applyInbox(ctx, tick)
	return err
}

// applyInbox applies pending inbound observations in arrival order. A fact whose
// runtime credential was superseded stays pending for audit instead of being
// applied under a different owner.
func (w *Worker) applyInbox(ctx context.Context, tick *WorkerTick) (int, error) {
	rows, err := w.store.Pool.Query(ctx, `
		SELECT workspace_id,source_runtime_id,event_id
		  FROM core.inbox
		 WHERE apply_state='pending'
		 ORDER BY received_at,event_id
		 LIMIT $1`, w.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("inbox: select pending: %w", err)
	}
	type ref struct{ workspace, runtime, event string }
	var refs []ref
	for rows.Next() {
		var item ref
		if err := rows.Scan(&item.workspace, &item.runtime, &item.event); err != nil {
			rows.Close()
			return 0, err
		}
		refs = append(refs, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var errs []error
	applied := 0
	for _, item := range refs {
		if err := w.store.ApplyObservation(ctx, item.workspace, item.runtime, item.event); err != nil {
			if Code(err) == taskCodeNotFound {
				continue
			}
			errs = append(errs, fmt.Errorf("inbox %s: %w", item.event, err))
			continue
		}
		applied++
		tick.InboxApplied++
	}
	return applied, errors.Join(errs...)
}

func (w *Worker) stepDeliverOutbox(ctx context.Context, tick *WorkerTick) error {
	_, err := w.deliverOutbox(ctx, tick)
	return err
}

func (w *Worker) deliverOutbox(ctx context.Context, tick *WorkerTick) (int, error) {
	sink := func(callCtx context.Context, id string, payload json.RawMessage) error {
		return w.cfg.OutboxSink(callCtx, id, []byte(payload))
	}
	delivered, err := w.store.DeliverOutbox(ctx, w.cfg.WorkerID, w.cfg.BatchSize, sink)
	tick.OutboxDelivered += delivered
	if err != nil {
		return delivered, fmt.Errorf("outbox deliver: %w", err)
	}
	return delivered, nil
}

// stepReconcile writes the existing 24h read-only timeout audit for every
// workspace and then acknowledges operator reconciliation requests read-only.
// Neither half retries, releases or reassigns an external effect.
func (w *Worker) stepReconcile(ctx context.Context, tick *WorkerTick) error {
	var errs []error
	workspaces, err := w.workspaces(ctx, w.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("reconcile: enumerate workspaces: %w", err)
	}
	for _, workspace := range workspaces {
		audited, err := w.store.ReconcileTimeouts(ctx, workspace)
		if err != nil {
			errs = append(errs, fmt.Errorf("reconcile %s: %w", workspace, err))
			continue
		}
		tick.ReconcileAudits += audited
	}
	if err := w.consumeManualReconcileRequests(ctx, tick); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (w *Worker) workspaces(ctx context.Context, limit int) ([]string, error) {
	if limit > workerMaxWorkspaces {
		limit = workerMaxWorkspaces
	}
	rows, err := w.store.Pool.Query(ctx, `SELECT id FROM core.workspace ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// consumeManualReconcileRequests acknowledges each unanswered operator request
// exactly once. The acknowledgement is an audit event that records how many
// external effects are still unresolved; it changes no execution, slot, claim or
// retry state.
func (w *Worker) consumeManualReconcileRequests(ctx context.Context, tick *WorkerTick) error {
	rows, err := w.store.Pool.Query(ctx, `
		SELECT e.workspace_id,e.task_id,e.id,e.seq
		  FROM core.task_event e
		 WHERE e.kind=$1
		   AND NOT EXISTS (
		       SELECT 1 FROM core.task_event a
		        WHERE a.workspace_id=e.workspace_id AND a.task_id=e.task_id
		          AND a.kind=$2 AND a.detail->>'request_event_id'=e.id)
		 ORDER BY e.workspace_id,e.task_id,e.seq
		 LIMIT $3`, workerManualReconcileRequestKind, workerManualReconcileAckKind, w.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("manual reconcile: select requests: %w", err)
	}
	type request struct {
		workspace, taskID, eventID string
		seq                        int64
	}
	var requests []request
	for rows.Next() {
		var item request
		if err := rows.Scan(&item.workspace, &item.taskID, &item.eventID, &item.seq); err != nil {
			rows.Close()
			return err
		}
		requests = append(requests, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var errs []error
	for _, item := range requests {
		processed, err := w.withTaskLease(ctx, item.workspace, item.taskID, func(lease *taskLease) error {
			if err := lease.valid(ctx); err != nil {
				return err
			}
			acked, err := w.ackManualReconcile(ctx, item.workspace, item.taskID, item.eventID, item.seq)
			if err != nil {
				return err
			}
			if acked {
				tick.ManualReconcileAcks++
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("manual reconcile %s: %w", item.eventID, err))
		}
		if !processed {
			tick.Skipped++
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) ackManualReconcile(ctx context.Context, workspace, taskID, requestEventID string, requestSeq int64) (bool, error) {
	acked := false
	err := w.store.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, workspace, taskID)
		if err != nil {
			if Code(err) == taskCodeNotFound {
				return nil
			}
			return err
		}
		var requestExists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM core.task_event
			               WHERE workspace_id=$1 AND task_id=$2 AND id=$3 AND kind=$4)`,
			workspace, taskID, requestEventID, workerManualReconcileRequestKind).Scan(&requestExists); err != nil {
			return err
		}
		if !requestExists {
			return nil
		}
		var already bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM core.task_event
			               WHERE workspace_id=$1 AND task_id=$2 AND kind=$3
			                 AND detail->>'request_event_id'=$4)`,
			workspace, taskID, workerManualReconcileAckKind, requestEventID).Scan(&already); err != nil {
			return err
		}
		if already {
			return nil
		}
		var unresolved int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM core.execution x
			 WHERE x.workspace_id=$1 AND x.task_id=$2
			   AND (x.state IN ('DISPATCHING','UNKNOWN')
			        OR COALESCE(x.disposition,'') IN ('submitted','pending','unknown')
			        OR (x.state='OBSERVED' AND x.disposition='confirmed'
			            AND NOT EXISTS (SELECT 1 FROM core.result r
			                            WHERE r.workspace_id=x.workspace_id AND r.task_id=x.task_id
			                              AND r.target_id=x.target_id AND r.valid)))`,
			workspace, taskID).Scan(&unresolved); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, workspace, taskID, workerManualReconcileAckKind, "worker:"+w.cfg.WorkerID, map[string]any{
			"request_event_id":      requestEventID,
			"request_seq":           requestSeq,
			"task_state":            task.State,
			"unresolved_executions": unresolved,
			"read_only":             true,
		}); err != nil {
			return err
		}
		acked = true
		return nil
	})
	return acked, err
}

func (w *Worker) stepDeadlines(ctx context.Context, tick *WorkerTick) error {
	sweep, err := w.store.SweepDeadlines(ctx, "", w.cfg.BatchSize)
	tick.DeadlineScanned += sweep.Scanned
	tick.DeadlineClosed += sweep.Closed
	if err != nil {
		return fmt.Errorf("deadline sweep: %w", err)
	}
	return nil
}
