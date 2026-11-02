package main

// P0-18 disposable pg_dump/restore drill.
//
// Stage "seed" builds one isolated fixture database through the real task
// engine and leaves three distinct external states: a request whose receipt
// was lost and is later observed pending, an execution whose receipt was lost
// with no later fact (UNKNOWN), and an operation issued after a permit but
// before the adapter call (DISPATCHING). A separate harness dumps the database with pg_dump, restores
// it into a fresh database, and runs stage "verify" against the restored copy.
//
// Verify first proves the restored bytes (projection, unresolved rows, ledger
// counts, schema objects) equal the source. The seed also persists the S1
// proposal work ledger, the S2 ARTEX graph mappings and an S4 login session so
// the restore covers every current table, with a generic schema+data
// fingerprint over the whole application catalog. It then replays the recovery
// paths a restarted process would run. Every adapter call counter must stay at
// zero: pending and unknown survive the restore, slots/claims stay held, and
// neither recovery nor the scheduler creates a second attempt. The public task
// read API (GET /v1/tasks/{id}) must still report the unresolved counts.
//
// The command refuses every database except the isolated /fixture socket
// fixture. It never reads production credentials and never links QQ.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
)

const (
	stageEnv    = "BBA_P018_STAGE"
	runEnv      = "BBA_P018_RUN"
	evidenceEnv = "BBA_P018_EVIDENCE_DIR"

	workspaceID = "ws_p018"
	principalID = "principal_p018"

	seedEvidenceName   = "seed-evidence.json"
	verifyEvidenceName = "verify-evidence.json"
)

var runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{3,31}$`)

// scenario is one seeded external state. The identifiers are written to
// seed-evidence.json so the verify stage never guesses or re-creates rows.
type scenario struct {
	Name        string `json:"name"`
	TaskID      string `json:"task_id"`
	OperationID string `json:"operation_id"`
	ExecutionID string `json:"execution_id"`
	AccountID   string `json:"account_id"`
	RuntimeID   string `json:"runtime_id"`
	TargetID    string `json:"target_id"`
	GroupID     string `json:"group_id"`
	// Proposal-mode ledger bound to this exact execution. The work item is the
	// durable unit the proposal worker would consume after a restart.
	WorkItemID    string `json:"work_item_id"`
	ProposalID    string `json:"proposal_id"`
	ApplicationID string `json:"application_id"`
	AttemptID     string `json:"attempt_id"`
}

type seedEvidence struct {
	Stage            string         `json:"stage"`
	Outcome          string         `json:"outcome"`
	RunID            string         `json:"run_id"`
	SourceDatabase   string         `json:"source_database"`
	RestoredDatabase string         `json:"restored_database"`
	Scenarios        []scenario     `json:"scenarios"`
	AdapterCalls     map[string]int `json:"adapter_calls_before_dump"`
	// S2 graph/ARTEX mapping evidence and the S4 login session are named so the
	// verify stage and the independent psql proof read the exact seeded rows.
	GraphRootExternal string   `json:"graph_root_external"`
	GraphOutboxID     string   `json:"graph_outbox_id"`
	GraphSnapshotID   string   `json:"graph_snapshot_id"`
	LoginSessionID    string   `json:"login_session_id"`
	LoginEventID      string   `json:"login_event_id"`
	Checks            []string `json:"checks"`
	Snapshot          snapshot `json:"snapshot"`
	Error             string   `json:"error,omitempty"`
}

type verifyEvidence struct {
	Stage                    string   `json:"stage"`
	Outcome                  string   `json:"outcome"`
	RunID                    string   `json:"run_id"`
	SourceDatabase           string   `json:"source_database"`
	RestoredDatabase         string   `json:"restored_database"`
	RestoredMatchesSource    bool     `json:"restored_matches_source"`
	AdapterCallsAfterRestore int      `json:"adapter_calls_after_restore"`
	RecoveredDispatching     int      `json:"recovered_dispatching"`
	RecoverySecondPass       int      `json:"recovery_second_pass"`
	ExpiredPermits           int      `json:"expired_permits"`
	ReconciledTimeouts       int      `json:"reconciled_timeouts"`
	ReconcileSecondPass      int      `json:"reconcile_second_pass"`
	WorkItemsReconciled      int      `json:"work_items_reconciled"`
	WorkItemsSecondPass      int      `json:"work_items_second_pass"`
	WorkItemsClaimable       int      `json:"work_items_claimable"`
	ExpiredWorkItemClaims    int      `json:"expired_work_item_claims"`
	Source                   snapshot `json:"source_before_recovery"`
	Before                   snapshot `json:"before_recovery"`
	After                    snapshot `json:"after_recovery"`
	// MismatchComponents names the compared parts that differed. The full
	// snapshots above and fingerprint-diff.txt in the evidence directory are
	// written before the stage is rejected, so a failed verify is diagnosable
	// without a rerun.
	MismatchComponents []string `json:"mismatch_components,omitempty"`
	Checks                   []string `json:"checks"`
	Error                    string   `json:"error,omitempty"`
}

func main() {
	stage := strings.TrimSpace(os.Getenv(stageEnv))
	runID := strings.TrimSpace(os.Getenv(runEnv))
	evidenceDir := strings.TrimSpace(os.Getenv(evidenceEnv))
	dsn := os.Getenv("BBA_FIXTURE_DSN")

	if stage != "seed" && stage != "verify" {
		fatalf(stage, "BBA_P018_STAGE must be seed or verify")
	}
	if !runIDPattern.MatchString(runID) {
		fatalf(stage, "BBA_P018_RUN must match %s", runIDPattern.String())
	}
	if evidenceDir == "" {
		fatalf(stage, "BBA_P018_EVIDENCE_DIR is required")
	}
	if err := fixtureDSN(dsn); err != nil {
		fatalf(stage, "%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	switch stage {
	case "seed":
		ev := &seedEvidence{Stage: stage, RunID: runID}
		if err := guard(func() error { return runSeed(ctx, dsn, runID, ev) }); err != nil {
			ev.Outcome = "FAIL"
			ev.Error = err.Error()
			_ = writeEvidence(evidenceDir, seedEvidenceName, ev)
			fatalf(stage, "%v", err)
		}
		ev.Outcome = "PASS"
		if err := writeEvidence(evidenceDir, seedEvidenceName, ev); err != nil {
			fatalf(stage, "write evidence: %v", err)
		}
		fmt.Printf("P0-18 seed PASS: source=%s restored_target=%s pending=1 unknown=1 dispatching=1 adapter_calls_before_dump=2\n",
			ev.SourceDatabase, ev.RestoredDatabase)
	case "verify":
		ev := &verifyEvidence{Stage: stage, RunID: runID}
		if err := guard(func() error { return runVerify(ctx, dsn, runID, evidenceDir, ev) }); err != nil {
			ev.Outcome = "FAIL"
			ev.Error = err.Error()
			_ = writeEvidence(evidenceDir, verifyEvidenceName, ev)
			fatalf(stage, "%v", err)
		}
		ev.Outcome = "PASS"
		if err := writeEvidence(evidenceDir, verifyEvidenceName, ev); err != nil {
			fatalf(stage, "write evidence: %v", err)
		}
		fmt.Printf("P0-18 restore PASS: restored=%s adapter_calls_after_restore=0 recovered=%d reconciled=%d new_executions=0 new_permits=0 new_retries=0 new_results=0 unresolved_displayed=yes\n",
			ev.RestoredDatabase, ev.RecoveredDispatching, ev.ReconciledTimeouts)
	}
}

// guard turns an assertion panic into a normal error so the stage still writes
// a FAIL evidence file instead of only a Go stack trace.
func guard(fn func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%v", recovered)
		}
	}()
	return fn()
}

func fatalf(stage, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "P0-18 %s FAIL: %s\n", stage, fmt.Sprintf(format, args...))
	os.Exit(1)
}

func must(err error, what string) {
	if err != nil {
		panic(errors.New(what + ": " + err.Error()))
	}
}

func require(ok bool, format string, args ...any) {
	if !ok {
		panic(errors.New(fmt.Sprintf(format, args...)))
	}
}

// fixtureDSN refuses anything that is not the isolated kernel fixture.
func fixtureDSN(dsn string) error {
	if os.Getenv("BBA_FIXTURE_ONLY") != "1" {
		return errors.New("BBA_FIXTURE_ONLY=1 is required")
	}
	if dsn == "" {
		return errors.New("BBA_FIXTURE_DSN is required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse fixture dsn: %w", err)
	}
	if cfg.User != "bba_fixture" || cfg.Host != "/fixture" || cfg.Database != "bba_fixture_admin" {
		return fmt.Errorf("refusing non-fixture database user=%q host=%q database=%q", cfg.User, cfg.Host, cfg.Database)
	}
	return nil
}

func databaseNames(runID string) (string, string) {
	return "p018_" + runID + "_src", "p018_" + runID + "_dst"
}

func databaseDSN(raw, database string) string {
	u, err := url.Parse(raw)
	must(err, "parse fixture dsn")
	u.Path = "/" + database
	return u.String()
}

func openDatabase(ctx context.Context, raw, database string) *taskengine.Store {
	dsn := databaseDSN(raw, database)
	cfg, err := pgx.ParseConfig(dsn)
	must(err, "parse database dsn")
	require(cfg.User == "bba_fixture" && cfg.Host == "/fixture" && cfg.Database == database,
		"refusing non-fixture database user=%q host=%q database=%q", cfg.User, cfg.Host, cfg.Database)
	store, err := taskengine.Open(ctx, dsn)
	must(err, "open "+database)
	return store
}

func runSeed(ctx context.Context, raw, runID string, ev *seedEvidence) error {
	src, dst := databaseNames(runID)
	ev.SourceDatabase, ev.RestoredDatabase = src, dst

	adminCfg, err := pgx.ParseConfig(raw)
	if err != nil {
		return err
	}
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	for _, database := range []string{src, dst} {
		_, err = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{database}.Sanitize()+` WITH (FORCE)`)
		if err != nil {
			return fmt.Errorf("drop stale %s: %w", database, err)
		}
	}
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{src}.Sanitize()); err != nil {
		return fmt.Errorf("create %s: %w", src, err)
	}

	store := openDatabase(ctx, raw, src)
	defer store.Close()
	must(store.Migrate(ctx), "migrate fixture")

	actor := drillActor()
	must(seedWorkspace(ctx, store), "seed workspace")
	accounts := []seededAccount{
		{ID: "acc_p018_pending", Runtime: "rt_p018_pending", SelfID: "10081", GroupID: "980000081"},
		{ID: "acc_p018_unknown", Runtime: "rt_p018_unknown", SelfID: "10082", GroupID: "980000082"},
		{ID: "acc_p018_dispatching", Runtime: "rt_p018_dispatching", SelfID: "10083", GroupID: "980000083"},
	}
	for _, account := range accounts {
		must(seedAccount(ctx, store, account), "seed account "+account.ID)
	}

	// Pending: the adapter was called once and the bridge later persisted a
	// pending external request. It is a possible effect, not success and not a
	// retryable no-effect.
	pendingTask, err := createReadyTask(ctx, store, actor, accounts[0], "P0-18 pending external", "pending")
	must(err, "create pending task")
	pending := seedPending(ctx, store, accounts[0], pendingTask)

	// Unknown: the adapter was invoked once and the process died before the
	// receipt was persisted. The durable result is UNKNOWN and must never be
	// retried automatically.
	unknownTask, err := createReadyTask(ctx, store, actor, accounts[1], "P0-18 unknown receipt", "unknown")
	must(err, "create unknown task")
	unknown := seedUnknown(ctx, store, accounts[1], unknownTask)

	// Dispatching: a permit was issued, the gateway entered DISPATCHING, and
	// the adapter call had not happened yet. A restart may only quarantine it
	// as UNKNOWN; it may never send.
	dispatchingTask, err := createReadyTask(ctx, store, actor, accounts[2], "P0-18 dispatching before send", "dispatching")
	must(err, "create dispatching task")
	dispatching := seedDispatching(ctx, store, accounts[2], dispatchingTask)

	ev.Scenarios = []scenario{pending, unknown, dispatching}
	ev.AdapterCalls = map[string]int{"pending": 1, "unknown": 1, "dispatching": 0}

	// Age the three attempts past the read-only 24h reconciliation boundary so
	// the verify stage exercises ReconcileTimeouts deterministically.
	must(ageExecutions(ctx, store), "age executions")

	// The durable proposal ledger is bound to the exact executions above, so the
	// drill can prove the proposal worker's no-resend behavior, not just the
	// legacy dispatch path. Graph mappings and the login session cover the S2 and
	// S4 tables; the generic fingerprint covers every other table automatically.
	must(seedProposalLedger(ctx, store, ev.Scenarios), "seed proposal work ledger")
	must(seedGraphArtifacts(ctx, store, ev.Scenarios[0].TaskID, accounts[0], ev), "seed graph mappings")
	must(seedLoginSession(ctx, store, accounts[0], ev), "seed login session")

	snap, err := captureSnapshot(ctx, store, ev.Scenarios)
	must(err, "capture seed snapshot")
	ev.Snapshot = snap

	assertSeedState(ev)
	ev.Checks = append(ev.Checks,
		"pending execution OBSERVED/pending/confirmed_submitted after a lost receipt, slot/claim/send lock retained",
		"unknown execution UNKNOWN/unknown/unknown with slot, claim and send lock",
		"dispatching gateway DISPATCHING with issued permit and no execution delivery",
		"read API reports pending_external_count=1 for pending and unknown_execution_count=1 for unknown",
		"adapter calls before dump: pending=1 unknown=1 dispatching=0",
		"proposal work items WAITING_RESULT bound to the exact unresolved executions, one attempt each",
		"fact/goal graph roots, provenance anchor, nodes, outbox, checkpoints and a bounded planning snapshot persisted",
		"online login session with immutable event persisted for the pending account",
		"generic schema and data fingerprints captured over every application table",
	)
	return nil
}

type seededAccount struct {
	ID      string
	Runtime string
	SelfID  string
	GroupID string
}

func seedWorkspace(ctx context.Context, store *taskengine.Store) error {
	if _, err := store.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES($1)`, workspaceID); err != nil {
		return err
	}
	_, err := store.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id,permissions) VALUES($1,$2,'{}')`, workspaceID, principalID)
	return err
}

func seedAccount(ctx context.Context, store *taskengine.Store, account seededAccount) error {
	_, err := store.Pool.Exec(ctx, `INSERT INTO core.account(
		workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,
		connection_state,login_state,control_state,risk_state,
		request_join_verified,membership_verified,quota_limit,heartbeat_at)
		VALUES($1,$2,$3,$4,$5,'verified','online','logged_in','active','clear',true,true,5,clock_timestamp())`,
		workspaceID, account.ID, account.Runtime, "adapter_"+account.ID, account.SelfID)
	if err != nil {
		return err
	}
	_, err = store.Pool.Exec(ctx, `INSERT INTO core.runtime_credentials(
		workspace_id,runtime_id,account_id,owner_epoch,membership_authority)
		VALUES($1,$2,$3,1,true)`, workspaceID, account.Runtime, account.ID)
	return err
}

func drillActor() taskengine.Actor {
	return taskengine.Actor{
		WorkspaceID: workspaceID,
		PrincipalID: principalID,
		Permissions: map[string]bool{
			"task.create": true, "task.read": true, "task.control": true,
			"task.approve": true, "task.manage_all": true,
		},
	}
}

func runtimeActor(account seededAccount) taskengine.RuntimeActor {
	return taskengine.RuntimeActor{
		WorkspaceID: workspaceID,
		RuntimeID:   account.Runtime,
		OwnerEpoch:  1,
		AccountIDs:  []string{account.ID},
	}
}

func createReadyTask(ctx context.Context, store *taskengine.Store, actor taskengine.Actor, account seededAccount, title, suffix string) (contract.Task, error) {
	approvalID := "ap_p018_" + suffix
	_, err := store.Pool.Exec(ctx, `INSERT INTO core.approval(
		workspace_id,id,principal_id,scope_revision,account_ids,target_ids,max_attempts,expires_at,join_reason,verification_message)
		VALUES($1,$2,$3,1,$4,$5,10,clock_timestamp()+interval '1 day',$6,$7)`,
		workspaceID, approvalID, principalID, []string{account.ID}, []string{account.GroupID}, "fixture reason", "fixture answer")
	if err != nil {
		return contract.Task{}, err
	}
	mode := contract.ExecutionMode("live")
	req := contract.CreateTaskRequest{
		Type: "group.acquire", Title: title, JoinReason: "fixture reason", VerificationMessage: "fixture answer",
		Goal:              contract.Goal{Metric: "new_unique_groups_for_account_pool", Count: 1, StrictTargetCap: true, RequireFinalMembershipCheck: true},
		AllowedAccountIds: []contract.Id{contract.Id(account.ID)},
		CandidateSource: contract.CandidateSource{
			Kind:   "fixed",
			Groups: []contract.FixedCandidate{{GroupId: contract.QQId(account.GroupID), SourceRef: "p018_fixed_import"}},
		},
		ExecutionMode: &mode,
		DeadlineAt:    time.Now().UTC().Add(2 * time.Hour),
		Limits: contract.TaskLimits{
			MaxCandidates: 1, MaxAttemptsPerAction: 1, MaxTotalExternalAttempts: 10, MaxModelAttempts: 0,
		},
		ApprovalRef: nullable.NewNullableWithValue(approvalID),
		Submit:      true,
	}
	task, err := store.CreateTask(ctx, actor, "p018-create-"+suffix, req)
	if err != nil {
		return contract.Task{}, err
	}
	for {
		done, err := store.ExpandFixedPage(ctx, actor, string(task.Id), 100)
		if err != nil {
			return contract.Task{}, err
		}
		if done {
			break
		}
	}
	err = store.CompleteBaseline(ctx, actor, string(task.Id), []taskengine.BaselineSnapshot{{
		AccountID: account.ID, EvidenceRef: "p018_complete_baseline_" + suffix,
		ObservedAt: time.Now().UTC(), Complete: true, GroupIDs: []string{},
	}})
	if err != nil {
		return contract.Task{}, err
	}
	task, err = store.GetTask(ctx, actor, string(task.Id))
	if err != nil {
		return contract.Task{}, err
	}
	if _, err = store.ControlTask(ctx, actor, string(task.Id), "p018-start-"+suffix, contract.TaskControlRequest{
		Action: contract.TaskControlRequestActionStart, ExpectedVersion: task.Version, Reason: "P0-18 restore drill",
	}); err != nil {
		return contract.Task{}, err
	}
	return store.GetTask(ctx, actor, string(task.Id))
}

func reserve(ctx context.Context, store *taskengine.Store, task contract.Task, account seededAccount) taskengine.Reservation {
	res, err := store.Reserve(ctx, workspaceID, string(task.Id), account.ID)
	must(err, "reserve "+account.ID)
	require(res.AccountID == account.ID && res.TargetID != "" && res.OperationID != "" && res.ExecutionID != "",
		"reservation incomplete: %+v", res)
	return res
}

func acceptAndPermit(ctx context.Context, store *taskengine.Store, account seededAccount, res taskengine.Reservation, ttl time.Duration) taskengine.Reservation {
	rt := runtimeActor(account)
	accepted, err := store.AcceptOperation(ctx, rt, res)
	must(err, "accept operation")
	permit, err := store.IssuePermit(ctx, rt, accepted.OperationID, ttl)
	must(err, "issue permit")
	require(permit.State == "issued" && permit.OperationID == accepted.OperationID, "permit not issued: %+v", permit)
	return accepted
}

func seedPending(ctx context.Context, store *taskengine.Store, account seededAccount, task contract.Task) scenario {
	rt := runtimeActor(account)
	res := reserve(ctx, store, task, account)
	res = acceptAndPermit(ctx, store, account, res, 10*time.Second)
	// The adapter call happened but the receipt was lost, so the bridge can
	// only persist UNKNOWN. A later runtime query observes that the external
	// request is still pending; the execution becomes OBSERVED/pending while
	// the gateway receipt stays UNKNOWN and the send lock stays held.
	adapter := &recordingAdapter{err: errors.New("p018 pending: receipt lost after the external request")}
	outcome, err := store.Dispatch(ctx, rt, res.OperationID, identityOK, adapter)
	require(err != nil, "pending dispatch with lost receipt returned nil error")
	require(adapter.calls == 1, "pending adapter calls=%d want 1", adapter.calls)
	require(outcome.Disposition == "unknown" && outcome.EffectCertainty == "unknown",
		"pending dispatch outcome=%+v", outcome)

	observation := taskengine.Observation{
		EventID: "p018_evt_pending", OperationID: res.OperationID, AccountID: account.ID,
		TargetID: res.TargetID, Kind: "operation.observed", State: "pending",
		EvidenceRef: "p018_pending_external_request", SourceEpoch: 1, SourceSequence: 1,
		ObservedAt: time.Now().UTC().Add(time.Second), SnapshotComplete: false,
	}
	must(store.ReceiveObservation(ctx, rt, observation), "receive pending observation")
	must(store.ApplyObservation(ctx, workspaceID, account.Runtime, observation.EventID), "apply pending observation")

	return scenario{
		Name: "pending", TaskID: string(task.Id), OperationID: res.OperationID,
		ExecutionID: res.ExecutionID, AccountID: account.ID, RuntimeID: account.Runtime,
		TargetID: res.TargetID, GroupID: account.GroupID,
	}
}

func seedUnknown(ctx context.Context, store *taskengine.Store, account seededAccount, task contract.Task) scenario {
	rt := runtimeActor(account)
	res := reserve(ctx, store, task, account)
	res = acceptAndPermit(ctx, store, account, res, 10*time.Second)
	adapter := &recordingAdapter{err: errors.New("p018 simulated crash after the external call")}
	outcome, err := store.Dispatch(ctx, rt, res.OperationID, identityOK, adapter)
	require(err != nil, "dispatch with adapter error returned nil error")
	require(adapter.calls == 1, "unknown adapter calls=%d want 1", adapter.calls)
	require(outcome.Disposition == "unknown" && outcome.EffectCertainty == "unknown",
		"unknown dispatch outcome=%+v", outcome)

	return scenario{
		Name: "unknown", TaskID: string(task.Id), OperationID: res.OperationID,
		ExecutionID: res.ExecutionID, AccountID: account.ID, RuntimeID: account.Runtime,
		TargetID: res.TargetID, GroupID: account.GroupID,
	}
}

func seedDispatching(ctx context.Context, store *taskengine.Store, account seededAccount, task contract.Task) scenario {
	res := reserve(ctx, store, task, account)
	res = acceptAndPermit(ctx, store, account, res, 30*time.Second)
	return scenario{
		Name: "dispatching", TaskID: string(task.Id), OperationID: res.OperationID,
		ExecutionID: res.ExecutionID, AccountID: account.ID, RuntimeID: account.Runtime,
		TargetID: res.TargetID, GroupID: account.GroupID,
	}
}

func ageExecutions(ctx context.Context, store *taskengine.Store) error {
	_, err := store.Pool.Exec(ctx, `UPDATE core.execution
		SET started_at=clock_timestamp()-interval '25 hours',
		    observed_at=clock_timestamp()-interval '25 hours'
		WHERE workspace_id=$1`, workspaceID)
	return err
}

func sha256Hex(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// seedProposalLedger writes the durable proposal-mode chain for each scenario:
// applied proposal -> application -> materialization -> work item -> attempt,
// bound to the exact execution that scenario produced. It deliberately does not
// re-run the materialize command; that flow has its own permanent tests. The
// drill needs the persisted rows a restarted process would read.
func seedProposalLedger(ctx context.Context, store *taskengine.Store, scenarios []scenario) error {
	for i := range scenarios {
		sc := &scenarios[i]
		var version, scopeRevision, authRevision int
		var approvalID string
		if err := store.Pool.QueryRow(ctx, `
			UPDATE core.task
			   SET orchestration_mode='proposal',
			       version=version+1,
			       authorization_revision=authorization_revision+1
			 WHERE workspace_id=$1 AND id=$2 AND orchestration_mode='legacy'
			 RETURNING version,scope_revision,authorization_revision,COALESCE(approval_id,'')`,
			workspaceID, sc.TaskID).Scan(&version, &scopeRevision, &authRevision, &approvalID); err != nil {
			return fmt.Errorf("switch %s to proposal mode: %w", sc.TaskID, err)
		}
		proposalID := "prop_p018_" + sc.Name
		applicationID := "app_p018_" + sc.Name
		materializationID := "mat_p018_" + sc.Name
		workItemID := "wi_p018_" + sc.Name
		attemptID := "wia_p018_" + sc.Name
		proposalDigest := sha256Hex("p018-proposal", sc.TaskID, sc.ExecutionID)
		scopeDigest := sha256Hex("p018-scope", sc.TaskID, sc.AccountID, sc.TargetID, sc.ExecutionID)

		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO core.plan_proposal(
			  workspace_id,id,task_id,principal_id,expected_task_version,scope_revision,template,
			  proposal_kind,state,digest,steps,questions,rationale_summary,reason,
			  approval_id,approved_by,approved_at,applied_by,applied_at,expires_at,created_at)
			VALUES($1,$2,$3,$4,$5,$6,'controlled_participation_v1','use_template','applied',$7,
			  '[]'::jsonb,'[]'::jsonb,'P0-18 restore drill proposal','p0-18 fixture',
			  $8,$4,clock_timestamp()-interval '2 minutes',$4,clock_timestamp()-interval '1 minute',
			  clock_timestamp()+interval '1 day',clock_timestamp()-interval '3 minutes')`,
			workspaceID, proposalID, sc.TaskID, principalID, version, scopeRevision, proposalDigest, approvalID); err != nil {
			return fmt.Errorf("seed proposal %s: %w", sc.Name, err)
		}
		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO core.plan_application(
			  workspace_id,id,task_id,proposal_id,proposal_digest,task_version,scope_revision,
			  approval_id,step_no,action,account_id,target_id,group_external_id,receipt_id,applied_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,'request_join',$9,$10,$11,$12,$13)`,
			workspaceID, applicationID, sc.TaskID, proposalID, proposalDigest, version, scopeRevision,
			approvalID, sc.AccountID, sc.TargetID, sc.GroupID, "rcpt_p018_"+sc.Name, principalID); err != nil {
			return fmt.Errorf("seed application %s: %w", sc.Name, err)
		}
		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO core.work_materialization(
			  workspace_id,id,task_id,proposal_id,proposal_digest,approval_id,task_version,
			  authorization_revision,materialized_by,materialized_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp()-interval '45 seconds')`,
			workspaceID, materializationID, sc.TaskID, proposalID, proposalDigest, approvalID, version,
			authRevision, principalID); err != nil {
			return fmt.Errorf("seed materialization %s: %w", sc.Name, err)
		}
		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO core.work_item(
			  workspace_id,id,task_id,materialization_id,application_id,proposal_id,step_id,step_no,
			  kind,account_id,target_id,scope_digest,authorization_version,state,blocked_reason,
			  depends_on,lease_owner,lease_epoch,lease_until,execution_id,attempt_count,last_error)
			VALUES($1,$2,$3,$4,$5,$6,'step-1',1,'request_join',$7,$8,$9,$10,'WAITING_RESULT',NULL,
			  '[]'::jsonb,NULL,0,NULL,$11,1,NULL)`,
			workspaceID, workItemID, sc.TaskID, materializationID, applicationID, proposalID,
			sc.AccountID, sc.TargetID, scopeDigest, authRevision, sc.ExecutionID); err != nil {
			return fmt.Errorf("seed work item %s: %w", sc.Name, err)
		}
		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO core.work_item_attempt(
			  workspace_id,id,work_item_id,attempt_no,lease_epoch,execution_id,outcome,
			  evidence_ref,observation_event_id,error_code,started_at,finished_at)
			VALUES($1,$2,$3,1,1,$4,NULL,NULL,NULL,NULL,clock_timestamp()-interval '2 minutes',NULL)`,
			workspaceID, attemptID, workItemID, sc.ExecutionID); err != nil {
			return fmt.Errorf("seed work item attempt %s: %w", sc.Name, err)
		}
		sc.WorkItemID = workItemID
		sc.ProposalID = proposalID
		sc.ApplicationID = applicationID
		sc.AttemptID = attemptID
	}
	return nil
}

