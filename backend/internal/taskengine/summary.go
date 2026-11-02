package taskengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Summary.v1 storage and the read-only GroupContext projection consumed by the
// Character runtime. The backend never generates or repairs a Summary: it
// checks account/group/window/provenance/expiry and refuses anything else.
const (
	summarySchemaID          = "Summary.v1"
	summarySourceCollector   = "collector.summary.v1"
	summarySourceChatPolicy  = "backend.chat-policy"
	summaryGeneratedSkew     = 5 * time.Minute
	summaryContextPolicyTTL  = 5 * time.Minute
	summaryEvidenceStatement = "member_statement"
	summaryEvidenceStatistic = "deterministic_statistic"
	summaryEvidenceInference = "model_inference"
)

var summaryHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SummaryScope mirrors Summary.v1.collector_scope.
type SummaryScope struct {
	AccountID        string `json:"account_id"`
	ConversationKind string `json:"conversation_kind"`
	ConversationID   string `json:"conversation_id"`
}

type SummaryProducer struct {
	AccountID            string       `json:"account_id"`
	CollectorScope       SummaryScope `json:"collector_scope"`
	PluginVersion        string       `json:"plugin_version"`
	SourceManifestSHA256 string       `json:"source_manifest_sha256"`
}

type SummaryTopicPayload struct {
	Summary     string   `json:"summary"`
	MessageRefs []string `json:"message_refs"`
}

// SummaryPayload is the wire Summary.v1 object. It deliberately mirrors
// contracts/schemas/models.json#/Summary.
type SummaryPayload struct {
	SchemaID       string                `json:"schema_id"`
	WindowStart    time.Time             `json:"window_start"`
	WindowEnd      time.Time             `json:"window_end"`
	CollectorScope SummaryScope          `json:"collector_scope"`
	PrivacyClass   string                `json:"privacy_class"`
	RetentionDays  int                   `json:"retention_days"`
	ExpiresAt      time.Time             `json:"expires_at"`
	Producer       SummaryProducer       `json:"producer"`
	Topics         []SummaryTopicPayload `json:"topics"`
	Limitations    []string              `json:"limitations"`
}

// GroupSummaryIngestRequest is the collector's validated Summary plus the
// evidence classification that Summary.v1 itself does not carry.
type GroupSummaryIngestRequest struct {
	AccountID          string         `json:"account_id"`
	SummaryID          string         `json:"summary_id"`
	WindowID           string         `json:"window_id"`
	WindowMessageCount int            `json:"window_message_count"`
	GeneratedAt        time.Time      `json:"generated_at"`
	Summary            SummaryPayload `json:"summary"`
	TopicEvidence      []string       `json:"topic_evidence"`
}

