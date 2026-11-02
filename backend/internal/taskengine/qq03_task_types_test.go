package taskengine

// QQ-03 task-type matrix tests (offline PostgreSQL fixture only; no live QQ,
// no real writes). Contract: every QQ business type is recognised, persisted
// and readable through the existing Task Engine lifecycle. group.acquire keeps
// its existing execution chain unchanged. group.analyze, chat,
// customer.add_me and customer.add_customer have no evidenced adapter execution
// (docs/capabilities/P0-02.md and the friend-request gap), so submit must fail
// closed as CAPABILITY_UNAVAILABLE with zero write side effects, unknown types
// stay rejected, and nothing in this slice may fabricate execution success.

import (
	"context"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/oapi-codegen/nullable"
)

// qq03NewTypes are the types this slice wires for identification,
// persistence and read-back only; execution stays explicitly unsupported.
var qq03NewTypes = []string{"group.analyze", "chat", "customer.add_me", "customer.add_customer"}

func qq03Request(typ, mode string, submit bool) contract.CreateTaskRequest {
	fixed := []contract.FixedCandidate{{GroupId: "900300001", SourceRef: "qq03_import"}}
	m := contract.ExecutionMode(mode)
	return contract.CreateTaskRequest{
		Type:                contract.CreateTaskRequestType(typ),
		JoinReason:          "",
		VerificationMessage: "",
		Title:               "qq03 fixture task",
		Goal:                contract.Goal{Metric: contract.NewUniqueGroupsForAccountPool, Count: 1, StrictTargetCap: true, RequireFinalMembershipCheck: true},
		AllowedAccountIds:   []string{"acc_qq03"},
		CandidateSource:     contract.CandidateSource{Kind: contract.CandidateSourceKind("fixed"), Groups: fixed},
		ExecutionMode:       &m,
		DeadlineAt:          time.Now().UTC().Add(time.Hour),
		Limits:              contract.TaskLimits{MaxCandidates: 1, MaxAttemptsPerAction: 1, MaxTotalExternalAttempts: 1, MaxModelAttempts: 0},
		ApprovalRef:         nullable.NewNullNullable[string](),
		Submit:              submit,
	}
}

func qq03Count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestQQ03TypeMatrixCreateAndReadBack asserts each supported type is
// recognised, persisted as a DRAFT and read back with its real type, including
// read_only mode for the read-only analysis type.
func TestQQ03TypeMatrixCreateAndReadBack(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_qq03", "rt_qq03", 1)
	actor := fixtureActor()
	ctx := context.Background()
	for _, typ := range append([]string{"group.acquire"}, qq03NewTypes...) {
		for _, mode := range []string{"dry_run", "read_only"} {
			req := ttRequest([]string{"900300001"}, []string{"acc_qq03"}, 1)
			if typ != "group.acquire" {
				req = qq03Request(typ, mode, false)
			} else {
				req.Submit = false
			}
			if mode == "read_only" {
				m := contract.ExecutionMode("read_only")
				req.ExecutionMode = &m
			}
			key := "qq03-matrix-" + typ + "-" + mode
			task, err := s.CreateTask(ctx, actor, key, req)
			if err != nil {
				t.Fatalf("create %s/%s: %v", typ, mode, Code(err))
			}
			if task.State != contract.TaskState("DRAFT") {
				t.Fatalf("create %s/%s state=%s want DRAFT", typ, mode, task.State)
			}
			got, err := s.GetTask(ctx, actor, task.Id)
			if err != nil {
				t.Fatalf("read back %s/%s: %v", typ, mode, Code(err))
			}
			if string(got.Type) != typ {
				t.Fatalf("read back type=%s want %s", got.Type, typ)
			}
		}
	}
}

// TestQQ03UnknownTypeRejected keeps the closed union: unknown types never
// persist and are never executed.
func TestQQ03UnknownTypeRejected(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_qq03", "rt_qq03", 1)
	ctx := context.Background()
	_, err := s.CreateTask(ctx, fixtureActor(), "qq03-unknown", qq03Request("customer.ping", "dry_run", false))
	if Code(err) != taskCodeValidation {
		t.Fatalf("unknown type code=%v want %s", Code(err), taskCodeValidation)
	}
	if n := qq03Count(t, s, `SELECT count(*) FROM core.task`); n != 0 {
		t.Fatalf("unknown type persisted %d tasks", n)
	}
}