// seedGraphArtifacts persists the S2 dual-graph mapping: both roots, a
// provenance anchor, the external nodes, a sent pull, an ambiguous goal write
// that must stay needs_reconcile, per-partition checkpoints and one bounded
// planning snapshot.
func seedGraphArtifacts(ctx context.Context, store *taskengine.Store, taskID string, account seededAccount, ev *seedEvidence) error {
	factRoot := "p018_fact_" + taskID
	goalRoot := "p018_goal_" + taskID
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO core.task_graph_roots(
		  workspace_id,task_id,graph_kind,provider,external_root,graph_revision,last_sync_at)
		VALUES($1,$2,'fact','artex',$3,1,clock_timestamp()),
		       ($1,$2,'goal','artex',$4,1,clock_timestamp())`,
		workspaceID, taskID, factRoot, goalRoot); err != nil {
		return fmt.Errorf("seed graph roots: %w", err)
	}
	summaryID := sha256Hex("p018-summary", taskID, account.GroupID)
	manifest := sha256Hex("p018-manifest", taskID, account.GroupID)
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO core.task_graph_anchors(
		  workspace_id,task_id,anchor_id,graph_kind,provider,account_id,group_id,summary_id,summary_version,
		  manifest_sha256,projection_sequence,external_node_id,external_provenance,provenance)
		VALUES($1,$2,'anc_p018_fact','fact','artex',$3,$4,$5,1,$6,3,'p018_fact_node','fact','fact'),
		       ($1,$2,'anc_p018_goal','goal','artex',$3,$4,NULL,NULL,NULL,0,NULL,NULL,'inference')`,
		workspaceID, taskID, account.ID, account.GroupID, summaryID, manifest); err != nil {
		return fmt.Errorf("seed graph anchors: %w", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO core.graph_sync_nodes(
		  workspace_id,task_id,graph_kind,provider,external_node_id,external_kind,external_state,
		  origin,local_key,payload,provenance,observed_revision)
		VALUES($1,$2,'fact','artex','p018_fact_node','group','observed','p018','','{}'::jsonb,'fact',1),
		       ($1,$2,'goal','artex','p018_goal_node','goal','active','p018','p018_goal_key',
		        '{"text":"p018 fixture goal"}'::jsonb,'inference',1)`,
		workspaceID, taskID); err != nil {
		return fmt.Errorf("seed graph nodes: %w", err)
	}
	outboxID := "gop_p018_goal_" + taskID
	pullID := "gop_p018_pull_" + taskID
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO core.graph_sync_outbox(
		  workspace_id,task_id,graph_kind,provider,id,operation,idempotency_key,payload,payload_digest,
		  scope,sequence,state,attempts,max_attempts,external_node_id,last_error_code,last_error,next_attempt_at)
		VALUES($1,$2,'goal','artex',$3,'goal.create',$4,'{"title":"p018 fixture goal"}'::jsonb,$5,
		       'p018',2,'needs_reconcile',1,3,NULL,'ARTEX_AMBIGUOUS','external write outcome unknown',clock_timestamp()),
		       ($1,$2,'fact','artex',$6,'graph.pull',$7,'{}'::jsonb,$8,'p018',1,'sent',1,3,NULL,NULL,NULL,clock_timestamp())`,
		workspaceID, taskID, outboxID, "p018-goal:"+taskID, sha256Hex("p018-goal-payload", taskID),
		pullID, "p018-pull:"+taskID, sha256Hex("p018-pull-payload", taskID)); err != nil {
		return fmt.Errorf("seed graph outbox: %w", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO core.graph_sync_checkpoints(
		  workspace_id,task_id,graph_kind,provider,source,scope_key,offset_value)
		VALUES($1,$2,'fact','artex','core.outbox','p018',5),
		       ($1,$2,'goal','artex','group_summary','p018',2)`,
		workspaceID, taskID); err != nil {
		return fmt.Errorf("seed graph checkpoints: %w", err)
	}
	var version, scopeRevision, authRevision int
	if err := store.Pool.QueryRow(ctx, `SELECT version,scope_revision,authorization_revision
		FROM core.task WHERE workspace_id=$1 AND id=$2`, workspaceID, taskID).
		Scan(&version, &scopeRevision, &authRevision); err != nil {
		return fmt.Errorf("read task for planning snapshot: %w", err)
	}
	snapshotID := "snap_p018_" + taskID
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO core.graph_planning_snapshots(
		  workspace_id,task_id,snapshot_id,graph_kind,provider,graph_revision,task_version,scope_revision,
		  authorization_revision,model_version,template_version,summary_refs,anchor_ids,candidates,
		  candidate_count,round,max_rounds,digest,state,expires_at)
		VALUES($1,$2,$3,'fact','artex',1,$4,$5,$6,'p018-model.v1','p018-template.v1',
		       '[]'::jsonb,'[]'::jsonb,'[]'::jsonb,0,1,3,$7,'open',clock_timestamp()+interval '1 hour')`,
		workspaceID, taskID, snapshotID, version, scopeRevision, authRevision,
		sha256Hex("p018-snapshot", taskID)); err != nil {
		return fmt.Errorf("seed planning snapshot: %w", err)
	}
	ev.GraphRootExternal = factRoot
	ev.GraphOutboxID = outboxID
	ev.GraphSnapshotID = snapshotID
	return nil
}

// seedLoginSession persists the S4 online session and its immutable event. The
// session is already projected into core.account, as the real provider path
// requires after an identity readback.
func seedLoginSession(ctx context.Context, store *taskengine.Store, account seededAccount, ev *seedEvidence) error {
	sessionID := "login_p018_" + account.ID
	eventID := "logevt_p018_" + account.ID
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO core.login_session(
		  workspace_id,id,account_id,adapter_instance_id,runtime_id,provider,state,generation,version,
		  qr_expires_at,identity_self_id,identity_nickname,verified_at,binding_checked,projected,
		  failure_reason,expires_at,created_at)
		VALUES($1,$2,$3,$4,$5,'fixture','ONLINE',1,2,clock_timestamp()+interval '5 minutes',
		       $6,'p018 fixture',clock_timestamp(),true,true,NULL,
		       clock_timestamp()+interval '1 hour',clock_timestamp()-interval '1 minute')`,
		workspaceID, sessionID, account.ID, "adapter_"+account.ID, account.Runtime, account.SelfID); err != nil {
		return fmt.Errorf("seed login session: %w", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO core.login_session_event(workspace_id,session_id,seq,id,kind,generation,detail,actor)
		VALUES($1,$2,1,$3,'online',1,'{}'::jsonb,'p018')`,
		workspaceID, sessionID, eventID); err != nil {
		return fmt.Errorf("seed login session event: %w", err)
	}
	ev.LoginSessionID = sessionID
	ev.LoginEventID = eventID
	return nil
}

