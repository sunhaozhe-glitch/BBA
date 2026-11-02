package taskengine

// One proposal consumer, one task coordinator, bounded admitted work slots.
// Planner never executes; all effects retain the existing bridge gates.
import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"time"
)

const proposalWorkLease = 30 * time.Second

type proposalLeaseContextKey struct{}

func (w *Worker) stepProposal(ctx context.Context, tick *WorkerTick) error {
	if !w.cfg.ProposalExecution {
		return nil
	}
	var errs []error
	swept, err := w.store.SweepExpiredWorkItemClaims(ctx, w.cfg.BatchSize)
	tick.ProposalSwept += swept
	if err != nil {
		errs = append(errs, fmt.Errorf("proposal sweep: %w", err))
	}
	projected, err := w.store.ReconcileWorkItemStates(ctx, w.cfg.BatchSize)
	tick.ProposalProjected += projected
	if err != nil {
		errs = append(errs, fmt.Errorf("proposal projection: %w", err))
	}
	rows, err := w.store.Pool.Query(ctx, `SELECT t.workspace_id,t.id,COALESCE(t.work_slots,1)
 FROM core.task t WHERE t.orchestration_mode='proposal' AND t.state='RUNNING' AND t.deadline_at>clock_timestamp()
 AND EXISTS(SELECT 1 FROM core.work_item wi LEFT JOIN core.execution e ON (e.workspace_id,e.id)=(wi.workspace_id,wi.execution_id)
 WHERE wi.workspace_id=t.workspace_id AND wi.task_id=t.id AND (wi.state='READY' OR (wi.state='WAITING_RESULT' AND e.state IN ('CREATED','ACCEPTED'))))
 ORDER BY t.created_at,t.id LIMIT $1`, w.cfg.BatchSize)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	type ref struct {
		ws, id string
		slots  int
	}
	var tasks []ref
	for rows.Next() {
		var r ref
		if err = rows.Scan(&r.ws, &r.id, &r.slots); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	type result struct {
		tick WorkerTick
		err  error
	}
	results := make(chan result, len(tasks))
	for _, r := range tasks {
		go func(r ref) {
			var local WorkerTick
			_, e := w.withTaskLease(ctx, r.ws, r.id, func(l *taskLease) error { return w.coordinateProposal(ctx, l, r.ws, r.id, r.slots, &local) })
			results <- result{local, e}
		}(r)
	}
	for range tasks {
		r := <-results
		mergeProposalTick(tick, r.tick)
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	return errors.Join(errs...)
}
func mergeProposalTick(dst *WorkerTick, src WorkerTick) {
	dst.ProposalClaimed += src.ProposalClaimed
	dst.ProposalReserved += src.ProposalReserved
	dst.ProposalDispatched += src.ProposalDispatched
	dst.ProposalResumed += src.ProposalResumed
	dst.Skipped += src.Skipped
}

// The lease outlives every child including outcome persistence. READY stays in DB.
// A child result is the only way a slot is freed; UNKNOWN is never made READY.
func (w *Worker) coordinateProposal(parent context.Context, lease *taskLease, ws, taskID string, slots int, tick *WorkerTick) error {
	if slots < 1 || slots > MaxTaskWorkSlots {
		return problem(taskCodeValidation)
	}
	ctx, cancel := context.WithTimeout(parent, w.cfg.TickTimeout)
	defer cancel()
	watchDone := make(chan struct{})
	watchStop := make(chan struct{})
	var watchErr error
	go func() {
		defer close(watchDone)
		timer := time.NewTicker(25 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-watchStop:
				return
			case <-ctx.Done():
				return
			case <-w.stop:
				cancel()
				return
			case <-timer.C:
				probe, c := context.WithTimeout(ctx, 500*time.Millisecond)
				e := lease.valid(probe)
				c()
				if e != nil {
					watchErr = e
					cancel()
					return
				}
			}
		}
	}()
	type result struct {
		tick WorkerTick
		err  error
	}
	done := make(chan result, slots)
	active, admitted := 0, 0
	seen := []string{}
	var errs []error
	for {
		for active < slots && admitted < w.cfg.BatchSize && ctx.Err() == nil && !w.stopped() {
			if e := lease.valid(ctx); e != nil {
				errs = append(errs, e)
				cancel()
				break
			}
			id, op, e := w.nextPendingProposal(ctx, ws, taskID, seen)
			if e != nil {
				errs = append(errs, e)
				cancel()
				break
			}
			var claim *WorkItemClaim
			if id == "" {
				claim, e = w.store.claimReadyWorkItem(ctx, w.cfg.WorkerID, proposalWorkLease, ws, taskID, seen)
				if e != nil {
					errs = append(errs, e)
					cancel()
					break
				}
				if claim == nil {
					break
				}
				id = claim.WorkItemID
				tick.ProposalClaimed++
			}
			seen = append(seen, id)
			active++
			admitted++
			go func(claim *WorkItemClaim, op string) {
				var local WorkerTick
				var e error
				if claim != nil {
					e = w.processClaimWithLease(ctx, lease, *claim, &local)
				} else {
					var r Reservation
					r, e = w.pendingWorkItemReservation(ctx, ws, op)
					if e == nil {
						local.ProposalResumed++
						e = w.dispatchReservation(ctx, lease, r, &local)
					}
				}
				done <- result{local, e}
			}(claim, op)
		}
		if active == 0 {
			break
		}
		r := <-done
		active--
		mergeProposalTick(tick, r.tick)
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	close(watchStop)
	<-watchDone
	if watchErr != nil {
		errs = append(errs, watchErr)
	}
	if ctx.Err() != nil {
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}
func (w *Worker) nextPendingProposal(ctx context.Context, ws, taskID string, seen []string) (string, string, error) {
	var id, op string
	e := w.store.Pool.QueryRow(ctx, `SELECT wi.id,e.operation_id FROM core.work_item wi
 JOIN core.execution e ON (e.workspace_id,e.id)=(wi.workspace_id,wi.execution_id)
 WHERE wi.workspace_id=$1 AND wi.task_id=$2 AND wi.state='WAITING_RESULT' AND e.state IN ('CREATED','ACCEPTED')
 AND NOT(wi.id=ANY($3::text[])) ORDER BY wi.created_at,wi.id LIMIT 1`, ws, taskID, seen).Scan(&id, &op)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", "", nil
	}
	return id, op, e
}
func (w *Worker) processClaimWithLease(ctx context.Context, lease *taskLease, claim WorkItemClaim, tick *WorkerTick) error {
	if err := lease.valid(ctx); err != nil {
		return err
	}
	r, err := w.store.ReserveWorkItem(ctx, claim)
	if err != nil {
		switch Code(err) {
		case workItemCodeLeaseLost, schedCodeNoCapacity, schedCodeCapabilityUnavail, schedCodeNoWork, schedCodeLocalBudget, taskCodeStateConflict:
			tick.Skipped++
			return nil
		}
		return fmt.Errorf("reserve work %s: %w", claim.WorkItemID, err)
	}
	tick.ProposalReserved++
	return w.dispatchReservation(ctx, lease, r, tick)
}

// Compatibility helper for the historical unique-coordinator diagnostic.
func (w *Worker) processWorkItemClaim(ctx context.Context, claim WorkItemClaim, tick *WorkerTick) error {
	processed, err := w.withTaskLease(ctx, claim.WorkspaceID, claim.TaskID, func(l *taskLease) error { return w.processClaimWithLease(ctx, l, claim, tick) })
	if err != nil {
		return err
	}
	if !processed {
		return w.store.ReleaseWorkItemClaim(ctx, claim, workItemStateReady, "worker_busy")
	}
	return nil
}
func (w *Worker) dispatchReservation(ctx context.Context, lease *taskLease, r Reservation, tick *WorkerTick) error {
	rt := runtimeActorFor(r)
	if err := lease.valid(ctx); err != nil {
		return err
	}
	if _, err := w.store.AcceptOperation(ctx, rt, r); err != nil {
		return fmt.Errorf("accept %s: %w", r.OperationID, err)
	}
	if err := lease.valid(ctx); err != nil {
		return err
	}
	if _, err := w.store.IssuePermit(ctx, rt, r.OperationID, w.cfg.PermitTTL); err != nil {
		return fmt.Errorf("permit %s: %w", r.OperationID, err)
	}
	if err := lease.valid(ctx); err != nil {
		return err
	}
	_, err := w.store.Dispatch(context.WithValue(ctx, proposalLeaseContextKey{}, lease), rt, r.OperationID, w.identityCheck, w.adapter)
	if err != nil {
		// Only an actual persisted outcome may be counted as dispatched. Infrastructure
		// errors before persistence are observable failures, never silently swallowed.
		var state string
		readCtx, c := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer c()
		e := w.store.Pool.QueryRow(readCtx, `SELECT state FROM core.execution WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.ExecutionID).Scan(&state)
		if e != nil {
			return errors.Join(err, e)
		}
		if state != "UNKNOWN" && state != "WAITING_EXTERNAL" && state != "SUCCEEDED" && state != "FAILED" {
			return err
		}
		w.cfg.Logf("proposal dispatch %s persisted state=%s: %v", r.OperationID, state, err)
	}
	tick.ProposalDispatched++
	return nil
}
func (w *Worker) pendingWorkItemReservation(ctx context.Context, workspace, operationID string) (Reservation, error) {
	var res Reservation
	err := w.store.tx(ctx, func(tx pgx.Tx) error {
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
	return res, err
}
