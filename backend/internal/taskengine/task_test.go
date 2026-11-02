package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/oapi-codegen/nullable"
)

// ttRequest builds a minimal valid fixed-import dry-run request for tests.
func ttRequest(groups, accounts []string, goal int) contract.CreateTaskRequest {
	fixed := make([]contract.FixedCandidate, 0, len(groups))
	for _, g := range groups {
		fixed = append(fixed, contract.FixedCandidate{GroupId: g, SourceRef: "tt_import"})
	}
	mode := contract.ExecutionMode("dry_run")
	return contract.CreateTaskRequest{
		Type:                contract.CreateTaskRequestType("group.acquire"),
		JoinReason:          "fixture reason",
		VerificationMessage: "fixture answer",
		Title:               "tt fixture task",
		Goal:                contract.Goal{Metric: contract.NewUniqueGroupsForAccountPool, Count: goal, StrictTargetCap: true, RequireFinalMembershipCheck: true},
		AllowedAccountIds:   accounts,
		CandidateSource:     contract.CandidateSource{Kind: contract.CandidateSourceKind("fixed"), Groups: fixed},
		ExecutionMode:       &mode,
		DeadlineAt:          time.Now().UTC().Add(time.Hour),
		Limits: contract.TaskLimits{
			MaxCandidates:            len(groups),
			MaxAttemptsPerAction:     3,
			MaxTotalExternalAttempts: 100,
			MaxModelAttempts:         0,
		},
		ApprovalRef: nullable.NewNullNullable[string](),
		Submit:      true,
	}
}

func ttCreate(t *testing.T, s *Store, key string, req contract.CreateTaskRequest) contract.Task {
	t.Helper()
	task, err := s.CreateTask(context.Background(), fixtureActor(), key, req)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

func ttJSONBody(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ttServe(handler http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func ttErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope contract.ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, recorder.Body.String())
	}
	return envelope.Error.Code
}

func TestTaskCreateIdempotency(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_tt", "rt_tt", 5)
	ctx := context.Background()
	actor := fixtureActor()
	req := ttRequest([]string{"9000001", "9000002"}, []string{"acc_tt"}, 1)

	first := ttCreate(t, s, "create-key-1", req)
	second, err := s.CreateTask(ctx, actor, "create-key-1", req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if first.Id != second.Id {
		t.Fatalf("replay created a different task: %q != %q", first.Id, second.Id)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task WHERE workspace_id=$1`, actor.WorkspaceID); got != 1 {
		t.Fatalf("replay must not create a second task, got %d", got)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task_account WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, first.Id); got != 1 {
		t.Fatalf("task account rows = %d, want 1", got)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.goal_slot WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, first.Id); got != 1 {
		t.Fatalf("goal slots = %d, want 1", got)
	}

	// Same key, different body must be rejected rather than silently creating
	// or returning a resource for a different request.
	changed := req
	changed.Title = "different request"
	if _, err := s.CreateTask(ctx, actor, "create-key-1", changed); Code(err) != taskCodeIdempotency {
		t.Fatalf("reused key code = %q, want %q", Code(err), taskCodeIdempotency)
	}
	// Same body with a fresh key is a distinct request.
	third := ttCreate(t, s, "create-key-2", req)
	if third.Id == first.Id {
		t.Fatal("fresh idempotency key must create a fresh task")
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task WHERE workspace_id=$1`, actor.WorkspaceID); got != 2 {
		t.Fatalf("task count = %d, want 2", got)
	}

	// The HTTP surface must authenticate first, decode strictly, use generated
	// envelopes, and replay through the same idempotency key.
	handler := s.Handler(func(*http.Request) (Actor, error) { return actor, nil })
	bogus := map[string]any{}
	if err := json.Unmarshal(ttJSONBody(t, req), &bogus); err != nil {
		t.Fatal(err)
	}
	bogus["unexpected_field"] = true
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(ttJSONBody(t, bogus)))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", "http-bogus")
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400", rec.Code)
	}
	var errorEnvelope contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &errorEnvelope); err != nil {
		t.Fatal(err)
	}
	if errorEnvelope.Error.Code != taskCodeValidation {
		t.Fatalf("unknown field code = %q, want %q", errorEnvelope.Error.Code, taskCodeValidation)
	}

	rec = httptest.NewRecorder()
	httpReq = httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(ttJSONBody(t, req)))
	httpReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key status = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	httpReq = httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(ttJSONBody(t, req)))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", "http-key-1")
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("HTTP create status = %d, want 201", rec.Code)
	}
	var created contract.TaskEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.Id == "" || created.Data.State != contract.TaskStateVALIDATING {
		t.Fatalf("unexpected HTTP task projection: %+v", created.Data)
	}
	if created.Meta.RequestId == "" || created.Meta.NextCursor.IsNull() == false {
		t.Fatalf("envelope meta not populated: %+v", created.Meta)
	}
	rec = httptest.NewRecorder()
	httpReq = httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(ttJSONBody(t, req)))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", "http-key-1")
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("HTTP replay status = %d, want 201", rec.Code)
	}
	var replayed contract.TaskEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Data.Id != created.Data.Id {
		t.Fatalf("HTTP replay id = %q, want %q", replayed.Data.Id, created.Data.Id)
	}

	rec = httptest.NewRecorder()
	httpReq = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+created.Data.Id, nil)
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP get status = %d, want 200", rec.Code)
	}
	var fetched contract.TaskEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.Data.Id != created.Data.Id {
		t.Fatalf("HTTP get returned %q, want %q", fetched.Data.Id, created.Data.Id)
	}

	controlBody := ttJSONBody(t, contract.TaskControlRequest{Action: contract.TaskControlRequestActionCancel, ExpectedVersion: fetched.Data.Version, Reason: "http test cancel"})
	rec = httptest.NewRecorder()
	httpReq = httptest.NewRequest(http.MethodPost, "/v1/tasks/"+created.Data.Id+"/controls", bytes.NewReader(controlBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", "http-control-1")
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("HTTP control status = %d, want 202", rec.Code)
	}
	var receipt contract.ReceiptEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Data.State != contract.Accepted || receipt.Data.ResourceId != created.Data.Id || receipt.Data.Action != contract.ReceiptActionCancel {
		t.Fatalf("unexpected receipt: %+v", receipt.Data)
	}

	rec = httptest.NewRecorder()
	httpReq = httptest.NewRequest(http.MethodPut, "/v1/tasks", bytes.NewReader(nil))
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d, want 405", rec.Code)
	}
}

