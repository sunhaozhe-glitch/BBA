package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Login session vocabulary. The wire DTO is declared locally until the parent
// registers the login schemas in contracts/common.json; the HTTP layer only
// emits these strings.
const (
	loginStateCreated        = "CREATED"
	loginStateQRReady        = "QR_READY"
	loginStateScanned        = "SCANNED"
	loginStateAuthenticating = "AUTHENTICATING"
	loginStateOnline         = "ONLINE"
	loginStateExpired        = "EXPIRED"
	loginStateCancelled      = "CANCELLED"
	loginStateFailed         = "FAILED"
)

const (
	loginFailureProvider         = "PROVIDER_ERROR"
	loginFailureQRExpired        = "QR_EXPIRED"
	loginFailureTimeout          = "TIMEOUT"
	loginFailureIdentityMismatch = "IDENTITY_MISMATCH"
	loginFailureIdentityBound    = "IDENTITY_ALREADY_BOUND"
	loginProviderFixture         = "fixture"
	loginProviderNapCat          = "napcat"
	loginProviderDisabled        = "disabled"
	loginNextRequestQR           = "request_qr"
	loginNextPoll                = "poll"
	loginNextNone                = "none"
	loginPermReadOperator        = "operator.read"
	loginPermReadTask            = "task.read"
	loginMaxSessionsPerAccount   = 20
	loginDefaultSessionTTL       = 10 * time.Minute
	loginDefaultQRTTL            = 2 * time.Minute
	loginMaxQRImageBytes         = 512 << 10
)

// Login-specific wire codes. They are mapped to HTTP statuses in login_http.go;
// no raw provider message, token, URL or QQ identifier reaches a client.
const (
	loginCodeGenerationStale     = "LOGIN_GENERATION_STALE"
	loginCodeStateConflict       = "LOGIN_STATE_CONFLICT"
	loginCodeSessionActive       = "LOGIN_SESSION_ACTIVE"
	loginCodeProviderFailed      = "LOGIN_PROVIDER_FAILED"
	loginCodeIdentityMismatch    = "LOGIN_IDENTITY_MISMATCH"
	loginCodeIdentityAlreadyUsed = "LOGIN_IDENTITY_ALREADY_BOUND"
)

// LoginAccount is the server-side binding a provider may use. Secrets and URLs
// live only inside the provider; this struct carries no credential.
type LoginAccount struct {
	WorkspaceID       string
	AccountID         string
	AdapterInstanceID string
	RuntimeID         string
	ExpectedSelfID    string
}

// LoginQR is a short-lived QR image returned by the provider. ExpiresAt is a
// provider hint; the store clamps it to its own QR/session deadline, and the
// image is never persisted or logged.
type LoginQR struct {
	ImageDataURL string
	ExpiresAt    time.Time
}

// LoginPhase is the provider-neutral session phase.
type LoginPhase string

const (
	LoginPhaseWaiting        LoginPhase = "waiting"
	LoginPhaseScanned        LoginPhase = "scanned"
	LoginPhaseAuthenticating LoginPhase = "authenticating"
	LoginPhaseOnline         LoginPhase = "online"
	LoginPhaseExpired        LoginPhase = "expired"
	LoginPhaseFailed         LoginPhase = "failed"
)

// LoginStatus is the read-only provider view.
type LoginStatus struct {
	Phase  LoginPhase
	Reason string
}

// LoginIdentity is the verified QQ identity readback used to gate projection.
type LoginIdentity struct {
	SelfID   string
	Nickname string
}

// LoginProvider is the server-side adapter boundary. Implementations perform
// bounded calls; the store owns every durable transition and fence.
type LoginProvider interface {
	Name() string
	Start(context.Context, LoginAccount) (LoginQR, error)
	Refresh(context.Context, LoginAccount) (LoginQR, error)
	Status(context.Context, LoginAccount) (LoginStatus, error)
	Identity(context.Context, LoginAccount) (LoginIdentity, error)
}

