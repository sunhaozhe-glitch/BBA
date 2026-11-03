package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const operatorConversationPath = "/v1/operator/conversations"

// OperatorConversationHandler exposes durable conversations, review-only QQ
// drafts, and local notification projections. It never calls a QQ writer.
func (s *Store) OperatorConversationHandler(auth func(*http.Request) (Actor, error)) http.Handler {
	return &operatorConversationHTTP{store: s, auth: auth}
}

type operatorConversationHTTP struct {
	store *Store
	auth  func(*http.Request) (Actor, error)
}

func (h *operatorConversationHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
	}
	actor, err := h.auth(r)
	if err != nil || actor.WorkspaceID == "" || actor.PrincipalID == "" {
		fail(http.StatusUnauthorized, "UNAUTHENTICATED")
		return
	}
	// The inbound adapter trusts runtime identity only; operator principals
	// cannot forge it, and runtime identities are refused on every other path.
	if r.URL.Path == "/v1/operator/qq-inbox/events" {
		if actor.RuntimeID == "" || !actor.Permissions["chat.trigger"] {
			fail(http.StatusForbidden, "ACTION_FORBIDDEN")
			return
		}
	} else if actor.RuntimeID != "" {
		fail(http.StatusUnauthorized, "UNAUTHENTICATED")
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	permission := "conversation.read"
	if r.Method != http.MethodGet {
		permission = "conversation.write"
	}
	if strings.HasPrefix(path, "/v1/operator/notifications") && r.Method == http.MethodPost {
		permission = "notification.read"
	}
	if !actor.Permissions[permission] {
		fail(http.StatusForbidden, "ACTION_FORBIDDEN")
		return
	}
	var principal bool
	if err = h.store.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM core.principal WHERE workspace_id=$1 AND id=$2)`, actor.WorkspaceID, actor.PrincipalID).Scan(&principal); err != nil {
		fail(503, taskCodeStoreUnavailable)
		return
	}
	if !principal {
		fail(403, "ACTION_FORBIDDEN")
		return
	}
	switch {
	case path == operatorConversationPath && r.Method == http.MethodGet:
		h.listConversations(w, r, actor)
	case path == operatorConversationPath && r.Method != http.MethodGet:
		fail(405, "METHOD_NOT_ALLOWED")
	case strings.HasPrefix(path, operatorConversationPath+"/") && strings.HasSuffix(path, "/messages") && r.Method == http.MethodGet:
		h.messages(w, r, actor, strings.TrimSuffix(strings.TrimPrefix(path, operatorConversationPath+"/"), "/messages"))
	case strings.HasPrefix(path, "/v1/operator/agent-threads/") && strings.HasSuffix(path, "/messages") && r.Method == http.MethodPost:
		h.agentMessage(w, r, actor, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/operator/agent-threads/"), "/messages"))
	case path == "/v1/operator/qq-drafts" && r.Method == http.MethodPost:
		h.createDraft(w, r, actor)
	case strings.HasPrefix(path, "/v1/operator/qq-drafts/") && r.Method == http.MethodGet:
		h.getDraft(w, r, actor, strings.TrimPrefix(path, "/v1/operator/qq-drafts/"))
	case path == "/v1/operator/notifications" && r.Method == http.MethodGet:
		h.notifications(w, r, actor)
	case path == "/v1/operator/qq-inbox/events" && r.Method == http.MethodPost:
		h.ingestEvent(w, r, actor)
	case strings.HasPrefix(path, "/v1/operator/notifications/") && r.Method == http.MethodPost:
		h.markNotification(w, r, actor, strings.TrimPrefix(path, "/v1/operator/notifications/"))
	default:
		if strings.HasPrefix(path, "/v1/operator/") {
			fail(405, "METHOD_NOT_ALLOWED")
		} else {
			fail(404, taskCodeNotFound)
		}
	}
}

func operatorDecode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, `{"error":{"code":"UNSUPPORTED_MEDIA_TYPE"}}`, 415)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, `{"error":{"code":"VALIDATION_ERROR"}}`, 400)
		return false
	}
	return true
}

func operatorPage(r *http.Request) (int, int, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 100 {
			return 0, 0, problem(taskCodeValidation)
		}
		limit = n
	}
	offset := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 0 || n > 1000000 {
			return 0, 0, problem(taskCodeValidation)
		}
		offset = n
	}
	return offset, limit, nil
}

func (h *operatorConversationHTTP) listConversations(w http.ResponseWriter, r *http.Request, a Actor) {
	off, lim, e := operatorPage(r)
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	rows, e := h.store.Pool.Query(r.Context(), `SELECT c.id,c.kind,c.account_id,c.conversation_kind,c.conversation_id,c.version,c.created_at,COALESCE(max(m.created_at),c.created_at),count(m.id) FILTER (WHERE m.source='qq' AND m.direction='inbound') FROM core.operator_conversation c LEFT JOIN core.operator_message m ON (m.workspace_id,m.conversation_id)=(c.workspace_id,c.id) WHERE c.workspace_id=$1 AND ($2='' OR c.account_id=$2) AND ($3='' OR c.conversation_kind=$3) AND ($4='' OR c.conversation_id=$4) AND (coalesce(cardinality($7::text[]),0)=0 OR c.account_id=ANY($7)) GROUP BY c.workspace_id,c.id ORDER BY max(m.created_at) DESC NULLS LAST,c.id LIMIT $5 OFFSET $6`, a.WorkspaceID, r.URL.Query().Get("account_id"), r.URL.Query().Get("conversation_kind"), r.URL.Query().Get("conversation_id"), lim+1, off, a.AccountIDs)
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, kind, ck, cid string
		var account *string
		var v int64
		var created, last time.Time
		var unread int64
		if e = rows.Scan(&id, &kind, &account, &ck, &cid, &v, &created, &last, &unread); e != nil {
			thWriteError(w, r, e)
			return
		}
		items = append(items, map[string]any{"id": id, "kind": kind, "account_id": account, "conversation_kind": ck, "conversation_id": cid, "version": v, "created_at": created, "last_event_at": last, "unread_count": unread})
	}
	if rows.Err() != nil {
		thWriteError(w, r, rows.Err())
		return
	}
	more := len(items) > lim
	if more {
		items = items[:lim]
	}
	next := ""
	if more {
		next = strconv.Itoa(off + lim)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "next_cursor": next}})
}

func (h *operatorConversationHTTP) messages(w http.ResponseWriter, r *http.Request, a Actor, id string) {
	if id == "" || strings.Contains(id, "/") {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	off, lim, e := operatorPage(r)
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	var ok bool
	if e = h.store.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM core.operator_conversation WHERE workspace_id=$1 AND id=$2 AND (coalesce(cardinality($3::text[]),0)=0 OR account_id IS NULL OR account_id=ANY($3)))`, a.WorkspaceID, id, a.AccountIDs).Scan(&ok); e != nil || !ok {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	rows, e := h.store.Pool.Query(r.Context(), `SELECT id,source,direction,delivery_state,body,body_digest,seq,created_at FROM core.operator_message WHERE workspace_id=$1 AND conversation_id=$2 ORDER BY seq DESC LIMIT $3 OFFSET $4`, a.WorkspaceID, id, lim+1, off)
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var mid, src, dir, state, body, dig string
		var seq int64
		var at time.Time
		if e = rows.Scan(&mid, &src, &dir, &state, &body, &dig, &seq, &at); e != nil {
			thWriteError(w, r, e)
			return
		}
		items = append(items, map[string]any{"id": mid, "source": src, "direction": dir, "delivery_state": state, "body": body, "body_digest": dig, "seq": seq, "created_at": at})
	}
	more := len(items) > lim
	if more {
		items = items[:lim]
	}
	next := ""
	if more {
		next = strconv.Itoa(off + lim)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "next_cursor": next}})
}