func identityOK(context.Context, taskengine.Reservation) error { return nil }

// recordingAdapter counts every adapter call. After a restore the count must
// stay at zero; a nonempty outcome would be a bug, so it deliberately fails.
type recordingAdapter struct {
	calls   int
	outcome taskengine.SendOutcome
	err     error
}

func (a *recordingAdapter) RequestJoin(context.Context, taskengine.Reservation) (taskengine.SendOutcome, error) {
	a.calls++
	return a.outcome, a.err
}

func assertSeedState(ev *seedEvidence) {
	require(len(ev.Scenarios) == 3, "seed scenarios=%d want 3", len(ev.Scenarios))
	byName := scenarioMap(ev.Scenarios)
	for _, name := range []string{"pending", "unknown", "dispatching"} {
		_, ok := byName[name]
		require(ok, "missing scenario %s", name)
	}
	pending := rowFor(ev.Snapshot, byName["pending"])
	require(pending.ExecutionState == "OBSERVED" && pending.Disposition == "pending" && pending.Effect == "confirmed_submitted",
		"pending row=%+v", pending)
	require(pending.GatewayState == "UNKNOWN" && pending.PermitState == "consumed", "pending gateway/permit=%+v", pending)
	require(pending.ReservedSlots == 1 && pending.ActiveClaims == 1 && pending.HeldSendLocks == 1, "pending holds=%+v", pending)

	unknown := rowFor(ev.Snapshot, byName["unknown"])
	require(unknown.ExecutionState == "UNKNOWN" && unknown.Disposition == "unknown" && unknown.Effect == "unknown",
		"unknown row=%+v", unknown)
	require(unknown.GatewayState == "UNKNOWN" && unknown.PermitState == "consumed", "unknown gateway/permit=%+v", unknown)
	require(unknown.ReservedSlots == 1 && unknown.ActiveClaims == 1 && unknown.HeldSendLocks == 1, "unknown holds=%+v", unknown)

	dispatching := rowFor(ev.Snapshot, byName["dispatching"])
	require(dispatching.ExecutionState == "ACCEPTED" && dispatching.Effect == "no_effect", "dispatching row=%+v", dispatching)
	require(dispatching.GatewayState == "DISPATCHING" && dispatching.PermitState == "issued", "dispatching gateway/permit=%+v", dispatching)
	require(dispatching.ReservedSlots == 1 && dispatching.ActiveClaims == 1 && dispatching.HeldSendLocks == 1, "dispatching holds=%+v", dispatching)

	projections := projectionMap(ev.Snapshot.Projections)
	pendingTask := projections[byName["pending"].TaskID]
	require(pendingTask.PendingExternalCount == 1 && pendingTask.UnknownExecutionCount == 0 &&
		pendingTask.ConfirmedUniqueCount == 0 && pendingTask.ReservedTargetCount == 1, "pending projection=%+v", pendingTask)
	unknownTask := projections[byName["unknown"].TaskID]
	require(unknownTask.PendingExternalCount == 0 && unknownTask.UnknownExecutionCount == 1 &&
		unknownTask.ConfirmedUniqueCount == 0 && unknownTask.ReservedTargetCount == 1, "unknown projection=%+v", unknownTask)
	dispatchingTask := projections[byName["dispatching"].TaskID]
	require(dispatchingTask.PendingExternalCount == 0 && dispatchingTask.UnknownExecutionCount == 0 &&
		dispatchingTask.ConfirmedUniqueCount == 0 && dispatchingTask.ReservedTargetCount == 1, "dispatching projection=%+v", dispatchingTask)

	for _, table := range []string{
		"core.work_materialization", "core.work_item", "core.work_item_attempt",
		"core.task_graph_roots", "core.task_graph_anchors", "core.graph_sync_nodes",
		"core.graph_sync_outbox", "core.graph_sync_checkpoints", "core.graph_planning_snapshots",
		"core.login_session", "core.login_session_event",
	} {
		requireTable(ev.Snapshot, table)
	}
	require(len(ev.Snapshot.WorkItems) == 3, "proposal work items=%d want 3", len(ev.Snapshot.WorkItems))
	for _, item := range ev.Snapshot.WorkItems {
		require(item.State == "WAITING_RESULT", "work item %s state=%s want WAITING_RESULT", item.WorkItemID, item.State)
		require(item.Attempts == 1 && item.AttemptCount == 1,
			"work item %s attempts=%d attempt_count=%d want 1", item.WorkItemID, item.Attempts, item.AttemptCount)
		require(item.ExecutionID != "" && item.ProposalID != "" && item.ApplicationID != "",
			"work item %s is not bound to its causal chain: %+v", item.WorkItemID, item)
	}
	require(ev.GraphRootExternal != "" && ev.GraphOutboxID != "" && ev.GraphSnapshotID != "",
		"graph evidence ids incomplete: root=%q outbox=%q snapshot=%q",
		ev.GraphRootExternal, ev.GraphOutboxID, ev.GraphSnapshotID)
	require(ev.LoginSessionID != "" && ev.LoginEventID != "",
		"login evidence ids incomplete: session=%q event=%q", ev.LoginSessionID, ev.LoginEventID)
	require(ev.Snapshot.SchemaFingerprint != "" && ev.Snapshot.DataFingerprint != "",
		"generic schema/data fingerprints are missing")
}