// TestQQ03UnsupportedSubmitFailClosed: submitting a type without an evidenced
// execution chain fails closed as CAPABILITY_UNAVAILABLE before any state is
// written: no task row, no executions, no reservations, no outbox rows, no
// chat permit chain rows. Nothing is fabricated as success and nothing can be
// auto-resent because nothing was dispatched.
func TestQQ03UnsupportedSubmitFailClosed(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_qq03", "rt_qq03", 1)
	ctx := context.Background()
	for _, typ := range qq03NewTypes {
		_, err := s.CreateTask(ctx, fixtureActor(), "qq03-submit-"+typ, qq03Request(typ, "dry_run", true))
		if Code(err) != taskCodeCapabilityUnavailable {
			t.Fatalf("submit %s code=%v want %s", typ, Code(err), taskCodeCapabilityUnavailable)
		}
	}
	if n := qq03Count(t, s, `SELECT count(*) FROM core.task`); n != 0 {
		t.Fatalf("fail-closed submit persisted %d task rows", n)
	}
	for _, q := range []string{
		`SELECT count(*) FROM core.execution`,
		`SELECT count(*) FROM core.goal_slot WHERE state<>'free'`,
		`SELECT count(*) FROM core.outbox`,
		`SELECT count(*) FROM bridge.outbox`,
		`SELECT count(*) FROM core.chat_send_permit`,
		`SELECT count(*) FROM core.chat_send_usage`,
		`SELECT count(*) FROM core.chat_send_outcome`,
		`SELECT count(*) FROM core.chat_trigger`,
	} {
		if n := qq03Count(t, s, q); n != 0 {
			t.Fatalf("zero write side effects violated: %s -> %d", q, n)
		}
	}
}

// TestQQ03CrossScopeFailClosed: task rows are workspace-scoped on read, and a
// new-type task cannot bind an account from another workspace.
func TestQQ03CrossScopeFailClosed(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_qq03", "rt_qq03", 1)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, fixtureActor(), "qq03-scope", qq03Request("chat", "read_only", false))
	if err != nil {
		t.Fatalf("create chat draft: %v", Code(err))
	}
	other := Actor{WorkspaceID: "ws_qq03b", PrincipalID: "principal_qq03b", Permissions: map[string]bool{"task.read": true, "task.create": true}}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_qq03b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES('ws_qq03b','principal_qq03b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTask(ctx, other, task.Id); Code(err) != taskCodeNotFound {
		t.Fatalf("cross-workspace read code=%v want %s", Code(err), taskCodeNotFound)
	}
	if _, err := s.CreateTask(ctx, other, "qq03-scope-b", qq03Request("chat", "read_only", false)); Code(err) != taskCodeValidation {
		t.Fatalf("cross-workspace account code=%v want %s", Code(err), taskCodeValidation)
	}
}

// TestQQ03LegacyGroupAcquireChainUnchanged: the only type with an existing
// execution chain must keep its pre-existing submit behaviour (accepted for
// validation), proving the new gate never touched the old semantics.
func TestQQ03LegacyGroupAcquireChainUnchanged(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_qq03", "rt_qq03", 1)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, fixtureActor(), "qq03-legacy", ttRequest([]string{"900300001"}, []string{"acc_qq03"}, 1))
	if err != nil {
		t.Fatalf("legacy group.acquire submit: %v", Code(err))
	}
	if task.State != contract.TaskState("VALIDATING") {
		t.Fatalf("legacy submit state=%s want VALIDATING", task.State)
	}
	got, err := s.GetTask(ctx, fixtureActor(), task.Id)
	if err != nil || string(got.Type) != "group.acquire" {
		t.Fatalf("legacy read back type=%s err=%v", got.Type, Code(err))
	}
}

// TestQQ03ChatTaskKeepsChatPermitChainSeparate: a chat task type is a Task
// Engine lifecycle object only; it must not touch the daily chat-send permit
// chain (which remains the separate runtime send boundary).
func TestQQ03ChatTaskKeepsChatPermitChainSeparate(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_qq03", "rt_qq03", 1)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, fixtureActor(), "qq03-chat-sep", qq03Request("chat", "read_only", false))
	if err != nil {
		t.Fatalf("create chat draft: %v", Code(err))
	}
	if got, err := s.GetTask(ctx, fixtureActor(), task.Id); err != nil || string(got.Type) != "chat" {
		t.Fatalf("chat read back type=%s err=%v", got.Type, Code(err))
	}
	for _, q := range []string{
		`SELECT count(*) FROM core.chat_send_permit`,
		`SELECT count(*) FROM core.chat_send_usage`,
		`SELECT count(*) FROM core.chat_send_outcome`,
		`SELECT count(*) FROM core.chat_trigger`,
		`SELECT count(*) FROM core.chat_send_policy`,
	} {
		if n := qq03Count(t, s, q); n != 0 {
			t.Fatalf("chat task leaked into permit chain: %s -> %d", q, n)
		}
	}
}