// LoginSessionView is the console read projection. qr_png_data_url is filled
// by the HTTP handler from the bounded in-memory QR cache; it is never read
// from the database and never survives a restart.
type LoginSessionView struct {
	ID                string              `json:"id"`
	WorkspaceID       string              `json:"workspace_id"`
	AccountID         string              `json:"account_id"`
	AdapterInstanceID string              `json:"adapter_instance_id"`
	RuntimeID         string              `json:"runtime_id"`
	Provider          string              `json:"provider"`
	State             string              `json:"state"`
	Generation        int64               `json:"generation"`
	Version           int                 `json:"version"`
	Expired           bool                `json:"expired"`
	QRAvailable       bool                `json:"qr_available"`
	QRPNGDataURL      *string             `json:"qr_png_data_url"`
	QRExpiresAt       *time.Time          `json:"qr_expires_at"`
	NextAction        string              `json:"next_action"`
	FailureReason     *string             `json:"failure_reason"`
	Identity          *loginIdentityView  `json:"identity"`
	Projection        loginProjectionView `json:"projection"`
	ExpiresAt         time.Time           `json:"expires_at"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
}

type loginIdentityView struct {
	SelfID         string    `json:"self_id"`
	Nickname       string    `json:"nickname"`
	VerifiedAt     time.Time `json:"verified_at"`
	BindingChecked bool      `json:"binding_checked"`
}

type loginProjectionView struct {
	Projected              bool   `json:"projected"`
	AccountLoginState      string `json:"account_login_state"`
	AccountConnectionState string `json:"account_connection_state"`
	IdentityStatus         string `json:"identity_status"`
}

// loginSessionRow is the durable row plus the account binding read in the same
// statement. accountSelfID is nullable: an unbound account may be bound by the
// first verified online readback.
type loginSessionRow struct {
	WorkspaceID       string
	ID                string
	AccountID         string
	AdapterInstanceID string
	RuntimeID         string
	Provider          string
	State             string
	Generation        int64
	Version           int
	QRExpiresAt       *time.Time
	IdentitySelfID    *string
	IdentityNickname  *string
	VerifiedAt        *time.Time
	BindingChecked    bool
	Projected         bool
	FailureReason     *string
	ExpiresAt         time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	AccountSelfID     *string
	AccountAdapter    string
	AccountRuntime    string
	AccountLogin      string
	AccountConnection string
	AccountIdentity   string
}

const loginSessionColumns = `s.workspace_id,s.id,s.account_id,s.adapter_instance_id,s.runtime_id,s.provider,
	s.state,s.generation,s.version,s.qr_expires_at,s.identity_self_id,s.identity_nickname,s.verified_at,
	s.binding_checked,s.projected,s.failure_reason,s.expires_at,s.created_at,s.updated_at,
	a.self_id,a.adapter_instance_id,a.runtime_id,a.login_state,a.connection_state,a.identity_status`

func scanLoginSession(row pgx.Row) (loginSessionRow, error) {
	var r loginSessionRow
	err := row.Scan(&r.WorkspaceID, &r.ID, &r.AccountID, &r.AdapterInstanceID, &r.RuntimeID, &r.Provider,
		&r.State, &r.Generation, &r.Version, &r.QRExpiresAt, &r.IdentitySelfID, &r.IdentityNickname, &r.VerifiedAt,
		&r.BindingChecked, &r.Projected, &r.FailureReason, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt,
		&r.AccountSelfID, &r.AccountAdapter, &r.AccountRuntime, &r.AccountLogin, &r.AccountConnection, &r.AccountIdentity)
	return r, err
}

func loginSessionQuery(where string, lock bool) string {
	query := `SELECT ` + loginSessionColumns + ` FROM core.login_session s
		JOIN core.account a ON (a.workspace_id,a.id)=(s.workspace_id,s.account_id) WHERE ` + where
	if lock {
		query += ` FOR UPDATE OF s`
	}
	return query
}

func loginReadAllowed(actor Actor) error {
	if actor.WorkspaceID == "" || actor.PrincipalID == "" {
		return problem(taskCodeUnauthenticated)
	}
	if actor.RuntimeID != "" {
		return problem(taskCodeForbidden)
	}
	if !actor.Permissions[loginPermReadOperator] && !actor.Permissions[loginPermReadTask] && !actor.Permissions["task.manage_all"] {
		return problem(taskCodeForbidden)
	}
	return nil
}

func loginWriteAllowed(actor Actor) error {
	if err := loginReadAllowed(actor); err != nil {
		return err
	}
	if !actor.Permissions[taskPermControl] && !actor.Permissions["task.manage_all"] {
		return problem(taskCodeForbidden)
	}
	return nil
}

func loginSessionIdentifier(id string) bool {
	return taskValidID(id) && !strings.ContainsAny(id, "/?#")
}

// loginSessionView projects the durable row. Expiry is derived from timestamps
// for reads and is persisted only by an explicit fenced write.
func loginSessionView(r loginSessionRow, now time.Time) LoginSessionView {
	view := LoginSessionView{
		ID:                r.ID,
		WorkspaceID:       r.WorkspaceID,
		AccountID:         r.AccountID,
		AdapterInstanceID: r.AdapterInstanceID,
		RuntimeID:         r.RuntimeID,
		Provider:          r.Provider,
		State:             r.State,
		Generation:        r.Generation,
		Version:           r.Version,
		QRExpiresAt:       r.QRExpiresAt,
		FailureReason:     r.FailureReason,
		ExpiresAt:         r.ExpiresAt,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
		Projection: loginProjectionView{
			Projected:              r.Projected,
			AccountLoginState:      r.AccountLogin,
			AccountConnectionState: r.AccountConnection,
			IdentityStatus:         r.AccountIdentity,
		},
	}
	if r.IdentitySelfID != nil && r.VerifiedAt != nil {
		view.Identity = &loginIdentityView{
			SelfID:         *r.IdentitySelfID,
			Nickname:       loginDeref(r.IdentityNickname),
			VerifiedAt:     *r.VerifiedAt,
			BindingChecked: r.BindingChecked,
		}
	}
	if loginSessionOpen(r.State) && (now.After(r.ExpiresAt) || (r.QRExpiresAt != nil && now.After(*r.QRExpiresAt))) {
		view.Expired = true
	}
	switch r.State {
	case loginStateCreated, loginStateExpired:
		view.NextAction = loginNextRequestQR
	case loginStateQRReady, loginStateScanned, loginStateAuthenticating:
		view.NextAction = loginNextPoll
	default:
		view.NextAction = loginNextNone
	}
	return view
}

func loginDeref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func loginSessionOpen(state string) bool {
	switch state {
	case loginStateCreated, loginStateQRReady, loginStateScanned, loginStateAuthenticating:
		return true
	}
	return false
}

func loginSessionTerminal(state string) bool {
	switch state {
	case loginStateOnline, loginStateExpired, loginStateCancelled, loginStateFailed:
		return true
	}
	return false
}

func loginUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// loginIdemResponse is the durable idempotency payload. It deliberately stores
// only the session identity and the generation that completed, never the QR
// image: a lost response replay re-reads the session and never calls the
// provider again or re-delivers a secret.
type loginIdemResponse struct {
	SessionID  string `json:"session_id"`
	Generation int64  `json:"generation"`
}

// loginIdemLookup performs the replay pre-check before any provider call, so a
// retry of an already completed poll cannot advance state or hit the provider.
func (s *Store) loginIdemLookup(ctx context.Context, actor Actor, path, key, bodyDigest string) (loginIdemResponse, bool, error) {
	var storedDigest string
	var stored []byte
	err := s.Pool.QueryRow(ctx, `SELECT digest,response FROM core.idempotency
		WHERE workspace_id=$1 AND principal_id=$2 AND method='POST' AND path=$3 AND key=$4`,
		actor.WorkspaceID, actor.PrincipalID, path, key).Scan(&storedDigest, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return loginIdemResponse{}, false, nil
	}
	if err != nil {
		return loginIdemResponse{}, false, err
	}
	if storedDigest != bodyDigest {
		return loginIdemResponse{}, false, problem(taskCodeIdempotency)
	}
	var out loginIdemResponse
	if err := json.Unmarshal(stored, &out); err != nil || out.SessionID == "" {
		return loginIdemResponse{}, false, problem(taskCodeStoreUnavailable)
	}
	return out, true, nil
}

func loginWriteIdempotency(ctx context.Context, tx pgx.Tx, actor Actor, path, key, bodyDigest, sessionID string, generation int64) error {
	return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, sessionID,
		loginIdemResponse{SessionID: sessionID, Generation: generation})
}

// loginReadSession loads one session read-only. It never writes and never
// calls a provider, so a page refresh cannot mutate session state.
func (s *Store) loginReadSession(ctx context.Context, actor Actor, id string) (loginSessionRow, error) {
	if err := loginReadAllowed(actor); err != nil {
		return loginSessionRow{}, err
	}
	if !loginSessionIdentifier(id) {
		return loginSessionRow{}, problem(taskCodeNotFound)
	}
	row, err := scanLoginSession(s.Pool.QueryRow(ctx, loginSessionQuery(`s.workspace_id=$1 AND s.id=$2`, false), actor.WorkspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return loginSessionRow{}, problem(taskCodeNotFound)
	}
	return row, err
}

// loginLoadForWrite locks the session row and re-reads the account binding.
func loginLoadForWrite(ctx context.Context, tx pgx.Tx, workspaceID, id string) (loginSessionRow, error) {
	row, err := scanLoginSession(tx.QueryRow(ctx, loginSessionQuery(`s.workspace_id=$1 AND s.id=$2`, true), workspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return row, problem(taskCodeNotFound)
	}
	return row, err
}

func loginReload(ctx context.Context, tx pgx.Tx, workspaceID, id string) (loginSessionRow, error) {
	row, err := scanLoginSession(tx.QueryRow(ctx, loginSessionQuery(`s.workspace_id=$1 AND s.id=$2`, false), workspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return row, problem(taskCodeNotFound)
	}
	return row, err
}

// loginAppendEvent records a durable audit event inside the session
// transaction. The session row is locked first, so max(seq)+1 is serialized.
// detail carries bounded provider/state facts only, never a QR or credential.
func loginAppendEvent(ctx context.Context, tx pgx.Tx, row loginSessionRow, kind string, detail map[string]any, actor string) error {
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(seq),0)+1 FROM core.login_session_event WHERE workspace_id=$1 AND session_id=$2`, row.WorkspaceID, row.ID).Scan(&seq); err != nil {
		return err
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["state"] = row.State
	detail["generation"] = row.Generation
	payload, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO core.login_session_event(workspace_id,session_id,seq,id,kind,generation,detail,actor) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		row.WorkspaceID, row.ID, seq, newID("login_evt"), kind, row.Generation, payload, actor)
	return err
}