type GroupSummaryIngestResponse struct {
	Created        bool      `json:"created"`
	SummaryID      string    `json:"summary_id"`
	SummaryVersion int       `json:"summary_version"`
	WindowID       string    `json:"window_id"`
	GeneratedAt    time.Time `json:"generated_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type GroupContextTopic struct {
	Summary      string   `json:"summary"`
	MessageRefs  []string `json:"message_refs"`
	EvidenceKind string   `json:"evidence_kind"`
}

type GroupContextActivityWindow struct {
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	MessageCount int       `json:"message_count"`
}

// GroupContext is the untrusted, read-only reference data the Character
// runtime may place in a prompt. It never encodes a send decision: permits and
// the daily budget gate remain authoritative.
type GroupContext struct {
	AccountID              string                      `json:"account_id"`
	GroupID                string                      `json:"group_id"`
	SummaryRef             *string                     `json:"summary_ref"`
	SummaryID              *string                     `json:"summary_id"`
	SummaryVersion         *int                        `json:"summary_version"`
	SourceManifestSHA256   *string                     `json:"source_manifest_sha256"`
	PrivacyClass           string                      `json:"privacy_class"`
	Topics                 []GroupContextTopic         `json:"topics"`
	ActivityWindow         *GroupContextActivityWindow `json:"activity_window"`
	ActiveSpeakerAccountID *string                     `json:"active_speaker_account_id"`
	CooldownUntil          *time.Time                  `json:"cooldown_until"`
	DailyRemaining         *int                        `json:"daily_remaining"`
	ParticipationGoal      *string                     `json:"participation_goal"`
	Source                 string                      `json:"source"`
	GeneratedAt            time.Time                   `json:"generated_at"`
	ExpiresAt              time.Time                   `json:"expires_at"`
}

// SummaryRef selects one exact stored version. An empty SummaryID means "the
// newest live version".
type SummaryRef struct {
	SummaryID      string
	SummaryVersion int
}

func summaryValidText(value string, max int) bool {
	return !taskBlank(value) && utf8.RuneCountInString(value) <= max
}

func summaryValidTopic(topic SummaryTopicPayload) bool {
	if !summaryValidText(topic.Summary, 4000) || topic.MessageRefs == nil || len(topic.MessageRefs) > 500 {
		return false
	}
	for _, ref := range topic.MessageRefs {
		if !summaryValidText(ref, 256) {
			return false
		}
	}
	return true
}

// validateGroupSummaryIngest checks the Summary envelope before any database
// work. It fails closed on missing arrays so an omitted field cannot be read as
// an empty one.
func validateGroupSummaryIngest(req GroupSummaryIngestRequest, now time.Time) error {
	s := req.Summary
	if !chatValidID(req.AccountID) || !summaryHex64.MatchString(req.SummaryID) || !summaryHex64.MatchString(req.WindowID) {
		return problem(taskCodeValidation)
	}
	if req.WindowMessageCount < 0 || req.GeneratedAt.IsZero() || now.Sub(req.GeneratedAt) < -summaryGeneratedSkew {
		return problem(taskCodeValidation)
	}
	if s.SchemaID != summarySchemaID || s.CollectorScope.ConversationKind != chatConversationGroup ||
		!taskValidQQID(s.CollectorScope.ConversationID) || !taskValidQQID(s.CollectorScope.AccountID) {
		return problem(taskCodeValidation)
	}
	if s.WindowStart.IsZero() || s.WindowEnd.IsZero() || s.ExpiresAt.IsZero() ||
		s.WindowStart.After(s.WindowEnd) || req.GeneratedAt.Before(s.WindowStart) {
		return problem(taskCodeValidation)
	}
	if s.RetentionDays < 1 || s.RetentionDays > 365 || !s.ExpiresAt.After(req.GeneratedAt) {
		return problem(taskCodeValidation)
	}
	if s.ExpiresAt.After(s.WindowStart.AddDate(0, 0, s.RetentionDays)) || !s.ExpiresAt.After(now) {
		return problem(taskCodeValidation)
	}
	if s.PrivacyClass != "local_only" && s.PrivacyClass != "redacted_external_allowed" {
		return problem(taskCodeValidation)
	}
	if s.Producer.AccountID != s.CollectorScope.AccountID ||
		s.Producer.CollectorScope != s.CollectorScope ||
		!summaryValidText(s.Producer.PluginVersion, 128) ||
		!summaryHex64.MatchString(s.Producer.SourceManifestSHA256) {
		return problem(taskCodeValidation)
	}
	if s.Topics == nil || s.Limitations == nil || req.TopicEvidence == nil {
		return problem(taskCodeValidation)
	}
	if len(s.Topics) > 20 || len(s.Limitations) > 20 || len(s.Topics) != len(req.TopicEvidence) {
		return problem(taskCodeValidation)
	}
	for index, topic := range s.Topics {
		evidence := req.TopicEvidence[index]
		if !summaryValidTopic(topic) ||
			(evidence != summaryEvidenceStatement && evidence != summaryEvidenceStatistic && evidence != summaryEvidenceInference) {
			return problem(taskCodeValidation)
		}
		// A local-only Summary may not carry model-derived topics: those would
		// imply message bodies crossed the collector boundary.
		if s.PrivacyClass == "local_only" && req.TopicEvidence[index] == summaryEvidenceInference {
			return problem(taskCodeValidation)
		}
	}
	for _, limitation := range s.Limitations {
		if !summaryValidText(limitation, 1000) {
			return problem(taskCodeValidation)
		}
	}
	return nil
}

// IngestGroupSummary stores one immutable Summary version for the caller's own
// account. Version numbers are assigned per account/group under an advisory
// lock; replaying the same summary_id with the same payload returns the stored
// version unchanged.
func (s *Store) IngestGroupSummary(ctx context.Context, actor RuntimeActor, req GroupSummaryIngestRequest) (GroupSummaryIngestResponse, error) {
	var out GroupSummaryIngestResponse
	payloadDigest, err := digest(req)
	if err != nil {
		return out, err
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := authorizeRuntime(ctx, tx, actor, req.AccountID); err != nil {
			return err
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if err := validateGroupSummaryIngest(req, now); err != nil {
			return err
		}
		var selfID string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(self_id,'') FROM core.account WHERE workspace_id=$1 AND id=$2`,
			actor.WorkspaceID, req.AccountID).Scan(&selfID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(taskCodeNotFound)
			}
			return err
		}
		if selfID != req.Summary.CollectorScope.AccountID {
			return problem(taskCodeValidation)
		}
		groupID := req.Summary.CollectorScope.ConversationID
		lockKey := sha256.Sum256([]byte(actor.WorkspaceID + "\x00" + req.AccountID + "\x00" + groupID))
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, hex.EncodeToString(lockKey[:])); err != nil {
			return err
		}
		storedDigest, storedVersion, found, err := readSummaryIdentity(ctx, tx, actor.WorkspaceID, req.AccountID, groupID, req.SummaryID)
		if err != nil {
			return err
		}
		if found {
			if storedDigest != payloadDigest {
				return problem(taskCodeIdempotency)
			}
			out = GroupSummaryIngestResponse{
				Created:        false,
				SummaryID:      req.SummaryID,
				SummaryVersion: storedVersion,
				WindowID:       req.WindowID,
				GeneratedAt:    req.GeneratedAt,
				ExpiresAt:      req.Summary.ExpiresAt,
			}
			return nil
		}
		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(summary_version),0)+1 FROM core.group_summary
			WHERE workspace_id=$1 AND account_id=$2 AND group_id=$3`,
			actor.WorkspaceID, req.AccountID, groupID).Scan(&nextVersion); err != nil {
			return err
		}
		topics, err := jsonSummaryTopics(req.Summary.Topics, req.TopicEvidence)
		if err != nil {
			return err
		}
		limitations, err := json.Marshal(req.Summary.Limitations)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO core.group_summary(
			workspace_id,account_id,group_id,summary_id,summary_version,window_id,window_message_count,
			window_start,window_end,generated_at,expires_at,privacy_class,retention_days,
			source_manifest_sha256,plugin_version,collector_account_id,topics,limitations,source_digest)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			actor.WorkspaceID, req.AccountID, groupID, req.SummaryID, nextVersion, req.WindowID,
			req.WindowMessageCount, req.Summary.WindowStart, req.Summary.WindowEnd, req.GeneratedAt,
			req.Summary.ExpiresAt, req.Summary.PrivacyClass, req.Summary.RetentionDays,
			req.Summary.Producer.SourceManifestSHA256, req.Summary.Producer.PluginVersion,
			req.Summary.CollectorScope.AccountID, topics, limitations, payloadDigest); err != nil {
			return err
		}
		out = GroupSummaryIngestResponse{
			Created:        true,
			SummaryID:      req.SummaryID,
			SummaryVersion: nextVersion,
			WindowID:       req.WindowID,
			GeneratedAt:    req.GeneratedAt,
			ExpiresAt:      req.Summary.ExpiresAt,
		}
		return nil
	})
	return out, err
}

func readSummaryIdentity(ctx context.Context, tx pgx.Tx, workspaceID, accountID, groupID, summaryID string) (string, int, bool, error) {
	var storedDigest string
	var version int
	err := tx.QueryRow(ctx, `SELECT source_digest,summary_version FROM core.group_summary
		WHERE workspace_id=$1 AND account_id=$2 AND group_id=$3 AND summary_id=$4`,
		workspaceID, accountID, groupID, summaryID).Scan(&storedDigest, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return storedDigest, version, true, nil
}

type storedSummaryTopic struct {
	Summary      string   `json:"summary"`
	MessageRefs  []string `json:"message_refs"`
	EvidenceKind string   `json:"evidence_kind"`
}

func jsonSummaryTopics(topics []SummaryTopicPayload, evidence []string) ([]byte, error) {
	out := make([]storedSummaryTopic, 0, len(topics))
	for index, topic := range topics {
		out = append(out, storedSummaryTopic{
			Summary:      topic.Summary,
			MessageRefs:  topic.MessageRefs,
			EvidenceKind: evidence[index],
		})
	}
	return json.Marshal(out)
}

// GroupContextForRuntime reads the newest live Summary for one account/group
// and projects it as untrusted reference context. When no Summary is live the
// projection degrades to the chat policy alone; an explicitly requested
// summary version that is unknown or expired is a 404.
func (s *Store) GroupContextForRuntime(ctx context.Context, actor RuntimeActor, accountID, groupID string, ref SummaryRef) (GroupContext, error) {
	return s.groupContextRead(ctx, actor.WorkspaceID, accountID, groupID, ref, func(tx pgx.Tx) error {
		return authorizeRuntime(ctx, tx, actor, accountID)
	})
}

// groupContextRead is the single GroupContext projection shared by the runtime
// credential path and the operator report path. authorize runs inside the
// read transaction before any context row is read; a nil authorize is only used
// by Store.GroupContextForOperator after it has enforced the operator scope.
func (s *Store) groupContextRead(ctx context.Context, workspaceID, accountID, groupID string, ref SummaryRef, authorize func(pgx.Tx) error) (GroupContext, error) {
	out := GroupContext{AccountID: accountID, GroupID: groupID, Topics: []GroupContextTopic{}}
	if !chatValidID(accountID) || !taskValidQQID(groupID) ||
		(ref.SummaryID != "" && (!summaryHex64.MatchString(ref.SummaryID) || ref.SummaryVersion < 1)) {
		return out, problem(taskCodeValidation)
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if authorize != nil {
			if err := authorize(tx); err != nil {
				return err
			}
		}
		now, err := s.dbNow(ctx, tx)
		if err != nil {
			return err
		}
		var selfID string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(self_id,'') FROM core.account WHERE workspace_id=$1 AND id=$2`,
			workspaceID, accountID).Scan(&selfID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return problem(taskCodeNotFound)
			}
			return err
		}
		if taskBlank(selfID) {
			return problem(chatCodeAccountRestricted)
		}

		enabled, limit, issued := false, 1, 0
		err = tx.QueryRow(ctx, `SELECT enabled,daily_limit FROM core.chat_send_policy WHERE workspace_id=$1 AND account_id=$2`,
			workspaceID, accountID).Scan(&enabled, &limit)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT issued FROM core.chat_send_usage
			WHERE workspace_id=$1 AND account_id=$2 AND window_start=`+chatUsageDateExpr,
			workspaceID, accountID).Scan(&issued)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		remaining := 0
		if enabled {
			remaining = limit - issued
			if remaining < 0 {
				remaining = 0
			}
		}
		out.DailyRemaining = &remaining

		var speaker string
		var lease time.Time
		err = tx.QueryRow(ctx, `SELECT active_speaker_account_id,lease_expires_at
			FROM core.group_binding WHERE workspace_id=$1 AND group_id=$2`,
			workspaceID, groupID).Scan(&speaker, &lease)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if speaker == accountID && lease.After(now) {
				out.ActiveSpeakerAccountID = &speaker
			} else if lease.After(now) {
				// Another account holds the group speaker lease. The requesting
				// account is limited until that lease ends; this is data only and
				// never replaces the permit gate.
				out.CooldownUntil = &lease
			}
		}

		query := `SELECT summary_id,summary_version,window_start,window_end,window_message_count,topics,
			generated_at,LEAST(expires_at,window_start+make_interval(days=>retention_days)) AS effective_expires_at,
			privacy_class,source_manifest_sha256
			FROM core.group_summary
			WHERE workspace_id=$1 AND account_id=$2 AND group_id=$3
			AND expires_at>$4 AND window_start+make_interval(days=>retention_days)>$4
			AND generated_at<=$5`
		args := []any{workspaceID, accountID, groupID, now, now.Add(summaryGeneratedSkew)}
		if ref.SummaryID != "" {
			query += ` AND summary_id=$6 AND summary_version=$7`
			args = append(args, ref.SummaryID, ref.SummaryVersion)
		}
		query += ` ORDER BY summary_version DESC LIMIT 1`

		var summaryID, privacyClass, manifest string
		var version, messageCount int
		var windowStart, windowEnd, generatedAt, expiresAt time.Time
		var topicsJSON []byte
		err = tx.QueryRow(ctx, query, args...).Scan(
			&summaryID, &version, &windowStart, &windowEnd, &messageCount, &topicsJSON,
			&generatedAt, &expiresAt, &privacyClass, &manifest)
		if errors.Is(err, pgx.ErrNoRows) {
			if ref.SummaryID != "" {
				return problem(taskCodeNotFound)
			}
			out.Source = summarySourceChatPolicy
			out.PrivacyClass = "none"
			out.GeneratedAt = now
			out.ExpiresAt = now.Add(summaryContextPolicyTTL)
			return nil
		}
		if err != nil {
			return err
		}
		var topics []storedSummaryTopic
		if err := json.Unmarshal(topicsJSON, &topics); err != nil {
			return problem(taskCodeStoreUnavailable)
		}
		summaryRef := summaryID + "@v" + strconv.Itoa(version)
		out.SummaryRef = &summaryRef
		out.SummaryID = &summaryID
		out.SummaryVersion = &version
		out.SourceManifestSHA256 = &manifest
		out.PrivacyClass = privacyClass
		out.Source = summarySourceCollector
		out.GeneratedAt = generatedAt
		out.ExpiresAt = expiresAt
		out.ActivityWindow = &GroupContextActivityWindow{Start: windowStart, End: windowEnd, MessageCount: messageCount}
		for _, topic := range topics {
			out.Topics = append(out.Topics, GroupContextTopic{
				Summary:      topic.Summary,
				MessageRefs:  topic.MessageRefs,
				EvidenceKind: topic.EvidenceKind,
			})
		}
		return nil
	})
	return out, err
}

// PurgeExpiredGroupSummaries deletes summaries whose expiry or source
// retention window has passed. Reads already filter both conditions, so this is
// physical retention cleanup rather than the enforcement point.
func (s *Store) PurgeExpiredGroupSummaries(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 10_000 {
		return 0, problem(taskCodeValidation)
	}
	var removed int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM core.group_summary WHERE ctid IN (
			SELECT ctid FROM core.group_summary
			WHERE expires_at<=clock_timestamp()
			OR window_start+make_interval(days=>retention_days)<=clock_timestamp()
			ORDER BY expires_at LIMIT $1)`, limit)
		if err != nil {
			return err
		}
		removed = tag.RowsAffected()
		return nil
	})
	return removed, err
}
