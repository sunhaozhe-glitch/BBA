package taskengine

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ContentRule is an operator configuration, never sending authority.
type ContentRule struct {
	ID            string `json:"id"`
	Version       int    `json:"version"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Expression    string `json:"expression"`
	CaseSensitive bool   `json:"case_sensitive"`
	Group         string `json:"group"`
	Priority      int    `json:"priority"`
	Enabled       bool   `json:"enabled"`
	Deleted       bool   `json:"deleted"`
	UpdatedBy     string `json:"updated_by"`
	UpdatedAt     string `json:"updated_at"`
}

func contentLock(ctx context.Context, tx pgx.Tx, ws string) error {
	_, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,732613))`, ws)
	return e
}
func contentMatcher(r ContentRule) (*regexp.Regexp, error) {
	if !utf8.ValidString(r.Expression) || len(r.Expression) == 0 || len(r.Expression) > 512 || len(r.Name) == 0 || len(r.Name) > 100 || len(r.Group) > 100 || r.Priority < 0 || r.Priority > 10000 {
		return nil, problem(taskCodeValidation)
	}
	expr := r.Expression
	switch r.Kind {
	case "keyword":
		expr = regexp.QuoteMeta(expr)
	case "pattern":
	default:
		return nil, problem(taskCodeValidation)
	}
	if !r.CaseSensitive {
		expr = "(?i:" + expr + ")"
	}
	re, e := regexp.Compile(expr)
	if e != nil || re.MatchString("") {
		return nil, problem(taskCodeValidation)
	}
	return re, nil
}
func contentLoad(ctx context.Context, tx pgx.Tx, ws string) ([]ContentRule, int64, error) {
	var rev int64
	if e := tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM core.content_revision WHERE workspace_id=$1),0)`, ws).Scan(&rev); e != nil {
		return nil, 0, e
	}
	rows, e := tx.Query(ctx, `SELECT id,version,document FROM core.content_rule WHERE workspace_id=$1 ORDER BY (document->>'priority')::int DESC,id`, ws)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	items := []ContentRule{}
	for rows.Next() {
		var id string
		var v int
		var b []byte
		if e = rows.Scan(&id, &v, &b); e != nil {
			return nil, 0, e
		}
		var r ContentRule
		if e = json.Unmarshal(b, &r); e != nil {
			return nil, 0, e
		}
		r.ID = id
		r.Version = v
		if !r.Deleted {
			if _, e = contentMatcher(r); e != nil {
				return nil, 0, e
			}
			items = append(items, r)
		}
	}
	return items, rev, rows.Err()
}
func contentMatch(items []ContentRule, text string) ([]map[string]any, error) {
	if !utf8.ValidString(text) || len(text) == 0 || len(text) > 8192 {
		return nil, problem(taskCodeValidation)
	}
	hits := []map[string]any{}
	for _, r := range items {
		if !r.Enabled {
			continue
		}
		re, e := contentMatcher(r)
		if e != nil {
			return nil, e
		}
		if loc := re.FindStringIndex(text); loc != nil {
			hits = append(hits, map[string]any{"rule_id": r.ID, "rule_version": r.Version, "byte_start": loc[0], "byte_end": loc[1]})
		}
	}
	return hits, nil
}

// contentInspect persists only digests and match offsets, never original text.
// The caller holds the workspace content lock until writer invocation completes.
func (s *Store) contentInspect(ctx context.Context, ws, account, kind, conversation, send, task, text, payloadDigest string, unsupported bool) (string, error) {
	return s.contentInspectFields(ctx, ws, account, kind, conversation, send, task, []contentField{{"text", text}}, payloadDigest, unsupported)
}

type contentField struct{ Name, Text string }

func (s *Store) contentInspectFields(ctx context.Context, ws, account, kind, conversation, send, task string, fields []contentField, payloadDigest string, unsupported bool) (string, error) {
	decision := "allowed"
	e := s.tx(ctx, func(tx pgx.Tx) error {
		items, rev, e := contentLoad(ctx, tx, ws)
		if e != nil {
			return e
		}
		hits := []map[string]any{}
		fieldEvidence := []map[string]any{}
		for _, field := range fields {
			// Empty optional platform fields have no matches, not missing authority.
			var matches []map[string]any
			if field.Text != "" {
				matches, e = contentMatch(items, field.Text)
				if e != nil {
					return e
				}
			}
			for _, hit := range matches {
				hit["field"] = field.Name
				hits = append(hits, hit)
			}
			fieldEvidence = append(fieldEvidence, map[string]any{"field": field.Name, "text_digest": ChatTextDigest(field.Text), "text_bytes": len(field.Text)})
			if strings.Contains(strings.ToLower(field.Text), "[cq:") {
				unsupported = true
			}
		}
		if unsupported {
			decision = "unsupported"
		} else if len(hits) > 0 {
			decision = "blocked"
		}
		evidence, _ := json.Marshal(map[string]any{"hits": hits, "fields": fieldEvidence, "raw_text_stored": false})
		_, e = tx.Exec(ctx, `INSERT INTO core.content_review(workspace_id,id,account_id,conversation_kind,conversation_id,send_id,task_id,payload_digest,rules_version,decision,evidence) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, ws, newID("review"), account, kind, conversation, send, task, payloadDigest, rev, decision, evidence)
		return e
	})
	return decision, e
}

// Digest-only chat consumption is retired unconditionally in chat_permit.go.