// loginApplyState writes one fenced non-ONLINE transition. It re-checks the
// expected generation under the row lock, so a late provider response can never
// overwrite a newer QR or reopen a cancelled session.
func loginApplyState(ctx context.Context, tx pgx.Tx, row loginSessionRow, expectedGeneration int64, state string, qrExpiresAt *time.Time, failure *string) (loginSessionRow, error) {
	if row.Generation != expectedGeneration {
		return row, problem(loginCodeGenerationStale)
	}
	if !loginSessionOpen(row.State) {
		return row, problem(loginCodeStateConflict)
	}
	// Only a new QR generation or a terminal transition advances the fence.
	// Scanned/authenticating observations keep the same generation so a client
	// can poll repeatedly without chasing a moving expected_generation.
	generation := row.Generation
	if state == loginStateQRReady || loginSessionTerminal(state) {
		generation++
	}
	if _, err := tx.Exec(ctx, `UPDATE core.login_session SET
			state=$3,generation=$4,version=version+1,qr_expires_at=coalesce($5,qr_expires_at),
			failure_reason=$6,updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND id=$2`,
		row.WorkspaceID, row.ID, state, generation, qrExpiresAt, failure); err != nil {
		return row, err
	}
	return loginReload(ctx, tx, row.WorkspaceID, row.ID)
}

