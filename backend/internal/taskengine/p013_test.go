package taskengine

import (
	"context"
	"testing"
	"time"
)

// p013ApplyBridgeEvent pumps the bridge outbox A that Dispatch wrote and
// applies exactly that event. It returns the event id so tests can prove the
// inbox row was persisted and reduced.
func p013ApplyBridgeEvent(t *testing.T, s *Store, rt RuntimeActor, r Reservation) string {
	t.Helper()
	ctx := context.Background()
	n, err := s.PumpBridgeOutbox(ctx, rt)
	if err != nil || n != 1 {
		t.Fatalf("pump bridge outbox n=%d err=%v want 1,nil", n, err)
	}
	var eventID string
	if err := s.Pool.QueryRow(ctx,
		`SELECT event_id FROM core.inbox
		 WHERE workspace_id=$1 AND operation_id=$2 AND apply_state='pending'`,
		r.WorkspaceID, r.OperationID).Scan(&eventID); err != nil {
		t.Fatalf("find bridge inbox event: %v", err)
	}
	if err := s.ApplyObservation(ctx, r.WorkspaceID, r.RuntimeID, eventID); err != nil {
		t.Fatalf("apply bridge observation: %v", err)
	}
	return eventID
}

func p013Execution(t *testing.T, s *Store, r Reservation) (state, disposition, effect, requestRef string) {
	t.Helper()
	if err := s.Pool.QueryRow(context.Background(),
		`SELECT state,COALESCE(disposition,''),effect_certainty,COALESCE(external_request_ref,'')
		   FROM core.execution WHERE workspace_id=$1 AND id=$2`,
		r.WorkspaceID, r.ExecutionID).Scan(&state, &disposition, &effect, &requestRef); err != nil {
		t.Fatalf("read execution: %v", err)
	}
	return
}

