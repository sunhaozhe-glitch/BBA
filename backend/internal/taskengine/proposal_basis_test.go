package taskengine

// Proposal basis provenance. Generation records which live Summary version and
// which graph anchor it actually consulted, plus the rule that selected them. The
// chain here is the fixture chain the audit asks for: proposal, approval,
// application, worker consumption over the offline test transport, ledger
// flowback, and the next proposal. The BBA ledger stays execution truth; a
// statistic Summary is reference data, not verified group semantic understanding,
// and none of this touches ARTEX.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"bba.local/taskengine/internal/contract"
)

type basisEnvelope struct {
	Data struct {
		ProposalId          string                `json:"proposal_id"`
		TaskId              string                `json:"task_id"`
		State               string                `json:"state"`
		Digest              string                `json:"digest"`
		ExpectedTaskVersion int                   `json:"expected_task_version"`
		ScopeRevision       int                   `json:"scope_revision"`
		ExpiresAt           string                `json:"expires_at"`
		Proposal            contract.PlanProposal `json:"proposal"`
		Basis               []PlanProposalBasis   `json:"basis"`
	} `json:"data"`
}

// basisFields is one persisted provenance row, read straight from the fixture
// database so the API projection can be compared against storage.
type basisFields struct {
	Account  string
	Group    string
	Target   string
	Anchor   any
	Summary  any
	Version  any
	Evidence string
	Manifest any
	Reason   string
}

func basisRow(t *testing.T, s *Store, proposalID string) basisFields {
	t.Helper()
	var row basisFields
	err := s.Pool.QueryRow(context.Background(), `SELECT account_id,group_id,target_id,
			anchor_id,summary_id,summary_version,evidence_kinds::text,source_manifest_sha256,selection_reason
		FROM core.plan_proposal_basis WHERE workspace_id='ws_fixture' AND proposal_id=$1`, proposalID).
		Scan(&row.Account, &row.Group, &row.Target, &row.Anchor, &row.Summary, &row.Version,
			&row.Evidence, &row.Manifest, &row.Reason)
	if err != nil {
		t.Fatalf("read basis row for %s: %v", proposalID, err)
	}
	return row
}

// basisVersion normalizes the integer pgx hands back for an integer column so a
// test can compare it against a literal.
func basisVersion(t *testing.T, value any) int64 {
	t.Helper()
	switch v := value.(type) {
	case nil:
		return -1
	case int32:
		return int64(v)
	case int64:
		return int64(v)
	case int:
		return int64(v)
	default:
		t.Fatalf("unexpected basis version value %T", value)
		return -1
	}
}

// basisLiveRequest builds the same generation request against the task's current
// version and scope, so the test exercises the anti-replanning bound rather than
// the version gate.
func basisLiveRequest(t *testing.T, s *Store, taskID string) contract.PlanProposalRequest {
	t.Helper()
	task, err := s.GetTask(context.Background(), fixtureActor(), taskID)
	if err != nil {
		t.Fatalf("read task for request: %v", err)
	}
	return contract.PlanProposalRequest{
		TaskId:          task.Id,
		ExpectedVersion: task.Version,
		ScopeRevision:   task.ScopeRevision,
		ProposalKind:    contract.PlanProposalRequestProposalKindUseTemplate,
		Reason:          "fixture chain second proposal",
	}
}

func basisDecode(t *testing.T, recorder *httptest.ResponseRecorder) basisEnvelope {
	t.Helper()
	var envelope basisEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode proposal view: %v (body %q)", err, recorder.Body.String())
	}
	return envelope
}