func (h *operatorConversationHTTP) agentMessage(w http.ResponseWriter, r *http.Request, a Actor, id string) {
	if id == "" || strings.Contains(id, "/") {
		thWriteError(w, r, problem(taskCodeValidation))
		return
	}
	var b struct {
		Body string `json:"body"`
	}
	if !operatorDecode(w, r, &b) {
		return
	}
	if !utf8.ValidString(b.Body) || len(b.Body) == 0 || len(b.Body) > 8192 || r.Header.Get("Idempotency-Key") == "" {
		thWriteError(w, r, problem(taskCodeValidation))
		return
	}
	if a.RuntimeID != "" {
		thWriteError(w, r, problem("RESOURCE_NOT_FOUND"))
		return
	}
	d := ChatTextDigest(b.Body)
	ctx := r.Context()
	var mid string
	var response map[string]any
	e := h.store.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM core.principal WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, a.WorkspaceID, a.PrincipalID); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, a.WorkspaceID, a.PrincipalID, r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"))
		if err != nil {
			return err
		}
		if found {
			if rec.digest != d {
				return problem(taskCodeIdempotency)
			}
			return json.Unmarshal(rec.response, &response)
		}
		_, er := tx.Exec(ctx, `INSERT INTO core.operator_conversation(workspace_id,id,kind,conversation_kind,conversation_id) VALUES($1,$2,'agent','agent',$2) ON CONFLICT DO NOTHING`, a.WorkspaceID, id)
		if er != nil {
			return er
		}
		var kind, ck, cid string
		var account *string
		if er = tx.QueryRow(ctx, `SELECT kind,conversation_kind,conversation_id,account_id FROM core.operator_conversation WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, a.WorkspaceID, id).Scan(&kind, &ck, &cid, &account); er != nil {
			return er
		}
		// Workspace/account/kind are immutable; legacy Agent display identifiers may
		// differ from the internal conversation row ID.
		if kind != "agent" || ck != "agent" || account != nil {
			return problem("RESOURCE_NOT_FOUND")
		}
		var seq int64
		if er = tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0)+1 FROM core.operator_message WHERE workspace_id=$1 AND conversation_id=$2`, a.WorkspaceID, id).Scan(&seq); er != nil {
			return er
		}
		mid = newID("msg")
		_, er = tx.Exec(ctx, `INSERT INTO core.operator_message(workspace_id,id,conversation_id,source,direction,delivery_state,body,body_digest,event_id,seq) VALUES($1,$2,$3,'agent','inbound','received',$4,$5,$6,$7)`, a.WorkspaceID, mid, id, b.Body, d, r.Header.Get("Idempotency-Key"), seq)
		if er != nil {
			return er
		}
		_, er = tx.Exec(ctx, `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,conversation_id,summary) VALUES($1,$2,$3,'agent.message',$4,$5,'Agent thread message') ON CONFLICT(workspace_id,COALESCE(conversation_id,''),event_key) DO NOTHING`, a.WorkspaceID, newID("notice"), "agent-message:"+id+":"+r.Header.Get("Idempotency-Key"), mid, id)
		if er != nil {
			return er
		}
		response = map[string]any{"data": map[string]any{"id": mid, "conversation_id": id, "source": "agent", "direction": "inbound", "delivery_state": "received", "body_digest": d, "send_permit": "absent"}}
		return taskWriteIdempotency(ctx, tx, a.WorkspaceID, a.PrincipalID, r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), d, mid, response)
	})
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(response)
}

