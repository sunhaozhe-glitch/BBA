package main

// kernel-smoke exercises the real PostgreSQL kernel using synthetic accounts
// and a recording adapter. It refuses every DSN except the isolated runner's
// fixture admin Unix socket; no QQ transport is linked into this command.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func check(ok bool, message string) {
	if !ok {
		panic(message)
	}
}

type recordingAdapter struct{ calls int }

func (a *recordingAdapter) RequestJoin(context.Context, taskengine.Reservation) (taskengine.SendOutcome, error) {
	a.calls++
	return taskengine.SendOutcome{Disposition: "submitted", EffectCertainty: "confirmed_submitted", EvidenceRef: "fixture_submission", RequestRef: "fixture_request"}, nil
}
func main() {
	ctx := context.Background()
	check(os.Getenv("BBA_FIXTURE_ONLY") == "1", "only tools/kernel.sh smoke is permitted")
	cfg, err := pgx.ParseConfig(os.Getenv("BBA_FIXTURE_DSN"))
	must(err)
	check(cfg.Database == "bba_fixture_admin" && cfg.User == "bba_fixture" && cfg.Host == "/fixture", "refusing non-fixture database")
	admin, err := pgx.ConnectConfig(ctx, cfg)
	must(err)
	defer admin.Close(ctx)
	name := fmt.Sprintf("smoke_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	must(err)
	defer func() {
		_, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		must(err)
	}()
	dbURL, err := url.Parse(os.Getenv("BBA_FIXTURE_DSN"))
	must(err)
	dbURL.Path = "/" + name
	fixtureDSN := dbURL.String()
	s, err := taskengine.Open(ctx, fixtureDSN)
	must(err)
	defer s.Close()
	must(s.Migrate(ctx))
	_, err = s.Pool.Exec(ctx, `INSERT INTO core.workspace VALUES('ws_fixture'); INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','operator'); INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit,heartbeat_at) VALUES('ws_fixture','acc_a','runtime_a','adapter_a','10001','verified','online','logged_in','active','clear',true,true,10,now()); INSERT INTO core.runtime_credentials(workspace_id,runtime_id,account_id,owner_epoch,membership_authority) VALUES('ws_fixture','runtime_a','acc_a',1,true); INSERT INTO core.approval(workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message) VALUES('ws_fixture','approval','operator',1,ARRAY['acc_a'],ARRAY['20001','20002'],10,now()+interval '1 hour','fixture reason','fixture answer')`)
	must(err)
	actor := taskengine.Actor{WorkspaceID: "ws_fixture", PrincipalID: "operator", Permissions: map[string]bool{"task.create": true, "task.read": true, "task.control": true, "task.approve": true}}
	runtime := taskengine.RuntimeActor{WorkspaceID: "ws_fixture", RuntimeID: "runtime_a", OwnerEpoch: 1, AccountIDs: []string{"acc_a"}}
	server := httptest.NewServer(s.Handler(func(r *http.Request) (taskengine.Actor, error) {
		if r.Header.Get("Authorization") != "Bearer fixture-operator" {
			return taskengine.Actor{}, errors.New("unauthenticated")
		}
		return actor, nil
	}))
	defer server.Close()
	mode := contract.ExecutionMode("live")
	req := contract.CreateTaskRequest{Type: "group.acquire", Title: "fixture end-to-end", JoinReason: "fixture reason", VerificationMessage: "fixture answer", Goal: contract.Goal{Metric: "new_unique_groups_for_account_pool", Count: 1, StrictTargetCap: true, RequireFinalMembershipCheck: true}, AllowedAccountIds: []string{"acc_a"}, CandidateSource: contract.CandidateSource{Kind: "fixed", Groups: []contract.FixedCandidate{{GroupId: "20001", SourceRef: "fixture-import"}, {GroupId: "20002", SourceRef: "fixture-import"}}}, ExecutionMode: &mode, DeadlineAt: time.Now().UTC().Add(30 * time.Minute), Limits: contract.TaskLimits{MaxCandidates: 2, MaxAttemptsPerAction: 3, MaxTotalExternalAttempts: 10, MaxModelAttempts: 0}, ApprovalRef: nullable.NewNullableWithValue("approval"), Submit: true}
	encoded, err := json.Marshal(req)
	must(err)
	create := func() contract.Task {
		r, err := http.NewRequest(http.MethodPost, server.URL+"/v1/tasks", bytes.NewReader(encoded))
		must(err)
		r.Header.Set("Authorization", "Bearer fixture-operator")
		r.Header.Set("Idempotency-Key", "fixture-create")
		r.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(r)
		must(err)
		defer response.Body.Close()
		check(response.StatusCode == 201 || response.StatusCode == 200, fmt.Sprintf("create HTTP status %d", response.StatusCode))
		var envelope contract.TaskEnvelope
		must(json.NewDecoder(response.Body).Decode(&envelope))
		return envelope.Data
	}
	task := create()
	same := create()
	check(task.Id == same.Id, "HTTP idempotency created another task")
	done, err := s.ExpandFixedPage(ctx, actor, task.Id, 1)
	must(err)
	check(!done, "first page unexpectedly complete")
	reopened, err := taskengine.Open(ctx, fixtureDSN)
	must(err)
	done, err = reopened.ExpandFixedPage(ctx, actor, task.Id, 1)
	must(err)
	reopened.Close()
	check(done, "checkpoint did not resume after reopen")
	must(s.CompleteBaseline(ctx, actor, task.Id, []taskengine.BaselineSnapshot{{AccountID: "acc_a", EvidenceRef: "fixture-complete-baseline", ObservedAt: time.Now().UTC(), Complete: true, GroupIDs: []string{}}}))
	task, err = s.GetTask(ctx, actor, task.Id)
	must(err)
	_, err = s.ControlTask(ctx, actor, task.Id, "fixture-start", contract.TaskControlRequest{Action: "start", ExpectedVersion: task.Version, Reason: "fixture authorization"})
	must(err)
	reservation, err := s.Reserve(ctx, "ws_fixture", task.Id, "acc_a")
	must(err)
	_, err = s.AcceptOperation(ctx, runtime, reservation)
	must(err)
	_, err = s.AcceptOperation(ctx, runtime, reservation)
	must(err)
	permit, err := s.IssuePermit(ctx, runtime, reservation.OperationID, 10*time.Second)
	must(err)
	samePermit, err := s.IssuePermit(ctx, runtime, reservation.OperationID, 10*time.Second)
	must(err)
	check(permit.ID == samePermit.ID && permit.ExpiresAt.Equal(samePermit.ExpiresAt), "duplicate permit changed authorization")
	adapter := &recordingAdapter{}
	identityProof := func(context.Context, taskengine.Reservation) error { return nil } // synthetic verified identity only
	_, err = s.Dispatch(ctx, runtime, reservation.OperationID, identityProof, adapter)
	must(err)
	_, _ = s.Dispatch(ctx, runtime, reservation.OperationID, identityProof, adapter)
	check(adapter.calls == 1, "duplicate dispatch called adapter twice")
	task, err = s.GetTask(ctx, actor, task.Id)
	must(err)
	fmt.Printf("SUBMISSION COUNTS confirmed=%d reserved=%d pending=%d unknown=%d\n", task.ConfirmedUniqueCount, task.ReservedTargetCount, task.PendingExternalCount, task.UnknownExecutionCount)
	var facts []byte
	must(s.Pool.QueryRow(ctx, `SELECT jsonb_build_object('execution',(SELECT jsonb_agg(to_jsonb(e)) FROM core.execution e WHERE task_id=$1),'holds',(SELECT jsonb_agg(to_jsonb(g)) FROM core.goal_slot g WHERE task_id=$1),'subtasks',(SELECT jsonb_agg(to_jsonb(st)) FROM core.subtask st WHERE task_id=$1),'claims',(SELECT jsonb_agg(to_jsonb(c)) FROM core.target_claim c WHERE task_id=$1))`, task.Id).Scan(&facts))
	fmt.Printf("SUBMISSION FACTS %s\n", facts)
	check(task.ConfirmedUniqueCount == 0 && task.ReservedTargetCount == 1 && task.PendingExternalCount == 1, "submission counted as success or released hold")
	observation := taskengine.Observation{EventID: "fixture_membership", OperationID: reservation.OperationID, AccountID: "acc_a", TargetID: reservation.TargetID, Kind: "membership.observed", State: "joined", EvidenceRef: "fixture_joined", SourceEpoch: 1, SourceSequence: 1, ObservedAt: time.Now().UTC(), SnapshotComplete: true}
	must(s.ReceiveObservation(ctx, runtime, observation))
	must(s.ReceiveObservation(ctx, runtime, observation))
	must(s.ApplyObservation(ctx, "ws_fixture", "runtime_a", observation.EventID))
	must(s.ApplyObservation(ctx, "ws_fixture", "runtime_a", observation.EventID))
	task, err = s.GetTask(ctx, actor, task.Id)
	must(err)
	check(task.ConfirmedUniqueCount == 1 && task.ReservedTargetCount == 0, "trusted membership not applied exactly once")
	delivered := 0
	_, err = s.DeliverOutbox(ctx, "fixture-worker", 100, func(_ context.Context, _ string, _ json.RawMessage) error { delivered++; return nil })
	must(err)
	var results, events, operations, inbox int
	must(s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM core.result),(SELECT count(*) FROM core.task_event),(SELECT count(*) FROM bridge.gateway_operation),(SELECT count(*) FROM core.inbox)`).Scan(&results, &events, &operations, &inbox))
	check(results == 1 && operations == 1 && inbox == 1, "persistent ledgers duplicated")
	fmt.Printf("PASS: HTTP create + internal Store.ControlTask identity=%s; checkpoint resumed; adapter_calls=%d; confirmed=%d; results=%d operations=%d inbox=%d task_events=%d delivered_outbox=%d; PostgreSQL fixture only; real_QQ_calls=0\n", task.Id, adapter.calls, task.ConfirmedUniqueCount, results, operations, inbox, events, delivered)
	smokeChat(ctx, s, runtime)
}