func TestTaskAuthorization(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_auth", "rt_auth", 5)
	ctx := context.Background()
	actor := fixtureActor()
	task := ttCreate(t, s, "auth-create", ttRequest([]string{"9000011"}, []string{"acc_auth"}, 1))

	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES($1,'principal_other')`, actor.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES($1,'principal_manager')`, actor.WorkspaceID); err != nil {
		t.Fatal(err)
	}

	// A different principal in the same workspace must not see the task.
	otherPermissions := make(map[string]bool, len(actor.Permissions))
	for permission, allowed := range actor.Permissions {
		if permission != "task.manage_all" {
			otherPermissions[permission] = allowed
		}
	}
	other := Actor{WorkspaceID: actor.WorkspaceID, PrincipalID: "principal_other", Permissions: otherPermissions}
	if _, err := s.GetTask(ctx, other, task.Id); Code(err) != taskCodeNotFound {
		t.Fatalf("cross-principal read code = %q, want %q", Code(err), taskCodeNotFound)
	}
	// Missing permission is rejected before object lookup.
	noPerms := Actor{WorkspaceID: actor.WorkspaceID, PrincipalID: actor.PrincipalID, Permissions: map[string]bool{}}
	if _, err := s.GetTask(ctx, noPerms, task.Id); Code(err) != taskCodeForbidden {
		t.Fatalf("missing permission code = %q, want %q", Code(err), taskCodeForbidden)
	}
	if _, err := s.CreateTask(ctx, noPerms, "no-perm-create", ttRequest([]string{"9000011"}, []string{"acc_auth"}, 1)); Code(err) != taskCodeForbidden {
		t.Fatalf("create without permission code = %q, want %q", Code(err), taskCodeForbidden)
	}
	// Permission is checked before task lookup or payload validation.
	if _, err := s.ExpandFixedPage(ctx, noPerms, "task_missing", 1); Code(err) != taskCodeForbidden {
		t.Fatalf("expand without permission code = %q, want %q", Code(err), taskCodeForbidden)
	}
	if err := s.CompleteBaseline(ctx, noPerms, "task_missing", nil); Code(err) != taskCodeForbidden {
		t.Fatalf("baseline without permission code = %q, want %q", Code(err), taskCodeForbidden)
	}
	// A trusted resolver actor from an unknown workspace must fail closed.
	foreign := Actor{WorkspaceID: "ws_other", PrincipalID: "principal_fixture", Permissions: actor.Permissions}
	if _, err := s.CreateTask(ctx, foreign, "foreign-create", ttRequest([]string{"9000011"}, []string{"acc_auth"}, 1)); Code(err) != taskCodeUnauthenticated {
		t.Fatalf("foreign workspace code = %q, want %q", Code(err), taskCodeUnauthenticated)
	}

	// task.manage_all grants cross-principal control, still scoped to workspace.
	manager := Actor{
		WorkspaceID: actor.WorkspaceID,
		PrincipalID: "principal_manager",
		Permissions: map[string]bool{"task.read": true, "task.control": true, "task.manage_all": true},
	}
	managed, err := s.GetTask(ctx, manager, task.Id)
	if err != nil {
		t.Fatalf("manage_all read: %v", err)
	}
	if _, err := s.ControlTask(ctx, manager, task.Id, "manager-cancel", contract.TaskControlRequest{
		Action:          contract.TaskControlRequestActionCancel,
		ExpectedVersion: managed.Version,
		Reason:          "manager fixture cancel",
	}); err != nil {
		t.Fatalf("manage_all control: %v", err)
	}

	// Task-entry runtime actors are constrained to the runtime/account binding
	// of every account in the task.
	entryBad := Actor{
		WorkspaceID: actor.WorkspaceID,
		PrincipalID: actor.PrincipalID,
		RuntimeID:   "rt_other",
		AccountIDs:  []string{"acc_auth"},
		Permissions: actor.Permissions,
	}
	if _, err := s.GetTask(ctx, entryBad, task.Id); Code(err) != taskCodeNotFound {
		t.Fatalf("wrong runtime code = %q, want %q", Code(err), taskCodeNotFound)
	}
	entryGood := entryBad
	entryGood.RuntimeID = "rt_auth"
	if _, err := s.GetTask(ctx, entryGood, task.Id); err != nil {
		t.Fatalf("correct runtime read: %v", err)
	}

	// HTTP authentication failures never reach object storage or replay.
	handler := s.Handler(func(*http.Request) (Actor, error) { return Actor{}, problem(taskCodeUnauthenticated) })
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Id, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated HTTP status = %d, want 401", rec.Code)
	}
}