func requireTable(snap snapshot, name string) {
	for _, table := range snap.Tables {
		if table.Name == name {
			return
		}
	}
	panic(errors.New("seeded table missing from the schema snapshot: " + name))
}

func runVerify(ctx context.Context, raw, runID string, evidenceDir string, ev *verifyEvidence) error {
	src, dst := databaseNames(runID)
	ev.SourceDatabase, ev.RestoredDatabase = src, dst

	seed, err := readSeedEvidence(evidenceDir, runID, src, dst)
	if err != nil {
		return err
	}
	require(len(seed.Scenarios) == 3, "seed scenarios=%d want 3", len(seed.Scenarios))

	source := openDatabase(ctx, raw, src)
	defer source.Close()
	restored := openDatabase(ctx, raw, dst)
	defer restored.Close()

	sourceSnap, err := captureSnapshot(ctx, source, seed.Scenarios)
	if err != nil {
		return err
	}
	before, err := captureSnapshot(ctx, restored, seed.Scenarios)
	if err != nil {
		return err
	}
	ev.Source = sourceSnap
	ev.Before = before
	if !snapshotsEqual(sourceSnap, before) {
		ev.MismatchComponents = snapshotDifferences(sourceSnap, before)
		_ = writeFingerprintDiff(evidenceDir, sourceSnap, before)
		return fmt.Errorf("restored database does not match the dumped source before recovery (components: %s); see fingerprint-diff.txt",
			strings.Join(ev.MismatchComponents, ","))
	}
	ev.RestoredMatchesSource = true
	ev.Checks = append(ev.Checks,
		"pg_dump/pg_restore reproduced schema objects, projections, unresolved rows and ledger counts")

	// The public read API must display the unresolved work before recovery.
	assertProjections(before, seed.Scenarios, false)
	ev.Checks = append(ev.Checks, "read API before recovery shows pending=1 unknown=1 dispatching-not-yet-unknown")

	byName := scenarioMap(seed.Scenarios)
	dispatching := byName["dispatching"]
	adapters := map[string]*recordingAdapter{
		"pending":     {err: errors.New("p018: adapter must not be called after restore")},
		"unknown":     {err: errors.New("p018: adapter must not be called after restore")},
		"dispatching": {err: errors.New("p018: adapter must not be called after restore")},
	}

	// Recovery order matters and mirrors a restarted runtime: quarantine the
	// durable DISPATCHING operation before any expiry handling can classify a
	// never-consumed permit. Recovery itself must not send.
	recovered, err := restored.RecoverDispatching(ctx, runtimeActorFor(dispatching))
	must(err, "recover dispatching")
	require(recovered == 1, "recovered=%d want 1", recovered)
	ev.RecoveredDispatching = recovered
	second, err := restored.RecoverDispatching(ctx, runtimeActorFor(dispatching))
	must(err, "second recover dispatching")
	require(second == 0, "second recovery=%d want 0", second)
	ev.RecoverySecondPass = second
	for _, name := range []string{"pending", "unknown"} {
		other, err := restored.RecoverDispatching(ctx, runtimeActorFor(byName[name]))
		must(err, "recover "+name)
		require(other == 0, "%s recovery=%d want 0", name, other)
	}

	expired := 0
	for _, sc := range seed.Scenarios {
		n, err := restored.ExpirePermits(ctx, runtimeActorFor(sc))
		must(err, "expire permits "+sc.Name)
		expired += n
	}
	require(expired == 0, "expire permits=%d want 0 (an unresolved effect is never retired)", expired)
	ev.ExpiredPermits = expired

	// The public read API must show the proposal work ledger before recovery.
	assertWorkItems(before, seed.Scenarios, map[string]string{
		"pending": "WAITING_RESULT", "unknown": "WAITING_RESULT", "dispatching": "WAITING_RESULT",
	})

	// The proposal worker's bounded recovery: RecoverDispatching already
	// projected the quarantined DISPATCHING execution, so this pass sees the
	// unknown execution plus the still-observed pending one. The pending item
	// stays WAITING_RESULT; nothing may become claimable and no attempt may be
	// created.
	projectedWork, err := restored.ReconcileWorkItemStates(ctx, 10)
	must(err, "reconcile work items")
	require(projectedWork == 2, "work items reconciled=%d want 2 (UNKNOWN plus still-observed pending)", projectedWork)
	ev.WorkItemsReconciled = projectedWork
	projectedWorkAgain, err := restored.ReconcileWorkItemStates(ctx, 10)
	must(err, "second reconcile work items")
	require(projectedWorkAgain == 1, "second work item reconcile=%d want 1 (the pending item stays observed)", projectedWorkAgain)
	ev.WorkItemsSecondPass = projectedWorkAgain
	claim, err := restored.ClaimReadyWorkItem(ctx, "p018-verify", 30*time.Second)
	must(err, "claim ready work item")
	require(claim == nil, "work item became claimable after restore: %+v", claim)
	ev.WorkItemsClaimable = 0
	sweptItems, err := restored.SweepExpiredWorkItemClaims(ctx, 10)
	must(err, "sweep work item claims")
	require(sweptItems == 0, "expired work item claims swept=%d want 0", sweptItems)
	ev.ExpiredWorkItemClaims = sweptItems

	reconciled, err := restored.ReconcileTimeouts(ctx, workspaceID)
	must(err, "reconcile timeouts")
	require(reconciled == 3, "reconciled=%d want 3", reconciled)
	ev.ReconciledTimeouts = reconciled
	again, err := restored.ReconcileTimeouts(ctx, workspaceID)
	must(err, "second reconcile timeouts")
	require(again == 0, "second reconcile=%d want 0", again)
	ev.ReconcileSecondPass = again

	// A restarted worker that tries to dispatch every unresolved operation
	// gets the durable outcome and never reaches the adapter.
	for _, sc := range seed.Scenarios {
		outcome, err := restored.Dispatch(ctx, runtimeActorFor(sc), sc.OperationID, identityOK, adapters[sc.Name])
		require(err == nil, "dispatch %s after restore: %v", sc.Name, err)
		require(outcome.EffectCertainty != "confirmed", "dispatch %s returned confirmed=%+v", sc.Name, outcome)
	}
	totalAdapterCalls := 0
	for _, a := range adapters {
		totalAdapterCalls += a.calls
	}
	require(totalAdapterCalls == 0, "adapter calls after restore=%d want 0", totalAdapterCalls)
	ev.AdapterCallsAfterRestore = totalAdapterCalls

	// Neither an explicit retry nor a scheduler reservation may create work.
	for _, sc := range seed.Scenarios {
		err := restored.ScheduleRetry(ctx, workspaceID, sc.ExecutionID, "same_account", time.Now().UTC().Add(time.Minute))
		require(taskengine.Code(err) == "EXTERNAL_OUTCOME_UNKNOWN",
			"ScheduleRetry %s code=%q want EXTERNAL_OUTCOME_UNKNOWN", sc.Name, taskengine.Code(err))
	}
	for _, sc := range seed.Scenarios {
		_, err := restored.Reserve(ctx, workspaceID, sc.TaskID, sc.AccountID)
		code := taskengine.Code(err)
		require(code == "NO_WORK" || code == "NO_CAPACITY",
			"Reserve %s code=%q err=%v", sc.Name, code, err)
	}
	// Re-issuing a permit returns the already persisted permit and never mints
	// a second send authorization. The database row remains 'issued'; the
	// returned projection may say 'expired' if the dump/restore took longer
	// than the original short TTL.
	existingPermit, err := restored.IssuePermit(ctx, runtimeActorFor(dispatching), dispatching.OperationID, 10*time.Second)
	must(err, "re-issue permit after restore")
	require(existingPermit.ID != "" && (existingPermit.State == "issued" || existingPermit.State == "expired"),
		"re-issued permit=%+v want the persisted permit", existingPermit)

	after, err := captureSnapshot(ctx, restored, seed.Scenarios)
	if err != nil {
		return err
	}
	ev.After = after
	assertAfterRecovery(before, after, seed.Scenarios)
	assertProjections(after, seed.Scenarios, true)
	ev.Checks = append(ev.Checks,
		"RecoverDispatching quarantined the DISPATCHING operation once and only once",
		"ExpirePermits retired nothing: unresolved effects keep their slot and claim",
		"ReconcileTimeouts wrote only deduplicated manual reconciliation audit events",
		"Dispatch and IssuePermit after restore made zero adapter calls and minted no new permit",
		"ScheduleRetry and Reserve created no retry, execution, permit, result or claim",
		"read API after recovery shows pending_external_count=1 and unknown_execution_count=2 with reserved targets retained",
	)
	return nil
}

