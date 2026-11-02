package taskengine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func rejectsSQL(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	err := s.tx(context.Background(), func(tx pgx.Tx) error { _, err := tx.Exec(context.Background(), sql, args...); return err })
	var pgerr *pgconn.PgError
	if err == nil || !strings.HasPrefix(sqlState(err, &pgerr), "23") {
		t.Fatalf("constraint allowed invalid mutation or failed outside constraint: %v", err)
	}
}
func sqlState(err error, target **pgconn.PgError) string {
	if errors.As(err, target) {
		return (*target).Code
	}
	return ""
}

func TestSchemaConstraints(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_a", "runtime_a", 20)
	task := createReadyTask(t, s, []string{"acc_a"}, []string{"100", "101"}, 1, 3)
	r := reserveFixture(t, s, task.Id, "")
	t.Run("workspace foreign keys", func(t *testing.T) {
		rejectsSQL(t, s, `INSERT INTO core.task_account(workspace_id,task_id,account_id) VALUES('ws_other',$1,'acc_a')`, task.Id)
	})
	t.Run("target unique", func(t *testing.T) {
		rejectsSQL(t, s, `INSERT INTO core.target(workspace_id,id,external_id) VALUES('ws_fixture','duplicate',$1)`, r.GroupID)
	})
	t.Run("fixed slot range", func(t *testing.T) {
		rejectsSQL(t, s, `INSERT INTO core.goal_slot(workspace_id,task_id,slot_no) VALUES('ws_fixture',$1,2)`, task.Id)
	})
	t.Run("result cannot come from submitted", func(t *testing.T) {
		rejectsSQL(t, s, `INSERT INTO core.result(workspace_id,id,task_id,target_id,account_id,execution_id,source_runtime_id,source_event_id,evidence_ref,confirmed_at) VALUES('ws_fixture','result_fake',$1,$2,$3,$4,'runtime_a','missing_event','fixture',now())`, task.Id, r.TargetID, r.AccountID, r.ExecutionID)
	})
	t.Run("immutable execution payload", func(t *testing.T) {
		rejectsSQL(t, s, `UPDATE core.execution SET account_id='other' WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
	})
	t.Run("closed enum", func(t *testing.T) {
		rejectsSQL(t, s, `UPDATE core.execution SET state='SUCCESS' WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
	})
	t.Run("success requires result", func(t *testing.T) {
		rejectsSQL(t, s, `UPDATE core.goal_slot SET state='fulfilled',fulfilled_at=now() WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id)
	})
	_, err := s.Pool.Exec(context.Background(), `UPDATE core.execution SET state='UNKNOWN',effect_certainty='unknown',disposition='unknown',started_at=now() WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"UNKNOWN", "OBSERVED"} {
		t.Run("uncertainty retains holds "+state, func(t *testing.T) {
			if state == "OBSERVED" {
				_, err := s.Pool.Exec(context.Background(), `UPDATE core.execution SET state='OBSERVED',effect_certainty='confirmed_submitted',disposition='pending' WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
				if err != nil {
					t.Fatal(err)
				}
			}
			rejectsSQL(t, s, `UPDATE core.goal_slot SET state='free',target_id=NULL,reserved_at=NULL WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id)
			rejectsSQL(t, s, `UPDATE core.target_claim SET state='resolved',resolved_at=now() WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id)
			rejectsSQL(t, s, `DELETE FROM core.goal_slot WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id)
			rejectsSQL(t, s, `UPDATE core.execution SET state='OBSERVED',effect_certainty='no_effect',disposition='failed_no_effect' WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
		})
	}
	t.Run("no uncertain retry", func(t *testing.T) {
		rejectsSQL(t, s, `INSERT INTO core.retry(workspace_id,id,task_id,subtask_id,previous_execution_id,strategy,reason_code,due_at,state) VALUES('ws_fixture','bad_retry',$1,$2,$3,'reselect','timeout',now(),'scheduled')`, task.Id, r.SubtaskID, r.ExecutionID)
	})
	t.Run("unknown cannot be rewritten confirmed to release target protection", func(t *testing.T) {
		rejectsSQL(t, s, `UPDATE core.execution SET state='OBSERVED',disposition='confirmed',effect_certainty='confirmed' WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
		rejectsSQL(t, s, `UPDATE core.execution SET state='OBSERVED',disposition='rejected',effect_certainty='confirmed_rejected' WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID)
		rejectsSQL(t, s, `UPDATE core.goal_slot SET state='free',target_id=NULL,reserved_at=NULL WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id)
	})
	t.Run("dispatch permit immutable", func(t *testing.T) {
		_, err := s.Pool.Exec(context.Background(), `INSERT INTO core.dispatch_permit(workspace_id,id,execution_id,operation_id,account_id,target_id,request_digest,scope_revision,owner_epoch,fence_token,expires_at,state) VALUES('ws_fixture','permit_fixture',$1,$2,$3,$4,$5,1,1,1,clock_timestamp()+interval '1 minute','issued')`, r.ExecutionID, r.OperationID, r.AccountID, r.TargetID, r.Digest)
		if err != nil {
			t.Fatal(err)
		}
		rejectsSQL(t, s, `UPDATE core.dispatch_permit SET target_id='other_target' WHERE workspace_id='ws_fixture' AND id='permit_fixture'`)
		rejectsSQL(t, s, `DELETE FROM core.dispatch_permit WHERE workspace_id='ws_fixture' AND id='permit_fixture'`)
	})
	t.Run("execution attempt budget", func(t *testing.T) {
		rejectsSQL(t, s, `INSERT INTO core.execution(workspace_id,id,task_id,subtask_id,operation_id,account_id,target_id,runtime_id,owner_epoch,fence_token,scope_revision,attempt_no,request_digest,request_payload) VALUES('ws_fixture','attempt_over_budget',$1,$2,'op_over_budget',$3,$4,'runtime_a',1,5,1,4,repeat('a',64),'{}'::jsonb)`, task.Id, r.SubtaskID, r.AccountID, r.TargetID)
	})
	t.Run("persistent audit append only", func(t *testing.T) {
		rejectsSQL(t, s, `DELETE FROM core.task_event WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id)
	})
	t.Run("compatible migration reopen", func(t *testing.T) {
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND id=$1`, r.ExecutionID); got != 1 {
			t.Fatalf("migration reopen changed existing execution: %d", got)
		}
	})
	got, err := s.GetTask(context.Background(), fixtureActor(), task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfirmedUniqueCount != 0 || got.ReservedTargetCount != 1 || got.PendingExternalCount != 1 {
		t.Fatalf("pending became success or lost hold: %+v", got)
	}
}
