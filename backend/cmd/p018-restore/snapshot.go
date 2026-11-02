package main

// Canonical observations shared by the seed and verify stages. Everything here
// is read-only: schema identity, row counts, unresolved rows, account holds,
// the public task projection, and the audit deltas the recovery paths add.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"github.com/jackc/pgx/v5"
)

type tableCount struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

type objectCounts struct {
	Functions int64 `json:"functions"`
	Triggers  int64 `json:"triggers"`
	Indexes   int64 `json:"indexes"`
	Views     int64 `json:"views"`
}

type totals struct {
	Executions            int64 `json:"executions"`
	GatewayOperations     int64 `json:"gateway_operations"`
	DispatchPermits       int64 `json:"dispatch_permits"`
	Retries               int64 `json:"retries"`
	Results               int64 `json:"results"`
	ReservedGoalSlots     int64 `json:"reserved_goal_slots"`
	ActiveTargetClaims    int64 `json:"active_target_claims"`
	HeldActionLocks       int64 `json:"held_action_locks"`
	Inbox                 int64 `json:"inbox"`
	TaskEvents            int64 `json:"task_events"`
	Outbox                int64 `json:"outbox"`
	BridgeOutbox          int64 `json:"bridge_outbox"`
	ChatPermits           int64 `json:"chat_permits"`
	ChatOutcomes          int64 `json:"chat_outcomes"`
	ModelRequests         int64 `json:"model_requests"`
	AccountUsageConsumed  int64 `json:"account_usage_consumed"`
	AccountUsageReserved  int64 `json:"account_usage_reserved"`
	ManualReconciliations int64 `json:"manual_reconciliations"`
	RecoveryObservations  int64 `json:"recovery_observations"`
	ReconcileOutbox       int64 `json:"reconcile_outbox"`
}

type unresolvedRow struct {
	TaskID             string `json:"task_id"`
	OperationID        string `json:"operation_id"`
	ExecutionID        string `json:"execution_id"`
	AccountID          string `json:"account_id"`
	TargetID           string `json:"target_id"`
	ExecutionState     string `json:"execution_state"`
	Disposition        string `json:"disposition"`
	Effect             string `json:"effect_certainty"`
	GatewayState       string `json:"gateway_state"`
	GatewayDisposition string `json:"gateway_disposition"`
	GatewayEffect      string `json:"gateway_effect_certainty"`
	PermitState        string `json:"permit_state"`
	ReservedSlots      int    `json:"reserved_slots"`
	ActiveClaims       int    `json:"active_claims"`
	HeldSendLocks      int    `json:"held_send_locks"`
	SubtaskState       string `json:"subtask_state"`
}

type projection struct {
	Scenario              string `json:"scenario"`
	TaskID                string `json:"task_id"`
	State                 string `json:"state"`
	ConfirmedUniqueCount  int    `json:"confirmed_unique_count"`
	ReservedTargetCount   int    `json:"reserved_target_count"`
	PendingExternalCount  int    `json:"pending_external_count"`
	UnknownExecutionCount int    `json:"unknown_execution_count"`
}

type workItemRow struct {
	TaskID        string `json:"task_id"`
	WorkItemID    string `json:"work_item_id"`
	State         string `json:"state"`
	AttemptCount  int    `json:"attempt_count"`
	ExecutionID   string `json:"execution_id"`
	ProposalID    string `json:"proposal_id"`
	ApplicationID string `json:"application_id"`
	Attempts      int    `json:"attempts"`
}

type snapshot struct {
	SchemaVersion     int             `json:"schema_version"`
	SchemaFingerprint string          `json:"schema_fingerprint"`
	DataFingerprint   string          `json:"data_fingerprint"`
	// FingerprintLines keep the exact catalog/data lines behind the hashes so a
	// mismatch can be diagnosed offline (fingerprint-diff.txt) without a rerun.
	SchemaLines []string `json:"schema_lines,omitempty"`
	DataLines   []string `json:"data_lines,omitempty"`
	Tables            []tableCount    `json:"tables"`
	Objects           objectCounts    `json:"objects"`
	Unresolved        []unresolvedRow `json:"unresolved"`
	Projections       []projection    `json:"projections"`
	WorkItems         []workItemRow   `json:"work_items"`
	Totals            totals          `json:"totals"`
}

