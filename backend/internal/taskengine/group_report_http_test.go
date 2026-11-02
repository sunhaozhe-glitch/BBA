package taskengine

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestGroupReportOperatorAuthIsolationAndExactVersion covers the security
// boundary of the operator GroupContext report: runtime credentials are not an
// operator substitute, cross-workspace accounts stay invisible, a read-only
// operator cannot write through the route, and an explicitly requested Summary
// version is returned instead of the newest one.
func TestGroupReportOperatorAuthIsolationAndExactVersion(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_report", "rt_report", 2)
	seedAccount(t, s, "acc_report_other", "rt_report_other", 2)

	ingest := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc_report", "rt_report")))
	first := summaryFixtureRequest(t, s, "acc_report", "930301")
	firstBody := summaryDecodeIngest(t, chatServe(t, ingest, http.MethodPost, groupSummaryIngestPath, first, chatHeaders("report-summary-1")))
	if firstBody.SummaryVersion < 1 {
		t.Fatalf("first Summary version=%d want >=1", firstBody.SummaryVersion)
	}
	second := summaryFixtureRequest(t, s, "acc_report", "930301")
	second.SummaryID = summaryHex64Seed("report-summary-second")
	second.Summary.Topics = []SummaryTopicPayload{{Summary: "second summary topic", MessageRefs: []string{"msg_fixture_second"}}}
	second.TopicEvidence = []string{summaryEvidenceStatistic}
	secondBody := summaryDecodeIngest(t, chatServe(t, ingest, http.MethodPost, groupSummaryIngestPath, second, chatHeaders("report-summary-2")))
	if secondBody.SummaryVersion <= firstBody.SummaryVersion {
		t.Fatalf("second Summary version=%d did not supersede %d", secondBody.SummaryVersion, firstBody.SummaryVersion)
	}

	reader := Actor{
		WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture",
		Permissions: map[string]bool{"operator.read": true, "task.read": true},
	}
	report := s.GroupReportHandler(chatOperatorAuth(reader))
	headers := map[string]string{"Authorization": "Operator chat-test-token"}
	basePath := "/v1/accounts/acc_report/groups/930301/context"

	// A runtime credential presented through an operator authenticator must not
	// be projected as an operator actor.
	runtimeActor := reader
	runtimeActor.RuntimeID = "rt_report"
	runtimeHandler := s.GroupReportHandler(chatOperatorAuth(runtimeActor))
	if recorder := chatServe(t, runtimeHandler, http.MethodGet, basePath, nil, headers); recorder.Code != http.StatusForbidden || ttErrorCode(t, recorder) != taskCodeForbidden {
		t.Fatalf("runtime actor report status=%d code=%s want 403/%s body=%s", recorder.Code, ttErrorCode(t, recorder), taskCodeForbidden, recorder.Body.String())
	}

	// A read-only operator sees the newest live version by default.
	newestRecorder := chatServe(t, report, http.MethodGet, basePath, nil, headers)
	if newestRecorder.Code != http.StatusOK {
		t.Fatalf("newest report status=%d body=%s", newestRecorder.Code, newestRecorder.Body.String())
	}
	newest := summaryDecodeContext(t, newestRecorder)
	if newest.Source != summarySourceCollector || newest.SummaryID == nil || *newest.SummaryID != second.SummaryID ||
		newest.SummaryVersion == nil || *newest.SummaryVersion != secondBody.SummaryVersion {
		t.Fatalf("newest report=%+v want %s@v%d", newest, second.SummaryID, secondBody.SummaryVersion)
	}
	if len(newest.Topics) != 1 || newest.Topics[0].Summary != "second summary topic" || len(newest.Topics[0].MessageRefs) == 0 {
		t.Fatalf("newest topics=%+v want the second Summary topic with refs", newest.Topics)
	}

	// The exact pinned version must be returned, not silently upgraded to the
	// newest live row.
	exactPath := basePath + "?summary_id=" + first.SummaryID + "&summary_version=" + strconv.Itoa(firstBody.SummaryVersion)
	exactRecorder := chatServe(t, report, http.MethodGet, exactPath, nil, headers)
	if exactRecorder.Code != http.StatusOK {
		t.Fatalf("exact report status=%d body=%s", exactRecorder.Code, exactRecorder.Body.String())
	}
	exact := summaryDecodeContext(t, exactRecorder)
	if exact.Source != summarySourceCollector || exact.SummaryID == nil || *exact.SummaryID != first.SummaryID ||
		exact.SummaryVersion == nil || *exact.SummaryVersion != firstBody.SummaryVersion {
		t.Fatalf("exact report=%+v want %s@v%d", exact, first.SummaryID, firstBody.SummaryVersion)
	}
	if exact.SummaryRef == nil || *exact.SummaryRef != first.SummaryID+"@v"+strconv.Itoa(firstBody.SummaryVersion) {
		t.Fatalf("exact summary_ref=%v want %s@v%d", exact.SummaryRef, first.SummaryID, firstBody.SummaryVersion)
	}
	if len(exact.Topics) != 1 || exact.Topics[0].Summary != "fixture statistic topic" || len(exact.Topics[0].MessageRefs) == 0 {
		t.Fatalf("exact topics=%+v want the first Summary topic with refs", exact.Topics)
	}

	// A different workspace owns the same-looking account id; the operator's
	// workspace must not resolve it, even though a live Summary exists there.
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.workspace(id) VALUES('ws_other')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.account(
		workspace_id,id,runtime_id,adapter_instance_id,self_id,identity_status,connection_state,
		login_state,control_state,risk_state,request_join_verified,membership_verified,quota_limit)
		VALUES('ws_other','acc_report_foreign','rt_report_foreign','adapter_report_foreign','900001',
		'verified','online','logged_in','active','clear',true,true,2)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	foreignID := summaryHex64Seed("report-summary-foreign")
	if _, err := s.Pool.Exec(context.Background(), `INSERT INTO core.group_summary(
		workspace_id,account_id,group_id,summary_id,summary_version,window_id,window_message_count,
		window_start,window_end,generated_at,expires_at,privacy_class,retention_days,
		source_manifest_sha256,plugin_version,collector_account_id,topics,limitations,source_digest)
		VALUES('ws_other','acc_report_foreign','930302',$1,1,$2,1,$3,$4,$4,$5,'local_only',30,$6,
		'fixture-foreign','900001','[]','[]',$7)`,
		foreignID, summaryHex64Seed("window-report-foreign"), now.Add(-time.Hour), now.Add(-time.Minute),
		now.Add(24*time.Hour), summaryHex64Seed("manifest-report-foreign"), summaryHex64Seed("digest-report-foreign")); err != nil {
		t.Fatal(err)
	}
	foreignPath := "/v1/accounts/acc_report_foreign/groups/930302/context"
	if recorder := chatServe(t, report, http.MethodGet, foreignPath, nil, headers); recorder.Code != http.StatusNotFound || ttErrorCode(t, recorder) != taskCodeNotFound {
		t.Fatalf("cross-workspace report status=%d code=%s want 404/%s body=%s", recorder.Code, ttErrorCode(t, recorder), taskCodeNotFound, recorder.Body.String())
	}

	// Optional actor account scope is enforced before any Summary is read.
	narrowed := reader
	narrowed.AccountIDs = []string{"acc_report"}
	narrowedHandler := s.GroupReportHandler(chatOperatorAuth(narrowed))
	otherPath := "/v1/accounts/acc_report_other/groups/930303/context"
	if recorder := chatServe(t, narrowedHandler, http.MethodGet, otherPath, nil, headers); recorder.Code != http.StatusNotFound {
		t.Fatalf("out-of-scope account report status=%d want 404 body=%s", recorder.Code, recorder.Body.String())
	}

	// Reads are side-effect free: repeat the exact read and compare the full
	// projection plus the durable Summary/account/binding state.
	beforeSummary := scalarInt(t, s, `SELECT count(*) FROM core.group_summary WHERE workspace_id='ws_fixture'`)
	beforeAccountVersion := scalarInt(t, s, `SELECT version FROM core.account WHERE workspace_id='ws_fixture' AND id='acc_report'`)
	beforeBindings := scalarInt(t, s, `SELECT count(*) FROM core.group_binding WHERE workspace_id='ws_fixture'`)
	firstRead := summaryDecodeContext(t, chatServe(t, report, http.MethodGet, exactPath, nil, headers))
	secondRead := summaryDecodeContext(t, chatServe(t, report, http.MethodGet, exactPath, nil, headers))
	if !reflect.DeepEqual(firstRead, secondRead) {
		t.Fatalf("repeated read changed projection: first=%+v second=%+v", firstRead, secondRead)
	}
	if afterSummary := scalarInt(t, s, `SELECT count(*) FROM core.group_summary WHERE workspace_id='ws_fixture'`); afterSummary != beforeSummary {
		t.Fatalf("read wrote Summary rows: before=%d after=%d", beforeSummary, afterSummary)
	}
	if afterVersion := scalarInt(t, s, `SELECT version FROM core.account WHERE workspace_id='ws_fixture' AND id='acc_report'`); afterVersion != beforeAccountVersion {
		t.Fatalf("read wrote account version: before=%d after=%d", beforeAccountVersion, afterVersion)
	}
	if afterBindings := scalarInt(t, s, `SELECT count(*) FROM core.group_binding WHERE workspace_id='ws_fixture'`); afterBindings != beforeBindings {
		t.Fatalf("read wrote group bindings: before=%d after=%d", beforeBindings, afterBindings)
	}

	// Authentication and method failures do not fall through to a projection.
	if recorder := chatServe(t, report, http.MethodGet, basePath, nil, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated report status=%d want 401 body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, report, http.MethodPost, basePath, nil, headers); recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST report status=%d allow=%q want 405 GET body=%s", recorder.Code, recorder.Header().Get("Allow"), recorder.Body.String())
	}
	noPermission := Actor{WorkspaceID: "ws_fixture", PrincipalID: "principal_fixture", Permissions: map[string]bool{}}
	if recorder := chatServe(t, s.GroupReportHandler(chatOperatorAuth(noPermission)), http.MethodGet, basePath, nil, headers); recorder.Code != http.StatusForbidden {
		t.Fatalf("read-denied report status=%d want 403 body=%s", recorder.Code, recorder.Body.String())
	}
}