func runtimeActorFor(sc scenario) taskengine.RuntimeActor {
	return taskengine.RuntimeActor{
		WorkspaceID: workspaceID, RuntimeID: sc.RuntimeID, OwnerEpoch: 1, AccountIDs: []string{sc.AccountID},
	}
}

func assertProjections(snap snapshot, scenarios []scenario, afterRecovery bool) {
	byName := scenarioMap(scenarios)
	projections := projectionMap(snap.Projections)
	pending := projections[byName["pending"].TaskID]
	unknown := projections[byName["unknown"].TaskID]
	dispatching := projections[byName["dispatching"].TaskID]
	require(pending.ConfirmedUniqueCount == 0 && pending.ReservedTargetCount == 1 && pending.PendingExternalCount == 1,
		"pending projection=%+v", pending)
	require(pending.UnknownExecutionCount == 0, "pending unknown count=%d want 0", pending.UnknownExecutionCount)
	require(unknown.ConfirmedUniqueCount == 0 && unknown.ReservedTargetCount == 1 && unknown.PendingExternalCount == 0,
		"unknown projection=%+v", unknown)
	require(unknown.UnknownExecutionCount == 1, "unknown count=%d want 1", unknown.UnknownExecutionCount)
	require(dispatching.ConfirmedUniqueCount == 0 && dispatching.ReservedTargetCount == 1 && dispatching.PendingExternalCount == 0,
		"dispatching projection=%+v", dispatching)
	if afterRecovery {
		require(dispatching.UnknownExecutionCount == 1, "recovered dispatching unknown count=%d want 1", dispatching.UnknownExecutionCount)
	} else {
		require(dispatching.UnknownExecutionCount == 0, "dispatching unknown count=%d want 0 before recovery", dispatching.UnknownExecutionCount)
	}
	for _, p := range []projection{pending, unknown, dispatching} {
		require(p.State != "SUCCEEDED" && p.State != "PARTIAL",
			"unresolved task reported success state=%s", p.State)
	}
}