// TestPlanProposalBasisRecordsLiveVersionAndSecondObservation freezes the
// persisted linkage: a newly created proposal records the live Summary version it
// consulted, and a second observation is what the next proposal records. The
// operator read path must return the values storage holds.
func TestPlanProposalBasisRecordsLiveVersionAndSecondObservation(t *testing.T) {
	s := fixtureStore(t)
	task, _ := proposalFixtureTask(t, s)
	runtime := "rt_proposal"
	account := "acc_proposal"
	group := "9000101"
	summaryHandler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(fixtureRuntime(account, runtime)))

	first := summaryFixtureRequest(t, s, account, group)
	firstBody := summaryDecodeIngest(t, chatServe(t, summaryHandler, http.MethodPost, groupSummaryIngestPath, first, chatHeaders("basis-ingest-1")))
	if !firstBody.Created || firstBody.SummaryVersion != 1 {
		t.Fatalf("first observation=%+v want created version 1", firstBody)
	}

	proposalID := proposalCreateFixture(t, s, task, "basis-create-1").ResourceId
	row := basisRow(t, s, proposalID)
	if row.Account != account || row.Group != group {
		t.Fatalf("basis scope=%s/%s want %s/%s", row.Account, row.Group, account, group)
	}
	if row.Target == "" {
		t.Fatal("basis target is empty")
	}
	if row.Summary != first.SummaryID || basisVersion(t, row.Version) != 1 {
		t.Fatalf("basis cites %v@%v want the first live version %s@1", row.Summary, row.Version, first.SummaryID)
	}
	if row.Manifest != first.Summary.Producer.SourceManifestSHA256 {
		t.Fatalf("basis manifest=%v want the first observation manifest %s", row.Manifest, first.Summary.Producer.SourceManifestSHA256)
	}
	if row.Evidence != `["`+summaryEvidenceStatistic+`"]` {
		t.Fatalf("basis evidence=%s want the first observation evidence", row.Evidence)
	}
	if row.Anchor != nil {
		t.Fatalf("basis anchor=%v want null; this fixture task has no graph projection", row.Anchor)
	}
	if row.Reason != planProposalBasisLiveSummary+planProposalBasisNoAnchor {
		t.Fatalf("basis reason=%q want the live-version rule plus the no-anchor note", row.Reason)
	}

	// The operator read path must agree with the stored row.
	proposalHandler := s.ProposalHandler(func(*http.Request) (Actor, error) { return fixtureActor(), nil }, true)
	view := basisDecode(t, ttServe(proposalHandler, http.MethodGet,
		"/v1/tasks/"+task.Id+"/plan-proposals/"+proposalID, nil, nil))
	if view.Data.ProposalId != proposalID || len(view.Data.Basis) != 1 {
		t.Fatalf("view basis=%+v want one recorded row", view.Data.Basis)
	}
	recorded := view.Data.Basis[0]
	if recorded.AccountId != account || recorded.GroupId != group || recorded.TargetId != row.Target ||
		recorded.SummaryId == nil || *recorded.SummaryId != first.SummaryID ||
		recorded.SummaryVersion == nil || *recorded.SummaryVersion != 1 ||
		recorded.SourceManifestSha256 == nil || *recorded.SourceManifestSha256 != first.Summary.Producer.SourceManifestSHA256 ||
		recorded.AnchorId != nil || recorded.SelectionReason != row.Reason ||
		len(recorded.EvidenceKinds) != 1 || recorded.EvidenceKinds[0] != summaryEvidenceStatistic {
		t.Fatalf("view basis row does not match storage: %+v", recorded)
	}

	// A second observation of the same scope is what the next proposal records.
	second := summaryFixtureRequest(t, s, account, group)
	second.SummaryID = summaryHex64Seed("basis-second-observation")
	second.TopicEvidence = []string{summaryEvidenceStatement}
	second.Summary.Topics = []SummaryTopicPayload{{Summary: "second observation topic", MessageRefs: []string{"msg_fixture_1"}}}
	secondBody := summaryDecodeIngest(t, chatServe(t, summaryHandler, http.MethodPost, groupSummaryIngestPath, second, chatHeaders("basis-ingest-2")))
	if !secondBody.Created || secondBody.SummaryVersion != 2 {
		t.Fatalf("second observation=%+v want created version 2", secondBody)
	}

	// A second task over the same account/group is the next proposal opportunity:
	// the first task's proposal is still pending, so its digest deduplicates.
	secondTask := createReadyTask(t, s, []string{account}, []string{group}, 1, 3)
	nextID := proposalCreateFixture(t, s, secondTask, "basis-create-2").ResourceId
	if nextID == proposalID {
		t.Fatal("the next proposal reused the pending proposal id")
	}
	next := basisRow(t, s, nextID)
	if next.Account != account || next.Group != group {
		t.Fatalf("next basis scope=%s/%s want %s/%s", next.Account, next.Group, account, group)
	}
	if next.Summary != second.SummaryID || basisVersion(t, next.Version) != 2 {
		t.Fatalf("next basis cites %v@%v want the second observation %s@2", next.Summary, next.Version, second.SummaryID)
	}
	if next.Manifest != second.Summary.Producer.SourceManifestSHA256 {
		t.Fatalf("next basis manifest=%v want %s", next.Manifest, second.Summary.Producer.SourceManifestSHA256)
	}
	if next.Evidence != `["`+summaryEvidenceStatement+`"]` {
		t.Fatalf("next basis evidence=%s want the second observation evidence", next.Evidence)
	}
	if next.Reason != planProposalBasisLiveSummary+planProposalBasisNoAnchor {
		t.Fatalf("next basis reason=%q want the live-version rule", next.Reason)
	}

	// A replayed create returns the stored proposal and writes no second row.
	proposalCreateFixture(t, s, secondTask, "basis-create-3")
	if got := scalarInt(t, s, `SELECT count(*) FROM core.plan_proposal_basis WHERE workspace_id='ws_fixture' AND proposal_id=$1`, nextID); got != 1 {
		t.Fatalf("basis rows after replay=%d want 1", got)
	}
}