func (h *operatorConversationHTTP) createDraft(w http.ResponseWriter, r *http.Request, a Actor) {
	var b struct {
		AccountID        string `json:"account_id"`
		ConversationKind string `json:"conversation_kind"`
		ConversationID   string `json:"conversation_id"`
		Body             string `json:"body"`
		ExpectedVersion  int64  `json:"expected_version"`
	}
	if !operatorDecode(w, r, &b) {
		return
	}
	if b.AccountID == "" || b.ConversationID == "" || (b.ConversationKind != "group" && b.ConversationKind != "private") || !utf8.ValidString(b.Body) || len(b.Body) == 0 || len(b.Body) > 8192 || b.ExpectedVersion != 1 || r.Header.Get("Idempotency-Key") == "" {
		thWriteError(w, r, problem(taskCodeValidation))
		return
	}
	if a.RuntimeID != "" || (len(a.AccountIDs) > 0 && !contains(a.AccountIDs, b.AccountID)) {
		thWriteError(w, r, problem("RESOURCE_NOT_FOUND"))
		return
	}
	ctx := r.Context()
	var draftID, msgID string
	decision := "draft"
	var rulesVersion int64
	var evidence []byte
	bodyDigest := ChatTextDigest(b.Body)
	requestBytes, _ := json.Marshal(b)
	requestDigest := ChatTextDigest(string(requestBytes))
	var response map[string]any
	e := h.store.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM core.principal WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, a.WorkspaceID, a.PrincipalID); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, a.WorkspaceID, a.PrincipalID, r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"))
		if err != nil {
			return err
		}
		if found {
			if rec.digest != requestDigest {
				return problem(taskCodeIdempotency)
			}
			return json.Unmarshal(rec.response, &response)
		}
		var exists bool
		if er := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.account WHERE workspace_id=$1 AND id=$2)`, a.WorkspaceID, b.AccountID).Scan(&exists); er != nil {
			return er
		}
		if !exists {
			return problem("RESOURCE_NOT_FOUND")
		}
		conv := newID("conv")
		var kind, account, external string
		_, er := tx.Exec(ctx, `INSERT INTO core.operator_conversation(workspace_id,id,kind,account_id,conversation_kind,conversation_id) VALUES($1,$2,'qq',$3,$4,$5) ON CONFLICT DO NOTHING`, a.WorkspaceID, conv, b.AccountID, b.ConversationKind, b.ConversationID)
		if er != nil {
			return er
		}
		if er = tx.QueryRow(ctx, `SELECT id,kind,account_id,conversation_id FROM core.operator_conversation WHERE workspace_id=$1 AND kind='qq' AND account_id=$2 AND conversation_kind=$3 AND conversation_id=$4 FOR UPDATE`, a.WorkspaceID, b.AccountID, b.ConversationKind, b.ConversationID).Scan(&conv, &kind, &account, &external); er != nil {
			return er
		}
		if kind != "qq" || account != b.AccountID || external != b.ConversationID {
			return problem("RESOURCE_NOT_FOUND")
		}
		items, rev, er := contentLoad(ctx, tx, a.WorkspaceID)
		if er != nil {
			return er
		}
		hits, er := contentMatch(items, b.Body)
		if er != nil {
			return er
		}
		rulesVersion = rev
		if strings.Contains(strings.ToLower(b.Body), "[cq:") {
			decision = "unsupported"
		} else if len(hits) > 0 {
			decision = "blocked"
		}
		evidence, _ = json.Marshal(map[string]any{"hits": hits, "text_digest": bodyDigest, "raw_text_stored_in_review": false, "send_permit": "absent"})
		var seq int64
		if er = tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0)+1 FROM core.operator_message WHERE workspace_id=$1 AND conversation_id=$2`, a.WorkspaceID, conv).Scan(&seq); er != nil {
			return er
		}
		msgID = newID("msg")
		draftID = newID("draft")
		state := decision
		if state == "unsupported" {
			state = "blocked"
		}
		_, er = tx.Exec(ctx, `INSERT INTO core.operator_message(workspace_id,id,conversation_id,source,direction,delivery_state,body,body_digest,event_id,seq) VALUES($1,$2,$3,'qq','outbound_draft',$4,$5,$6,$7,$8)`, a.WorkspaceID, msgID, conv, state, b.Body, bodyDigest, r.Header.Get("Idempotency-Key"), seq)
		if er != nil {
			return er
		}
		_, er = tx.Exec(ctx, `INSERT INTO core.qq_draft(workspace_id,id,conversation_id,account_id,message_id,payload_digest,rules_version,decision,evidence,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, a.WorkspaceID, draftID, conv, b.AccountID, msgID, bodyDigest, rev, decision, evidence, a.PrincipalID)
		if er != nil {
			return er
		}
		_, er = tx.Exec(ctx, `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(workspace_id,COALESCE(conversation_id,''),event_key) DO NOTHING`, a.WorkspaceID, newID("notice"), "draft:"+draftID, "qq.draft.created", draftID, b.AccountID, conv, "QQ draft review: "+decision)
		if er != nil {
			return er
		}
		response = map[string]any{"data": map[string]any{"id": draftID, "message_id": msgID, "payload_digest": bodyDigest, "rules_version": rulesVersion, "decision": decision, "evidence": json.RawMessage(evidence), "send_permit": "absent", "version": 1}}
		return taskWriteIdempotency(ctx, tx, a.WorkspaceID, a.PrincipalID, r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), requestDigest, draftID, response)
	})
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(response)
}

func (h *operatorConversationHTTP) getDraft(w http.ResponseWriter, r *http.Request, a Actor, id string) {
	if id == "" || strings.Contains(id, "/") {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	var account, conv, msg, dig, decision string
	var rev, version int64
	var evidence []byte
	e := h.store.Pool.QueryRow(r.Context(), `SELECT account_id,conversation_id,message_id,payload_digest,rules_version,decision,evidence,version FROM core.qq_draft WHERE workspace_id=$1 AND id=$2 AND (coalesce(cardinality($3::text[]),0)=0 OR account_id=ANY($3))`, a.WorkspaceID, id, a.AccountIDs).Scan(&account, &conv, &msg, &dig, &rev, &decision, &evidence, &version)
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "account_id": account, "conversation_id": conv, "message_id": msg, "payload_digest": dig, "rules_version": rev, "decision": decision, "evidence": json.RawMessage(evidence), "send_permit": "absent", "version": version}})
}

// operatorVerifyRuntimeCredential proves the calling runtime identity inside
// the write transaction: the account row must name this runtime as its current
// owner and a persisted credential row must exist for this
// workspace/runtime/account, be active, and sit at the account's current owner
// epoch, and the epoch claim carried by the presented token must match both.
// Shared locks on both rows keep a revocation or epoch rotation from landing
// between this proof and writes in the same transaction.
func operatorVerifyRuntimeCredential(ctx context.Context, tx pgx.Tx, a Actor, accountID string) error {
	if a.RuntimeID == "" {
		return problem("RESOURCE_NOT_FOUND")
	}
	var accountEpoch int64
	var accountRuntime string
	if err := tx.QueryRow(ctx, `SELECT owner_epoch,runtime_id FROM core.account WHERE workspace_id=$1 AND id=$2 FOR SHARE`, a.WorkspaceID, accountID).Scan(&accountEpoch, &accountRuntime); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("RESOURCE_NOT_FOUND")
		}
		return err
	}
	var credentialEpoch int64
	var active bool
	if err := tx.QueryRow(ctx, `SELECT owner_epoch,active FROM core.runtime_credentials WHERE workspace_id=$1 AND runtime_id=$2 AND account_id=$3 FOR SHARE`, a.WorkspaceID, a.RuntimeID, accountID).Scan(&credentialEpoch, &active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return problem("RESOURCE_NOT_FOUND")
		}
		return err
	}
	if accountRuntime != a.RuntimeID || !active || a.OwnerEpoch <= 0 || a.OwnerEpoch != accountEpoch || credentialEpoch != accountEpoch {
		return problem("RESOURCE_NOT_FOUND")
	}
	return nil
}

func (h *operatorConversationHTTP) ingestEvent(w http.ResponseWriter, r *http.Request, a Actor) {
	var b struct {
		AccountID        string `json:"account_id"`
		ConversationKind string `json:"conversation_kind"`
		ConversationID   string `json:"conversation_id"`
		EventID          string `json:"event_id"`
		Body             string `json:"body"`
	}
	if !operatorDecode(w, r, &b) {
		return
	}
	if b.AccountID == "" || b.ConversationID == "" || b.EventID == "" || (b.ConversationKind != "group" && b.ConversationKind != "private") || !utf8.ValidString(b.Body) || len(b.Body) == 0 || len(b.Body) > 8192 || r.Header.Get("Idempotency-Key") == "" {
		thWriteError(w, r, problem(taskCodeValidation))
		return
	}
	if !contains(a.AccountIDs, b.AccountID) {
		thWriteError(w, r, problem("RESOURCE_NOT_FOUND"))
		return
	}
	ctx := r.Context()
	digest := ChatTextDigest(b.Body)
	var existingID, existingDigest, messageID, conversationID string
	var replay bool
	err := h.store.tx(ctx, func(tx pgx.Tx) error {
		if er := operatorVerifyRuntimeCredential(ctx, tx, a, b.AccountID); er != nil {
			return er
		}
		conv := newID("conv")
		if _, er := tx.Exec(ctx, `INSERT INTO core.operator_conversation(workspace_id,id,kind,account_id,conversation_kind,conversation_id) VALUES($1,$2,'qq',$3,$4,$5) ON CONFLICT DO NOTHING`, a.WorkspaceID, conv, b.AccountID, b.ConversationKind, b.ConversationID); er != nil {
			return er
		}
		if er := tx.QueryRow(ctx, `SELECT id FROM core.operator_conversation WHERE workspace_id=$1 AND kind='qq' AND account_id=$2 AND conversation_kind=$3 AND conversation_id=$4`, a.WorkspaceID, b.AccountID, b.ConversationKind, b.ConversationID).Scan(&conv); er != nil {
			return er
		}
		if er := tx.QueryRow(ctx, `SELECT id,body_digest FROM core.operator_message WHERE workspace_id=$1 AND conversation_id=$2 AND event_id=$3`, a.WorkspaceID, conv, b.EventID).Scan(&existingID, &existingDigest); er != nil {
			if er != pgx.ErrNoRows {
				return er
			}
		} else {
			if existingDigest != digest {
				return problem(taskCodeIdempotency)
			}
			messageID, conversationID, replay = existingID, conv, true
			return nil
		}
		var seq int64
		if er := tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0)+1 FROM core.operator_message WHERE workspace_id=$1 AND conversation_id=$2`, a.WorkspaceID, conv).Scan(&seq); er != nil {
			return er
		}
		messageID = newID("msg")
		conversationID = conv
		if _, er := tx.Exec(ctx, `INSERT INTO core.operator_message(workspace_id,id,conversation_id,source,direction,delivery_state,body,body_digest,event_id,seq) VALUES($1,$2,$3,'qq','inbound','received',$4,$5,$6,$7)`, a.WorkspaceID, messageID, conv, b.Body, digest, b.EventID, seq); er != nil {
			return er
		}
		_, er := tx.Exec(ctx, `INSERT INTO core.operator_notification(workspace_id,id,event_key,event_type,object_id,account_id,conversation_id,summary,evidence_ref) VALUES($1,$2,$3,'qq.message.inbound',$4,$5,$6,'QQ inbox event received',$7) ON CONFLICT(workspace_id,COALESCE(conversation_id,''),event_key) DO NOTHING`, a.WorkspaceID, newID("notice"), "inbox:"+b.EventID, messageID, b.AccountID, conv, "sha256:"+digest)
		return er
	})
	if err != nil {
		thWriteError(w, r, err)
		return
	}
	if replay {
		w.WriteHeader(200)
	} else {
		w.WriteHeader(201)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": messageID, "conversation_id": conversationID, "source": "qq", "direction": "inbound", "delivery_state": "received", "event_id": b.EventID, "body_digest": digest, "idempotent_replay": replay}})
}