func assertAfterRecovery(before, after snapshot, scenarios []scenario) {
	byName := scenarioMap(scenarios)
	pending := rowFor(after, byName["pending"])
	require(pending.ExecutionState == "OBSERVED" && pending.Disposition == "pending" && pending.Effect == "confirmed_submitted",
		"pending after restore=%+v", pending)
	require(pending.GatewayState == "UNKNOWN" && pending.PermitState == "consumed", "pending gateway/permit=%+v", pending)
	require(pending.ReservedSlots == 1 && pending.ActiveClaims == 1 && pending.HeldSendLocks == 1, "pending holds=%+v", pending)

	unknown := rowFor(after, byName["unknown"])
	require(unknown.ExecutionState == "UNKNOWN" && unknown.Disposition == "unknown" && unknown.Effect == "unknown",
		"unknown after restore=%+v", unknown)
	require(unknown.GatewayState == "UNKNOWN" && unknown.PermitState == "consumed", "unknown gateway/permit=%+v", unknown)
	require(unknown.ReservedSlots == 1 && unknown.ActiveClaims == 1 && unknown.HeldSendLocks == 1, "unknown holds=%+v", unknown)

	dispatching := rowFor(after, byName["dispatching"])
	require(dispatching.ExecutionState == "UNKNOWN" && dispatching.Disposition == "unknown" && dispatching.Effect == "unknown",
		"dispatching after recovery=%+v", dispatching)
	require(dispatching.GatewayState == "UNKNOWN" && dispatching.PermitState == "issued",
		"dispatching gateway/permit=%+v", dispatching)
	require(dispatching.ReservedSlots == 1 && dispatching.ActiveClaims == 1 && dispatching.HeldSendLocks == 1,
		"dispatching holds=%+v", dispatching)

	require(after.Totals.Executions == before.Totals.Executions, "executions %d->%d", before.Totals.Executions, after.Totals.Executions)
	require(after.Totals.GatewayOperations == before.Totals.GatewayOperations, "gateway operations changed")
	require(after.Totals.DispatchPermits == before.Totals.DispatchPermits, "dispatch permits changed")
	require(after.Totals.Retries == 0 && after.Totals.Results == 0, "retries=%d results=%d want 0", after.Totals.Retries, after.Totals.Results)
	require(after.Totals.ReservedGoalSlots == 3 && after.Totals.ActiveTargetClaims == 3 && after.Totals.HeldActionLocks == 3,
		"holds after recovery=%+v", after.Totals)
	require(after.Totals.TaskEvents-before.Totals.TaskEvents == 5, "task events delta=%d want 5 (3 reconciliations + 2 work item projections)", after.Totals.TaskEvents-before.Totals.TaskEvents)
	require(after.Totals.Outbox-before.Totals.Outbox == 5, "outbox delta=%d want 5 (3 reconciliations + 2 work item projections)", after.Totals.Outbox-before.Totals.Outbox)
	require(after.Totals.BridgeOutbox-before.Totals.BridgeOutbox == 1, "bridge outbox delta=%d want 1", after.Totals.BridgeOutbox-before.Totals.BridgeOutbox)
	require(after.Totals.AccountUsageConsumed == before.Totals.AccountUsageConsumed &&
		after.Totals.AccountUsageReserved == before.Totals.AccountUsageReserved, "account usage changed after restore")
	require(after.Totals.ManualReconciliations-before.Totals.ManualReconciliations == 3,
		"manual reconciliation events=%d want 3", after.Totals.ManualReconciliations-before.Totals.ManualReconciliations)
	require(after.Totals.RecoveryObservations-before.Totals.RecoveryObservations == 1,
		"recovery observations=%d want 1", after.Totals.RecoveryObservations-before.Totals.RecoveryObservations)

	// Proposal work items follow the durable executions: the pending item stays
	// observed, the two UNKNOWN executions become UNKNOWN, and no attempt was
	// added by the recovery pass.
	assertWorkItems(after, scenarios, map[string]string{
		"pending": "WAITING_RESULT", "unknown": "UNKNOWN", "dispatching": "UNKNOWN",
	})
	for _, item := range after.WorkItems {
		require(item.Attempts == 1 && item.AttemptCount == 1,
			"work item %s attempts after recovery=%d attempt_count=%d want 1", item.WorkItemID, item.Attempts, item.AttemptCount)
	}
}

