package taskengine

// Summary.v1 store and GroupContext regressions. Every case runs through the
// real HTTP handler and the real Store path against the isolated fixture
// database, so a written row and the returned JSON must agree. The collector's
// own tests do not cover this side: version assignment, replay, digest conflict,
// concurrency, account/self-id mapping, cross-scope refusal, expiry and version
// selection are enforced here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func summaryHex64Seed(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func summarySelfID(t *testing.T, s *Store, account string) string {
	t.Helper()
	return fixtureString(t, s, `SELECT self_id FROM core.account WHERE workspace_id=$1 AND id=$2`, "ws_fixture", account)
}

// summaryFixtureRequest is a valid Summary.v1 ingest for one account/group. The
// account's persisted QQ self_id is required by the account/self-id mapping rule,
// so it is read back instead of assumed.
func summaryFixtureRequest(t *testing.T, s *Store, account, group string) GroupSummaryIngestRequest {
	t.Helper()
	selfID := summarySelfID(t, s, account)
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := SummaryScope{AccountID: selfID, ConversationKind: chatConversationGroup, ConversationID: group}
	return GroupSummaryIngestRequest{
		AccountID:          account,
		SummaryID:          summaryHex64Seed("summary-" + account + "-" + group),
		WindowID:           summaryHex64Seed("window-" + account + "-" + group),
		WindowMessageCount: 3,
		GeneratedAt:        now.Add(-time.Minute),
		TopicEvidence:      []string{summaryEvidenceStatistic},
		Summary: SummaryPayload{
			SchemaID:       summarySchemaID,
			WindowStart:    now.Add(-30 * time.Minute),
			WindowEnd:      now.Add(-time.Minute),
			CollectorScope: scope,
			PrivacyClass:   "local_only",
			RetentionDays:  30,
			ExpiresAt:      now.Add(24 * time.Hour),
			Producer: SummaryProducer{
				AccountID:            selfID,
				CollectorScope:       scope,
				PluginVersion:        "fixture-collector",
				SourceManifestSHA256: summaryHex64Seed("manifest-" + account),
			},
			Topics:      []SummaryTopicPayload{{Summary: "fixture statistic topic", MessageRefs: []string{"msg_fixture_1"}}},
			Limitations: []string{"deterministic statistic only; no model inference"},
		},
	}
}

type summaryIngestBody struct {
	Created        bool   `json:"created"`
	SummaryID      string `json:"summary_id"`
	SummaryVersion int    `json:"summary_version"`
	WindowID       string `json:"window_id"`
}

type summaryTopicBody struct {
	Summary      string   `json:"summary"`
	MessageRefs  []string `json:"message_refs"`
	EvidenceKind string   `json:"evidence_kind"`
}

type groupContextBody struct {
	AccountID      string             `json:"account_id"`
	GroupID        string             `json:"group_id"`
	SummaryRef     *string            `json:"summary_ref"`
	SummaryID      *string            `json:"summary_id"`
	SummaryVersion *int               `json:"summary_version"`
	PrivacyClass   string             `json:"privacy_class"`
	Topics         []summaryTopicBody `json:"topics"`
	Source         string             `json:"source"`
}

func summaryDecodeIngest(t *testing.T, recorder *httptest.ResponseRecorder) summaryIngestBody {
	t.Helper()
	var envelope struct {
		Data summaryIngestBody `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode ingest envelope: %v (body %q)", err, recorder.Body.String())
	}
	return envelope.Data
}

func summaryDecodeContext(t *testing.T, recorder *httptest.ResponseRecorder) groupContextBody {
	t.Helper()
	var envelope struct {
		Data groupContextBody `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode group-context envelope: %v (body %q)", err, recorder.Body.String())
	}
	return envelope.Data
}

