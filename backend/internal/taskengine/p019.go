package taskengine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bba.local/taskengine/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
)

const p019DefaultLimit = 50

func p019PageValues(values url.Values) (string, int, error) {
	cursor := values.Get("cursor")
	if len(cursor) > 512 {
		return "", 0, problem(taskCodeValidation)
	}
	limit := p019DefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return "", 0, problem(taskCodeValidation)
		}
		limit = parsed
	}
	return cursor, limit, nil
}

func p019Cursor(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x00")))
}

func p019DecodeCursor(cursor string, count int) ([]string, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, problem(taskCodeCursorExpired)
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != count {
		return nil, problem(taskCodeCursorExpired)
	}
	return parts, nil
}

func (s *Store) ListAccounts(ctx context.Context, actor Actor, cursor string, limit int) ([]contract.Account, string, bool, error) {
	if actor.WorkspaceID == "" || actor.PrincipalID == "" {
		return nil, "", false, problem("UNAUTHENTICATED")
	}
	if actor.RuntimeID != "" {
		return nil, "", false, problem(taskCodeForbidden)
	}
	if !actor.Permissions["operator.read"] && !actor.Permissions["task.manage_all"] {
		return nil, "", false, problem(taskCodeForbidden)
	}
	if limit < 1 || limit > 100 {
		return nil, "", false, problem(taskCodeValidation)
	}
	parts, err := p019DecodeCursor(cursor, 1)
	if err != nil {
		return nil, "", false, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, "", false, err
	}
	defer tx.Rollback(ctx)
	query := `SELECT a.id,a.workspace_id,a.self_id,a.runtime_id,a.owner_epoch,a.connection_state,a.login_state,a.control_state,a.risk_state,a.heartbeat_at,a.version,h.capabilities
		FROM core.account a LEFT JOIN LATERAL (
			SELECT rh.capabilities FROM core.runtime_heartbeat rh
			WHERE rh.workspace_id=a.workspace_id AND rh.account_id=a.id AND rh.owner_epoch=a.owner_epoch AND rh.disposition='applied'
			ORDER BY rh.sequence DESC LIMIT 1
		) h ON true
		WHERE a.workspace_id=$1`
	args := []any{actor.WorkspaceID}
	if len(parts) == 1 {
		query += ` AND a.id>$2`
		args = append(args, parts[0])
	}
	query += fmt.Sprintf(" ORDER BY a.id LIMIT $%d", len(args)+1)
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, "", false, err
	}
	items := make([]contract.Account, 0, limit+1)
	for rows.Next() {
		var item contract.Account
		var selfID *string
		var heartbeat *time.Time
		var capabilityJSON []byte
		if err := rows.Scan(&item.Id, &item.WorkspaceId, &selfID, &item.RuntimeId, &item.OwnerEpoch,
			&item.ConnectionState, &item.LoginState, &item.ControlState, &item.RiskState, &heartbeat, &item.Version, &capabilityJSON); err != nil {
			rows.Close()
			return nil, "", false, err
		}
		if selfID != nil {
			item.SelfId.Set(*selfID)
		}
		item.HeartbeatAt = nullableTime(heartbeat)
		item.Capabilities = []contract.Capability{}
		if len(capabilityJSON) > 0 {
			var capabilities []contract.Capability
			if err := json.Unmarshal(capabilityJSON, &capabilities); err != nil {
				rows.Close()
				return nil, "", false, problem(taskCodeStoreUnavailable)
			}
			for _, capability := range capabilities {
				if capability.AccountId == item.Id {
					item.Capabilities = append(item.Capabilities, capability)
				}
			}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", false, err
	}
	rows.Close()
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	next := ""
	if hasMore {
		next = p019Cursor(string(items[len(items)-1].Id))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", false, err
	}
	return items, next, hasMore, nil
}