// assertWorkItems checks the durable work ledger against the scenario's exact
// causal chain: same work item id, same execution, expected state.
func assertWorkItems(snap snapshot, scenarios []scenario, want map[string]string) {
	require(len(snap.WorkItems) == len(scenarios), "work items=%d want %d", len(snap.WorkItems), len(scenarios))
	index := make(map[string]workItemRow, len(snap.WorkItems))
	for _, row := range snap.WorkItems {
		index[row.TaskID] = row
	}
	for _, sc := range scenarios {
		row, ok := index[sc.TaskID]
		require(ok, "missing work item for task %s", sc.TaskID)
		require(row.WorkItemID == sc.WorkItemID, "work item id=%s want %s", row.WorkItemID, sc.WorkItemID)
		require(row.ExecutionID == sc.ExecutionID, "work item execution=%s want %s", row.ExecutionID, sc.ExecutionID)
		require(row.ProposalID == sc.ProposalID && row.ApplicationID == sc.ApplicationID,
			"work item %s is not bound to proposal/application %s/%s", row.WorkItemID, sc.ProposalID, sc.ApplicationID)
		require(row.State == want[sc.Name], "work item %s state=%s want %s", row.WorkItemID, row.State, want[sc.Name])
	}
}

func scenarioMap(scenarios []scenario) map[string]scenario {
	out := make(map[string]scenario, len(scenarios))
	for _, sc := range scenarios {
		out[sc.Name] = sc
	}
	return out
}

func rowFor(snap snapshot, sc scenario) unresolvedRow {
	for _, row := range snap.Unresolved {
		if row.OperationID == sc.OperationID {
			return row
		}
	}
	panic(errors.New("missing unresolved row for operation " + sc.OperationID))
}

func projectionMap(projections []projection) map[string]projection {
	out := make(map[string]projection, len(projections))
	for _, p := range projections {
		out[p.TaskID] = p
	}
	return out
}

func readSeedEvidence(dir, runID, src, dst string) (seedEvidence, error) {
	var seed seedEvidence
	raw, err := os.ReadFile(evidencePath(dir, seedEvidenceName))
	if err != nil {
		return seed, fmt.Errorf("read seed evidence: %w", err)
	}
	if err = jsonUnmarshal(raw, &seed); err != nil {
		return seed, fmt.Errorf("decode seed evidence: %w", err)
	}
	if seed.Outcome != "PASS" {
		return seed, fmt.Errorf("seed outcome=%q", seed.Outcome)
	}
	if seed.RunID != runID || seed.SourceDatabase != src || seed.RestoredDatabase != dst {
		return seed, fmt.Errorf("seed evidence does not belong to run %s", runID)
	}
	return seed, nil
}