// summaryInsertVersion writes a version row directly, for states the ingest
// validator refuses to accept (already expired, future-dated generation).
func summaryInsertVersion(t *testing.T, s *Store, account, group, summaryID string, version int, generatedAt, expiresAt time.Time, retentionDays int) {
	t.Helper()
	selfID := summarySelfID(t, s, account)
	_, err := s.Pool.Exec(context.Background(), `INSERT INTO core.group_summary(
		workspace_id,account_id,group_id,summary_id,summary_version,window_id,window_message_count,
		window_start,window_end,generated_at,expires_at,privacy_class,retention_days,
		source_manifest_sha256,plugin_version,collector_account_id,topics,limitations,source_digest)
		VALUES('ws_fixture',$1,$2,$3,$4,$5,1,$6,$7,$8,$9,'local_only',$10,$11,'fixture-collector',$12,'[]','[]',$13)`,
		account, group, summaryID, version,
		summaryHex64Seed("window-"+summaryID),
		generatedAt.Add(-time.Hour), generatedAt, generatedAt, expiresAt, retentionDays,
		summaryHex64Seed("manifest-"+summaryID), selfID, summaryHex64Seed("digest-"+summaryID))
	if err != nil {
		t.Fatalf("insert summary version %d: %v", version, err)
	}
}