// loginApplyOnline verifies the readback identity against the account binding
// and only then projects readiness. A scan alone never reaches this function.
func loginApplyOnline(ctx context.Context, tx pgx.Tx, row loginSessionRow, expectedGeneration int64, identity LoginIdentity) (loginSessionRow, string, error) {
	if row.Generation != expectedGeneration {
		return row, "", problem(loginCodeGenerationStale)
	}
	if !loginSessionOpen(row.State) {
		return row, "", problem(loginCodeStateConflict)
	}
	var accountSelfID *string
	var accountAdapter, accountRuntime, loginState, connectionState, identityStatus string
	err := tx.QueryRow(ctx, `SELECT self_id,adapter_instance_id,runtime_id,login_state,connection_state,identity_status
		FROM core.account WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, row.WorkspaceID, row.AccountID).
		Scan(&accountSelfID, &accountAdapter, &accountRuntime, &loginState, &connectionState, &identityStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, "", problem(taskCodeNotFound)
	}
	if err != nil {
		return row, "", err
	}
	fail := func(reason string) (loginSessionRow, string, error) {
		updated, err := loginApplyState(ctx, tx, row, expectedGeneration, loginStateFailed, nil, &reason)
		if err != nil {
			return row, "", err
		}
		if err := loginAppendEvent(ctx, tx, updated, "failed", map[string]any{"reason": reason}, "login"); err != nil {
			return row, "", err
		}
		code := loginCodeIdentityMismatch
		if reason == loginFailureIdentityBound {
			code = loginCodeIdentityAlreadyUsed
		}
		return updated, code, nil
	}
	if !taskValidQQID(identity.SelfID) || taskBlank(identity.Nickname) || len(identity.Nickname) > 128 ||
		row.AdapterInstanceID != accountAdapter || accountAdapter == "" || accountRuntime != row.RuntimeID {
		return fail(loginFailureIdentityMismatch)
	}
	if accountSelfID != nil && *accountSelfID != identity.SelfID {
		return fail(loginFailureIdentityMismatch)
	}
	if accountSelfID == nil {
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.account WHERE workspace_id=$1 AND self_id=$2 AND id<>$3)`,
			row.WorkspaceID, identity.SelfID, row.AccountID).Scan(&taken); err != nil {
			return row, "", err
		}
		if taken {
			return fail(loginFailureIdentityBound)
		}
	}
	changed := accountSelfID == nil || *accountSelfID != identity.SelfID ||
		identityStatus != "verified" || connectionState != "online" || loginState != "logged_in"
	if changed {
		// A savepoint confines a concurrent workspace-unique self_id conflict to
		// the projection statement without aborting the session transaction.
		if _, err := tx.Exec(ctx, `SAVEPOINT login_projection`); err != nil {
			return row, "", err
		}
		_, err := tx.Exec(ctx, `UPDATE core.account SET
				self_id=coalesce(self_id,$3),identity_status='verified',connection_state='online',login_state='logged_in',version=version+1
			WHERE workspace_id=$1 AND id=$2`, row.WorkspaceID, row.AccountID, identity.SelfID)
		if err != nil {
			if _, rbErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT login_projection`); rbErr != nil {
				return row, "", rbErr
			}
			if _, relErr := tx.Exec(ctx, `RELEASE SAVEPOINT login_projection`); relErr != nil {
				return row, "", relErr
			}
			if loginUniqueViolation(err) {
				return fail(loginFailureIdentityBound)
			}
			return row, "", err
		}
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT login_projection`); err != nil {
			return row, "", err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE core.login_session SET
			state='ONLINE',generation=$3,version=version+1,
			identity_self_id=$4,identity_nickname=$5,verified_at=clock_timestamp(),
			binding_checked=true,projected=true,failure_reason=NULL,updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND id=$2`, row.WorkspaceID, row.ID, row.Generation+1, identity.SelfID, identity.Nickname); err != nil {
		return row, "", err
	}
	updated, err := loginReload(ctx, tx, row.WorkspaceID, row.ID)
	if err != nil {
		return row, "", err
	}
	if err := loginAppendEvent(ctx, tx, updated, "online", map[string]any{"adapter_instance_id": row.AdapterInstanceID}, "login"); err != nil {
		return row, "", err
	}
	return updated, "", nil
}

// loginExpireDue persists EXPIRED when the session or QR deadline has passed.
// It returns the current row; expired reports whether the transition happened.
func (s *Store) loginExpireDue(ctx context.Context, actor Actor, id string) (loginSessionRow, bool, error) {
	var row loginSessionRow
	expired := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		locked, err := loginLoadForWrite(ctx, tx, actor.WorkspaceID, id)
		if err != nil {
			return err
		}
		row = locked
		if !loginSessionOpen(locked.State) {
			return nil
		}
		now := time.Now().UTC()
		reason := ""
		switch {
		case now.After(locked.ExpiresAt):
			reason = loginFailureTimeout
		case locked.QRExpiresAt != nil && now.After(*locked.QRExpiresAt):
			reason = loginFailureQRExpired
		default:
			return nil
		}
		updated, err := loginApplyState(ctx, tx, locked, locked.Generation, loginStateExpired, nil, &reason)
		if err != nil {
			return err
		}
		if err := loginAppendEvent(ctx, tx, updated, "expired", map[string]any{"reason": reason}, "login"); err != nil {
			return err
		}
		row, expired = updated, true
		return nil
	})
	return row, expired, err
}

// loginCreate creates the durable scoped session. A second open session for the
// account is rejected by the partial unique index, including under races.
func (s *Store) loginCreate(ctx context.Context, actor Actor, provider LoginProvider, accountID, path, key, bodyDigest string, opts loginOptions) (LoginSessionView, error) {
	if err := loginWriteAllowed(actor); err != nil {
		return LoginSessionView{}, err
	}
	if provider == nil {
		return LoginSessionView{}, problem(taskCodeFeatureDisabled)
	}
	if idem, found, err := s.loginIdemLookup(ctx, actor, path, key, bodyDigest); err != nil {
		return LoginSessionView{}, err
	} else if found {
		row, err := s.loginReadSession(ctx, actor, idem.SessionID)
		return loginSessionView(row, time.Now().UTC()), err
	}
	if !loginSessionIdentifier(accountID) {
		return LoginSessionView{}, problem(taskCodeNotFound)
	}
	var view LoginSessionView
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var runtimeID, adapterInstanceID string
		var selfID *string
		err := tx.QueryRow(ctx, `SELECT runtime_id,adapter_instance_id,self_id FROM core.account WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, actor.WorkspaceID, accountID).Scan(&runtimeID, &adapterInstanceID, &selfID)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(taskCodeNotFound)
		}
		if err != nil {
			return err
		}
		var openID string
		err = tx.QueryRow(ctx, `SELECT id FROM core.login_session WHERE workspace_id=$1 AND account_id=$2 AND state IN ('CREATED','QR_READY','SCANNED','AUTHENTICATING')`, actor.WorkspaceID, accountID).Scan(&openID)
		if err == nil {
			return problem(loginCodeSessionActive)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		sessionID := newID("login")
		if _, err := tx.Exec(ctx, `INSERT INTO core.login_session(
				workspace_id,id,account_id,adapter_instance_id,runtime_id,provider,state,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,'CREATED',clock_timestamp()+$7::interval)`,
			actor.WorkspaceID, sessionID, accountID, adapterInstanceID, runtimeID, provider.Name(),
			fmt.Sprintf("%d seconds", int64(opts.sessionTTL/time.Second))); err != nil {
			if loginUniqueViolation(err) {
				return problem(loginCodeSessionActive)
			}
			return err
		}
		row, err := loginReload(ctx, tx, actor.WorkspaceID, sessionID)
		if err != nil {
			return err
		}
		if err := loginAppendEvent(ctx, tx, row, "created", map[string]any{"provider": provider.Name()}, actor.PrincipalID); err != nil {
			return err
		}
		if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, row.ID, row.Generation); err != nil {
			return err
		}
		view = loginSessionView(row, time.Now().UTC())
		return nil
	})
	return view, err
}

