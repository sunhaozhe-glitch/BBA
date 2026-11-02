package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oapi-codegen/nullable"

	"bba.local/taskengine/internal/contract"
)

// Error codes are the stable codes from the wire contract. They are used
// verbatim so the HTTP layer can map them without translating internal errors.
const (
	taskCodeValidation            = "VALIDATION_FAILED"
	taskCodeUnauthenticated       = "UNAUTHENTICATED"
	taskCodeForbidden             = "ACTION_FORBIDDEN"
	taskCodeNotFound              = "RESOURCE_NOT_FOUND"
	taskCodeVersionConflict       = "TASK_VERSION_CONFLICT"
	taskCodeIdempotency           = "IDEMPOTENCY_CONFLICT"
	taskCodeStateConflict         = "STATE_CONFLICT"
	taskCodeStoreUnavailable      = "STATE_STORE_UNAVAILABLE"
	taskCodePayloadTooLarge       = "PAYLOAD_TOO_LARGE"
	taskCodeUnsupportedMedia      = "UNSUPPORTED_MEDIA_TYPE"
	taskCodeMethodNotAllowed      = "METHOD_NOT_ALLOWED"
	taskCodeCursorExpired         = "CURSOR_EXPIRED"
	taskCodeFeatureDisabled       = "FEATURE_DISABLED"
	taskCodeCapabilityUnavailable = "CAPABILITY_UNAVAILABLE"
)

const (
	taskPermCreate  = "task.create"
	taskPermControl = "task.control"
)

// taskQQPattern mirrors core.qq_id: an unsigned decimal without leading zero.
var taskQQPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// taskCreatePath is the idempotency path used by the HTTP create endpoint.
const taskCreatePath = "/v1/tasks"

func taskControlPath(taskID string) string {
	return "/v1/tasks/" + taskID + "/controls"
}

func taskBlank(s string) bool { return strings.TrimSpace(s) == "" }

// taskValidID mirrors the core.id domain. IDs are opaque ASCII identifiers
// issued by this service; a non-ASCII identifier would be rejected by the
// database domain length semantics only after JSON round trips, so check it
// early and fail closed.
func taskValidID(s string) bool {
	if taskBlank(s) {
		return false
	}
	return len(s) >= 1 && len(s) <= 128
}

func taskValidQQID(s string) bool { return taskQQPattern.MatchString(s) }

func taskCheckIdempotencyKey(key string) error {
	if taskBlank(key) || len(key) > 128 {
		return problem(taskCodeValidation)
	}
	return nil
}

type taskIdemRecord struct {
	digest   string
	response []byte
}

// taskReadIdempotency returns the stored record for the exact resource scope.
// Callers must authorize the actor before using a replay.
func taskReadIdempotency(ctx context.Context, tx pgx.Tx, workspaceID, principalID, method, path, key string) (taskIdemRecord, bool, error) {
	var rec taskIdemRecord
	err := tx.QueryRow(ctx, `
		SELECT digest, response
		FROM core.idempotency
		WHERE workspace_id=$1 AND principal_id=$2 AND method=$3 AND path=$4 AND key=$5`,
		workspaceID, principalID, method, path, key,
	).Scan(&rec.digest, &rec.response)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskIdemRecord{}, false, nil
	}
	if err != nil {
		return taskIdemRecord{}, false, err
	}
	return rec, true, nil
}

func taskWriteIdempotency(ctx context.Context, tx pgx.Tx, workspaceID, principalID, method, path, key, bodyDigest, resourceID string, response any) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO core.idempotency(workspace_id,principal_id,method,path,key,digest,resource_id,response)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		workspaceID, principalID, method, path, key, bodyDigest, resourceID, payload)
	return err
}

func taskUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// taskApproval is the trusted, server-side approval payload. The wire request
// only carries an opaque approval_ref; it never asserts scope.
type taskApproval struct {
	PrincipalID         string
	ScopeRevision       int
	AccountIDs          []string
	TargetIDs           []string
	MaxAttempts         int
	ExpiresAt           time.Time
	Revoked             bool
	JoinReason          string
	VerificationMessage string
}

func taskLoadApproval(ctx context.Context, tx pgx.Tx, workspaceID, approvalID string) (taskApproval, error) {
	var a taskApproval
	err := tx.QueryRow(ctx, `
		SELECT principal_id, scope_revision, account_ids, target_ids, max_attempts, expires_at, revoked,
		       join_reason, verification_message
		FROM core.approval WHERE workspace_id=$1 AND id=$2`,
		workspaceID, approvalID,
	).Scan(&a.PrincipalID, &a.ScopeRevision, &a.AccountIDs, &a.TargetIDs, &a.MaxAttempts, &a.ExpiresAt, &a.Revoked,
		&a.JoinReason, &a.VerificationMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskApproval{}, problem(taskCodeForbidden)
	}
	if err != nil {
		return taskApproval{}, err
	}
	return a, nil
}

// taskValidateApprovalScope verifies every live-scoped dimension server side. It
// never trusts a caller-provided scope, and it fails closed on missing arrays.
func taskValidateApprovalScope(ctx context.Context, tx pgx.Tx, workspaceID, approvalID, principalID string, scopeRevision int, accounts, targets []string, maxAttempts int) error {
	a, err := taskLoadApproval(ctx, tx, workspaceID, approvalID)
	if err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return err
	}
	if a.Revoked || !a.ExpiresAt.After(now) || a.PrincipalID != principalID || a.ScopeRevision != scopeRevision {
		return problem(taskCodeForbidden)
	}
	if len(accounts) == 0 || len(targets) == 0 {
		return problem(taskCodeForbidden)
	}
	approvedAccounts := make(map[string]struct{}, len(a.AccountIDs))
	for _, id := range a.AccountIDs {
		approvedAccounts[id] = struct{}{}
	}
	approvedTargets := make(map[string]struct{}, len(a.TargetIDs))
	for _, id := range a.TargetIDs {
		approvedTargets[id] = struct{}{}
	}
	for _, id := range accounts {
		if _, ok := approvedAccounts[id]; !ok {
			return problem(taskCodeForbidden)
		}
	}
	for _, id := range targets {
		if _, ok := approvedTargets[id]; !ok {
			return problem(taskCodeForbidden)
		}
	}
	if maxAttempts < 1 || maxAttempts > a.MaxAttempts {
		return problem(taskCodeForbidden)
	}
	return nil
}

