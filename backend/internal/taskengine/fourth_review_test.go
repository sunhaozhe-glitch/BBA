package taskengine

import (
	"context"
	"testing"
	"time"
)

func TestFourthTerminalLeaveHistory(t *testing.T) {
	for _, terminal := range []string{"CANCELLED", "SUCCEEDED", "PARTIAL", "FAILED"} {
		for _, correlated := range []bool{true, false} {
			name := terminal + "/passive"
			if correlated {
				name = terminal + "/correlated"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				s, rt, r := bridgeFixture(t, []string{"fourth_history"}, []string{"996001"}, 1)
				acceptFixture(t, s, rt, r)
				if _, err := s.IssuePermit(ctx, rt, r.OperationID, 30*time.Second); err != nil {
					t.Fatal(err)
				}
				a := &fakeJoinAdapter{outcome: SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fourth_history_submit"}}
				if _, err := s.Dispatch(ctx, rt, r.OperationID, identityOK, a); err != nil {
					t.Fatal(err)
				}
				obsTestReceiveApply(t, s, r, obsTestObservation(r, "fourth_history_join", 1, "membership.observed", "joined", true, "fourth_history_join_proof", time.Now().UTC()))
				// Goal confirmation now closes a complete task automatically. Set
				// this fixture's terminal state directly to isolate late-leave
				// accounting rather than exercise the control transition.
				obsTestExec(t, s, "UPDATE core.task SET state=$2,version=version+1 WHERE id=$1", r.TaskID, terminal)
				before := fixtureString(t, s, "SELECT state FROM core.subtask WHERE id=$1", r.SubtaskID)
				leave := obsTestObservation(r, "fourth_history_leave", 2, "membership.observed", "not_joined", true, "fourth_history_leave_proof", time.Now().UTC())
				if !correlated {
					leave.OperationID = ""
				}
				// Replays must not double-audit or change the closed counters.
				obsTestReceiveApply(t, s, r, leave)
				obsTestReceiveApply(t, s, r, leave)
				if got := fixtureString(t, s, "SELECT state FROM core.membership WHERE target_id=$1", r.TargetID); got != "not_joined" {
					t.Fatalf("current membership=%s", got)
				}
				if got := fixtureString(t, s, "SELECT apply_state FROM core.inbox WHERE event_id=$1", leave.EventID); got != "applied" {
					t.Fatalf("leave inbox=%s", got)
				}
				if got := fixtureString(t, s, "SELECT state FROM core.task WHERE id=$1", r.TaskID); got != terminal {
					t.Fatalf("closed task=%s want %s", got, terminal)
				}
				if scalarInt(t, s, "SELECT count(*) FROM core.result WHERE valid") != 1 || scalarInt(t, s, "SELECT count(*) FROM core.goal_slot WHERE state='fulfilled'") != 1 {
					t.Fatal("terminal leave rewrote closed result or fulfilled slot")
				}
				if got := fixtureString(t, s, "SELECT state FROM core.subtask WHERE id=$1", r.SubtaskID); got != before {
					t.Fatalf("closed subtask changed %s -> %s", before, got)
				}
				kind := "membership.passive_leave_terminal"
				if correlated {
					kind = "membership.late_observed"
				}
				if scalarInt(t, s, "SELECT count(*) FROM core.task_event WHERE kind=$1", kind) != 1 || scalarInt(t, s, "SELECT count(*) FROM core.task_event WHERE kind='result.invalidated'") != 0 || a.count() != 1 {
					t.Fatal("late audit duplicated, result invalidated, or operation resent")
				}
			})
		}
	}
}

func TestFourthObservationWriteArbitration(t *testing.T) {
	for _, mode := range []string{"same_digest", "different_digest", "source_position"} {
		t.Run(mode, func(t *testing.T) {
			s, rt, r := bridgeFixture(t, []string{"fourth_arbitrate"}, []string{"996101"}, 1)
			ctx := context.Background()
			first := obsTestObservation(r, "fourth_arbitrate_event", 1, "membership.observed", "joined", true, "fourth_arbitrate_join", time.Now().UTC())
			first.OperationID = ""
			second := first
			switch mode {
			case "different_digest":
				second.SourceSequence = 2
				second.State = "not_joined"
				second.EvidenceRef = "fourth_arbitrate_leave"
			case "source_position":
				second.EventID = "fourth_arbitrate_collision"
			}
			hold, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(ctx)
			if _, err := hold.Exec(ctx, "LOCK TABLE core.inbox IN SHARE MODE"); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 2)
			for _, obs := range []Observation{first, second} {
				go func(o Observation) { done <- s.ReceiveObservation(ctx, rt, o) }(obs)
			}
			remediationWait(t, "both observation inserts reached write barrier", func() bool {
				return scalarInt(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%INSERT INTO core.inbox%'`) == 2
			})
			if err := hold.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			accepted, conflicts := 0, 0
			for range 2 {
				err := <-done
				if err == nil {
					accepted++
				} else if Code(err) == "OBSERVATION_DIGEST_CONFLICT" {
					conflicts++
				} else {
					t.Errorf("unclassified observation collision: %v", err)
				}
			}
			wantAccepted, wantConflicts := 1, 1
			if mode == "same_digest" {
				wantAccepted, wantConflicts = 2, 0
			}
			t.Logf("accepted=%d conflicts=%d rows=%d", accepted, conflicts, scalarInt(t, s, "SELECT count(*) FROM core.inbox"))
			if accepted != wantAccepted || conflicts != wantConflicts || scalarInt(t, s, "SELECT count(*) FROM core.inbox") != 1 {
				t.Fatalf("write arbitration accepted=%d conflicts=%d want %d/%d", accepted, conflicts, wantAccepted, wantConflicts)
			}
			if mode == "same_digest" {
				return
			}
			stored := fixtureString(t, s, "SELECT digest FROM core.inbox")
			winner := first
			n, err := normalizeObservation(first)
			if err != nil {
				t.Fatal(err)
			}
			dg, err := digest(n)
			if err != nil {
				t.Fatal(err)
			}
			loser := second
			if dg != stored {
				winner, loser = second, first
			}
			if err := s.ReceiveObservation(ctx, rt, winner); err != nil {
				t.Fatalf("winner replay: %v", err)
			}
			if err := s.ReceiveObservation(ctx, rt, loser); Code(err) != "OBSERVATION_DIGEST_CONFLICT" {
				t.Fatalf("loser replay: %v", err)
			}
			// Read through another pool checkout after both methods returned:
			// a rejected request must leave durable evidence, not rolled-back audit.
			if scalarInt(t, s, `SELECT count(*) FROM core.observation_conflict WHERE incoming_event_id=$1 AND stored_event_id=$2 AND stored_digest=$3 AND incoming_payload->>'EventID'=$1 AND stored_payload->>'EventID'=$2`, loser.EventID, winner.EventID, stored) != 2 {
				t.Fatal("observation conflict audit missing or disconnected from winning evidence")
			}
			rejectsSQL(t, s, "DELETE FROM core.observation_conflict")
			rejectsSQL(t, s, "UPDATE core.observation_conflict SET conflict_kind='event_digest'")
		})
	}
}