func nullableTime(value *time.Time) nullable.Nullable[time.Time] {
	out := nullable.NewNullNullable[time.Time]()
	if value != nil {
		out.Set(value.UTC())
	}
	return out
}
func (s *Store) ListTasks(ctx context.Context, actor Actor, cursor string, limit int) ([]contract.Task, string, bool, error) {
	if err := actor.require("task.read"); err != nil {
		return nil, "", false, err
	}
	if limit < 1 || limit > 100 {
		return nil, "", false, problem(taskCodeValidation)
	}
	parts, err := p019DecodeCursor(cursor, 2)
	if err != nil {
		return nil, "", false, err
	}
	var after time.Time
	if len(parts) == 2 {
		micros, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || !taskValidID(parts[1]) {
			return nil, "", false, problem(taskCodeCursorExpired)
		}
		after = time.UnixMicro(micros).UTC()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, "", false, err
	}
	defer tx.Rollback(ctx)
	query := `SELECT ` + taskColumns + `,
		(SELECT count(*) FROM core.result r WHERE r.workspace_id=t.workspace_id AND r.task_id=t.id AND r.valid),
		(SELECT count(*) FROM core.goal_slot gs WHERE gs.workspace_id=t.workspace_id AND gs.task_id=t.id AND gs.state='reserved'),
		(SELECT count(*) FROM core.execution e WHERE e.workspace_id=t.workspace_id AND e.task_id=t.id AND e.disposition IN ('pending','submitted')),
		(SELECT count(*) FROM core.execution e WHERE e.workspace_id=t.workspace_id AND e.task_id=t.id AND e.state='UNKNOWN')
		FROM core.task t WHERE t.workspace_id=$1`
	args := []any{actor.WorkspaceID}
	if !actor.Permissions["task.manage_all"] {
		query += ` AND t.principal_id=$2`
		args = append(args, actor.PrincipalID)
	}
	if len(parts) == 2 {
		query += fmt.Sprintf(" AND (t.created_at,t.id)>($%d,$%d)", len(args)+1, len(args)+2)
		args = append(args, after, parts[1])
	}
	query += fmt.Sprintf(" ORDER BY t.created_at,t.id LIMIT $%d", len(args)+1)
	args = append(args, limit+1)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, "", false, err
	}
	type listedTask struct {
		row                                   TaskRow
		confirmed, reserved, pending, unknown int
	}
	tasks := make([]listedTask, 0, limit+1)
	for rows.Next() {
		var item listedTask
		err := rows.Scan(&item.row.WorkspaceID, &item.row.ID, &item.row.PrincipalID, &item.row.State, &item.row.Mode,
			&item.row.Title, &item.row.Version, &item.row.ScopeRevision, &item.row.GoalCount, &item.row.MaxAttempts,
			&item.row.MaxTotalAttempts, &item.row.ImportOffset, &item.row.BaselineComplete, &item.row.Deadline,
			&item.row.ApprovalID, &item.row.ImportDone, &item.row.CreatedAt,
			&item.row.OrchestrationMode, &item.row.AuthorizationRevision, &item.row.RequestPayload,
			&item.confirmed, &item.reserved, &item.pending, &item.unknown)
		item.row.Type = taskTypeFromPayload(item.row.RequestPayload)
		if err != nil {
			rows.Close()
			return nil, "", false, err
		}
		tasks = append(tasks, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", false, err
	}
	rows.Close()
	hasMore := len(tasks) > limit
	if hasMore {
		tasks = tasks[:limit]
	}
	items := make([]contract.Task, 0, len(tasks))
	for _, listed := range tasks {
		item := taskProjection(listed.row, listed.row.CreatedAt)
		item.ConfirmedUniqueCount = listed.confirmed
		item.ReservedTargetCount = listed.reserved
		item.PendingExternalCount = listed.pending
		item.UnknownExecutionCount = listed.unknown
		items = append(items, item)
	}
	next := ""
	if hasMore {
		last := tasks[len(tasks)-1].row
		next = p019Cursor(strconv.FormatInt(last.CreatedAt.UnixMicro(), 10), last.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", false, err
	}
	return items, next, hasMore, nil
}

func (s *Store) ReconcileTask(ctx context.Context, actor Actor, taskID, key string, request contract.ReconcileRequest) (contract.Receipt, error) {
	var out contract.Receipt
	if err := actor.require(taskPermControl); err != nil {
		return out, err
	}
	if taskCheckIdempotencyKey(key) != nil || request.ExpectedVersion < 1 || taskBlank(request.Reason) || len(request.Reason) > 500 || !taskValidID(taskID) {
		return out, problem(taskCodeValidation)
	}
	if actor.RuntimeID != "" {
		return out, problem(taskCodeForbidden)
	}
	bodyDigest, err := digest(request)
	if err != nil {
		return out, problem(taskCodeValidation)
	}
	path := "/v1/tasks/" + taskID + "/reconcile"
	err = s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, taskPermControl); err != nil {
			return err
		}
		record, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
		if err != nil {
			return err
		}
		if found {
			if record.digest != bodyDigest {
				return problem(taskCodeIdempotency)
			}
			return json.Unmarshal(record.response, &out)
		}
		if request.ExpectedVersion != task.Version {
			return problem(taskCodeVersionConflict)
		}
		if err := appendEvent(ctx, tx, task.WorkspaceID, task.ID, "manual_reconciliation_requested", actor.PrincipalID, map[string]any{
			"expected_version": request.ExpectedVersion,
			"reason":           request.Reason,
			"read_only":        true,
		}); err != nil {
			return err
		}
		var acceptedAt time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&acceptedAt); err != nil {
			return err
		}
		out = contract.Receipt{
			AcceptedAt: contract.UTCDateTime(acceptedAt.UTC()), Action: contract.ReceiptActionReconcile,
			ExpectedVersion: request.ExpectedVersion, Id: newID("receipt"), ResourceId: taskID, State: contract.Accepted,
		}
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, taskID, out)
	})
	if err != nil {
		if taskUniqueViolation(err) {
			return s.replayReconcileReceipt(ctx, actor, path, key, bodyDigest)
		}
		return contract.Receipt{}, err
	}
	return out, nil
}

func (s *Store) replayReconcileReceipt(ctx context.Context, actor Actor, path, key, bodyDigest string) (contract.Receipt, error) {
	var out contract.Receipt
	err := s.tx(ctx, func(tx pgx.Tx) error {
		record, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
		if err != nil {
			return err
		}
		if !found {
			return problem(taskCodeStoreUnavailable)
		}
		if record.digest != bodyDigest {
			return problem(taskCodeIdempotency)
		}
		return json.Unmarshal(record.response, &out)
	})
	return out, err
}