// loginIssueQR issues or refreshes the QR under the generation fence. Provider
// work happens before the write transaction; the transaction revalidates the
// generation so a stale response cannot land.
func (s *Store) loginIssueQR(ctx context.Context, actor Actor, provider LoginProvider, id, path, key, bodyDigest string, expectedGeneration int64, opts loginOptions) (LoginSessionView, *loginQRArtifact, error) {
	if err := loginWriteAllowed(actor); err != nil {
		return LoginSessionView{}, nil, err
	}
	if provider == nil {
		return LoginSessionView{}, nil, problem(taskCodeFeatureDisabled)
	}
	if !loginSessionIdentifier(id) {
		return LoginSessionView{}, nil, problem(taskCodeNotFound)
	}
	if idem, found, err := s.loginIdemLookup(ctx, actor, path, key, bodyDigest); err != nil {
		return LoginSessionView{}, nil, err
	} else if found {
		row, err := s.loginReadSession(ctx, actor, idem.SessionID)
		return loginSessionView(row, time.Now().UTC()), nil, err
	}
	if row, expired, err := s.loginExpireDue(ctx, actor, id); err != nil {
		return LoginSessionView{}, nil, err
	} else if expired {
		return loginSessionView(row, time.Now().UTC()), nil, problem(loginCodeStateConflict)
	}
	row, err := s.loginReadSession(ctx, actor, id)
	if err != nil {
		return LoginSessionView{}, nil, err
	}
	if row.Generation != expectedGeneration {
		return LoginSessionView{}, nil, problem(loginCodeGenerationStale)
	}
	if !loginSessionOpen(row.State) {
		return loginSessionView(row, time.Now().UTC()), nil, problem(loginCodeStateConflict)
	}
	account := loginAccount(row)
	kind := "qr_refreshed"
	var qr LoginQR
	if row.State == loginStateCreated {
		kind = "qr_issued"
		qr, err = provider.Start(ctx, account)
	} else {
		qr, err = provider.Refresh(ctx, account)
	}
	if err != nil {
		return s.loginFailProvider(ctx, actor, id, path, key, bodyDigest, expectedGeneration, "qr")
	}
	if !strings.HasPrefix(qr.ImageDataURL, "data:image/") || len(qr.ImageDataURL) > loginMaxQRImageBytes {
		return s.loginFailProvider(ctx, actor, id, path, key, bodyDigest, expectedGeneration, "qr")
	}
	now := time.Now().UTC()
	limit := now.Add(opts.qrTTL)
	expires := qr.ExpiresAt
	if expires.IsZero() || !expires.After(now) || expires.After(limit) {
		expires = limit
	}
	if expires.After(row.ExpiresAt) {
		expires = row.ExpiresAt
	}
	var view LoginSessionView
	artifact := &loginQRArtifact{Generation: row.Generation + 1, ImageDataURL: qr.ImageDataURL, ExpiresAt: expires}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		locked, err := loginLoadForWrite(ctx, tx, actor.WorkspaceID, id)
		if err != nil {
			return err
		}
		if locked.Generation != expectedGeneration {
			return problem(loginCodeGenerationStale)
		}
		if !loginSessionOpen(locked.State) {
			return problem(loginCodeStateConflict)
		}
		updated, err := loginApplyState(ctx, tx, locked, expectedGeneration, loginStateQRReady, &expires, nil)
		if err != nil {
			return err
		}
		if err := loginAppendEvent(ctx, tx, updated, kind, map[string]any{"qr_expires_at": expires.UTC().Format(time.RFC3339)}, actor.PrincipalID); err != nil {
			return err
		}
		if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation); err != nil {
			return err
		}
		view = loginSessionView(updated, time.Now().UTC())
		return nil
	})
	if err != nil {
		return LoginSessionView{}, nil, err
	}
	return view, artifact, nil
}