func TestGroupSummaryIngestPersistsAndReplaysThroughHTTP(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_sum_a", "rt_sum_a", 2)
	actor := fixtureRuntime("acc_sum_a", "rt_sum_a")
	handler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(actor))
	req := summaryFixtureRequest(t, s, "acc_sum_a", "930201")

	created := summaryDecodeIngest(t, chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, req, chatHeaders("sum-create")))
	if created.Created != true || created.SummaryVersion != 1 || created.SummaryID != req.SummaryID || created.WindowID != req.WindowID {
		t.Fatalf("first ingest=%+v want created version 1", created)
	}
	// The returned JSON must match the stored row, not just echo the request.
	var storedVersion int
	var storedWindow, storedAccount, storedGroup, evidenceKind string
	err := s.Pool.QueryRow(context.Background(), `SELECT summary_version,window_id,account_id,group_id,topics->0->>'evidence_kind'
		FROM core.group_summary WHERE workspace_id='ws_fixture' AND summary_id=$1`, req.SummaryID).
		Scan(&storedVersion, &storedWindow, &storedAccount, &storedGroup, &evidenceKind)
	if err != nil {
		t.Fatalf("read stored summary: %v", err)
	}
	if storedVersion != created.SummaryVersion || storedWindow != created.WindowID ||
		storedAccount != "acc_sum_a" || storedGroup != "930201" || evidenceKind != summaryEvidenceStatistic {
		t.Fatalf("stored row=%d/%s/%s/%s evidence=%s does not match response %+v",
			storedVersion, storedWindow, storedAccount, storedGroup, evidenceKind, created)
	}

	replay := chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, req, chatHeaders("sum-replay"))
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d want 200 body=%s", replay.Code, replay.Body.String())
	}
	replayBody := summaryDecodeIngest(t, replay)
	if replayBody.Created || replayBody.SummaryVersion != 1 || replayBody.SummaryID != req.SummaryID {
		t.Fatalf("replay advanced the version: %+v", replayBody)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.group_summary WHERE workspace_id='ws_fixture'`); got != 1 {
		t.Fatalf("replay wrote a second row: %d want 1", got)
	}
}

// TestGroupSummaryScopedLockKeyWithSeparatorStaysScoped covers the lock-key
// separator edge: identifiers that embed the separator must not raise a Postgres
// encoding error, and the version counter must stay scoped per account/group.
func TestGroupSummaryScopedLockKeyWithSeparatorStaysScoped(t *testing.T) {
	s := fixtureStore(t)
	const group = "930211"
	table := []struct {
		account string
		runtime string
	}{
		{"acc:sep", "rt_sep_colon"},
		{"acc.sep", "rt_sep_dot"},
		{"acc_sep", "rt_sep_underscore"},
	}
	// Identifiers embedding the lock-key separator must reach storage without a
	// Postgres encoding error, and every scope keeps its own version counter.
	for _, entry := range table {
		seedAccount(t, s, entry.account, entry.runtime, 2)
		handler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(fixtureRuntime(entry.account, entry.runtime)))
		req := summaryFixtureRequest(t, s, entry.account, group)
		body := summaryDecodeIngest(t, chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, req, chatHeaders("sep-"+entry.account)))
		if !body.Created || body.SummaryVersion != 1 {
			t.Fatalf("account %q ingest=%+v want created version 1", entry.account, body)
		}
		if got := fixtureString(t, s, `SELECT account_id FROM core.group_summary WHERE workspace_id='ws_fixture' AND summary_id=$1`, req.SummaryID); got != entry.account {
			t.Fatalf("row attribution mismatch for %q: got %q", entry.account, got)
		}
	}

	// A second observation moves only the colon account's counter.
	colonHandler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc:sep", "rt_sep_colon")))
	second := summaryFixtureRequest(t, s, "acc:sep", group)
	second.SummaryID = summaryHex64Seed("sep-second")
	secondBody := summaryDecodeIngest(t, chatServe(t, colonHandler, http.MethodPost, groupSummaryIngestPath, second, chatHeaders("sep-second")))
	if !secondBody.Created || secondBody.SummaryVersion != 2 {
		t.Fatalf("second observation=%+v want version 2", secondBody)
	}
	for _, account := range []string{"acc.sep", "acc_sep"} {
		if got := fixtureString(t, s, `SELECT max(summary_version)::text FROM core.group_summary WHERE workspace_id='ws_fixture' AND account_id=$1 AND group_id=$2`, account, group); got != "1" {
			t.Fatalf("scope %q counter moved to %s want 1", account, got)
		}
	}
}

// TestGroupSummaryConcurrentVersionsAreUniqueAndIncreasing runs two distinct
// ingests for one account/group at once; the advisory lock must serialize them
// into two unique, increasing versions.
func TestGroupSummaryConcurrentVersionsAreUniqueAndIncreasing(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_sum_race", "rt_sum_race", 2)
	actor := fixtureRuntime("acc_sum_race", "rt_sum_race")
	handler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(actor))

	first := summaryFixtureRequest(t, s, "acc_sum_race", "930221")
	second := summaryFixtureRequest(t, s, "acc_sum_race", "930221")
	second.SummaryID = summaryHex64Seed("summary-race-b")
	second.WindowID = summaryHex64Seed("window-race-b")
	second.TopicEvidence = []string{summaryEvidenceStatement}
	second.Summary.Topics = []SummaryTopicPayload{{Summary: "race topic b", MessageRefs: []string{"msg_fixture_2"}}}

	type result struct {
		version int
		created bool
		err     error
	}
	results := make([]result, 2)
	requests := []GroupSummaryIngestRequest{first, second}
	var wg sync.WaitGroup
	for index, request := range requests {
		wg.Add(1)
		go func(index int, request GroupSummaryIngestRequest) {
			defer wg.Done()
			out, err := s.IngestGroupSummary(context.Background(), actor, request)
			results[index] = result{version: out.SummaryVersion, created: out.Created, err: err}
		}(index, request)
	}
	wg.Wait()
	if results[0].err != nil || results[1].err != nil {
		t.Fatalf("concurrent ingest errors: %v / %v", results[0].err, results[1].err)
	}
	if !results[0].created || !results[1].created {
		t.Fatalf("concurrent ingests did not both create rows: %+v", results)
	}
	versions := map[int]bool{results[0].version: true, results[1].version: true}
	if !versions[1] || !versions[2] {
		t.Fatalf("concurrent versions=%v,%v want unique {1,2}", results[0].version, results[1].version)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.group_summary WHERE workspace_id='ws_fixture' AND account_id=$1`, "acc_sum_race"); got != 2 {
		t.Fatalf("stored versions=%d want 2", got)
	}
	if got := scalarInt(t, s, `SELECT count(DISTINCT summary_version) FROM core.group_summary WHERE workspace_id='ws_fixture' AND account_id=$1`, "acc_sum_race"); got != 2 {
		t.Fatalf("duplicate version numbers: distinct=%d want 2", got)
	}
	// Reading each version back through the HTTP route must return the data of the
	// request that created it: concurrent writers may not cross contents.
	for _, request := range requests {
		version := scalarInt(t, s, `SELECT summary_version FROM core.group_summary WHERE workspace_id='ws_fixture' AND summary_id=$1`, request.SummaryID)
		body := summaryDecodeContext(t, chatServe(t, handler, http.MethodGet,
			fmt.Sprintf("%s?account_id=acc_sum_race&group_id=930221&summary_id=%s&summary_version=%d", groupContextPath, request.SummaryID, version),
			nil, map[string]string{"Authorization": "Runtime chat-test-token"}))
		if body.Source != summarySourceCollector || body.SummaryID == nil || *body.SummaryID != request.SummaryID ||
			body.SummaryVersion == nil || *body.SummaryVersion != version ||
			len(body.Topics) != 1 || body.Topics[0].Summary != request.Summary.Topics[0].Summary ||
			body.Topics[0].EvidenceKind != request.TopicEvidence[0] {
			t.Fatalf("exact read for %s@v%d crossed scopes: %+v", request.SummaryID, version, body)
		}
	}
}