// TestPlanProposalIntentThenIndependentObservationEnablesNextBasis checks the
// anti-replanning evidence boundary. Apply records intent only; the existing
// task scheduler runs independently and never consumes that application.
// A subsequent ledger observation permits a new proposal with its own basis.
func TestPlanProposalIntentThenIndependentObservationEnablesNextBasis(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	account, runtime := "acc_basis_chain", "rt_basis_chain"
	seedAccount(t, s, account, runtime, 3)
	task := createReadyTask(t, s, []string{account}, []string{"9000111", "9000112"}, 2, 3)
	var approvalID string
	if err := s.Pool.QueryRow(ctx, `SELECT approval_id FROM core.task WHERE workspace_id='ws_fixture' AND id=$1`,
		task.Id).Scan(&approvalID); err != nil {
		t.Fatalf("read task approval: %v", err)
	}
	summaryHandler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(fixtureRuntime(account, runtime)))

	adapter := &workerCountAdapter{}
	worker := workerFixture(t, s, adapter, WorkerSteps{Dispatch: true})

	firstScope := summaryFixtureRequest(t, s, account, "9000111")
	firstScopeBody := summaryDecodeIngest(t, chatServe(t, summaryHandler, http.MethodPost, groupSummaryIngestPath, firstScope, chatHeaders("chain-ingest-1")))
	if !firstScopeBody.Created {
		t.Fatalf("chain first scope observation=%+v want created", firstScopeBody)
	}
	secondScope := summaryFixtureRequest(t, s, account, "9000112")
	secondScope.SummaryID = summaryHex64Seed("chain-summary-2")

	proposalID := proposalCreateFixture(t, s, task, "chain-create-1").ResourceId
	firstRow := basisRow(t, s, proposalID)
	if firstRow.Summary != firstScope.SummaryID || basisVersion(t, firstRow.Version) != 1 {
		t.Fatalf("chain first basis cites %v@%v want the live version %s@1", firstRow.Summary, firstRow.Version, firstScope.SummaryID)
	}

	proposalApproveFixture(t, s, task, proposalID, approvalID, "chain-approve-1")
	applied, err := s.ApplyPlanProposal(ctx, fixtureActor(), task.Id, proposalID, "chain-apply-1", contract.PlanApplyRequest{
		ApprovalRef:     approvalID,
		ExpectedVersion: task.Version,
		ProposalDigest:  proposalDigestFixture(t, s, task.Id, proposalID),
	})
	if err != nil {
		t.Fatalf("chain apply: %v", err)
	}
	if applied.State != contract.Accepted {
		t.Fatalf("apply receipt=%+v want accepted", applied)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.plan_application WHERE workspace_id='ws_fixture' AND proposal_id=$1`, proposalID); got != 1 {
		t.Fatalf("application rows=%d want 1", got)
	}

	// The application records intent only. No execution, permit, claim or bridge
	// operation is created; the scheduler does not consume this application.
	for _, table := range []string{"core.execution", "core.dispatch_permit", "core.target_claim", "bridge.outbox"} {
		if got := scalarInt(t, s, `SELECT count(*) FROM `+table+` WHERE workspace_id='ws_fixture'`); got != 0 {
			t.Fatalf("%s rows right after apply=%d want 0 (apply never calls out)", table, got)
		}
	}

	// The anti-replanning bound refuses another proposal until trusted ledger
	// evidence exists for this task.
	secondRequest := basisLiveRequest(t, s, task.Id)
	if _, err := s.CreatePlanProposal(ctx, fixtureActor(), task.Id, "chain-create-2", secondRequest); Code(err) != taskCodeStateConflict {
		t.Fatalf("second proposal before ledger flowback code=%v want %q", Code(err), taskCodeStateConflict)
	}

	// Independent task scheduling moves the ledger, not application consumption.
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("chain worker tick: %v", err)
	}
	if tick.Dispatched != 1 {
		t.Fatalf("worker tick dispatches=%d want 1", tick.Dispatched)
	}
	if adapter.count() != 1 {
		t.Fatalf("adapter calls=%d want 1", adapter.count())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 1 {
		t.Fatalf("executions after the tick=%d want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.retry WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 0 {
		t.Fatalf("retry rows after the tick=%d want 0", got)
	}

	// The next proposal is allowed now that the ledger has moved, it acts on the
	// unconsumed scope, and it records that scope's live Summary version.
	secondScopeBody := summaryDecodeIngest(t, chatServe(t, summaryHandler, http.MethodPost, groupSummaryIngestPath, secondScope, chatHeaders("chain-ingest-2")))
	if !secondScopeBody.Created || secondScopeBody.SummaryVersion != 1 {
		t.Fatalf("chain second scope observation=%+v want created version 1", secondScopeBody)
	}
	next, err := s.CreatePlanProposal(ctx, fixtureActor(), task.Id, "chain-create-3", basisLiveRequest(t, s, task.Id))
	if err != nil {
		t.Fatalf("next proposal after ledger flowback: %v", err)
	}
	if next.ResourceId == proposalID {
		t.Fatal("the next proposal reused the applied proposal id")
	}
	nextRow := basisRow(t, s, next.ResourceId)
	if nextRow.Account != account || nextRow.Group != "9000112" {
		t.Fatalf("next basis scope=%s/%s want %s/9000112 (the consumed scope drops out of selection)", nextRow.Account, nextRow.Group, account)
	}
	if nextRow.Summary != secondScope.SummaryID || basisVersion(t, nextRow.Version) != 1 {
		t.Fatalf("next basis cites %v@%v want the live version %s@1", nextRow.Summary, nextRow.Version, secondScope.SummaryID)
	}
	if nextRow.Reason != planProposalBasisLiveSummary+planProposalBasisNoAnchor {
		t.Fatalf("next basis reason=%q want the live-version rule", nextRow.Reason)
	}
}

// A pending proposal is not an execution gate for an already approved task.
// This counterexample prevents treating apply followed by Tick as causal proof.
func TestPlanProposalPendingIntentDoesNotGateIndependentScheduler(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	task, _ := proposalFixtureTask(t, s)
	proposalID := proposalCreateFixture(t, s, task, "independent-create").ResourceId
	adapter := &workerCountAdapter{}
	worker := workerFixture(t, s, adapter, WorkerSteps{Dispatch: true})
	tick, err := worker.Tick(ctx)
	if err != nil {
		t.Fatalf("independent scheduler tick: %v", err)
	}
	if tick.Dispatched != 1 || adapter.count() != 1 {
		t.Fatalf("scheduler dispatched=%d adapter calls=%d without apply, want 1 each", tick.Dispatched, adapter.count())
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.plan_application WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 0 {
		t.Fatalf("independent scheduling created %d applications, want 0", got)
	}
	if state := fixtureString(t, s, `SELECT state FROM core.plan_proposal WHERE workspace_id='ws_fixture' AND id=$1`, proposalID); state != "pending" {
		t.Fatalf("independent scheduling changed proposal state to %q", state)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.execution WHERE workspace_id='ws_fixture' AND task_id=$1`, task.Id); got != 1 {
		t.Fatalf("independent task executions=%d want 1", got)
	}
}