// userSchemaFilter is the shared definition of "the application's own schema":
// everything the kernel migrations create, with no PostgreSQL internals.
const userSchemaFilter = `n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'`

func captureSnapshot(ctx context.Context, store *taskengine.Store, scenarios []scenario) (snapshot, error) {
	var snap snapshot
	if err := store.Pool.QueryRow(ctx, `SELECT max(version) FROM core.schema_version`).Scan(&snap.SchemaVersion); err != nil {
		return snap, err
	}
	schemaFingerprint, schemaLines, err := captureSchemaFingerprint(ctx, store)
	if err != nil {
		return snap, err
	}
	snap.SchemaFingerprint = schemaFingerprint
	snap.SchemaLines = schemaLines
	dataFingerprint, dataLines, err := captureDataFingerprint(ctx, store)
	if err != nil {
		return snap, err
	}
	snap.DataFingerprint = dataFingerprint
	snap.DataLines = dataLines
	if err := store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN ('core','bridge')),
		(SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE NOT t.tgisinternal AND n.nspname IN ('core','bridge')),
		(SELECT count(*) FROM pg_indexes WHERE schemaname IN ('core','bridge')),
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE n.nspname IN ('core','bridge') AND c.relkind='v')`).
		Scan(&snap.Objects.Functions, &snap.Objects.Triggers, &snap.Objects.Indexes, &snap.Objects.Views); err != nil {
		return snap, err
	}
	tables, err := captureTableCounts(ctx, store)
	if err != nil {
		return snap, err
	}
	snap.Tables = tables
	unresolved, err := captureUnresolved(ctx, store, scenarios)
	if err != nil {
		return snap, err
	}
	snap.Unresolved = unresolved
	projections, err := captureProjections(ctx, store, scenarios)
	if err != nil {
		return snap, err
	}
	snap.Projections = projections
	workItems, err := captureWorkItems(ctx, store, scenarios)
	if err != nil {
		return snap, err
	}
	snap.WorkItems = workItems
	valueTotals, err := captureTotals(ctx, store)
	if err != nil {
		return snap, err
	}
	snap.Totals = valueTotals
	return snap, nil
}

// captureWorkItems reads the durable proposal work ledger for the drill's
// tasks. It is the read model the worker and the console both consume.
func captureWorkItems(ctx context.Context, store *taskengine.Store, scenarios []scenario) ([]workItemRow, error) {
	taskIDs := make([]string, 0, len(scenarios))
	for _, sc := range scenarios {
		taskIDs = append(taskIDs, sc.TaskID)
	}
	rows, err := store.Pool.Query(ctx, `
		SELECT wi.task_id,wi.id,wi.state,wi.attempt_count,COALESCE(wi.execution_id,''),
		       wi.proposal_id,wi.application_id,
		       (SELECT count(*) FROM core.work_item_attempt a
		         WHERE a.workspace_id=wi.workspace_id AND a.work_item_id=wi.id)
		  FROM core.work_item wi
		 WHERE wi.workspace_id=$1 AND wi.task_id=ANY($2::text[])
		 ORDER BY wi.task_id,wi.id`, workspaceID, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []workItemRow
	for rows.Next() {
		var row workItemRow
		if err := rows.Scan(&row.TaskID, &row.WorkItemID, &row.State, &row.AttemptCount, &row.ExecutionID,
			&row.ProposalID, &row.ApplicationID, &row.Attempts); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// captureSchemaFingerprint hashes the whole application catalog: relations,
// columns, defaults, constraints, indexes, functions, triggers and types. It is
// generic on purpose, so a migration that adds a table is covered without
// teaching the drill about that table.
func captureSchemaFingerprint(ctx context.Context, store *taskengine.Store) (string, []string, error) {
	queries := []string{
		`SELECT 'relation '||n.nspname||'.'||c.relname||' '||c.relkind::text
		   FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE ` + userSchemaFilter + ` AND c.relkind IN ('r','p','v','m','S','i')
		  ORDER BY 1`,
		`SELECT 'column '||n.nspname||'.'||c.relname||'.'||a.attname||' '||format_type(a.atttypid,a.atttypmod)||' '||a.attnotnull::text
		   FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE a.attnum>0 AND NOT a.attisdropped AND ` + userSchemaFilter + `
		  ORDER BY 1`,
		`SELECT 'default '||n.nspname||'.'||c.relname||'.'||a.attname||' '||pg_get_expr(d.adbin,d.adrelid)
		   FROM pg_attrdef d
		   JOIN pg_class c ON c.oid=d.adrelid
		   JOIN pg_namespace n ON n.oid=c.relnamespace
		   JOIN pg_attribute a ON a.attrelid=d.adrelid AND a.attnum=d.adnum
		  WHERE ` + userSchemaFilter + `
		  ORDER BY 1`,
		`SELECT 'constraint '||n.nspname||' '||con.conname||' '||pg_get_constraintdef(con.oid)
		   FROM pg_constraint con JOIN pg_namespace n ON n.oid=con.connamespace
		  WHERE ` + userSchemaFilter + `
		  ORDER BY 1`,
		`SELECT 'index '||schemaname||' '||indexname||' '||indexdef
		   FROM pg_indexes
		  WHERE schemaname NOT IN ('pg_catalog','information_schema')
		  ORDER BY 1`,
		`SELECT 'function '||n.nspname||'.'||p.proname||' '||md5(pg_get_functiondef(p.oid))
		   FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		  WHERE ` + userSchemaFilter + `
		  ORDER BY 1`,
		`SELECT 'trigger '||n.nspname||'.'||c.relname||' '||t.tgname||' '||md5(pg_get_triggerdef(t.oid))
		   FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE NOT t.tgisinternal AND ` + userSchemaFilter + `
		  ORDER BY 1`,
		`SELECT 'type '||n.nspname||'.'||t.typname||' '||t.typtype::text||' '||
		        COALESCE((SELECT string_agg(e.enumlabel,',' ORDER BY e.enumsortorder)
		                    FROM pg_enum e WHERE e.enumtypid=t.oid),'')
		   FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
		  WHERE ` + userSchemaFilter + `
		  ORDER BY 1`,
	}
	var lines []string
	for _, query := range queries {
		rows, err := store.Pool.Query(ctx, query)
		if err != nil {
			return "", nil, err
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				return "", nil, err
			}
			lines = append(lines, normalizeCatalogLine(line))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", nil, err
		}
	}
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	return hashLines(lines), sorted, nil
}

// normalizeCatalogLine removes the parts of a catalog-rendered definition that
// PostgreSQL does not preserve across pg_dump/pg_restore: redundant
// parentheses and insignificant whitespace. pg_dump re-emits a CHECK as
// "CHECK ((a AND b AND c))" where the migration wrote "CHECK (a AND b AND c)",
// so the deparsed text differs although the expression is identical. Comparing
// the token stream keeps the full-expression equality check without depending on
// the deparser's stable-but-not-identical formatting. Parentheses inside single
// quoted literals are preserved.
func normalizeCatalogLine(text string) string {
	out := make([]byte, 0, len(text))
	pendingSpace := false
	inQuote := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inQuote {
			out = append(out, c)
			if c == '\'' {
				if i+1 < len(text) && text[i+1] == '\'' {
					out = append(out, '\'')
					i++
				} else {
					inQuote = false
				}
			}
			continue
		}
		switch c {
		case '\'':
			if pendingSpace && len(out) > 0 {
				out = append(out, ' ')
			}
			pendingSpace = false
			out = append(out, c)
			inQuote = true
		case '(', ')', ' ', '\t', '\n', '\r':
			pendingSpace = true
		default:
			if pendingSpace && len(out) > 0 {
				out = append(out, ' ')
			}
			pendingSpace = false
			out = append(out, c)
		}
	}
	return string(out)
}

// captureDataFingerprint hashes every row of every application table plus the
// sequence positions. Row order is irrelevant: each row is hashed and the hashes
// are ordered, so the fingerprint is stable across physical row order.
func captureDataFingerprint(ctx context.Context, store *taskengine.Store) (string, []string, error) {
	rows, err := store.Pool.Query(ctx, `SELECT n.nspname, c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE `+userSchemaFilter+` AND c.relkind IN ('r','p')
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return "", nil, err
	}
	type tableName struct{ schema, name string }
	var names []tableName
	for rows.Next() {
		var item tableName
		if err := rows.Scan(&item.schema, &item.name); err != nil {
			rows.Close()
			return "", nil, err
		}
		names = append(names, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	lines := make([]string, 0, len(names)+8)
	for _, item := range names {
		qualified := pgx.Identifier{item.schema, item.name}.Sanitize()
		var count int64
		var digest string
		query := `SELECT count(*), COALESCE(md5(string_agg(h, '' ORDER BY h)), '')
			FROM (SELECT md5(t::text) AS h FROM ` + qualified + ` t) rows`
		if err := store.Pool.QueryRow(ctx, query).Scan(&count, &digest); err != nil {
			return "", nil, fmt.Errorf("fingerprint %s.%s: %w", item.schema, item.name, err)
		}
		lines = append(lines, fmt.Sprintf("table %s.%s rows=%d md5=%s", item.schema, item.name, count, digest))
	}
	seqRows, err := store.Pool.Query(ctx, `SELECT schemaname||'.'||sequencename, COALESCE(last_value::text,'null')
		FROM pg_sequences WHERE schemaname NOT IN ('pg_catalog','information_schema') ORDER BY 1`)
	if err != nil {
		return "", nil, err
	}
	for seqRows.Next() {
		var name, last string
		if err := seqRows.Scan(&name, &last); err != nil {
			seqRows.Close()
			return "", nil, err
		}
		lines = append(lines, "sequence "+name+" last_value="+last)
	}
	seqRows.Close()
	if err := seqRows.Err(); err != nil {
		return "", nil, err
	}
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	return hashLines(lines), sorted, nil
}

func hashLines(lines []string) string {
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

func captureTableCounts(ctx context.Context, store *taskengine.Store) ([]tableCount, error) {
	rows, err := store.Pool.Query(ctx, `SELECT n.nspname, c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname IN ('core','bridge') AND c.relkind='r'
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, err
	}
	type tableName struct{ schema, name string }
	var names []tableName
	for rows.Next() {
		var item tableName
		if err := rows.Scan(&item.schema, &item.name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]tableCount, 0, len(names))
	for _, item := range names {
		var count int64
		query := `SELECT count(*) FROM ` + pgx.Identifier{item.schema, item.name}.Sanitize()
		if err := store.Pool.QueryRow(ctx, query).Scan(&count); err != nil {
			return nil, err
		}
		out = append(out, tableCount{Name: item.schema + "." + item.name, Rows: count})
	}
	return out, nil
}

func captureUnresolved(ctx context.Context, store *taskengine.Store, scenarios []scenario) ([]unresolvedRow, error) {
	taskIDs := make([]string, 0, len(scenarios))
	for _, sc := range scenarios {
		taskIDs = append(taskIDs, sc.TaskID)
	}
	rows, err := store.Pool.Query(ctx, `SELECT
		e.task_id,e.operation_id,e.id,e.account_id,e.target_id,
		e.state,COALESCE(e.disposition,''),e.effect_certainty,
		COALESCE(g.state,''),COALESCE(g.disposition,''),COALESCE(g.effect_certainty,''),
		COALESCE(p.state,''),
		(SELECT count(*) FROM core.goal_slot s
		  WHERE s.workspace_id=e.workspace_id AND s.task_id=e.task_id
		    AND s.target_id=e.target_id AND s.state='reserved'),
		(SELECT count(*) FROM core.target_claim c
		  WHERE c.workspace_id=e.workspace_id AND c.task_id=e.task_id
		    AND c.target_id=e.target_id AND c.state='active'),
		(SELECT count(*) FROM core.account_action_lock l
		  WHERE l.workspace_id=e.workspace_id AND l.execution_id=e.id AND l.held),
		s.state
		FROM core.execution e
		JOIN core.subtask s ON s.workspace_id=e.workspace_id AND s.id=e.subtask_id
		LEFT JOIN bridge.gateway_operation g
		  ON g.workspace_id=e.workspace_id AND g.operation_id=e.operation_id
		LEFT JOIN core.dispatch_permit p
		  ON p.workspace_id=e.workspace_id AND p.operation_id=e.operation_id
		WHERE e.workspace_id=$1 AND e.task_id=ANY($2::text[])
		ORDER BY e.task_id`, workspaceID, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []unresolvedRow
	for rows.Next() {
		var row unresolvedRow
		if err := rows.Scan(&row.TaskID, &row.OperationID, &row.ExecutionID, &row.AccountID, &row.TargetID,
			&row.ExecutionState, &row.Disposition, &row.Effect,
			&row.GatewayState, &row.GatewayDisposition, &row.GatewayEffect, &row.PermitState,
			&row.ReservedSlots, &row.ActiveClaims, &row.HeldSendLocks, &row.SubtaskState); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func captureTotals(ctx context.Context, store *taskengine.Store) (totals, error) {
	var out totals
	err := store.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM core.execution WHERE workspace_id=$1),
		(SELECT count(*) FROM bridge.gateway_operation WHERE workspace_id=$1),
		(SELECT count(*) FROM core.dispatch_permit WHERE workspace_id=$1),
		(SELECT count(*) FROM core.retry WHERE workspace_id=$1),
		(SELECT count(*) FROM core.result WHERE workspace_id=$1),
		(SELECT count(*) FROM core.goal_slot WHERE workspace_id=$1 AND state='reserved'),
		(SELECT count(*) FROM core.target_claim WHERE workspace_id=$1 AND state='active'),
		(SELECT count(*) FROM core.account_action_lock WHERE workspace_id=$1 AND held),
		(SELECT count(*) FROM core.inbox WHERE workspace_id=$1),
		(SELECT count(*) FROM core.task_event WHERE workspace_id=$1),
		(SELECT count(*) FROM core.outbox WHERE workspace_id=$1),
		(SELECT count(*) FROM bridge.outbox WHERE workspace_id=$1),
		(SELECT count(*) FROM core.chat_send_permit WHERE workspace_id=$1),
		(SELECT count(*) FROM core.chat_send_outcome WHERE workspace_id=$1),
		(SELECT count(*) FROM core.model_request WHERE workspace_id=$1),
		(SELECT COALESCE(sum(consumed),0)::bigint FROM core.account_usage WHERE workspace_id=$1),
		(SELECT COALESCE(sum(reserved),0)::bigint FROM core.account_usage WHERE workspace_id=$1),
		(SELECT count(*) FROM core.task_event WHERE workspace_id=$1 AND kind='manual_reconciliation'),
		(SELECT count(*) FROM bridge.outbox WHERE workspace_id=$1 AND event_type='execution.observed'),
		(SELECT count(*) FROM core.outbox WHERE workspace_id=$1 AND event_type='manual_reconciliation')`,
		workspaceID).Scan(
		&out.Executions, &out.GatewayOperations, &out.DispatchPermits, &out.Retries,
		&out.Results, &out.ReservedGoalSlots, &out.ActiveTargetClaims, &out.HeldActionLocks,
		&out.Inbox, &out.TaskEvents, &out.Outbox, &out.BridgeOutbox,
		&out.ChatPermits, &out.ChatOutcomes, &out.ModelRequests,
		&out.AccountUsageConsumed, &out.AccountUsageReserved,
		&out.ManualReconciliations, &out.RecoveryObservations, &out.ReconcileOutbox)
	return out, err
}

// captureProjections reads the public task API, not internal rows. The handler
// runs on loopback (httptest) with a synthetic operator identity.
func captureProjections(ctx context.Context, store *taskengine.Store, scenarios []scenario) ([]projection, error) {
	actor := drillActor()
	server := httptest.NewServer(store.Handler(func(r *http.Request) (taskengine.Actor, error) {
		if r.Header.Get("Authorization") != "Bearer p018-fixture" {
			return taskengine.Actor{}, errors.New("unauthenticated")
		}
		return actor, nil
	}))
	defer server.Close()

	out := make([]projection, 0, len(scenarios))
	for _, sc := range scenarios {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/tasks/"+sc.TaskID, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer p018-fixture")
		response, err := server.Client().Do(req)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
			response.Body.Close()
			return nil, fmt.Errorf("GET task %s status=%d body=%s", sc.TaskID, response.StatusCode, string(body))
		}
		var envelope contract.TaskEnvelope
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope)
		response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode task %s: %w", sc.TaskID, err)
		}
		out = append(out, projection{
			Scenario:              sc.Name,
			TaskID:                string(envelope.Data.Id),
			State:                 string(envelope.Data.State),
			ConfirmedUniqueCount:  envelope.Data.ConfirmedUniqueCount,
			ReservedTargetCount:   envelope.Data.ReservedTargetCount,
			PendingExternalCount:  envelope.Data.PendingExternalCount,
			UnknownExecutionCount: envelope.Data.UnknownExecutionCount,
		})
	}
	return out, nil
}