func TestTaskVersionConflict(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_version", "rt_version", 5)
	ctx := context.Background()
	actor := fixtureActor()
	task := ttCreate(t, s, "version-create", ttRequest([]string{"9000021"}, []string{"acc_version"}, 1))

	request := contract.TaskControlRequest{
		Action:          contract.TaskControlRequestActionCancel,
		ExpectedVersion: task.Version + 1,
		Reason:          "stale view",
	}
	if _, err := s.ControlTask(ctx, actor, task.Id, "version-key-1", request); Code(err) != taskCodeVersionConflict {
		t.Fatalf("stale version code = %q, want %q", Code(err), taskCodeVersionConflict)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id); got != task.Version {
		t.Fatalf("stale control mutated version: %d != %d", got, task.Version)
	}
	var state string
	if err := s.Pool.QueryRow(ctx, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(task.State) {
		t.Fatalf("stale control mutated state: %q != %q", state, task.State)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.idempotency WHERE workspace_id=$1 AND principal_id=$2 AND path=$3 AND key=$4`,
		actor.WorkspaceID, actor.PrincipalID, taskControlPath(task.Id), "version-key-1"); got != 0 {
		t.Fatalf("failed CAS consumed idempotency key: %d rows", got)
	}

	// A rejected attempt must not consume the key; the corrected CAS succeeds.
	request.ExpectedVersion = task.Version
	receipt, err := s.ControlTask(ctx, actor, task.Id, "version-key-1", request)
	if err != nil {
		t.Fatalf("corrected CAS: %v", err)
	}
	if receipt.State != contract.Accepted || receipt.Action != contract.ReceiptActionCancel {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	versionAfter := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id)
	if versionAfter != task.Version+1 {
		t.Fatalf("version after control = %d, want %d", versionAfter, task.Version+1)
	}

	// A replay with the now-stale expected version still returns the original
	// accepted receipt and must not mutate the task again.
	replayed, err := s.ControlTask(ctx, actor, task.Id, "version-key-1", request)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.Id != receipt.Id {
		t.Fatalf("replay receipt id = %q, want %q", replayed.Id, receipt.Id)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id); got != versionAfter {
		t.Fatalf("replay mutated version: %d != %d", got, versionAfter)
	}
	changed := request
	changed.Reason = "different body"
	if _, err := s.ControlTask(ctx, actor, task.Id, "version-key-1", changed); Code(err) != taskCodeIdempotency {
		t.Fatalf("reused control key code = %q, want %q", Code(err), taskCodeIdempotency)
	}
}

func TestTemplateNoModelCalls(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_template", "rt_template", 5)
	seedAccount(t, s, "acc_template2", "rt_template2", 5)
	actor := fixtureActor()
	req := ttRequest([]string{"9000101", "9000102", "9000103"}, []string{"acc_template", "acc_template2"}, 2)
	keywords := []string{"ai", "local-model"}
	req.Keywords = &keywords
	task := ttCreate(t, s, "template-create", req)

	done, err := s.ExpandFixedPage(context.Background(), actor, task.Id, 100)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !done {
		t.Fatal("template expansion did not finish in one bounded page")
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.subtask WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, task.Id); got != 3 {
		t.Fatalf("subtasks = %d, want exactly the 3 imported candidates", got)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task_target WHERE workspace_id=$1 AND task_id=$2 AND source_ref='tt_import'`, actor.WorkspaceID, task.Id); got != 3 {
		t.Fatalf("task targets = %d, want 3 with import provenance", got)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.target WHERE workspace_id=$1`, actor.WorkspaceID); got != 3 {
		t.Fatalf("targets = %d, want 3; keywords must never create candidates", got)
	}
	var ids string
	if err := s.Pool.QueryRow(context.Background(), `
		SELECT string_agg(external_id, ',' ORDER BY external_id) FROM core.target WHERE workspace_id=$1`,
		actor.WorkspaceID).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if ids != "9000101,9000102,9000103" {
		t.Fatalf("target ids = %q, want fixed import only", ids)
	}
	if got := scalarInt(t, s, `SELECT import_offset FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id); got != 3 {
		t.Fatalf("import offset = %d, want 3", got)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task WHERE workspace_id=$1 AND id=$2 AND import_done`, actor.WorkspaceID, task.Id); got != 1 {
		t.Fatalf("import_done not set")
	}
	// No execution, permit, claim, bridge operation, or non-template event may
	// exist: template expansion is a pure deterministic derivation.
	for _, q := range []string{
		`SELECT count(*)::int FROM core.execution WHERE workspace_id=$1 AND task_id=$2`,
	} {
		if got := scalarInt(t, s, q, actor.WorkspaceID, task.Id); got != 0 {
			t.Fatalf("template expansion produced external/work rows via %q: %d", q, got)
		}
	}
	for _, q := range []string{
		`SELECT count(*)::int FROM core.dispatch_permit WHERE workspace_id=$1`,
		`SELECT count(*)::int FROM core.target_claim WHERE workspace_id=$1`,
		`SELECT count(*)::int FROM bridge.gateway_operation WHERE workspace_id=$1`,
	} {
		if got := scalarInt(t, s, q, actor.WorkspaceID); got != 0 {
			t.Fatalf("template expansion produced external/work rows via %q: %d", q, got)
		}
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task_event WHERE workspace_id=$1 AND task_id=$2 AND kind NOT IN ('task.created','task.import_expanded')`,
		actor.WorkspaceID, task.Id); got != 0 {
		t.Fatalf("template expansion produced non-template events: %d", got)
	}
	version := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id)
	done, err = s.ExpandFixedPage(context.Background(), actor, task.Id, 100)
	if err != nil || !done {
		t.Fatalf("repeat expansion done=%v err=%v", done, err)
	}
	if got := scalarInt(t, s, `SELECT version FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id); got != version {
		t.Fatalf("completed expansion is not idempotent: version %d != %d", got, version)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.subtask WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, task.Id); got != 3 {
		t.Fatalf("repeat expansion duplicated subtasks: %d", got)
	}

	// Baseline is never guessed from an absent or partial snapshot.
	ctx := context.Background()
	if err := s.CompleteBaseline(ctx, actor, task.Id, nil); Code(err) != taskCodeValidation {
		t.Fatalf("empty baseline code = %q, want %q", Code(err), taskCodeValidation)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.baseline_account WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, task.Id); got != 0 {
		t.Fatalf("rejected empty baseline wrote proof rows: %d", got)
	}
	firstSnapshots := []BaselineSnapshot{
		{AccountID: "acc_template", EvidenceRef: "fixture_evidence_1", ObservedAt: time.Now().UTC(), Complete: true, GroupIDs: []string{}},
		{AccountID: "acc_template2", EvidenceRef: "fixture_evidence_2", ObservedAt: time.Now().UTC(), Complete: false, GroupIDs: []string{}},
	}
	if err := s.CompleteBaseline(ctx, actor, task.Id, firstSnapshots); err != nil {
		t.Fatalf("partial baseline: %v", err)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task WHERE workspace_id=$1 AND id=$2 AND NOT baseline_complete AND state='VALIDATING'`,
		actor.WorkspaceID, task.Id); got != 1 {
		t.Fatal("incomplete account baseline must not make the task READY")
	}
	before, err := s.GetTask(ctx, actor, task.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ControlTask(ctx, actor, task.Id, "template-start-incomplete", contract.TaskControlRequest{
		Action:          contract.TaskControlRequestActionStart,
		ExpectedVersion: before.Version,
		Reason:          "start before baseline completion",
	}); Code(err) != taskCodeStateConflict {
		t.Fatalf("start with incomplete baseline code = %q, want %q", Code(err), taskCodeStateConflict)
	}
	if err := s.CompleteBaseline(ctx, actor, task.Id, []BaselineSnapshot{
		{AccountID: "acc_template2", EvidenceRef: "fixture_evidence_2b", ObservedAt: time.Now().UTC(), Complete: true, GroupIDs: []string{}},
	}); err != nil {
		t.Fatalf("completing baseline: %v", err)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task WHERE workspace_id=$1 AND id=$2 AND baseline_complete AND state='READY' AND baseline_ref IS NOT NULL`,
		actor.WorkspaceID, task.Id); got != 1 {
		t.Fatal("all-complete trusted baseline must make the task READY with a baseline ref")
	}
}

func TestSearchCheckpointResume(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_checkpoint", "rt_checkpoint", 5)
	ctx := context.Background()
	actor := fixtureActor()
	groups := []string{"9000201", "9000202", "9000203", "9000204", "9000205"}
	task := ttCreate(t, s, "checkpoint-create-1", ttRequest(groups, []string{"acc_checkpoint"}, 2))

	// Page 1 commits offset and rows together.
	done, err := s.ExpandFixedPage(ctx, actor, task.Id, 2)
	if err != nil || done {
		t.Fatalf("page 1 done=%v err=%v", done, err)
	}
	if got := scalarInt(t, s, `SELECT import_offset FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id); got != 2 {
		t.Fatalf("offset after page 1 = %d, want 2", got)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.subtask WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, task.Id); got != 2 {
		t.Fatalf("subtasks after page 1 = %d, want 2", got)
	}

	// A resumed process continues exactly at the committed checkpoint; it never
	// replays earlier pages and never skips candidates.
	done, err = s.ExpandFixedPage(ctx, actor, task.Id, 2)
	if err != nil || done {
		t.Fatalf("page 2 done=%v err=%v", done, err)
	}
	if got := scalarInt(t, s, `SELECT import_offset FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id); got != 4 {
		t.Fatalf("offset after resume = %d, want 4", got)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.subtask WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, task.Id); got != 4 {
		t.Fatalf("subtasks after resume = %d, want 4", got)
	}

	done, err = s.ExpandFixedPage(ctx, actor, task.Id, 100)
	if err != nil || !done {
		t.Fatalf("final page done=%v err=%v", done, err)
	}
	if got := scalarInt(t, s, `SELECT import_offset FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, task.Id); got != len(groups) {
		t.Fatalf("final offset = %d, want %d", got, len(groups))
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.subtask WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, task.Id); got != len(groups) {
		t.Fatalf("final subtasks = %d, want %d", got, len(groups))
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.subtask WHERE workspace_id=$1 AND task_id=$2 AND max_attempts=3 AND state='QUEUED'`, actor.WorkspaceID, task.Id); got != len(groups) {
		t.Fatalf("subtask template fields wrong: %d", got)
	}

	// A second task importing the same groups reuses canonical targets while
	// keeping its own task_target/subtask rows and checkpoint.
	task2 := ttCreate(t, s, "checkpoint-create-2", ttRequest(groups, []string{"acc_checkpoint"}, 2))
	done, err = s.ExpandFixedPage(ctx, actor, task2.Id, 100)
	if err != nil || !done {
		t.Fatalf("second task expand done=%v err=%v", done, err)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.target WHERE workspace_id=$1`, actor.WorkspaceID); got != len(groups) {
		t.Fatalf("canonical targets = %d, want %d (no duplicate target rows)", got, len(groups))
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.task_target WHERE workspace_id=$1 AND task_id=$2`, actor.WorkspaceID, task2.Id); got != len(groups) {
		t.Fatalf("second task targets = %d, want %d", got, len(groups))
	}
	if got := scalarInt(t, s, `SELECT count(DISTINCT target_id)::int FROM core.task_target WHERE workspace_id=$1 AND task_id IN ($2,$3)`,
		actor.WorkspaceID, task.Id, task2.Id); got != len(groups) {
		t.Fatalf("tasks do not share canonical target ids: %d distinct", got)
	}
}

func TestTaskEntryRuntimeBoundary(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_entry", "rt_entry", 5)
	seedAccount(t, s, "acc_other", "rt_other", 5)
	seedAccount(t, s, "acc_inactive", "rt_inactive", 5)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `
		UPDATE core.runtime_credentials SET active=false
		WHERE workspace_id=$1 AND account_id='acc_inactive'`, "ws_fixture"); err != nil {
		t.Fatal(err)
	}

	entry := fixtureActor()
	entry.RuntimeID = "rt_entry"
	entry.AccountIDs = []string{"acc_entry"}
	entry.SenderID = "sender_entry"

	req := ttRequest([]string{"9000301"}, []string{"acc_entry"}, 1)
	task, err := s.CreateTask(ctx, entry, "entry-create", req)
	if err != nil {
		t.Fatalf("entry-bound create: %v", err)
	}

	// Binding is checked before the idempotent replay is returned.
	unbound := entry
	unbound.AccountIDs = nil
	if _, err := s.CreateTask(ctx, unbound, "entry-create", req); Code(err) != taskCodeForbidden {
		t.Fatalf("unbound replay code = %q, want %q", Code(err), taskCodeForbidden)
	}
	// An account outside the actor allow-list is rejected.
	otherReq := ttRequest([]string{"9000302"}, []string{"acc_other"}, 1)
	if _, err := s.CreateTask(ctx, entry, "entry-other", otherReq); Code(err) != taskCodeForbidden {
		t.Fatalf("out-of-binding account code = %q, want %q", Code(err), taskCodeForbidden)
	}
	// An inactive runtime credential is rejected even when the runtime matches.
	inactive := entry
	inactive.RuntimeID = "rt_inactive"
	inactive.AccountIDs = []string{"acc_inactive"}
	inactive.SenderID = "sender_inactive"
	inactiveReq := ttRequest([]string{"9000303"}, []string{"acc_inactive"}, 1)
	if _, err := s.CreateTask(ctx, inactive, "entry-inactive", inactiveReq); Code(err) != taskCodeForbidden {
		t.Fatalf("inactive credential code = %q, want %q", Code(err), taskCodeForbidden)
	}

	// Runtime/task-entry actors may never start or resume.
	startReq := contract.TaskControlRequest{Action: contract.TaskControlRequestActionStart, ExpectedVersion: task.Version, Reason: "entry start"}
	if _, err := s.ControlTask(ctx, entry, task.Id, "entry-start", startReq); Code(err) != taskCodeForbidden {
		t.Fatalf("runtime start code = %q, want %q", Code(err), taskCodeForbidden)
	}
	resumeReq := contract.TaskControlRequest{Action: contract.TaskControlRequestActionResume, ExpectedVersion: task.Version, Reason: "entry resume"}
	if _, err := s.ControlTask(ctx, entry, task.Id, "entry-resume", resumeReq); Code(err) != taskCodeForbidden {
		t.Fatalf("runtime resume code = %q, want %q", Code(err), taskCodeForbidden)
	}
	// Pause is allowed through the runtime gate (state rejects it here), cancel succeeds.
	pauseReq := contract.TaskControlRequest{Action: contract.TaskControlRequestActionPause, ExpectedVersion: task.Version, Reason: "entry pause"}
	if _, err := s.ControlTask(ctx, entry, task.Id, "entry-pause", pauseReq); Code(err) != taskCodeStateConflict {
		t.Fatalf("runtime pause code = %q, want %q", Code(err), taskCodeStateConflict)
	}
	cancelReq := contract.TaskControlRequest{Action: contract.TaskControlRequestActionCancel, ExpectedVersion: task.Version, Reason: "entry cancel"}
	if _, err := s.ControlTask(ctx, entry, task.Id, "entry-cancel", cancelReq); err != nil {
		t.Fatalf("runtime cancel: %v", err)
	}

	handler := s.Handler(func(*http.Request) (Actor, error) { return entry, nil })
	baseHeaders := map[string]string{
		"X-QQ-Account-ID": "acc_entry",
		"X-QQ-Sender-ID":  "sender_entry",
	}
	if rec := ttServe(handler, http.MethodGet, "/v1/tasks/"+task.Id, nil, baseHeaders); rec.Code != http.StatusOK {
		t.Fatalf("runtime get status = %d, want 200", rec.Code)
	}
	missingAccount := map[string]string{"X-QQ-Sender-ID": "sender_entry"}
	if rec := ttServe(handler, http.MethodGet, "/v1/tasks/"+task.Id, nil, missingAccount); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing account header status = %d, want 401", rec.Code)
	}
	wrongSender := map[string]string{"X-QQ-Account-ID": "acc_entry", "X-QQ-Sender-ID": "wrong"}
	if rec := ttServe(handler, http.MethodGet, "/v1/tasks/"+task.Id, nil, wrongSender); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong sender header status = %d, want 401", rec.Code)
	}
	outsideAccount := map[string]string{"X-QQ-Account-ID": "acc_other", "X-QQ-Sender-ID": "sender_entry"}
	if rec := ttServe(handler, http.MethodGet, "/v1/tasks/"+task.Id, nil, outsideAccount); rec.Code != http.StatusUnauthorized {
		t.Fatalf("outside account header status = %d, want 401", rec.Code)
	}
	noSender := entry
	noSender.SenderID = ""
	noSenderHandler := s.Handler(func(*http.Request) (Actor, error) { return noSender, nil })
	if rec := ttServe(noSenderHandler, http.MethodGet, "/v1/tasks/"+task.Id, nil, baseHeaders); rec.Code != http.StatusUnauthorized {
		t.Fatalf("resolver missing sender status = %d, want 401", rec.Code)
	}

	startBody := ttJSONBody(t, startReq)
	controlHeaders := map[string]string{
		"Content-Type":    "application/json",
		"Idempotency-Key": "entry-http-start",
		"X-QQ-Account-ID": "acc_entry",
		"X-QQ-Sender-ID":  "sender_entry",
	}
	rec := ttServe(handler, http.MethodPost, "/v1/tasks/"+task.Id+"/controls", startBody, controlHeaders)
	if rec.Code != http.StatusForbidden || ttErrorCode(t, rec) != taskCodeForbidden {
		t.Fatalf("runtime HTTP start = status %d code %q", rec.Code, ttErrorCode(t, rec))
	}
}

func TestTaskHTTPStrictPresence(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_strict", "rt_strict", 5)
	actor := fixtureActor()
	handler := s.Handler(func(*http.Request) (Actor, error) { return actor, nil })
	base := ttRequest([]string{"9000601"}, []string{"acc_strict"}, 1)

	fresh := func() map[string]any {
		var object map[string]any
		if err := json.Unmarshal(ttJSONBody(t, base), &object); err != nil {
			t.Fatal(err)
		}
		return object
	}
	post := func(object map[string]any, contentType string) *httptest.ResponseRecorder {
		headers := map[string]string{"Idempotency-Key": newID("strict")}
		if contentType != "" {
			headers["Content-Type"] = contentType
		}
		return ttServe(handler, http.MethodPost, "/v1/tasks", ttJSONBody(t, object), headers)
	}
	expectValidation := func(label string, object map[string]any) {
		t.Helper()
		rec := post(object, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", label, rec.Code)
		}
		if code := ttErrorCode(t, rec); code != taskCodeValidation {
			t.Fatalf("%s code = %q, want %q", label, code, taskCodeValidation)
		}
	}
	expectMediaType := func(label string, contentType string) {
		t.Helper()
		rec := post(fresh(), contentType)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s status = %d, want 415", label, rec.Code)
		}
		if code := ttErrorCode(t, rec); code != taskCodeUnsupportedMedia {
			t.Fatalf("%s code = %q, want %q", label, code, taskCodeUnsupportedMedia)
		}
	}

	object := fresh()
	delete(object, "approval_ref")
	expectValidation("missing required nullable approval_ref", object)

	object = fresh()
	delete(object, "submit")
	expectValidation("missing required submit", object)

	object = fresh()
	object["execution_mode"] = nil
	expectValidation("null optional execution_mode", object)

	object = fresh()
	object["keywords"] = nil
	expectValidation("null optional keywords", object)

	object = fresh()
	limits := object["limits"].(map[string]any)
	delete(limits, "max_model_attempts")
	expectValidation("missing nested limits.max_model_attempts", object)

	object = fresh()
	goal := object["goal"].(map[string]any)
	delete(goal, "strict_target_cap")
	expectValidation("missing nested goal.strict_target_cap", object)

	object = fresh()
	source := object["candidate_source"].(map[string]any)
	groups := source["groups"].([]any)
	group := groups[0].(map[string]any)
	delete(group, "source_ref")
	expectValidation("missing nested candidate source_ref", object)

	object = fresh()
	object["deadline_at"] = "2030-10-25T12:00:00+02:00"
	expectValidation("non-UTC deadline_at offset", object)

	expectMediaType("missing content type", "")
	expectMediaType("wrong content type", "text/plain")

	// A present null approval_ref remains valid for a dry-run task.
	rec := post(fresh(), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid nullable approval_ref status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created contract.TaskEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Data.Id == "" {
		t.Fatalf("valid create envelope: %v", err)
	}

	controlPost := func(body map[string]any) *httptest.ResponseRecorder {
		headers := map[string]string{
			"Content-Type":    "application/json",
			"Idempotency-Key": newID("strict-control"),
		}
		return ttServe(handler, http.MethodPost, "/v1/tasks/"+created.Data.Id+"/controls", ttJSONBody(t, body), headers)
	}
	controlBody := map[string]any{"action": "cancel", "expected_version": created.Data.Version}
	rec = controlPost(controlBody)
	if rec.Code != http.StatusBadRequest || ttErrorCode(t, rec) != taskCodeValidation {
		t.Fatalf("control missing reason = status %d code %q", rec.Code, ttErrorCode(t, rec))
	}
	controlBody["reason"] = nil
	rec = controlPost(controlBody)
	if rec.Code != http.StatusBadRequest || ttErrorCode(t, rec) != taskCodeValidation {
		t.Fatalf("control null reason = status %d code %q", rec.Code, ttErrorCode(t, rec))
	}
}

func TestTaskPauseWithIssuedPermitAndLock(t *testing.T) {
	ctx := context.Background()
	stateOf := func(t *testing.T, s *Store, taskID string) string {
		t.Helper()
		var state string
		if err := s.Pool.QueryRow(ctx, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, "ws_fixture", taskID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	pause := func(t *testing.T, s *Store, task contract.Task) {
		t.Helper()
		current, err := s.GetTask(ctx, fixtureActor(), task.Id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ControlTask(ctx, fixtureActor(), task.Id, newID("pause"), contract.TaskControlRequest{
			Action:          contract.TaskControlRequestActionPause,
			ExpectedVersion: current.Version,
			Reason:          "truthful pause boundary",
		}); err != nil {
			t.Fatalf("pause: %v", err)
		}
	}

	t.Run("issued permit", func(t *testing.T) {
		s := fixtureStore(t)
		seedAccount(t, s, "acc_permit", "rt_permit", 5)
		task := createReadyTask(t, s, []string{"acc_permit"}, []string{"9000701"}, 1, 2)
		res := reserveFixture(t, s, task.Id, "")
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO core.dispatch_permit(workspace_id,id,execution_id,operation_id,account_id,target_id,
				request_digest,scope_revision,owner_epoch,fence_token,expires_at,state)
			SELECT e.workspace_id,'permit_pause',e.id,e.operation_id,e.account_id,e.target_id,
				e.request_digest,e.scope_revision,e.owner_epoch,e.fence_token,now()+interval '30 seconds','issued'
			FROM core.execution e WHERE e.workspace_id=$1 AND e.id=$2`, "ws_fixture", res.ExecutionID); err != nil {
			t.Fatal(err)
		}
		pause(t, s, task)
		if got := stateOf(t, s, task.Id); got != "PAUSING" {
			t.Fatalf("task with issued permit paused to %q, want PAUSING", got)
		}
		if got := scalarInt(t, s, `SELECT count(*)::int FROM core.dispatch_permit WHERE workspace_id=$1 AND id='permit_pause' AND state='issued'`,
			"ws_fixture"); got != 1 {
			t.Fatalf("pause changed an unresolved permit: %d issued", got)
		}
	})

	t.Run("held action lock", func(t *testing.T) {
		s := fixtureStore(t)
		seedAccount(t, s, "acc_lock", "rt_lock", 5)
		task := createReadyTask(t, s, []string{"acc_lock"}, []string{"9000702"}, 1, 2)
		res := reserveFixture(t, s, task.Id, "")
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO core.account_action_lock(workspace_id,account_id,action_family,execution_id,permit_id,held,expires_at)
			VALUES($1,$2,'group.join',$3,NULL,true,now()+interval '30 seconds')
			ON CONFLICT (workspace_id,account_id,action_family) DO UPDATE SET
				execution_id=EXCLUDED.execution_id,permit_id=NULL,held=true,expires_at=EXCLUDED.expires_at`,
			"ws_fixture", res.AccountID, res.ExecutionID); err != nil {
			t.Fatal(err)
		}
		pause(t, s, task)
		if got := stateOf(t, s, task.Id); got != "PAUSED" {
			t.Fatalf("quiescent task with safety lock paused to %q, want PAUSED", got)
		}
		if got := scalarInt(t, s, `SELECT count(*)::int FROM core.account_action_lock WHERE workspace_id=$1 AND held`,
			"ws_fixture"); got != 1 {
			t.Fatalf("pause auto-released a held action lock: %d held", got)
		}
	})
}
func TestPauseSettlesAfterAuthoritativeNoEffect(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_pause_noeffect", "rt_pause_noeffect", 5)
	task := createReadyTask(t, s, []string{"acc_pause_noeffect"}, []string{"9000703"}, 1, 2)
	res := reserveFixture(t, s, task.Id, "")
	rt := fixtureRuntime(res.AccountID, res.RuntimeID)
	if _, err := s.AcceptOperation(context.Background(), rt, res); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssuePermit(context.Background(), rt, res.OperationID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// The pause lands while the send is already in flight: the adapter has
	// been entered, so this is not a new dispatch the pause must suppress.
	entered := make(chan struct{})
	release := make(chan struct{})
	adapter := &pauseSignalAdapter{entered: entered, release: release}
	dispatchErr := make(chan error, 1)
	go func() {
		_, err := s.Dispatch(context.Background(), rt, res.OperationID, func(context.Context, Reservation) error { return nil }, adapter)
		dispatchErr <- err
	}()
	<-entered
	pauseRequest := contract.TaskControlRequest{Action: contract.TaskControlRequestActionPause, ExpectedVersion: task.Version, Reason: "pause during an in-flight send"}
	if _, err := s.ControlTask(context.Background(), fixtureActor(), task.Id, "pause-in-flight", pauseRequest); err != nil {
		t.Fatalf("pause in flight: %v", err)
	}
	if got := fixtureString(t, s, `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, res.WorkspaceID, res.TaskID); got != "PAUSING" {
		t.Fatalf("task with an in-flight send paused to %q, want PAUSING", got)
	}
	close(release)
	if err := <-dispatchErr; err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.Pool.QueryRow(context.Background(), `SELECT state FROM core.task WHERE workspace_id=$1 AND id=$2`, res.WorkspaceID, res.TaskID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "PAUSED" {
		t.Fatalf("task state=%s want PAUSED after no-effect receipt", state)
	}
}

type pauseSignalAdapter struct {
	entered chan struct{}
	release chan struct{}
}

func (a *pauseSignalAdapter) RequestJoin(context.Context, Reservation) (SendOutcome, error) {
	close(a.entered)
	<-a.release
	return SendOutcome{Disposition: "failed_no_effect", EffectCertainty: "no_effect", EvidenceRef: "pause_no_effect"}, nil
}

func TestBaselinePreservesExcludedTarget(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_excl", "rt_excl", 5)
	ctx := context.Background()
	actor := fixtureActor()
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO core.target(workspace_id,id,external_id,disabled)
		VALUES($1,'target_excluded','9000501',true)`, "ws_fixture"); err != nil {
		t.Fatal(err)
	}
	task := ttCreate(t, s, "excluded-create", ttRequest([]string{"9000501"}, []string{"acc_excl"}, 1))
	done, err := s.ExpandFixedPage(ctx, actor, task.Id, 100)
	if err != nil || !done {
		t.Fatalf("expand done=%v err=%v", done, err)
	}
	var eligibility string
	var covered bool
	if err := s.Pool.QueryRow(ctx, `
		SELECT eligibility, baseline_covered FROM core.task_target
		WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id).Scan(&eligibility, &covered); err != nil {
		t.Fatal(err)
	}
	if eligibility != "excluded" || covered {
		t.Fatalf("disabled target initial state = (%q,%v), want (excluded,false)", eligibility, covered)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.subtask WHERE workspace_id=$1 AND task_id=$2`,
		"ws_fixture", task.Id); got != 0 {
		t.Fatalf("excluded target produced %d subtasks", got)
	}
	if err := s.CompleteBaseline(ctx, actor, task.Id, []BaselineSnapshot{{
		AccountID:   "acc_excl",
		EvidenceRef: "fixture_excluded_baseline",
		ObservedAt:  time.Now().UTC(),
		Complete:    true,
		GroupIDs:    []string{"9000501"},
	}}); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if err := s.Pool.QueryRow(ctx, `
		SELECT eligibility, baseline_covered FROM core.task_target
		WHERE workspace_id=$1 AND task_id=$2`, "ws_fixture", task.Id).Scan(&eligibility, &covered); err != nil {
		t.Fatal(err)
	}
	if eligibility != "excluded" || covered {
		t.Fatalf("baseline overwrote excluded eligibility: (%q,%v), want (excluded,false)", eligibility, covered)
	}
	if got := scalarInt(t, s, `SELECT count(*)::int FROM core.baseline WHERE workspace_id=$1 AND task_id=$2`,
		"ws_fixture", task.Id); got != 1 {
		t.Fatalf("baseline proof rows = %d, want 1", got)
	}
}