func TestGroupSummaryReplayDigestConflictAndPrivacyEdges(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_sum_conflict", "rt_sum_conflict", 2)
	seedAccount(t, s, "acc_sum_other_runtime", "rt_sum_other", 2)
	handler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc_sum_conflict", "rt_sum_conflict")))

	base := summaryFixtureRequest(t, s, "acc_sum_conflict", "930231")
	if recorder := chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, base, chatHeaders("sum-c1")); recorder.Code != http.StatusCreated {
		t.Fatalf("baseline ingest status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	conflict := summaryFixtureRequest(t, s, "acc_sum_conflict", "930231")
	conflict.SummaryID = base.SummaryID
	conflict.Summary.Topics = []SummaryTopicPayload{{Summary: "different payload", MessageRefs: []string{"msg_fixture_1"}}}
	recorder := chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, conflict, chatHeaders("sum-c2"))
	if recorder.Code != http.StatusConflict || ttErrorCode(t, recorder) != taskCodeIdempotency {
		t.Fatalf("same id different payload status=%d code=%s want 409/%s", recorder.Code, ttErrorCode(t, recorder), taskCodeIdempotency)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.group_summary WHERE workspace_id='ws_fixture' AND summary_id=$1`, base.SummaryID); got != 1 {
		t.Fatalf("digest conflict wrote rows: %d want 1", got)
	}

	badID := summaryFixtureRequest(t, s, "acc_sum_conflict", "930231")
	badID.SummaryID = "not-hex-64"
	if recorder := chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, badID, chatHeaders("sum-c3")); recorder.Code != http.StatusBadRequest || ttErrorCode(t, recorder) != taskCodeValidation {
		t.Fatalf("non-hex summary_id status=%d code=%s want 400/%s", recorder.Code, ttErrorCode(t, recorder), taskCodeValidation)
	}

	// A local-only Summary may not carry model-derived topics: those would imply
	// message bodies crossed the collector boundary.
	localInference := summaryFixtureRequest(t, s, "acc_sum_conflict", "930231")
	localInference.SummaryID = summaryHex64Seed("summary-local-inference")
	localInference.TopicEvidence = []string{summaryEvidenceInference}
	if recorder := chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, localInference, chatHeaders("sum-c4")); recorder.Code != http.StatusBadRequest || ttErrorCode(t, recorder) != taskCodeValidation {
		t.Fatalf("local_only + model_inference status=%d code=%s want 400/%s", recorder.Code, ttErrorCode(t, recorder), taskCodeValidation)
	}

	// The rule is about the outbound policy, not a blanket ban: the same evidence
	// is accepted when the Summary is classified for external models.
	externalInference := summaryFixtureRequest(t, s, "acc_sum_conflict", "930231")
	externalInference.SummaryID = summaryHex64Seed("summary-external-inference")
	externalInference.Summary.PrivacyClass = "redacted_external_allowed"
	externalInference.TopicEvidence = []string{summaryEvidenceInference}
	recorder = chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, externalInference, chatHeaders("sum-c5"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("redacted_external_allowed + model_inference status=%d code=%s want 201", recorder.Code, ttErrorCode(t, recorder))
	}

	// Account/self-id mapping and cross-scope refusal.
	mismatch := summaryFixtureRequest(t, s, "acc_sum_conflict", "930231")
	mismatch.SummaryID = summaryHex64Seed("summary-selfid-mismatch")
	mismatch.Summary.CollectorScope.AccountID = "99999999"
	mismatch.Summary.Producer.AccountID = "99999999"
	if recorder := chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, mismatch, chatHeaders("sum-c6")); recorder.Code != http.StatusBadRequest || ttErrorCode(t, recorder) != taskCodeValidation {
		t.Fatalf("self_id mismatch status=%d code=%s want 400/%s", recorder.Code, ttErrorCode(t, recorder), taskCodeValidation)
	}
	foreign := summaryFixtureRequest(t, s, "acc_sum_other_runtime", "930231")
	foreign.SummaryID = summaryHex64Seed("summary-foreign")
	recorder = chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, foreign, chatHeaders("sum-c7"))
	if ttErrorCode(t, recorder) != "RESOURCE_NOT_FOUND" {
		t.Fatalf("cross-account ingest code=%s want RESOURCE_NOT_FOUND", ttErrorCode(t, recorder))
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.group_summary WHERE workspace_id='ws_fixture' AND account_id=$1`, "acc_sum_other_runtime"); got != 0 {
		t.Fatalf("cross-account ingest wrote rows: %d want 0", got)
	}
}

