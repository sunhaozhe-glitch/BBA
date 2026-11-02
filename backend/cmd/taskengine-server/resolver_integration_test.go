package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"github.com/jackc/pgx/v5"
)

const (
	authIntegrationToken   = "auth-integration-secret-token"
	authIntegrationWrong   = "wrong-token-9c1f"
	authIntegrationCredErr = "UNAUTHENTICATED"
)

// authIntegrationStore provisions a disposable database inside the isolated
// PostgreSQL fixture and seeds the workspace/principal the trusted resolver
// names. It refuses anything that is not the loopback-socket fixture.
func authIntegrationStore(t *testing.T) *taskengine.Store {
	t.Helper()
	if os.Getenv("BBA_FIXTURE_ONLY") != "1" {
		t.Fatal("isolated PostgreSQL fixture required: use tools/kernel.sh test")
	}
	dsn := os.Getenv("BBA_FIXTURE_DSN")
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "/fixture" || cfg.User != "bba_fixture" || cfg.Database != "bba_fixture_admin" {
		t.Fatal("refusing non-fixture database connection")
	}
	ctx := context.Background()
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := fmt.Sprintf("auth_http_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	dbURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbURL.Path = "/" + db
	store, err := taskengine.Open(ctx, dbURL.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop fixture database: %v", err)
		}
		admin.Close(ctx)
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("fixture migration: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_auth'); INSERT INTO core.principal(workspace_id,id) VALUES('ws_auth','p_auth')`); err != nil {
		t.Fatal(err)
	}
	return store
}

// authIntegrationResolver builds the production resolver from a real 0600
// file; the HTTP path under test is exactly the deployed one.
func authIntegrationResolver(t *testing.T) func(*http.Request) (taskengine.Actor, error) {
	t.Helper()
	digest := sha256.Sum256([]byte(authIntegrationToken))
	payload, err := json.Marshal(resolverFile{Credentials: []credentialRecord{{
		TokenSHA256: hex.EncodeToString(digest[:]),
		WorkspaceID: "ws_auth",
		PrincipalID: "p_auth",
		Permissions: []string{"operator.read", "task.read"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := loadResolver(writeResolverFixture(t, 0600, string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

// authIntegrationCounts reads every user table, not a fixed allowlist, so a
// write to an unexpected workspace/audit table cannot slip past the no-effect
// assertion.
func authIntegrationCounts(t *testing.T, store *taskengine.Store) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := store.Pool.Query(ctx, `SELECT schemaname, tablename FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema') ORDER BY 1,2`)
	if err != nil {
		t.Fatal(err)
	}
	var tables [][2]string
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, [2]string{schema, table})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(tables) == 0 {
		t.Fatal("fixture has no user tables; migration did not run")
	}
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		query := "SELECT count(*) FROM " + pgx.Identifier{table[0], table[1]}.Sanitize()
		if err := store.Pool.QueryRow(ctx, query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table[0]+"."+table[1]] = count
	}
	return counts
}

func authIntegrationDiff(before, after map[string]int) string {
	var diff []string
	for table, count := range before {
		if after[table] != count {
			diff = append(diff, fmt.Sprintf("%s:%d->%d", table, count, after[table]))
		}
	}
	for table, count := range after {
		if _, ok := before[table]; !ok {
			diff = append(diff, fmt.Sprintf("%s:new->%d", table, count))
		}
	}
	return strings.Join(diff, ", ")
}

// TestUnauthenticatedRequestsReturn401WithoutWrites locks the 02-contracts
// requirement that missing, wrong, non-Bearer and duplicate credentials fail
// closed with 401/UNAUTHENTICATED and no state effect. The HTTP layer is the
// real Store.Handler behind the real resolver; token material must not be
// echoed and client-supplied principal headers must not authenticate.
func TestUnauthenticatedRequestsReturn401WithoutWrites(t *testing.T) {
	store := authIntegrationStore(t)
	resolver := authIntegrationResolver(t)
	handler := taskEngineSurface(store.Handler(resolver))
	before := authIntegrationCounts(t, store)

	reconcile := `{"expected_version":1,"reason":"auth-contract-test"}`
	cases := []struct {
		name    string
		headers []string
	}{
		{name: "missing token"},
		{name: "wrong bearer", headers: []string{"Bearer " + authIntegrationWrong}},
		{name: "basic credentials", headers: []string{"Basic " + authIntegrationToken}},
		{name: "duplicate identical bearer", headers: []string{"Bearer " + authIntegrationToken, "Bearer " + authIntegrationToken}},
		{name: "duplicate valid and invalid", headers: []string{"Bearer " + authIntegrationToken, "Bearer " + authIntegrationWrong}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/task_missing/reconcile", strings.NewReader(reconcile))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "auth-fixture-idempotency")
			// A client-supplied principal header must never be consulted.
			request.Header.Set("X-Principal-ID", "p_auth")
			for _, header := range tc.headers {
				request.Header.Add("Authorization", header)
			}

			resolverRequest := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
			for _, header := range tc.headers {
				resolverRequest.Header.Add("Authorization", header)
			}
			if _, err := resolver(resolverRequest); err == nil {
				t.Fatal("resolver accepted an unauthenticated request")
			} else if code := taskengine.Code(err); code != authIntegrationCredErr {
				// Keep going so a wrong mapping is also visible at the HTTP
				// contract level in the same run.
				t.Errorf("resolver error maps to %q, want %q", code, authIntegrationCredErr)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusUnauthorized, body)
			}
			var envelope contract.ErrorEnvelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v; body = %s", err, body)
			}
			if envelope.Error.Code != authIntegrationCredErr {
				t.Fatalf("error code = %q, want %q; body = %s", envelope.Error.Code, authIntegrationCredErr, body)
			}
			if envelope.Error.Retryable {
				t.Fatalf("unauthenticated error marked retryable; body = %s", body)
			}
			if !envelope.Error.RetryAfterMs.IsNull() {
				t.Fatalf("unauthenticated error carries a retry hint; body = %s", body)
			}
			for _, secret := range []string{authIntegrationToken, authIntegrationWrong} {
				if strings.Contains(body, secret) {
					t.Fatalf("response echoes credential material %q: %s", secret, body)
				}
				for name, values := range recorder.Header() {
					for _, value := range values {
						if strings.Contains(value, secret) {
							t.Fatalf("response header %s echoes credential material %q", name, secret)
						}
					}
				}
			}
			if after := authIntegrationCounts(t, store); !reflect.DeepEqual(before, after) {
				t.Fatalf("unauthenticated request changed table rows: %s", authIntegrationDiff(before, after))
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	request.Header.Set("Authorization", "Bearer "+authIntegrationToken)
	request.Header.Set("X-Principal-ID", "spoofed")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("valid token control status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	if after := authIntegrationCounts(t, store); !reflect.DeepEqual(before, after) {
		t.Fatalf("authenticated read changed table rows: %s", authIntegrationDiff(before, after))
	}
}

// TestStoreFaultStays500WithValidToken verifies the other half of the mapping:
// a real storage outage stays a retryable 500 STATE_STORE_UNAVAILABLE instead
// of being swallowed by the credential path.
func TestStoreFaultStays500WithValidToken(t *testing.T) {
	store := authIntegrationStore(t)
	handler := taskEngineSurface(store.Handler(authIntegrationResolver(t)))

	request := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	request.Header.Set("Authorization", "Bearer "+authIntegrationToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("valid token control status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	store.Close()
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	request.Header.Set("Authorization", "Bearer "+authIntegrationToken)
	handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("store fault status = %d, want 500; body = %s", recorder.Code, body)
	}
	var envelope contract.ErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body = %s", err, body)
	}
	if envelope.Error.Code != "STATE_STORE_UNAVAILABLE" {
		t.Fatalf("store fault code = %q, want STATE_STORE_UNAVAILABLE; body = %s", envelope.Error.Code, body)
	}
	if !envelope.Error.Retryable {
		t.Fatalf("store fault is not marked retryable; body = %s", body)
	}
	if strings.Contains(body, authIntegrationToken) {
		t.Fatalf("store fault response echoes credential material: %s", body)
	}
}

func TestConsoleReadsAuthenticatedAccountAndTaskAPIsWithoutEnablingControls(t *testing.T) {
	store := authIntegrationStore(t)
	if _, err := store.Pool.Exec(context.Background(), `INSERT INTO core.account(workspace_id,id,runtime_id,adapter_instance_id) VALUES('ws_auth','acc_console','rt_console','adapter_console')`); err != nil {
		t.Fatal(err)
	}
	handler := consoleSurface(taskEngineSurface(store.Handler(authIntegrationResolver(t))))
	before := authIntegrationCounts(t, store)

	pageRequest := func(method, path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, nil)
		request.Header.Set("Authorization", "Bearer "+authIntegrationToken)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	if response := pageRequest(http.MethodGet, "/"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Task Engine") {
		t.Fatalf("console page status=%d body=%q", response.Code, response.Body.String())
	}
	accounts := pageRequest(http.MethodGet, "/v1/accounts?limit=50")
	if accounts.Code != http.StatusOK {
		t.Fatalf("accounts status=%d body=%s", accounts.Code, accounts.Body.String())
	}
	var accountEnvelope contract.AccountPageEnvelope
	if err := json.Unmarshal(accounts.Body.Bytes(), &accountEnvelope); err != nil {
		t.Fatalf("decode account page: %v body=%s", err, accounts.Body.String())
	}
	if len(accountEnvelope.Data.Items) != 1 || accountEnvelope.Data.Items[0].Id != "acc_console" {
		t.Fatalf("account page items=%+v, want acc_console", accountEnvelope.Data.Items)
	}
	tasks := pageRequest(http.MethodGet, "/v1/tasks?limit=50")
	if tasks.Code != http.StatusOK {
		t.Fatalf("tasks status=%d body=%s", tasks.Code, tasks.Body.String())
	}
	var taskEnvelope contract.TaskPageEnvelope
	if err := json.Unmarshal(tasks.Body.Bytes(), &taskEnvelope); err != nil {
		t.Fatalf("decode task page: %v body=%s", err, tasks.Body.String())
	}
	if len(taskEnvelope.Data.Items) != 0 {
		t.Fatalf("unexpected task fixtures: %+v", taskEnvelope.Data.Items)
	}
	if response := pageRequest(http.MethodPost, "/v1/tasks/task_missing/controls"); response.Code != http.StatusNotFound {
		t.Fatalf("controls status=%d, want 404", response.Code)
	}
	if after := authIntegrationCounts(t, store); !reflect.DeepEqual(before, after) {
		t.Fatalf("console requests changed database rows: %s", authIntegrationDiff(before, after))
	}
}