// loginFailProvider persists a durable FAILED PROVIDER_ERROR and returns 502.
func (s *Store) loginFailProvider(ctx context.Context, actor Actor, id, path, key, bodyDigest string, expectedGeneration int64, stage string) (LoginSessionView, *loginQRArtifact, error) {
	reason := loginFailureProvider
	err := s.tx(ctx, func(tx pgx.Tx) error {
		locked, err := loginLoadForWrite(ctx, tx, actor.WorkspaceID, id)
		if err != nil {
			return err
		}
		if locked.Generation != expectedGeneration {
			return problem(loginCodeGenerationStale)
		}
		if !loginSessionOpen(locked.State) {
			return problem(loginCodeStateConflict)
		}
		updated, err := loginApplyState(ctx, tx, locked, expectedGeneration, loginStateFailed, nil, &reason)
		if err != nil {
			return err
		}
		if err := loginAppendEvent(ctx, tx, updated, "failed", map[string]any{"reason": reason, "stage": stage}, "login"); err != nil {
			return err
		}
		return loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation)
	})
	if err != nil {
		return LoginSessionView{}, nil, err
	}
	return LoginSessionView{}, nil, problem(loginCodeProviderFailed)
}

// loginPoll asks the provider for the current phase and applies one fenced
// transition. A provider transport failure changes nothing (502); an identity
// mismatch persists FAILED and returns 409.
func (s *Store) loginPoll(ctx context.Context, actor Actor, provider LoginProvider, id, path, key, bodyDigest string, expectedGeneration int64) (LoginSessionView, error) {
	if err := loginWriteAllowed(actor); err != nil {
		return LoginSessionView{}, err
	}
	if provider == nil {
		return LoginSessionView{}, problem(taskCodeFeatureDisabled)
	}
	if !loginSessionIdentifier(id) {
		return LoginSessionView{}, problem(taskCodeNotFound)
	}
	if idem, found, err := s.loginIdemLookup(ctx, actor, path, key, bodyDigest); err != nil {
		return LoginSessionView{}, err
	} else if found {
		row, err := s.loginReadSession(ctx, actor, idem.SessionID)
		return loginSessionView(row, time.Now().UTC()), err
	}
	row, err := s.loginReadSession(ctx, actor, id)
	if err != nil {
		return LoginSessionView{}, err
	}
	if row.Generation != expectedGeneration {
		return LoginSessionView{}, problem(loginCodeGenerationStale)
	}
	if !loginSessionOpen(row.State) {
		return loginSessionView(row, time.Now().UTC()), problem(loginCodeStateConflict)
	}
	account := loginAccount(row)
	status, err := provider.Status(ctx, account)
	if err != nil {
		return LoginSessionView{}, problem(loginCodeProviderFailed)
	}
	var identity *LoginIdentity
	if status.Phase == LoginPhaseOnline {
		verified, err := provider.Identity(ctx, account)
		if err != nil {
			return LoginSessionView{}, problem(loginCodeProviderFailed)
		}
		identity = &verified
	}
	var view LoginSessionView
	var typedCode string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		locked, err := loginLoadForWrite(ctx, tx, actor.WorkspaceID, id)
		if err != nil {
			return err
		}
		if locked.Generation != expectedGeneration {
			return problem(loginCodeGenerationStale)
		}
		if !loginSessionOpen(locked.State) {
			return problem(loginCodeStateConflict)
		}
		now := time.Now().UTC()
		if now.After(locked.ExpiresAt) {
			updated, err := loginApplyState(ctx, tx, locked, expectedGeneration, loginStateExpired, nil, new(loginFailureTimeout))
			if err != nil {
				return err
			}
			if err := loginAppendEvent(ctx, tx, updated, "expired", map[string]any{"reason": loginFailureTimeout}, "login"); err != nil {
				return err
			}
			if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation); err != nil {
				return err
			}
			view = loginSessionView(updated, now)
			return nil
		}
		switch status.Phase {
		case LoginPhaseExpired:
			reason := loginFailureQRExpired
			updated, err := loginApplyState(ctx, tx, locked, expectedGeneration, loginStateExpired, nil, &reason)
			if err != nil {
				return err
			}
			if err := loginAppendEvent(ctx, tx, updated, "expired", map[string]any{"reason": reason}, "login"); err != nil {
				return err
			}
			if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation); err != nil {
				return err
			}
			view = loginSessionView(updated, now)
		case LoginPhaseScanned:
			updated, err := loginApplyState(ctx, tx, locked, expectedGeneration, loginStateScanned, nil, nil)
			if err != nil {
				return err
			}
			if err := loginAppendEvent(ctx, tx, updated, "scanned", nil, "login"); err != nil {
				return err
			}
			if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation); err != nil {
				return err
			}
			view = loginSessionView(updated, now)
		case LoginPhaseAuthenticating:
			updated, err := loginApplyState(ctx, tx, locked, expectedGeneration, loginStateAuthenticating, nil, nil)
			if err != nil {
				return err
			}
			if err := loginAppendEvent(ctx, tx, updated, "authenticating", nil, "login"); err != nil {
				return err
			}
			if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation); err != nil {
				return err
			}
			view = loginSessionView(updated, now)
		case LoginPhaseFailed:
			reason := status.Reason
			if reason == "" {
				reason = loginFailureProvider
			}
			updated, err := loginApplyState(ctx, tx, locked, expectedGeneration, loginStateFailed, nil, &reason)
			if err != nil {
				return err
			}
			if err := loginAppendEvent(ctx, tx, updated, "failed", map[string]any{"reason": reason}, "login"); err != nil {
				return err
			}
			if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation); err != nil {
				return err
			}
			view = loginSessionView(updated, now)
		case LoginPhaseOnline:
			if identity == nil {
				return problem(loginCodeProviderFailed)
			}
			updated, code, err := loginApplyOnline(ctx, tx, locked, expectedGeneration, *identity)
			if err != nil {
				return err
			}
			if code != "" {
				typedCode = code
				view = loginSessionView(updated, now)
				return loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation)
			}
			if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation); err != nil {
				return err
			}
			view = loginSessionView(updated, now)
		default:
			// Waiting/reconnecting: no transition, but the replay record still
			// prevents a lost-response retry from calling the provider again.
			view = loginSessionView(locked, now)
			if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, locked.ID, locked.Generation); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return LoginSessionView{}, err
	}
	if typedCode != "" {
		return view, problem(typedCode)
	}
	return view, nil
}