func TestGroupContextVersionSelectionAndExpiry(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_sum_read", "rt_sum_read", 2)
	actor := fixtureRuntime("acc_sum_read", "rt_sum_read")
	handler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(actor))

	first := summaryFixtureRequest(t, s, "acc_sum_read", "930241")
	firstBody := summaryDecodeIngest(t, chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, first, chatHeaders("sum-r1")))
	second := summaryFixtureRequest(t, s, "acc_sum_read", "930241")
	second.SummaryID = summaryHex64Seed("summary-read-second")
	second.TopicEvidence = []string{summaryEvidenceStatement}
	second.Summary.Topics = []SummaryTopicPayload{{Summary: "second observation", MessageRefs: []string{"msg_fixture_2"}}}
	secondBody := summaryDecodeIngest(t, chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, second, chatHeaders("sum-r2")))
	if firstBody.SummaryVersion >= secondBody.SummaryVersion {
		t.Fatalf("second observation did not supersede the first: %d vs %d", firstBody.SummaryVersion, secondBody.SummaryVersion)
	}

	newest := summaryDecodeContext(t, chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_read&group_id=930241", nil, map[string]string{"Authorization": "Runtime chat-test-token"}))
	if newest.Source != summarySourceCollector || newest.SummaryID == nil || *newest.SummaryID != second.SummaryID {
		t.Fatalf("newest live read=%+v want the second summary", newest)
	}
	if newest.SummaryVersion == nil || *newest.SummaryVersion != secondBody.SummaryVersion {
		t.Fatalf("newest live version=%v want %d", newest.SummaryVersion, secondBody.SummaryVersion)
	}
	if newest.SummaryRef == nil || *newest.SummaryRef != second.SummaryID+"@v"+strconv.Itoa(*newest.SummaryVersion) {
		t.Fatalf("summary_ref=%v want %s@v%d", newest.SummaryRef, second.SummaryID, secondBody.SummaryVersion)
	}
	if len(newest.Topics) != 1 || newest.Topics[0].EvidenceKind != summaryEvidenceStatement || newest.Topics[0].Summary != "second observation" {
		t.Fatalf("newest topics=%+v want the second observation only", newest.Topics)
	}

	exact := summaryDecodeContext(t, chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_read&group_id=930241&summary_id="+first.SummaryID+"&summary_version=1", nil, map[string]string{"Authorization": "Runtime chat-test-token"}))
	if exact.SummaryID == nil || *exact.SummaryID != first.SummaryID || len(exact.Topics) != 1 || exact.Topics[0].EvidenceKind != summaryEvidenceStatistic {
		t.Fatalf("exact version read=%+v want the first observation", exact)
	}

	summaryInsertVersion(t, s, "acc_sum_read", "930241", summaryHex64Seed("summary-expired"), 3,
		time.Now().UTC().Add(-72*time.Hour), time.Now().UTC().Add(-time.Hour), 10)
	recorder := chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_read&group_id=930241&summary_id="+summaryHex64Seed("summary-expired")+"&summary_version=3", nil, map[string]string{"Authorization": "Runtime chat-test-token"})
	if recorder.Code != http.StatusNotFound || ttErrorCode(t, recorder) != taskCodeNotFound {
		t.Fatalf("expired exact read status=%d code=%s want 404/%s", recorder.Code, ttErrorCode(t, recorder), taskCodeNotFound)
	}
	// The expired row must not be selected as newest either.
	afterExpiry := summaryDecodeContext(t, chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_read&group_id=930241", nil, map[string]string{"Authorization": "Runtime chat-test-token"}))
	if afterExpiry.SummaryID == nil || *afterExpiry.SummaryID != second.SummaryID {
		t.Fatalf("expired row leaked into newest read: %+v", afterExpiry)
	}

	// A future-dated generation beyond the accepted skew is not readable either.
	summaryInsertVersion(t, s, "acc_sum_read", "930242", summaryHex64Seed("summary-future"), 1,
		time.Now().UTC().Add(time.Hour), time.Now().UTC().Add(24*time.Hour), 30)
	recorder = chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_read&group_id=930242&summary_id="+summaryHex64Seed("summary-future")+"&summary_version=1", nil, map[string]string{"Authorization": "Runtime chat-test-token"})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("future-dated exact read status=%d want 404 body=%s", recorder.Code, recorder.Body.String())
	}
	// With no live Summary the projection degrades to the chat policy alone and
	// stays marked as data, never as a decision.
	degraded := summaryDecodeContext(t, chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_read&group_id=930242", nil, map[string]string{"Authorization": "Runtime chat-test-token"}))
	if degraded.Source != summarySourceChatPolicy || degraded.PrivacyClass != "none" || degraded.SummaryID != nil || len(degraded.Topics) != 0 {
		t.Fatalf("degraded projection=%+v want policy-only source with no topics", degraded)
	}
}

func TestGroupContextHTTPValidationAndAuth(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_sum_query", "rt_sum_query", 2)
	handler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc_sum_query", "rt_sum_query")))

	if recorder := chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_query&group_id=930251&unexpected=1", nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown query parameter status=%d want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_query&group_id=930251&summary_version=1", nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("version without summary_id status=%d want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_query&group_id=not-an-id", nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("non-QQ group id status=%d want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_query&group_id=930251", nil, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read status=%d want 401 body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodPost, groupContextPath, nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST on the read route status=%d want 405 body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := chatServe(t, handler, http.MethodGet, groupSummaryIngestPath, nil, map[string]string{"Authorization": "Runtime chat-test-token"}); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the write route status=%d want 405 body=%s", recorder.Code, recorder.Body.String())
	}
	// A live summary for the reading account is returned on the read route, so
	// the auth path and the storage path agree.
	req := summaryFixtureRequest(t, s, "acc_sum_query", "930251")
	if recorder := chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, req, chatHeaders("sum-query-1")); recorder.Code != http.StatusCreated {
		t.Fatalf("ingest status=%d want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	body := summaryDecodeContext(t, chatServe(t, handler, http.MethodGet, groupContextPath+"?account_id=acc_sum_query&group_id=930251", nil, map[string]string{"Authorization": "Runtime chat-test-token"}))
	if body.Source != summarySourceCollector || body.SummaryID == nil || *body.SummaryID != req.SummaryID {
		t.Fatalf("read after write=%+v want the ingested summary", body)
	}
}

func TestGroupSummaryPurgeOnlyRemovesExpiredRows(t *testing.T) {
	s := fixtureStore(t)
	seedAccount(t, s, "acc_sum_purge", "rt_sum_purge", 2)
	handler := s.GroupSummaryRuntimeHandler(chatRuntimeAuth(fixtureRuntime("acc_sum_purge", "rt_sum_purge")))
	req := summaryFixtureRequest(t, s, "acc_sum_purge", "930261")
	if recorder := chatServe(t, handler, http.MethodPost, groupSummaryIngestPath, req, chatHeaders("sum-purge")); recorder.Code != http.StatusCreated {
		t.Fatalf("ingest status=%d want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	summaryInsertVersion(t, s, "acc_sum_purge", "930261", summaryHex64Seed("summary-purge-expired"), 2,
		time.Now().UTC().Add(-72*time.Hour), time.Now().UTC().Add(-time.Hour), 10)

	if _, err := s.PurgeExpiredGroupSummaries(context.Background(), 0); err == nil {
		t.Fatal("zero purge limit accepted")
	}
	removed, err := s.PurgeExpiredGroupSummaries(context.Background(), 10)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Fatalf("purged=%d want 1 (the expired row only)", removed)
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.group_summary WHERE workspace_id='ws_fixture' AND summary_id=$1`, req.SummaryID); got != 1 {
		t.Fatalf("live summary removed by the purge: %d want 1", got)
	}
}