func (h *operatorConversationHTTP) notifications(w http.ResponseWriter, r *http.Request, a Actor) {
	off, lim, e := operatorPage(r)
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	rows, e := h.store.Pool.Query(r.Context(), `SELECT id,event_type,object_id,account_id,conversation_id,summary,evidence_ref,delivery_state,read_at,version,created_at FROM core.operator_notification WHERE workspace_id=$1 AND ($2='' OR account_id=$2) AND (coalesce(cardinality($5::text[]),0)=0 OR account_id=ANY($5)) ORDER BY created_at DESC,id DESC LIMIT $3 OFFSET $4`, a.WorkspaceID, r.URL.Query().Get("account_id"), lim+1, off, a.AccountIDs)
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, typ, obj, sum, ev, state string
		var account, conv *string
		var read *time.Time
		var version int64
		var at time.Time
		if e = rows.Scan(&id, &typ, &obj, &account, &conv, &sum, &ev, &state, &read, &version, &at); e != nil {
			thWriteError(w, r, e)
			return
		}
		items = append(items, map[string]any{"id": id, "event_type": typ, "object_id": obj, "account_id": account, "conversation_id": conv, "summary": sum, "evidence_ref": ev, "delivery_state": state, "read_at": read, "version": version, "created_at": at})
	}
	more := len(items) > lim
	if more {
		items = items[:lim]
	}
	next := ""
	if more {
		next = strconv.Itoa(off + lim)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "next_cursor": next}})
}

func (h *operatorConversationHTTP) markNotification(w http.ResponseWriter, r *http.Request, a Actor, id string) {
	if id == "" || strings.Contains(id, "/") {
		thWriteError(w, r, problem(taskCodeNotFound))
		return
	}
	var b struct {
		ExpectedVersion int64 `json:"expected_version"`
		Read            bool  `json:"read"`
	}
	if !operatorDecode(w, r, &b) {
		return
	}
	if b.ExpectedVersion < 1 {
		thWriteError(w, r, problem(taskCodeValidation))
		return
	}
	var version int64
	e := h.store.Pool.QueryRow(r.Context(), `UPDATE core.operator_notification SET read_at=CASE WHEN $4 THEN clock_timestamp() ELSE NULL END,version=version+1 WHERE workspace_id=$1 AND id=$2 AND version=$3 RETURNING version`, a.WorkspaceID, id, b.ExpectedVersion, b.Read).Scan(&version)
	if errors.Is(e, pgx.ErrNoRows) {
		thWriteError(w, r, problem("VERSION_CONFLICT"))
		return
	}
	if e != nil {
		thWriteError(w, r, e)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "read": b.Read, "version": version, "delivery_state": "local_only"}})
}