// loginCancel is terminal and bumps the generation, so an in-flight poll with
// the old generation can never reopen the session.
func (s *Store) loginCancel(ctx context.Context, actor Actor, id, path, key, bodyDigest string) (LoginSessionView, error) {
	if err := loginWriteAllowed(actor); err != nil {
		return LoginSessionView{}, err
	}
	if !loginSessionIdentifier(id) {
		return LoginSessionView{}, problem(taskCodeNotFound)
	}
	if idem, found, err := s.loginIdemLookup(ctx, actor, path, key, bodyDigest); err != nil {
		return LoginSessionView{}, err
	} else if found {
		row, err := s.loginReadSession(ctx, actor, idem.SessionID)
		return loginSessionView(row, time.Now().UTC()), err
	}
	var view LoginSessionView
	err := s.tx(ctx, func(tx pgx.Tx) error {
		locked, err := loginLoadForWrite(ctx, tx, actor.WorkspaceID, id)
		if err != nil {
			return err
		}
		switch {
		case locked.State == loginStateCancelled:
			view = loginSessionView(locked, time.Now().UTC())
			return loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, locked.ID, locked.Generation)
		case loginSessionTerminal(locked.State):
			return problem(loginCodeStateConflict)
		}
		updated, err := loginApplyState(ctx, tx, locked, locked.Generation, loginStateCancelled, nil, nil)
		if err != nil {
			return err
		}
		if err := loginAppendEvent(ctx, tx, updated, "cancelled", nil, actor.PrincipalID); err != nil {
			return err
		}
		if err := loginWriteIdempotency(ctx, tx, actor, path, key, bodyDigest, updated.ID, updated.Generation); err != nil {
			return err
		}
		view = loginSessionView(updated, time.Now().UTC())
		return nil
	})
	return view, err
}