// taskAccountIDs returns the task account set in stable order.
func taskAccountIDs(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT account_id FROM core.task_account
		WHERE workspace_id=$1 AND task_id=$2 ORDER BY account_id`, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// taskTargetExternalIDs returns imported target external IDs in stable order.
func taskTargetExternalIDs(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.external_id
		FROM core.task_target tt
		JOIN core.target t ON t.workspace_id=tt.workspace_id AND t.id=tt.target_id
		WHERE tt.workspace_id=$1 AND tt.task_id=$2
		ORDER BY t.external_id`, workspaceID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func taskValidateLiveScope(ctx context.Context, tx pgx.Tx, task TaskRow) error {
	if task.Mode != "live" {
		return nil
	}
	if task.ApprovalID == nil {
		return problem(taskCodeForbidden)
	}
	accounts, err := taskAccountIDs(ctx, tx, task.WorkspaceID, task.ID)
	if err != nil {
		return err
	}
	targets, err := taskTargetExternalIDs(ctx, tx, task.WorkspaceID, task.ID)
	if err != nil {
		return err
	}
	return taskValidateApprovalScope(ctx, tx, task.WorkspaceID, *task.ApprovalID, task.PrincipalID,
		task.ScopeRevision, accounts, targets, task.MaxAttempts)
}

// taskValidateAccounts verifies account ownership and that every account has
// trusted identity evidence. A task may not be created or resumed around an
// unverified identity.
func taskValidateAccounts(ctx context.Context, tx pgx.Tx, workspaceID string, accountIDs []string) error {
	if len(accountIDs) == 0 {
		return problem(taskCodeValidation)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, identity_status FROM core.account
		WHERE workspace_id=$1 AND id = ANY($2::text[])`, workspaceID, accountIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	verified := make(map[string]struct{}, len(accountIDs))
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return err
		}
		if status != "verified" {
			return problem(taskCodeForbidden)
		}
		verified[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range accountIDs {
		if _, ok := verified[id]; !ok {
			return problem(taskCodeValidation)
		}
	}
	return nil
}

// taskValidateEntryBinding enforces the task-entry trust boundary for actors
// carrying a runtime identity: every account must be explicitly allowed by the
// resolved actor, owned by its runtime, and backed by an active runtime
// credential. Operator actors (empty RuntimeID) are governed by their
// permission map and normal task authorization instead.
func taskValidateEntryBinding(ctx context.Context, tx pgx.Tx, actor Actor, accountIDs []string) error {
	if actor.RuntimeID == "" {
		return nil
	}
	if len(accountIDs) == 0 {
		return problem(taskCodeForbidden)
	}
	for _, id := range accountIDs {
		if !contains(actor.AccountIDs, id) {
			return problem(taskCodeForbidden)
		}
		var accountRuntime string
		var credentialActive bool
		err := tx.QueryRow(ctx, `
			SELECT a.runtime_id, EXISTS(
				SELECT 1 FROM core.runtime_credentials rc
				WHERE rc.workspace_id=a.workspace_id AND rc.runtime_id=a.runtime_id
				  AND rc.account_id=a.id AND rc.active)
			FROM core.account a
			WHERE a.workspace_id=$1 AND a.id=$2`,
			actor.WorkspaceID, id).Scan(&accountRuntime, &credentialActive)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(taskCodeForbidden)
		}
		if err != nil {
			return err
		}
		if accountRuntime != actor.RuntimeID || !credentialActive {
			return problem(taskCodeForbidden)
		}
	}
	return nil
}

// taskCreateValidated is the normalized create input produced after all trusted
// checks. Keeping it separate makes the INSERT path impossible to reach with
// unvalidated caller data.
type taskCreateValidated struct {
	taskType    string
	title       string
	mode        string
	deadline    time.Time
	groups      []contract.FixedCandidate
	accounts    []string
	approvalID  *string
	candidateJS []byte
	requestJS   []byte
}

func taskValidateCreateRequest(ctx context.Context, tx pgx.Tx, actor Actor, req contract.CreateTaskRequest) (taskCreateValidated, error) {
	var out taskCreateValidated

	if string(req.Type) != "group.acquire" {
		return qq03ValidateCreateRequest(ctx, tx, actor, req)
	}
	out.taskType = "group.acquire"
	title := strings.TrimSpace(req.Title)
	if title == "" || len(title) > 200 {
		return out, problem(taskCodeValidation)
	}
	out.title = title

	mode := "dry_run"
	if req.ExecutionMode != nil {
		switch string(*req.ExecutionMode) {
		case "dry_run", "read_only", "live":
			mode = string(*req.ExecutionMode)
		default:
			return out, problem(taskCodeValidation)
		}
	}
	out.mode = mode

	if string(req.Goal.Metric) != "new_unique_groups_for_account_pool" ||
		req.Goal.Count < 1 || req.Goal.Count > 10000 ||
		!bool(req.Goal.StrictTargetCap) || !bool(req.Goal.RequireFinalMembershipCheck) {
		return out, problem(taskCodeValidation)
	}
	if req.DeadlineAt.IsZero() || !req.DeadlineAt.After(time.Now()) {
		return out, problem(taskCodeValidation)
	}
	out.deadline = req.DeadlineAt.UTC()

	if string(req.CandidateSource.Kind) != "fixed" || len(req.CandidateSource.Groups) == 0 || len(req.CandidateSource.Groups) > 5000 {
		return out, problem(taskCodeValidation)
	}
	seenGroups := make(map[string]struct{}, len(req.CandidateSource.Groups))
	for _, c := range req.CandidateSource.Groups {
		if !taskValidQQID(c.GroupId) || !taskValidID(c.SourceRef) {
			return out, problem(taskCodeValidation)
		}
		if _, dup := seenGroups[c.GroupId]; dup {
			return out, problem(taskCodeValidation)
		}
		seenGroups[c.GroupId] = struct{}{}
	}
	out.groups = append([]contract.FixedCandidate(nil), req.CandidateSource.Groups...)

	if req.Goal.Count > len(out.groups) {
		return out, problem(taskCodeValidation)
	}

	if req.Limits.MaxCandidates < 1 || req.Limits.MaxCandidates > 5000 ||
		len(out.groups) > req.Limits.MaxCandidates ||
		req.Limits.MaxAttemptsPerAction < 1 || req.Limits.MaxAttemptsPerAction > 10 ||
		req.Limits.MaxTotalExternalAttempts < 1 || req.Limits.MaxTotalExternalAttempts > 10000 ||
		req.Limits.MaxModelAttempts < 0 || req.Limits.MaxModelAttempts > 10 {
		return out, problem(taskCodeValidation)
	}
	if req.Limits.MaxTotalExternalAttempts < req.Goal.Count ||
		req.Limits.MaxTotalExternalAttempts < req.Limits.MaxAttemptsPerAction {
		return out, problem(taskCodeValidation)
	}
	if req.Limits.MaxSearchPagesPerKeyword != nil {
		p := *req.Limits.MaxSearchPagesPerKeyword
		if p < 1 || p > 100 {
			return out, problem(taskCodeValidation)
		}
	}
	if req.Keywords != nil {
		if len(*req.Keywords) > 20 {
			return out, problem(taskCodeValidation)
		}
		seen := make(map[string]struct{}, len(*req.Keywords))
		for _, kw := range *req.Keywords {
			if taskBlank(kw) || len(kw) > 64 {
				return out, problem(taskCodeValidation)
			}
			if _, dup := seen[kw]; dup {
				return out, problem(taskCodeValidation)
			}
			seen[kw] = struct{}{}
		}
	}

	if len(req.AllowedAccountIds) == 0 || len(req.AllowedAccountIds) > 20 {
		return out, problem(taskCodeValidation)
	}
	seenAccounts := make(map[string]struct{}, len(req.AllowedAccountIds))
	for _, id := range req.AllowedAccountIds {
		if !taskValidID(id) {
			return out, problem(taskCodeValidation)
		}
		if _, dup := seenAccounts[id]; dup {
			return out, problem(taskCodeValidation)
		}
		seenAccounts[id] = struct{}{}
		out.accounts = append(out.accounts, id)
	}
	if err := taskValidateAccounts(ctx, tx, actor.WorkspaceID, out.accounts); err != nil {
		return out, err
	}

	if !req.ApprovalRef.IsSpecified() {
		// The wire member is required even when its value is null; omission is
		// indistinguishable from a caller bug and must never default silently.
		return out, problem(taskCodeValidation)
	}
	var approvalRef *string
	if !req.ApprovalRef.IsNull() {
		v := req.ApprovalRef.MustGet()
		if !taskValidID(v) {
			return out, problem(taskCodeValidation)
		}
		approvalRef = &v
	}
	if len([]rune(req.JoinReason)) > 500 || len([]rune(req.VerificationMessage)) > 500 {
		return out, problem(taskCodeValidation)
	}
	if mode == "live" && approvalRef == nil {
		return out, problem(taskCodeValidation)
	}
	if approvalRef != nil {
		a, err := taskLoadApproval(ctx, tx, actor.WorkspaceID, *approvalRef)
		if err != nil {
			return out, err
		}
		if a.PrincipalID != actor.PrincipalID {
			return out, problem(taskCodeForbidden)
		}
		targetIDs := make([]string, 0, len(out.groups))
		for _, c := range out.groups {
			targetIDs = append(targetIDs, c.GroupId)
		}
		if mode == "live" {
			if err := taskValidateApprovalScope(ctx, tx, actor.WorkspaceID, *approvalRef, actor.PrincipalID,
				1, out.accounts, targetIDs, req.Limits.MaxAttemptsPerAction); err != nil {
				return out, err
			}
			if a.JoinReason != req.JoinReason || a.VerificationMessage != req.VerificationMessage {
				return out, problem(taskCodeForbidden)
			}
		}
		out.approvalID = approvalRef
	}

	// The migration CHECK requires the wire object shape, not a bare array.
	wrapped, err := json.Marshal(contract.CandidateSource{Kind: req.CandidateSource.Kind, Groups: out.groups})
	if err != nil {
		return out, err
	}
	out.candidateJS = wrapped
	out.requestJS, err = json.Marshal(req)
	if err != nil {
		return out, err
	}
	return out, nil
}

// qq03NewTypeRegistry lists the QQ business task types this slice recognises,
// persists and reads back through the existing lifecycle without changing any
// group.acquire semantics. None of them has an evidenced adapter execution
// (docs/capabilities/P0-02.md and the friend-request gap), so this matrix is
// also documented in docs/qq03-task-type-matrix.md: submit must fail closed as
// CAPABILITY_UNAVAILABLE with zero write side effects, and nothing here may
// fabricate an executed success.
var qq03NewTypeRegistry = map[string]bool{
	"group.analyze":         true,
	"chat":                  true,
	"customer.add_me":       true,
	"customer.add_customer": true,
}

// qq03ValidateCreateRequest validates a recognised non-group.acquire type.
// Envelope rules (title, accounts, approval member, deadline, limits) follow
// the group.acquire rules; the differences are deliberate and documented:
// execution_mode is limited to dry_run/read_only, join_reason and
// verification_message must stay empty (contract envelope placeholders with no
// execution meaning for these types), approval_ref must stay null (approvals
// only bind the request_join action family), keyword discovery stays closed,
// the goal envelope is fixed at count 1, and the fixed import carries exactly
// one candidate: the analysis group, chat group or customer QQ id the row
// references. submit=true is refused as CAPABILITY_UNAVAILABLE before any row
// is written.
func qq03ValidateCreateRequest(ctx context.Context, tx pgx.Tx, actor Actor, req contract.CreateTaskRequest) (taskCreateValidated, error) {
	var out taskCreateValidated
	if !qq03NewTypeRegistry[string(req.Type)] {
		return out, problem(taskCodeValidation)
	}
	out.taskType = string(req.Type)

	title := strings.TrimSpace(req.Title)
	if title == "" || len(title) > 200 {
		return out, problem(taskCodeValidation)
	}
	out.title = title

	mode := "dry_run"
	if req.ExecutionMode != nil {
		switch string(*req.ExecutionMode) {
		case "dry_run", "read_only":
			mode = string(*req.ExecutionMode)
		default:
			return out, problem(taskCodeValidation)
		}
	}
	out.mode = mode

	if req.JoinReason != "" || req.VerificationMessage != "" {
		return out, problem(taskCodeValidation)
	}
	if req.Keywords != nil || req.Limits.MaxSearchPagesPerKeyword != nil {
		return out, problem(taskCodeValidation)
	}
	if string(req.Goal.Metric) != "new_unique_groups_for_account_pool" ||
		req.Goal.Count != 1 ||
		!bool(req.Goal.StrictTargetCap) || !bool(req.Goal.RequireFinalMembershipCheck) {
		return out, problem(taskCodeValidation)
	}
	if req.DeadlineAt.IsZero() || !req.DeadlineAt.After(time.Now()) {
		return out, problem(taskCodeValidation)
	}
	out.deadline = req.DeadlineAt.UTC()

	if string(req.CandidateSource.Kind) != "fixed" || len(req.CandidateSource.Groups) != 1 {
		return out, problem(taskCodeValidation)
	}
	candidate := req.CandidateSource.Groups[0]
	if !taskValidQQID(candidate.GroupId) || !taskValidID(candidate.SourceRef) {
		return out, problem(taskCodeValidation)
	}
	out.groups = []contract.FixedCandidate{candidate}

	if req.Limits.MaxCandidates < 1 || req.Limits.MaxCandidates > 5000 ||
		len(out.groups) > req.Limits.MaxCandidates ||
		req.Limits.MaxAttemptsPerAction < 1 || req.Limits.MaxAttemptsPerAction > 10 ||
		req.Limits.MaxTotalExternalAttempts < 1 || req.Limits.MaxTotalExternalAttempts > 10000 ||
		req.Limits.MaxModelAttempts < 0 || req.Limits.MaxModelAttempts > 10 ||
		req.Limits.MaxTotalExternalAttempts < req.Limits.MaxAttemptsPerAction {
		return out, problem(taskCodeValidation)
	}

	if len(req.AllowedAccountIds) == 0 || len(req.AllowedAccountIds) > 20 {
		return out, problem(taskCodeValidation)
	}
	seenAccounts := make(map[string]struct{}, len(req.AllowedAccountIds))
	for _, id := range req.AllowedAccountIds {
		if !taskValidID(id) {
			return out, problem(taskCodeValidation)
		}
		if _, dup := seenAccounts[id]; dup {
			return out, problem(taskCodeValidation)
		}
		seenAccounts[id] = struct{}{}
		out.accounts = append(out.accounts, id)
	}
	if err := taskValidateAccounts(ctx, tx, actor.WorkspaceID, out.accounts); err != nil {
		return out, err
	}

	if !req.ApprovalRef.IsSpecified() {
		// The wire member is required even when its value is null; omission is
		// indistinguishable from a caller bug and must never default silently.
		return out, problem(taskCodeValidation)
	}
	if !req.ApprovalRef.IsNull() {
		// No approval target exists without an execution chain; keeping
		// approval_ref null is part of the fail-closed contract for these types.
		return out, problem(taskCodeValidation)
	}

	if req.Submit {
		return out, problem(taskCodeCapabilityUnavailable)
	}

	wrapped, err := json.Marshal(contract.CandidateSource{Kind: req.CandidateSource.Kind, Groups: out.groups})
	if err != nil {
		return out, err
	}
	out.candidateJS = wrapped
	out.requestJS, err = json.Marshal(req)
	if err != nil {
		return out, err
	}
	return out, nil
}

func taskProjection(row TaskRow, createdAt time.Time) contract.Task {
	return contract.Task{
		BlockedReason:         nullable.NewNullNullable[string](),
		ConfirmedUniqueCount:  0,
		CreatedAt:             contract.UTCDateTime(createdAt.UTC()),
		DeadlineAt:            contract.UTCDateTime(row.Deadline.UTC()),
		ExecutionMode:         contract.ExecutionMode(row.Mode),
		GoalCount:             row.GoalCount,
		Id:                    row.ID,
		PendingExternalCount:  0,
		ReservedTargetCount:   0,
		ScopeRevision:         row.ScopeRevision,
		State:                 contract.TaskState(row.State),
		Title:                 row.Title,
		Type:                  contract.TaskType(row.Type),
		UnknownExecutionCount: 0,
		OrchestrationMode:     contract.OrchestrationMode(row.OrchestrationMode),
		AuthorizationRevision: row.AuthorizationRevision,
		Version:               row.Version,
		WorkspaceId:           row.WorkspaceID,
	}
}

func taskEnsurePrincipal(ctx context.Context, tx pgx.Tx, actor Actor) error {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM core.principal WHERE workspace_id=$1 AND id=$2)`,
		actor.WorkspaceID, actor.PrincipalID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return problem(taskCodeUnauthenticated)
	}
	return nil
}

// CreateTask creates one fixed-import acquire task. The request key is scoped
// by workspace/principal/method/path; a replay never creates a second task and
// a key reuse with a different body is rejected.
func (s *Store) CreateTask(ctx context.Context, actor Actor, key string, request contract.CreateTaskRequest) (contract.Task, error) {
	if err := actor.require(taskPermCreate); err != nil {
		return contract.Task{}, err
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return contract.Task{}, err
	}
	bodyDigest, err := digest(request)
	if err != nil {
		return contract.Task{}, err
	}

	var out contract.Task
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := taskEnsurePrincipal(ctx, tx, actor); err != nil {
			return err
		}
		if err := taskValidateEntryBinding(ctx, tx, actor, request.AllowedAccountIds); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", taskCreatePath, key)
		if err != nil {
			return err
		}
		if found {
			if rec.digest != bodyDigest {
				return problem(taskCodeIdempotency)
			}
			if err := json.Unmarshal(rec.response, &out); err != nil {
				return problem(taskCodeStoreUnavailable)
			}
			return nil
		}

		v, err := taskValidateCreateRequest(ctx, tx, actor, request)
		if err != nil {
			return err
		}

		taskID := newID("task")
		state := "DRAFT"
		if request.Submit {
			state = "VALIDATING"
		}
		var createdAt time.Time
		var approvalArg any
		if v.approvalID != nil {
			approvalArg = *v.approvalID
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO core.task(
				workspace_id,id,principal_id,title,state,mode,version,scope_revision,goal_count,
				deadline_at,approval_id,baseline_complete,max_attempts,max_total_attempts,
				candidate_source,request_payload,import_offset,import_done)
			VALUES($1,$2,$3,$4,$5,$6,1,1,$7,$8,$9,false,$10,$11,$12,$13,0,false)
			RETURNING created_at`,
			actor.WorkspaceID, taskID, actor.PrincipalID, v.title, state, v.mode,
			request.Goal.Count, v.deadline, approvalArg,
			request.Limits.MaxAttemptsPerAction, request.Limits.MaxTotalExternalAttempts,
			v.candidateJS, v.requestJS,
		).Scan(&createdAt)
		if err != nil {
			return err
		}

		for _, accountID := range v.accounts {
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.task_account(workspace_id,task_id,account_id)
				VALUES($1,$2,$3)`, actor.WorkspaceID, taskID, accountID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO core.goal_slot(workspace_id,task_id,slot_no,state)
			SELECT $1,$2,g,'free'::text FROM generate_series(1,$3::int) AS g`, actor.WorkspaceID, taskID, request.Goal.Count); err != nil {
			return err
		}

		if err := appendEvent(ctx, tx, actor.WorkspaceID, taskID, "task.created", actor.PrincipalID, map[string]any{
			"type":            string(request.Type),
			"mode":            v.mode,
			"goal_count":      request.Goal.Count,
			"candidate_count": len(v.groups),
			"accounts":        v.accounts,
			"submitted":       request.Submit,
		}); err != nil {
			return err
		}

		out = taskProjection(TaskRow{
			WorkspaceID:   actor.WorkspaceID,
			ID:            taskID,
			PrincipalID:   actor.PrincipalID,
			State:         state,
			Mode:          v.mode,
			Title:         v.title,
			Type:          v.taskType,
			Version:       1,
			ScopeRevision: 1,
			GoalCount:     request.Goal.Count,
			MaxAttempts:   request.Limits.MaxAttemptsPerAction,
			Deadline:      v.deadline,
			ApprovalID:    v.approvalID,
		}, createdAt)
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", taskCreatePath, key, bodyDigest, taskID, out)
	})
	if err != nil {
		// Two identical concurrent creates can race on the idempotency PK. The
		// loser must return the winner's stored response, not a spurious failure.
		if taskUniqueViolation(err) {
			var replay contract.Task
			lookupErr := s.tx(ctx, func(tx pgx.Tx) error {
				rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", taskCreatePath, key)
				if err != nil {
					return err
				}
				if !found {
					return problem(taskCodeStoreUnavailable)
				}
				if rec.digest != bodyDigest {
					return problem(taskCodeIdempotency)
				}
				return json.Unmarshal(rec.response, &replay)
			})
			if lookupErr != nil {
				return contract.Task{}, lookupErr
			}
			return replay, nil
		}
		return contract.Task{}, err
	}
	return out, nil
}

// taskLocalFlightCount counts work that can still become a local send: an
// execution the bridge may still dispatch, an issued and unexpired permit, and
// a managed adapter call still executing in this process. The process-local
// part is deliberately not a cross-process claim: recovery may already have
// rewritten the durable row to UNKNOWN while the local adapter has not
// returned. UNKNOWN outcomes, returned/pending external requests and retained
// send locks are not local flight; they keep their slot, claim, quota and lock
// for read-only observation, but they do not keep a pause from settling.
// This function never mutates or releases uncertainty itself.
func taskLocalFlightCount(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM core.execution e
			 WHERE e.workspace_id=$1 AND e.task_id=$2
			   AND e.state='DISPATCHING')
			+
			(SELECT count(*) FROM core.dispatch_permit p
			 JOIN core.execution e ON (e.workspace_id,e.id)=(p.workspace_id,p.execution_id)
			 WHERE p.workspace_id=$1 AND e.task_id=$2
			   AND p.state='issued' AND p.expires_at > now())`,
		workspaceID, taskID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n + activeLocalDispatches.count(workspaceID, taskID), nil
}

// settlePausingTask performs only the local lifecycle transition. It never
// frees slots, claims, usage or execution evidence.
func settlePausingTask(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) error {
	task, err := lockTask(ctx, tx, workspaceID, taskID)
	if err != nil {
		return err
	}
	if task.State != "PAUSING" {
		return nil
	}
	flights, err := taskLocalFlightCount(ctx, tx, workspaceID, taskID)
	if err != nil {
		return err
	}
	if flights != 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE core.task SET state='PAUSED',version=version+1 WHERE workspace_id=$1 AND id=$2 AND state='PAUSING'`, workspaceID, taskID); err != nil {
		return err
	}
	return appendEvent(ctx, tx, workspaceID, taskID, "task.pause_settled", "reconciler", map[string]any{"state": "PAUSED"})
}

func taskExecutionCount(ctx context.Context, tx pgx.Tx, workspaceID, taskID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM core.execution WHERE workspace_id=$1 AND task_id=$2`, workspaceID, taskID).Scan(&n)
	return n, err
}

// ControlTask applies one CAS-guarded lifecycle transition. Version checks are
// evaluated only for a fresh (non-replayed) request, but authorization is
// always evaluated before a replay is returned.
func (s *Store) ControlTask(ctx context.Context, actor Actor, taskID, key string, request contract.TaskControlRequest) (contract.Receipt, error) {
	if err := actor.require(taskPermControl); err != nil {
		return contract.Receipt{}, err
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return contract.Receipt{}, err
	}
	if request.ExpectedVersion < 1 || !request.Action.Valid() || taskBlank(request.Reason) || len(request.Reason) > 500 {
		return contract.Receipt{}, problem(taskCodeValidation)
	}
	if !taskValidID(taskID) {
		return contract.Receipt{}, problem(taskCodeNotFound)
	}
	if actor.RuntimeID != "" {
		// A task-entry runtime may pause or cancel its own task but may never
		// start or resume work; those are operator decisions.
		switch request.Action {
		case contract.TaskControlRequestActionStart, contract.TaskControlRequestActionResume:
			return contract.Receipt{}, problem(taskCodeForbidden)
		}
	}
	bodyDigest, err := digest(request)
	if err != nil {
		return contract.Receipt{}, err
	}
	path := taskControlPath(taskID)

	var out contract.Receipt
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, taskPermControl); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
		if err != nil {
			return err
		}
		if found {
			if rec.digest != bodyDigest {
				return problem(taskCodeIdempotency)
			}
			if err := json.Unmarshal(rec.response, &out); err != nil {
				return problem(taskCodeStoreUnavailable)
			}
			return nil
		}
		if request.ExpectedVersion != task.Version {
			return problem(taskCodeVersionConflict)
		}

		nextState, err := taskControlNextState(ctx, tx, task, request.Action)
		if err != nil {
			return err
		}

		if request.Action == contract.TaskControlRequestActionStart {
			if err := workSettingsLock(ctx, tx, actor.WorkspaceID); err != nil {
				return err
			}
			settings, err := s.loadWorkSettings(ctx, tx, actor.WorkspaceID)
			if err != nil {
				return err
			}
			slots := settings.WorkSlots
			if _, err = tx.Exec(ctx, `SELECT set_config('bba.task_work_slots',$1,true)`, fmt.Sprint(slots)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE core.task SET state=$3, version=version+1
			WHERE workspace_id=$1 AND id=$2 AND version=$4`,
			actor.WorkspaceID, taskID, nextState, task.Version); err != nil {
			return err
		}

		if err := appendEvent(ctx, tx, actor.WorkspaceID, taskID, "task.control", actor.PrincipalID, map[string]any{
			"action":           string(request.Action),
			"reason":           request.Reason,
			"expected_version": request.ExpectedVersion,
			"from":             task.State,
			"to":               nextState,
		}); err != nil {
			return err
		}
		if request.Action == contract.TaskControlRequestActionCancel {
			// Cancelling the task under the task lock also closes every durable
			// work item that provably produced no execution. Items waiting for a
			// trusted result stay observed and are settled by the bridge.
			if err := s.cancelWorkItemsUnderTask(ctx, tx, task); err != nil {
				return err
			}
		}

		var acceptedAt time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&acceptedAt); err != nil {
			return err
		}
		out = contract.Receipt{
			AcceptedAt:      contract.UTCDateTime(acceptedAt.UTC()),
			Action:          contract.ReceiptAction(request.Action),
			ExpectedVersion: request.ExpectedVersion,
			Id:              newID("receipt"),
			ResourceId:      taskID,
			State:           contract.Accepted,
		}
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, taskID, out)
	})
	if err != nil {
		if taskUniqueViolation(err) {
			var replay contract.Receipt
			lookupErr := s.tx(ctx, func(tx pgx.Tx) error {
				rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
				if err != nil {
					return err
				}
				if !found {
					return problem(taskCodeStoreUnavailable)
				}
				if rec.digest != bodyDigest {
					return problem(taskCodeIdempotency)
				}
				return json.Unmarshal(rec.response, &replay)
			})
			if lookupErr != nil {
				return contract.Receipt{}, lookupErr
			}
			return replay, nil
		}
		return contract.Receipt{}, err
	}
	return out, nil
}

func taskControlNextState(ctx context.Context, tx pgx.Tx, task TaskRow, action contract.TaskControlRequestAction) (string, error) {
	switch action {
	case contract.TaskControlRequestActionStart:
		// A persisted draft has no separate submit endpoint: an operator
		// start is the submission gate. Import, baseline, deadline, scope and
		// account checks below still apply, so a draft can never run on
		// unverified scope. VALIDATING means the caller already submitted it.
		switch task.State {
		case "DRAFT", "VALIDATING", "READY":
		default:
			return "", problem(taskCodeStateConflict)
		}
		if !task.ImportDone || !task.BaselineComplete {
			return "", problem(taskCodeStateConflict)
		}
		if !task.Deadline.After(time.Now()) {
			return "", problem(taskCodeStateConflict)
		}
		if err := taskValidateLiveScope(ctx, tx, task); err != nil {
			return "", err
		}
		accounts, err := taskAccountIDs(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return "", err
		}
		if err := taskValidateAccounts(ctx, tx, task.WorkspaceID, accounts); err != nil {
			return "", err
		}
		return "RUNNING", nil
	case contract.TaskControlRequestActionPause:
		switch task.State {
		case "RUNNING", "WAITING_EXTERNAL", "VERIFYING":
		default:
			return "", problem(taskCodeStateConflict)
		}
		n, err := taskLocalFlightCount(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return "", err
		}
		if n > 0 {
			return "PAUSING", nil
		}
		return "PAUSED", nil
	case contract.TaskControlRequestActionResume:
		switch task.State {
		case "PAUSED", "PAUSING", "BLOCKED":
		default:
			return "", problem(taskCodeStateConflict)
		}
		if !task.ImportDone || !task.BaselineComplete {
			return "", problem(taskCodeStateConflict)
		}
		if !task.Deadline.After(time.Now()) {
			return "", problem(taskCodeStateConflict)
		}
		n, err := taskLocalFlightCount(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return "", err
		}
		if n > 0 {
			// Resume must not pretend a local send still in flight is fresh
			// queued work; returned/unknown external requests stay observed.
			return "", problem(taskCodeStateConflict)
		}
		if err := taskValidateLiveScope(ctx, tx, task); err != nil {
			return "", err
		}
		accounts, err := taskAccountIDs(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return "", err
		}
		if err := taskValidateAccounts(ctx, tx, task.WorkspaceID, accounts); err != nil {
			return "", err
		}
		return "RUNNING", nil
	case contract.TaskControlRequestActionCancel:
		switch task.State {
		case "CANCELLED", "SUCCEEDED", "PARTIAL", "FAILED":
			return "", problem(taskCodeStateConflict)
		}
		return "CANCELLED", nil
	default:
		return "", problem(taskCodeValidation)
	}
}

// ExpandFixedPage imports one deterministic page of the fixed candidate list.
// It is pure template expansion: no model call, no search provider, and no
// external request. Offset and inserted rows commit in one transaction.
func (s *Store) ExpandFixedPage(ctx context.Context, actor Actor, taskID string, pageSize int) (bool, error) {
	if err := actor.require(taskPermControl); err != nil {
		return false, err
	}
	if pageSize < 1 || pageSize > 500 {
		return false, problem(taskCodeValidation)
	}
	if !taskValidID(taskID) {
		return false, problem(taskCodeNotFound)
	}
	done := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, taskPermControl); err != nil {
			return err
		}
		if task.ImportDone {
			done = true
			return nil
		}
		switch task.State {
		case "DRAFT", "VALIDATING":
		default:
			return problem(taskCodeStateConflict)
		}
		n, err := taskExecutionCount(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if n > 0 {
			return problem(taskCodeStateConflict)
		}

		var source contract.CandidateSource
		var raw []byte
		if err := tx.QueryRow(ctx, `
			SELECT candidate_source FROM core.task WHERE workspace_id=$1 AND id=$2`,
			actor.WorkspaceID, taskID).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &source); err != nil {
			return problem(taskCodeStateConflict)
		}
		if len(source.Groups) == 0 {
			// A fixed import is never guessed empty.
			return problem(taskCodeStateConflict)
		}
		offset := task.ImportOffset
		if offset < 0 || offset > len(source.Groups) {
			return problem(taskCodeStateConflict)
		}
		if offset == len(source.Groups) {
			if _, err := tx.Exec(ctx, `
				UPDATE core.task SET import_done=true, version=version+1
				WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, taskID); err != nil {
				return err
			}
			done = true
			return nil
		}

		end := offset + pageSize
		if end > len(source.Groups) {
			end = len(source.Groups)
		}
		for _, candidate := range source.Groups[offset:end] {
			var targetID string
			var disabled bool
			err := tx.QueryRow(ctx, `
				INSERT INTO core.target(workspace_id,id,external_id)
				VALUES($1,$2,$3)
				ON CONFLICT (workspace_id,external_id)
				DO UPDATE SET external_id=EXCLUDED.external_id
				RETURNING id, disabled`,
				actor.WorkspaceID, newID("target"), candidate.GroupId,
			).Scan(&targetID, &disabled)
			if err != nil {
				return err
			}
			eligibility := "eligible"
			if disabled {
				eligibility = "excluded"
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.task_target(workspace_id,task_id,target_id,source_ref,baseline_covered,eligibility)
				VALUES($1,$2,$3,$4,false,$5)
				ON CONFLICT (workspace_id,task_id,target_id) DO NOTHING`,
				actor.WorkspaceID, taskID, targetID, candidate.SourceRef, eligibility); err != nil {
				return err
			}
			if disabled {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.subtask(workspace_id,id,task_id,target_id,kind,state,max_attempts)
				VALUES($1,$2,$3,$4,'ensure_membership','QUEUED',$5)
				ON CONFLICT (workspace_id,task_id,target_id,kind) DO NOTHING`,
				actor.WorkspaceID, newID("subtask"), taskID, targetID, task.MaxAttempts); err != nil {
				return err
			}
		}

		done = end == len(source.Groups)
		if _, err := tx.Exec(ctx, `
			UPDATE core.task SET import_offset=$3, import_done=$4, version=version+1
			WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, taskID, end, done); err != nil {
			return err
		}
		return appendEvent(ctx, tx, actor.WorkspaceID, taskID, "task.import_expanded", actor.PrincipalID, map[string]any{
			"from":      offset,
			"to":        end,
			"page_size": pageSize,
			"done":      done,
		})
	})
	if err != nil {
		return false, err
	}
	return done, nil
}

// CompleteBaseline records trusted, read-only per-account membership proof.
// No snapshot is treated as an empty successful baseline: a complete proof is
// required for every task account before the task may become READY.
func (s *Store) CompleteBaseline(ctx context.Context, actor Actor, taskID string, snapshots []BaselineSnapshot) error {
	if err := actor.require(taskPermControl); err != nil {
		return err
	}
	if len(snapshots) == 0 {
		return problem(taskCodeValidation)
	}
	if !taskValidID(taskID) {
		return problem(taskCodeNotFound)
	}
	seen := make(map[string]struct{}, len(snapshots))
	for _, snap := range snapshots {
		if !taskValidID(snap.AccountID) || !taskValidID(snap.EvidenceRef) {
			return problem(taskCodeValidation)
		}
		if _, dup := seen[snap.AccountID]; dup {
			return problem(taskCodeValidation)
		}
		seen[snap.AccountID] = struct{}{}
		if snap.ObservedAt.IsZero() || snap.ObservedAt.After(time.Now().Add(5*time.Minute)) {
			return problem(taskCodeValidation)
		}
		groups := make(map[string]struct{}, len(snap.GroupIDs))
		for _, g := range snap.GroupIDs {
			if !taskValidQQID(g) {
				return problem(taskCodeValidation)
			}
			if _, dup := groups[g]; dup {
				return problem(taskCodeValidation)
			}
			groups[g] = struct{}{}
		}
	}

	return s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, taskPermControl); err != nil {
			return err
		}
		if task.BaselineComplete {
			// Baseline proof is a revision-scoped terminal fact.
			return nil
		}
		switch task.State {
		case "DRAFT", "VALIDATING":
		default:
			return problem(taskCodeStateConflict)
		}
		if !task.ImportDone {
			return problem(taskCodeStateConflict)
		}
		n, err := taskExecutionCount(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if n > 0 {
			return problem(taskCodeStateConflict)
		}
		if task.Mode == "live" {
			if err := taskValidateLiveScope(ctx, tx, task); err != nil {
				return err
			}
		}

		accounts, err := taskAccountIDs(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		accountSet := make(map[string]struct{}, len(accounts))
		for _, id := range accounts {
			accountSet[id] = struct{}{}
		}
		if len(accounts) == 0 {
			return problem(taskCodeStateConflict)
		}
		for _, snap := range snapshots {
			if _, ok := accountSet[snap.AccountID]; !ok {
				return problem(taskCodeValidation)
			}
		}

		for _, snap := range snapshots {
			if _, err := tx.Exec(ctx, `
				INSERT INTO core.baseline_account AS ba(workspace_id,task_id,account_id,evidence_ref,observed_at,complete)
				VALUES($1,$2,$3,$4,$5,$6)
				ON CONFLICT (workspace_id,task_id,account_id) DO UPDATE SET
					evidence_ref=CASE WHEN EXCLUDED.complete OR NOT ba.complete THEN EXCLUDED.evidence_ref ELSE ba.evidence_ref END,
					observed_at=GREATEST(ba.observed_at, EXCLUDED.observed_at),
					complete=(ba.complete OR EXCLUDED.complete)`,
				actor.WorkspaceID, taskID, snap.AccountID, snap.EvidenceRef, snap.ObservedAt.UTC(), snap.Complete); err != nil {
				return err
			}
			if !snap.Complete {
				continue
			}
			for _, group := range snap.GroupIDs {
				if _, err := tx.Exec(ctx, `
					INSERT INTO core.baseline AS b(workspace_id,task_id,account_id,target_external_id,evidence_ref,observed_at)
					VALUES($1,$2,$3,$4,$5,$6)
					ON CONFLICT (workspace_id,task_id,account_id,target_external_id) DO UPDATE SET
						evidence_ref=EXCLUDED.evidence_ref,
						observed_at=GREATEST(b.observed_at, EXCLUDED.observed_at)`,
					actor.WorkspaceID, taskID, snap.AccountID, group, snap.EvidenceRef, snap.ObservedAt.UTC()); err != nil {
					return err
				}
			}
		}

		// Targets already covered by any allowed task account are never new.
		if _, err := tx.Exec(ctx, `
			UPDATE core.task_target tt SET baseline_covered=true, eligibility='already_covered'
			FROM core.target t
			WHERE tt.workspace_id=$1 AND tt.task_id=$2
			  AND tt.eligibility='eligible'
			  AND t.workspace_id=tt.workspace_id AND t.id=tt.target_id
			  AND EXISTS (
				SELECT 1 FROM core.baseline b
				JOIN core.baseline_account ba
				  ON ba.workspace_id=b.workspace_id AND ba.task_id=b.task_id AND ba.account_id=b.account_id
				 AND ba.complete AND ba.evidence_ref=b.evidence_ref
				WHERE b.workspace_id=$1 AND b.task_id=$2
				  AND b.target_external_id=t.external_id)`,
			actor.WorkspaceID, taskID); err != nil {
			return err
		}

		var incomplete int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM core.task_account ta
			LEFT JOIN core.baseline_account ba
			  ON ba.workspace_id=ta.workspace_id AND ba.task_id=ta.task_id AND ba.account_id=ta.account_id
			WHERE ta.workspace_id=$1 AND ta.task_id=$2
			  AND (ba.complete IS NULL OR NOT ba.complete)`,
			actor.WorkspaceID, taskID).Scan(&incomplete); err != nil {
			return err
		}

		allComplete := incomplete == 0
		nextState := task.State
		baselineRef := ""
		if allComplete {
			baselineRef = newID("baseline")
			if task.State == "VALIDATING" {
				nextState = "READY"
			}
			if _, err := tx.Exec(ctx, `
				UPDATE core.task SET state=$3, baseline_complete=true, baseline_ref=$4, version=version+1
				WHERE workspace_id=$1 AND id=$2`,
				actor.WorkspaceID, taskID, nextState, baselineRef); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE core.task SET version=version+1
				WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, taskID); err != nil {
				return err
			}
		}

		detail := map[string]any{
			"complete_accounts": len(snapshots),
			"all_complete":      allComplete,
			"state":             nextState,
			"accounts":          taskSnapshotsDetail(snapshots),
		}
		if baselineRef != "" {
			detail["baseline_ref"] = baselineRef
		}
		kind := "task.baseline_progress"
		if allComplete {
			kind = "task.baseline_completed"
		}
		return appendEvent(ctx, tx, actor.WorkspaceID, taskID, kind, actor.PrincipalID, detail)
	})
}

func taskSnapshotsDetail(snapshots []BaselineSnapshot) []map[string]any {
	out := make([]map[string]any, 0, len(snapshots))
	for _, s := range snapshots {
		out = append(out, map[string]any{
			"account_id":   s.AccountID,
			"evidence_ref": s.EvidenceRef,
			"observed_at":  s.ObservedAt.UTC(),
			"complete":     s.Complete,
			"group_count":  len(s.GroupIDs),
		})
	}
	return out
}