func snapshotsEqual(left, right snapshot) bool {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return bytes.Equal(leftJSON, rightJSON)
}

// snapshotDifferences names the snapshot parts that differ, so a failed verify
// reports facts instead of only "does not match".
func snapshotDifferences(source, restored snapshot) []string {
	var out []string
	add := func(name string, a, b any) {
		if !sameJSON(a, b) {
			out = append(out, name)
		}
	}
	add("schema_version", source.SchemaVersion, restored.SchemaVersion)
	add("schema_fingerprint", source.SchemaFingerprint, restored.SchemaFingerprint)
	add("data_fingerprint", source.DataFingerprint, restored.DataFingerprint)
	add("schema_lines", source.SchemaLines, restored.SchemaLines)
	add("data_lines", source.DataLines, restored.DataLines)
	add("tables", source.Tables, restored.Tables)
	add("objects", source.Objects, restored.Objects)
	add("unresolved", source.Unresolved, restored.Unresolved)
	add("projections", source.Projections, restored.Projections)
	add("work_items", source.WorkItems, restored.WorkItems)
	add("totals", source.Totals, restored.Totals)
	return out
}

func sameJSON(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// writeFingerprintDiff records the exact catalog/data lines behind a fingerprint
// mismatch. It is written before the verify stage fails, so the evidence
// directory alone explains what pg_dump/pg_restore changed.
func writeFingerprintDiff(dir string, source, restored snapshot) error {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_fingerprint source=%s restored=%s\n", source.SchemaFingerprint, restored.SchemaFingerprint)
	fmt.Fprintf(&b, "data_fingerprint source=%s restored=%s\n", source.DataFingerprint, restored.DataFingerprint)
	diffLines(&b, "schema", source.SchemaLines, restored.SchemaLines)
	diffLines(&b, "data", source.DataLines, restored.DataLines)
	return os.WriteFile(filepath.Join(dir, "fingerprint-diff.txt"), []byte(b.String()), 0o600)
}

func diffLines(b *strings.Builder, name string, source, restored []string) {
	count := func(lines []string) map[string]int {
		out := make(map[string]int, len(lines))
		for _, line := range lines {
			out[line]++
		}
		return out
	}
	src, dst := count(source), count(restored)
	var onlySource, onlyRestored []string
	for line, n := range src {
		if dst[line] < n {
			onlySource = append(onlySource, line)
		}
	}
	for line, n := range dst {
		if src[line] < n {
			onlyRestored = append(onlyRestored, line)
		}
	}
	sort.Strings(onlySource)
	sort.Strings(onlyRestored)
	fmt.Fprintf(b, "\n== %s: %d only-in-source, %d only-in-restored ==\n", name, len(onlySource), len(onlyRestored))
	for _, line := range onlySource {
		fmt.Fprintf(b, "- %s\n", line)
	}
	for _, line := range onlyRestored {
		fmt.Fprintf(b, "+ %s\n", line)
	}
}

func evidencePath(dir, name string) string {
	return filepath.Join(dir, name)
}

func writeEvidence(dir, name string, value any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err = os.WriteFile(tmp, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func jsonUnmarshal(raw []byte, value any) error {
	return json.Unmarshal(raw, value)
}