func loginAccount(row loginSessionRow) LoginAccount {
	return LoginAccount{
		WorkspaceID:       row.WorkspaceID,
		AccountID:         row.AccountID,
		AdapterInstanceID: row.AdapterInstanceID,
		RuntimeID:         row.RuntimeID,
		ExpectedSelfID:    loginDeref(row.AccountSelfID),
	}
}

// loginList loads the bounded newest-first session list for one account.
func (s *Store) loginList(ctx context.Context, actor Actor, accountID string) ([]LoginSessionView, error) {
	if err := loginReadAllowed(actor); err != nil {
		return nil, err
	}
	if !loginSessionIdentifier(accountID) {
		return nil, problem(taskCodeNotFound)
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM core.account WHERE workspace_id=$1 AND id=$2)`, actor.WorkspaceID, accountID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, problem(taskCodeNotFound)
	}
	rows, err := s.Pool.Query(ctx, loginSessionQuery(`s.workspace_id=$1 AND s.account_id=$2`, false)+` ORDER BY s.created_at DESC,s.id DESC LIMIT $3`,
		actor.WorkspaceID, accountID, loginMaxSessionsPerAccount)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().UTC()
	items := make([]LoginSessionView, 0, 4)
	for rows.Next() {
		row, err := scanLoginSession(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, loginSessionView(row, now))
	}
	return items, rows.Err()
}