// P0-13: a submitted join keeps its adapter request reference and remains an
// external pending fact. Only an authoritative membership observation may
// turn it into a confirmed task result, with separate fact/task projections.
func TestP013SubmittedRequestRefThenConfirmedMembership(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"acc_p013"}, []string{"36374700"}, 1)
	ctx := context.Background()
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	const requestRef = "cleaner_request_ref_p013"
	adapter := &fakeJoinAdapter{outcome: SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted",
		EvidenceRef: "p013_submitted_evidence", RequestRef: requestRef,
	}}
	sent, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, adapter)
	if err != nil {
		t.Fatalf("dispatch submitted: %v", err)
	}
	if sent.RequestRef != requestRef || adapter.count() != 1 {
		t.Fatalf("sent=%+v adapter calls=%d", sent, adapter.count())
	}
	state, disposition, effect, storedRef := p013Execution(t, s, r)
	if state != "OBSERVED" || disposition != "submitted" || effect != "confirmed_submitted" || storedRef != requestRef {
		t.Fatalf("execution=%s/%s/%s ref=%q", state, disposition, effect, storedRef)
	}
	if got := fixtureString(t, s, `SELECT response->>'request_ref' FROM bridge.gateway_operation WHERE workspace_id=$1 AND operation_id=$2`, r.WorkspaceID, r.OperationID); got != requestRef {
		t.Fatalf("gateway request_ref=%q", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("submitted request credited %d results", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='reserved'`, r.WorkspaceID, r.TaskID, r.TargetID); got != 1 {
		t.Fatalf("submitted request lost reserved slot")
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='active'`, r.WorkspaceID, r.TaskID, r.TargetID); got != 1 {
		t.Fatalf("submitted request lost active claim")
	}

	// Submitted is externally unresolved until authoritative membership, and must
	// appear in both detail and list pending projections without becoming joined.
	assertProjection := func(confirmed, reserved, pending int) {
		t.Helper()
		detail, err := s.GetTask(ctx, fixtureActor(), r.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		items, _, _, err := s.ListTasks(ctx, fixtureActor(), "", 100)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range items {
			if string(item.Id) != r.TaskID {
				continue
			}
			found = true
			if item.ConfirmedUniqueCount != confirmed || item.ReservedTargetCount != reserved || item.PendingExternalCount != pending {
				t.Fatalf("submitted list projection: %+v want %d/%d/%d", item, confirmed, reserved, pending)
			}
		}
		if !found {
			t.Fatal("submitted task missing from list")
		}
		if detail.ConfirmedUniqueCount != confirmed || detail.ReservedTargetCount != reserved || detail.PendingExternalCount != pending {
			t.Fatalf("submitted detail projection: %+v want %d/%d/%d", detail, confirmed, reserved, pending)
		}
	}
	assertProjection(0, 1, 1)
	p013ApplyBridgeEvent(t, s, rt, r)
	assertProjection(0, 1, 1)
	state, disposition, effect, storedRef = p013Execution(t, s, r)
	if state != "OBSERVED" || disposition != "submitted" || effect != "confirmed_submitted" || storedRef != requestRef {
		t.Fatalf("applied submitted execution=%s/%s/%s ref=%q", state, disposition, effect, storedRef)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='execution.submitted'`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("submitted audit events=%d", got)
	}

	now := time.Now().UTC()
	approved := obsTestObservation(r, "p013_join_request_approved", 2, "join_request.observed", "approved", true, "p013_approved_evidence", now)
	obsTestReceiveApply(t, s, r, approved)
	state, disposition, effect, storedRef = p013Execution(t, s, r)
	if state != "OBSERVED" || disposition != "submitted" || effect != "confirmed_submitted" || storedRef != requestRef {
		t.Fatalf("approved request=%s/%s/%s ref=%q", state, disposition, effect, storedRef)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("request approval credited %d results", got)
	}

	joined := obsTestObservation(r, "p013_membership_joined", 3, "membership.observed", "joined", true, "p013_joined_evidence", now.Add(time.Second))
	obsTestReceiveApply(t, s, r, joined)
	assertProjection(1, 0, 0)
	state, disposition, effect, storedRef = p013Execution(t, s, r)
	if state != "OBSERVED" || disposition != "confirmed" || effect != "confirmed" || storedRef != requestRef {
		t.Fatalf("confirmed execution=%s/%s/%s ref=%q", state, disposition, effect, storedRef)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("valid results=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND target_id=$3 AND state='fulfilled'`, r.WorkspaceID, r.TaskID, r.TargetID); got != 1 {
		t.Fatalf("fulfilled slot=%d want 1", got)
	}
	if got := fixtureString(t, s, `SELECT state FROM core.membership WHERE workspace_id=$1 AND account_id=$2 AND target_id=$3`, r.WorkspaceID, r.AccountID, r.TargetID); got != "joined" {
		t.Fatalf("fact graph membership=%q want joined", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='result.confirmed'`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("result.confirmed audit events=%d", got)
	}
}

// P0-13: an explicit rejection stops the target, records an audit event, and
// never schedules an automatic retry or invokes the adapter again.
func TestP013RejectedStopsWithoutRetry(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"acc_p013_reject"}, []string{"36374701"}, 1)
	ctx := context.Background()
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeJoinAdapter{outcome: SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "p013_reject_submitted",
	}}
	if _, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, adapter); err != nil {
		t.Fatalf("dispatch submitted: %v", err)
	}
	p013ApplyBridgeEvent(t, s, rt, r)

	rejected := obsTestObservation(r, "p013_join_request_rejected", 2, "join_request.observed", "rejected", true, "p013_rejected_evidence", time.Now().UTC())
	obsTestReceiveApply(t, s, r, rejected)
	state, disposition, effect, _ := p013Execution(t, s, r)
	if state != "OBSERVED" || disposition != "rejected" || effect != "confirmed_rejected" {
		t.Fatalf("rejected execution=%s/%s/%s", state, disposition, effect)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.result WHERE workspace_id=$1 AND task_id=$2 AND valid`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("rejection credited %d results", got)
	}
	if got := fixtureString(t, s, `SELECT state FROM core.subtask WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.SubtaskID); got != "FAILED" {
		t.Fatalf("subtask state=%q want FAILED", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("rejection scheduled %d retries", got)
	}
	if adapter.count() != 1 {
		t.Fatalf("rejection invoked adapter %d times", adapter.count())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind='execution.rejected' AND (detail->>'retry')::boolean=false`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("rejection audit events=%d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2 AND state='free'`, r.WorkspaceID, r.TaskID); got != 1 {
		t.Fatalf("rejection free slots=%d want 1", got)
	}
}

// P0-13: a challenge that arrives after permit issuance is a stop condition.
// It must be rejected before the adapter and must not auto-retry.
func TestP013ChallengeAfterPermitStopsBeforeAdapter(t *testing.T) {
	s, rt, r := bridgeFixture(t, []string{"acc_p013_challenge"}, []string{"36374702"}, 1)
	ctx := context.Background()
	acceptFixture(t, s, rt, r)
	if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE core.account SET risk_state='challenge_required' WHERE workspace_id=$1 AND id=$2`, r.WorkspaceID, r.AccountID); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeJoinAdapter{outcome: SendOutcome{
		Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "p013_challenge_should_not_send",
	}}
	if _, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, adapter); Code(err) != "ACCOUNT_RESTRICTED" {
		t.Fatalf("challenge dispatch error=%v want ACCOUNT_RESTRICTED", err)
	}
	if adapter.count() != 0 {
		t.Fatalf("challenge invoked adapter %d times", adapter.count())
	}
	if got := executionState(t, s, r.OperationID); got != "ACCEPTED" {
		t.Fatalf("challenge moved execution to %s", got)
	}
	if got := permitState(t, s, r.OperationID); got != "issued" {
		t.Fatalf("challenge consumed permit: %s", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id=$1 AND task_id=$2`, r.WorkspaceID, r.TaskID); got != 0 {
		t.Fatalf("challenge scheduled %d retries", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND account_id=$2 AND held`, r.WorkspaceID, r.AccountID); got != 1 {
		t.Fatalf("challenge released account send lock")
	}
}
