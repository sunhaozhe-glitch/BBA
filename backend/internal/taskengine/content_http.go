package taskengine

import (
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ContentHandler never issues a permit, invokes a writer, or changes old approvals.
func (s *Store) ContentHandler(auth func(*http.Request) (Actor, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, code string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		a, e := auth(r)
		if e != nil || a.WorkspaceID == "" || a.PrincipalID == "" {
			fail(401, "UNAUTHENTICATED")
			return
		}
		if a.RuntimeID != "" {
			fail(403, taskCodeForbidden)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/operator/"), "/")
		if len(parts) > 3 || len(parts) < 1 || (parts[0] != "content-rules" && parts[0] != "content-reviews") {
			fail(404, taskCodeNotFound)
			return
		}
		rules := parts[0] == "content-rules"
		id := ""
		action := ""
		if len(parts) > 1 {
			id = parts[1]
		}
		if len(parts) > 2 {
			action = parts[2]
		}
		preview := rules && id == "preview" && action == ""
		permission := "content.read"
		if r.Method != "GET" && !preview {
			permission = "content.configure"
			if !rules {
				permission = "content.review"
			}
		}
		if !a.Permissions[permission] {
			fail(403, taskCodeForbidden)
			return
		}
		var exists bool
		if e = s.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM core.principal WHERE workspace_id=$1 AND id=$2)`, a.WorkspaceID, a.PrincipalID).Scan(&exists); e != nil {
			fail(503, taskCodeStoreUnavailable)
			return
		}
		if !exists {
			fail(403, taskCodeForbidden)
			return
		}
		decode := func(v any) bool {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				fail(415, "UNSUPPORTED_MEDIA_TYPE")
				return false
			}
			r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
			d := json.NewDecoder(r.Body)
			d.DisallowUnknownFields()
			if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
				fail(400, taskCodeValidation)
				return false
			}
			return true
		}
		ctx := r.Context()
		// All workspace rule gates are acquired BEFORE borrowing Pool. Holding
		// a pooled transaction while waiting here would starve send review and
		// result persistence when MaxConns=1. Reviews do not acquire this gate.
		var ruleGuard *contentGuard
		if rules {
			gate, err := s.acquireContentGuard(ctx, a.WorkspaceID)
			if err != nil {
				fail(503, taskCodeStoreUnavailable)
				return
			}
			defer gate.close()
			ruleGuard = gate
		}
		if preview {
			if r.Method != "POST" {
				fail(405, "METHOD_NOT_ALLOWED")
				return
			}
			var b struct {
				Text string `json:"text"`
			}
			if !decode(&b) {
				return
			}
			tx := ruleGuard.tx
			items, rev, e := contentLoad(ctx, tx, a.WorkspaceID)
			if e != nil {
				fail(503, taskCodeStoreUnavailable)
				return
			}
			hits, e := contentMatch(items, b.Text)
			if e != nil {
				fail(400, taskCodeValidation)
				return
			}
			decision := "allowed"
			if len(hits) > 0 {
				decision = "blocked"
			}
			if strings.Contains(strings.ToLower(b.Text), "[cq:") {
				decision = "unsupported"
			}
			reply(map[string]any{"decision": decision, "hits": hits, "rules_version": rev, "text_digest": ChatTextDigest(b.Text), "simulation": true, "grants_send_permission": false})
			return
		}
		if id != "" && !taskValidID(id) {
			fail(404, taskCodeNotFound)
			return
		}
		if rules && r.Method == "GET" {
			if action != "" && action != "history" {
				fail(404, taskCodeNotFound)
				return
			}
			if action == "history" {
				rows, e := s.Pool.Query(ctx, `SELECT document FROM core.content_rule_history WHERE workspace_id=$1 AND id=$2 ORDER BY version DESC`, a.WorkspaceID, id)
				if e != nil {
					fail(503, taskCodeStoreUnavailable)
					return
				}
				defer rows.Close()
				items := []json.RawMessage{}
				for rows.Next() {
					var b []byte
					if e = rows.Scan(&b); e != nil {
						fail(503, taskCodeStoreUnavailable)
						return
					}
					items = append(items, json.RawMessage(b))
				}
				if rows.Err() != nil {
					fail(503, taskCodeStoreUnavailable)
					return
				}
				if len(items) == 0 {
					fail(404, taskCodeNotFound)
					return
				}
				reply(map[string]any{"items": items})
				return
			}
			tx := ruleGuard.tx
			items, rev, e := contentLoad(ctx, tx, a.WorkspaceID)
			if e != nil {
				fail(503, taskCodeStoreUnavailable)
				return
			}
			if id != "" {
				for _, x := range items {
					if x.ID == id {
						reply(x)
						return
					}
				}
				fail(404, taskCodeNotFound)
				return
			}
			reply(map[string]any{"items": items, "version": rev, "can_configure": a.Permissions["content.configure"]})
			return
		}
		if rules {
			if action != "" || !((r.Method == "POST" && id == "") || ((r.Method == "PUT" || r.Method == "DELETE") && id != "")) {
				fail(405, "METHOD_NOT_ALLOWED")
				return
			}
			var b struct {
				ContentRule
				ExpectedVersion int `json:"expected_version"`
			}
			if !decode(&b) {
				return
			}
			if b.ID != "" || b.Version != 0 || b.Deleted || b.UpdatedAt != "" || b.UpdatedBy != "" {
				fail(400, taskCodeValidation)
				return
			}
			if r.Method != "DELETE" {
				if _, e = contentMatcher(b.ContentRule); e != nil {
					fail(400, taskCodeValidation)
					return
				}
			}
			var saved ContentRule
			e = func(tx pgx.Tx) error {
				var old ContentRule
				var v int
				var raw []byte
				if id != "" {
					e := tx.QueryRow(ctx, `SELECT version,document FROM core.content_rule WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, a.WorkspaceID, id).Scan(&v, &raw)
					if errors.Is(e, pgx.ErrNoRows) {
						return problem(taskCodeNotFound)
					}
					if e != nil {
						return e
					}
					if e = json.Unmarshal(raw, &old); e != nil {
						return e
					}
					if old.Deleted {
						return problem(taskCodeNotFound)
					}
				}
				if v != b.ExpectedVersion {
					return problem(taskCodeVersionConflict)
				}
				saved = b.ContentRule
				if r.Method == "DELETE" {
					saved = old
					saved.Enabled = false
					saved.Deleted = true
				}
				if id == "" {
					id = newID("rule")
				}
				saved.ID = id
				saved.Version = v + 1
				saved.UpdatedBy = a.PrincipalID
				saved.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				raw, e := json.Marshal(saved)
				if e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `INSERT INTO core.content_rule(workspace_id,id,version,document) VALUES($1,$2,$3,$4) ON CONFLICT(workspace_id,id) DO UPDATE SET version=excluded.version,document=excluded.document`, a.WorkspaceID, id, saved.Version, raw); e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `INSERT INTO core.content_rule_history(workspace_id,id,version,document,updated_by) VALUES($1,$2,$3,$4,$5)`, a.WorkspaceID, id, saved.Version, raw, a.PrincipalID); e != nil {
					return e
				}
				_, e = tx.Exec(ctx, `INSERT INTO core.content_revision(workspace_id,version) VALUES($1,1) ON CONFLICT(workspace_id) DO UPDATE SET version=core.content_revision.version+1`, a.WorkspaceID)
				return e
			}(ruleGuard.tx)
			if e == nil {
				e = ruleGuard.tx.Commit(ctx)
			}
			if e != nil {
				var ce *Error
				if errors.As(e, &ce) {
					fail(configStatus(e), Code(e))
				} else {
					fail(503, taskCodeStoreUnavailable)
				}
				return
			}
			reply(saved)
			return
		}
		// Reviews contain minimum-necessary digests and offsets, never raw text.
		if r.Method == "POST" && id != "" && action == "notes" {
			var b struct {
				Note string `json:"note"`
			}
			if !decode(&b) {
				return
			}
			if strings.TrimSpace(b.Note) == "" || len(b.Note) > 1000 {
				fail(400, taskCodeValidation)
				return
			}
			nid := newID("note")
			tag, e := s.Pool.Exec(ctx, `INSERT INTO core.content_review_note(workspace_id,review_id,id,note,updated_by) SELECT $1,$2,$3,$4,$5 WHERE EXISTS(SELECT 1 FROM core.content_review WHERE workspace_id=$1 AND id=$2)`, a.WorkspaceID, id, nid, b.Note, a.PrincipalID)
			if e != nil {
				fail(503, taskCodeStoreUnavailable)
				return
			}
			if tag.RowsAffected() == 0 {
				fail(404, taskCodeNotFound)
				return
			}
			reply(map[string]any{"id": nid, "recorded_only": true, "resent": false, "grants_send_permission": false})
			return
		}
		if r.Method != "GET" || action != "" {
			fail(405, "METHOD_NOT_ALLOWED")
			return
		}
		limit, offset := 20, 0
		for k, dst := range map[string]*int{"limit": &limit, "offset": &offset} {
			if v := r.URL.Query().Get(k); v != "" {
				n, e := strconv.Atoi(v)
				if e != nil {
					fail(400, taskCodeValidation)
					return
				}
				*dst = n
			}
		}
		if limit < 1 || limit > 100 || offset < 0 || offset > 1000000 {
			fail(400, taskCodeValidation)
			return
		}
		decision, account, send := r.URL.Query().Get("decision"), r.URL.Query().Get("account_id"), r.URL.Query().Get("send_id")
		if id != "" {
			limit = 1
			offset = 0
		}
		rows, e := s.Pool.Query(ctx, `SELECT id,account_id,conversation_kind,conversation_id,send_id,task_id,payload_digest,rules_version,decision,evidence,created_at FROM core.content_review WHERE workspace_id=$1 AND ($2='' OR id=$2) AND ($3='' OR decision=$3) AND ($4='' OR account_id=$4) AND ($5='' OR send_id=$5) ORDER BY created_at DESC,id DESC LIMIT $6 OFFSET $7`, a.WorkspaceID, id, decision, account, send, limit+1, offset)
		if e != nil {
			fail(503, taskCodeStoreUnavailable)
			return
		}
		items := []map[string]any{}
		for rows.Next() {
			var rid, ac, kind, conv, sid, tid, pd, dec string
			var rev int64
			var evidence []byte
			var at time.Time
			if e = rows.Scan(&rid, &ac, &kind, &conv, &sid, &tid, &pd, &rev, &dec, &evidence, &at); e != nil {
				rows.Close()
				fail(503, taskCodeStoreUnavailable)
				return
			}
			items = append(items, map[string]any{"id": rid, "account_id": ac, "conversation_kind": kind, "conversation_id": conv, "send_id": sid, "task_id": tid, "payload_digest": pd, "rules_version": rev, "decision": dec, "evidence": json.RawMessage(evidence), "created_at": at})
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			fail(503, taskCodeStoreUnavailable)
			return
		}
		if id != "" {
			if len(items) == 0 {
				fail(404, taskCodeNotFound)
				return
			}
			notes := []map[string]any{}
			nr, e := s.Pool.Query(ctx, `SELECT id,note,updated_by,created_at FROM core.content_review_note WHERE workspace_id=$1 AND review_id=$2 ORDER BY created_at,id`, a.WorkspaceID, id)
			if e != nil {
				fail(503, taskCodeStoreUnavailable)
				return
			}
			defer nr.Close()
			for nr.Next() {
				var nid, note, who string
				var at time.Time
				if e = nr.Scan(&nid, &note, &who, &at); e != nil {
					fail(503, taskCodeStoreUnavailable)
					return
				}
				notes = append(notes, map[string]any{"id": nid, "note": note, "updated_by": who, "created_at": at})
			}
			if nr.Err() != nil {
				fail(503, taskCodeStoreUnavailable)
				return
			}
			items[0]["notes"] = notes
			items[0]["can_note"] = a.Permissions["content.review"]
			reply(items[0])
			return
		}
		more := len(items) > limit
		if more {
			items = items[:limit]
		}
		reply(map[string]any{"items": items, "has_more": more, "offset": offset, "next_offset": offset + len(items)})
	})
}
