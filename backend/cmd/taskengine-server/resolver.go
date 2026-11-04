package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"bba.local/taskengine/internal/taskengine"
)

const (
	maxResolverFileBytes = 64 << 10
	// unauthenticatedCode is the contract code returned for any rejected
	// credential. The resolver must classify its own failures: an untyped
	// error would be reported as a retryable storage fault.
	unauthenticatedCode = "UNAUTHENTICATED"
)

// unauthenticatedError carries the contract code through the HTTP layer via
// the exported taskengine.Error type instead of a blanket mapping.
func unauthenticatedError() error { return &taskengine.Error{Code: unauthenticatedCode} }

type credentialRecord struct {
	TokenSHA256 string   `json:"token_sha256"`
	WorkspaceID string   `json:"workspace_id"`
	PrincipalID string   `json:"principal_id"`
	Permissions []string `json:"permissions"`
	RuntimeID   string   `json:"runtime_id,omitempty"`
	OwnerEpoch  int64    `json:"owner_epoch,omitempty"`
	AccountIDs  []string `json:"account_ids,omitempty"`
}

type resolverFile struct {
	Credentials []credentialRecord `json:"credentials"`
}

func readResolverFile(path string) (resolverFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return resolverFile{}, fmt.Errorf("stat resolver file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxResolverFileBytes {
		return resolverFile{}, errors.New("resolver file must be regular, bounded, and mode 0600 or stricter")
	}
	file, err := os.Open(path)
	if err != nil {
		return resolverFile{}, fmt.Errorf("open resolver file: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxResolverFileBytes+1))
	decoder.DisallowUnknownFields()
	var config resolverFile
	if err := decoder.Decode(&config); err != nil {
		return resolverFile{}, fmt.Errorf("decode resolver file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return resolverFile{}, errors.New("resolver file must contain exactly one JSON value")
	}
	if len(config.Credentials) == 0 {
		return resolverFile{}, errors.New("resolver file has no credentials")
	}
	tokens := make(map[string]bool, len(config.Credentials))
	for _, record := range config.Credentials {
		if len(record.TokenSHA256) != sha256.Size*2 || record.TokenSHA256 != strings.ToLower(record.TokenSHA256) {
			return resolverFile{}, errors.New("resolver token digest must be lowercase SHA-256")
		}
		if _, err := hex.DecodeString(record.TokenSHA256); err != nil || tokens[record.TokenSHA256] {
			return resolverFile{}, errors.New("resolver token digest is malformed or reused across identities")
		}
		tokens[record.TokenSHA256] = true
		runtime := record.RuntimeID != ""
		if runtime {
			if record.PrincipalID != "" || len(record.Permissions) != 0 || record.OwnerEpoch <= 0 || len(record.AccountIDs) == 0 {
				return resolverFile{}, errors.New("runtime resolver identity must be distinct and account scoped")
			}
		} else if !validCredentialID(record.PrincipalID) || len(record.Permissions) == 0 || record.OwnerEpoch != 0 || len(record.AccountIDs) != 0 {
			return resolverFile{}, errors.New("operator resolver identity is incomplete or contains runtime fields")
		}
	}
	return config, nil
}

func loadResolver(path string) (func(*http.Request) (taskengine.Actor, error), error) {
	config, err := readResolverFile(path)
	if err != nil {
		return nil, err
	}
	actors := make(map[string]taskengine.Actor, len(config.Credentials))
	for _, record := range config.Credentials {
		if record.RuntimeID != "" {
			continue
		}
		if !validCredentialID(record.WorkspaceID) || !validCredentialID(record.PrincipalID) || len(record.Permissions) == 0 {
			return nil, errors.New("resolver credential is incomplete")
		}
		if len(record.TokenSHA256) != sha256.Size*2 || record.TokenSHA256 != strings.ToLower(record.TokenSHA256) {
			return nil, errors.New("resolver token digest must be lowercase SHA-256")
		}
		if _, err := hex.DecodeString(record.TokenSHA256); err != nil {
			return nil, errors.New("resolver token digest must be lowercase SHA-256")
		}
		permissions := make(map[string]bool, len(record.Permissions))
		for _, permission := range record.Permissions {
			switch permission {
			case "operator.read", "task.read", "task.manage_all", "task.control", "task.create", "task.approve", "model.request", "model.configure", "agent.configure", "skill.configure", "content.read", "content.configure", "content.review", "system.read", "system.configure", "conversation.read", "conversation.write", "notification.read":
				permissions[permission] = true
			default:
				return nil, errors.New("resolver contains unsupported permission")
			}
		}
		if len(permissions) != len(record.Permissions) {
			return nil, errors.New("resolver contains duplicate permissions")
		}
		if _, exists := actors[record.TokenSHA256]; exists {
			return nil, errors.New("resolver contains duplicate token digests")
		}
		actors[record.TokenSHA256] = taskengine.Actor{
			WorkspaceID: record.WorkspaceID,
			PrincipalID: record.PrincipalID,
			Permissions: permissions,
		}
	}
	return func(request *http.Request) (taskengine.Actor, error) {
		if len(request.Header.Values("Authorization")) != 1 {
			return taskengine.Actor{}, unauthenticatedError()
		}
		header := request.Header.Get("Authorization")
		if len(header) <= len("Bearer ") || !strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
			return taskengine.Actor{}, unauthenticatedError()
		}
		token := header[len("Bearer "):]
		if strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
			return taskengine.Actor{}, unauthenticatedError()
		}
		digest := sha256.Sum256([]byte(token))
		key := hex.EncodeToString(digest[:])
		for expected, actor := range actors {
			if subtle.ConstantTimeCompare([]byte(key), []byte(expected)) == 1 {
				return actor, nil
			}
		}
		return taskengine.Actor{}, unauthenticatedError()
	}, nil
}

func loadRuntimeResolver(path string) (func(*http.Request) (taskengine.RuntimeActor, error), error) {
	config, err := readResolverFile(path)
	if err != nil {
		return nil, err
	}
	actors := make(map[string]taskengine.RuntimeActor, len(config.Credentials))
	for _, record := range config.Credentials {
		if record.RuntimeID == "" {
			continue
		}
		if !validCredentialID(record.WorkspaceID) || !validCredentialID(record.RuntimeID) || record.OwnerEpoch <= 0 || len(record.AccountIDs) == 0 {
			return nil, errors.New("runtime resolver credential is incomplete")
		}
		seen := make(map[string]bool, len(record.AccountIDs))
		for _, accountID := range record.AccountIDs {
			if !validCredentialID(accountID) || seen[accountID] {
				return nil, errors.New("runtime resolver account scope is invalid")
			}
			seen[accountID] = true
		}
		if len(record.TokenSHA256) != sha256.Size*2 || record.TokenSHA256 != strings.ToLower(record.TokenSHA256) {
			return nil, errors.New("resolver token digest must be lowercase SHA-256")
		}
		if _, exists := actors[record.TokenSHA256]; exists {
			return nil, errors.New("resolver contains duplicate runtime token digests")
		}
		actors[record.TokenSHA256] = taskengine.RuntimeActor{
			WorkspaceID: record.WorkspaceID,
			RuntimeID:   record.RuntimeID,
			OwnerEpoch:  record.OwnerEpoch,
			AccountIDs:  append([]string(nil), record.AccountIDs...),
		}
	}
	return func(request *http.Request) (taskengine.RuntimeActor, error) {
		if len(request.Header.Values("Authorization")) != 1 {
			return taskengine.RuntimeActor{}, unauthenticatedError()
		}
		header := request.Header.Get("Authorization")
		if len(header) <= len("Bearer ") || !strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
			return taskengine.RuntimeActor{}, unauthenticatedError()
		}
		token := header[len("Bearer "):]
		if strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
			return taskengine.RuntimeActor{}, unauthenticatedError()
		}
		digest := sha256.Sum256([]byte(token))
		key := hex.EncodeToString(digest[:])
		for expected, actor := range actors {
			if subtle.ConstantTimeCompare([]byte(key), []byte(expected)) == 1 {
				return actor, nil
			}
		}
		return taskengine.RuntimeActor{}, unauthenticatedError()
	}, nil
}

func validateDeploymentOrigin(origin, listenAddr string) (string, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Host != listenAddr || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("origin must be the fixed http loopback origin matching the listen address")
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return "", errors.New("listen address must include a loopback IP and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("Task Engine deployment must bind to a loopback IP")
	}
	return parsed.Host, nil
}

func taskEngineSurface(next http.Handler, developmentOperations ...bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devOps := len(developmentOperations) > 0 && developmentOperations[0]
		path := r.URL.Path
		allowed := r.Method == http.MethodGet && (path == "/v1/accounts" || path == "/v1/tasks" || strings.HasPrefix(path, "/v1/tasks/"))
		if r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/tasks/") && strings.HasSuffix(path, "/reconcile") {
			allowed = true
		}
		if devOps && r.Method == http.MethodPost {
			allowed = path == "/v1/tasks" ||
				(strings.HasPrefix(path, "/v1/tasks/") && (strings.HasSuffix(path, "/controls") || strings.HasSuffix(path, "/plan-proposals") ||
					strings.HasSuffix(path, "/approve") || strings.HasSuffix(path, "/apply") || strings.HasSuffix(path, "/reconcile")))
		}
		if !allowed {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func runtimeSurface(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		allowed := path == "/v1/internal/group-summaries" || path == "/v1/internal/group-context" ||
			path == "/v1/internal/chat-triggers" || path == "/v1/internal/chat-context" ||
			path == "/v1/internal/chat-send-permits" ||
			(strings.HasPrefix(path, "/v1/internal/chat-send-permits/") && strings.Count(strings.TrimPrefix(path, "/v1/internal/chat-send-permits/"), "/") == 1)
		if !allowed {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type serverRoutes struct {
	Operator, Proposal, Orchestration, Graph, Login, Model, Runtime, Report http.Handler
	Summaries, Chat, Permits, WorkSettings                                  http.Handler
	OperatorConversations                                                   http.Handler
	DevelopmentOperations                                                   bool
}

func serverAPISurface(routes serverRoutes) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/v1/operator/system/work-slots" || strings.HasPrefix(path, "/v1/operator/system/task-work-slots/"):
			if routes.WorkSettings == nil {
				http.NotFound(w, r)
				return
			}
			routes.WorkSettings.ServeHTTP(w, r)
		case path == "/v1/operator/conversations" ||
			(strings.HasPrefix(path, "/v1/operator/conversations/") && strings.HasSuffix(path, "/messages")) ||
			(strings.HasPrefix(path, "/v1/operator/agent-threads/") && strings.HasSuffix(path, "/messages")) ||
			path == "/v1/operator/qq-drafts" ||
			(strings.HasPrefix(path, "/v1/operator/qq-drafts/") && r.Method == http.MethodGet) ||
			(strings.HasPrefix(path, "/v1/operator/qq-inbox/events") && r.Method == http.MethodPost) ||
			path == "/v1/operator/notifications" ||
			(strings.HasPrefix(path, "/v1/operator/notifications/") && r.Method == http.MethodPost):
			if routes.OperatorConversations == nil {
				http.NotFound(w, r)
				return
			}
			routes.OperatorConversations.ServeHTTP(w, r)
		case path == "/v1/operator/runtime":
			routes.Runtime.ServeHTTP(w, r)
		case strings.HasPrefix(path, "/v1/accounts/") && strings.Contains(path, "/groups/") && strings.HasSuffix(path, "/context"):
			routes.Report.ServeHTTP(w, r)
		case path == "/v1/model-requests":
			routes.Model.ServeHTTP(w, r)
		case path == "/v1/login/status" || strings.HasPrefix(path, "/v1/login-sessions/") || (strings.HasPrefix(path, "/v1/accounts/") && strings.HasSuffix(path, "/login-sessions")):
			routes.Login.ServeHTTP(w, r)
		case strings.HasPrefix(path, "/v1/tasks/") && strings.Contains(path, "/graph-sync"):
			routes.Graph.ServeHTTP(w, r)
		case strings.HasPrefix(path, "/v1/tasks/") && (strings.HasSuffix(path, "/orchestration") || strings.HasSuffix(path, "/materialize") || strings.HasSuffix(path, "/work-items")):
			routes.Orchestration.ServeHTTP(w, r)
		case path == "/v1/internal/group-summaries" || path == "/v1/internal/group-context":
			runtimeSurface(routes.Summaries).ServeHTTP(w, r)
		case path == "/v1/internal/chat-triggers" || path == "/v1/internal/chat-context":
			runtimeSurface(routes.Chat).ServeHTTP(w, r)
		case path == "/v1/internal/chat-send-permits" || strings.HasPrefix(path, "/v1/internal/chat-send-permits/"):
			runtimeSurface(routes.Permits).ServeHTTP(w, r)
		case strings.HasPrefix(path, "/v1/tasks/") && strings.Contains(path, "/plan-proposals"):
			if !routes.DevelopmentOperations || routes.Proposal == nil {
				http.NotFound(w, r)
				return
			}
			taskEngineSurface(routes.Proposal, true).ServeHTTP(w, r)
		case path == "/v1/accounts" || path == "/v1/tasks" || strings.HasPrefix(path, "/v1/tasks/"):
			taskEngineSurface(routes.Operator, routes.DevelopmentOperations).ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// operatorConversationAuth composes the operator resolver with the runtime
// resolver so the operator conversation surface can authenticate both
// identities over real HTTP: operator credentials reach the list/message/
// draft/notification paths, and a runtime credential (never an operator one)
// is mapped to an Actor carrying RuntimeID, the trusted OwnerEpoch, and the
// permissions the inbox event ingest path requires (chat.trigger for the runtime gate,
// conversation.write for the handler's generic write gate). The runtime
// branch uses the runtime id as the principal anchor because the handler's
// principal-existence gate runs on every path; fixtures seed a matching
// core.principal row. Operator paths keep rejecting runtime identities at
// the handler, and a token that resolves in neither resolver keeps the
// operator resolver's UNAUTHENTICATED classification.
func operatorConversationAuth(operator func(*http.Request) (taskengine.Actor, error), runtime func(*http.Request) (taskengine.RuntimeActor, error)) func(*http.Request) (taskengine.Actor, error) {
	if runtime == nil {
		return operator
	}
	return func(request *http.Request) (taskengine.Actor, error) {
		actor, err := operator(request)
		if err == nil {
			return actor, nil
		}
		runtimeActor, runtimeErr := runtime(request)
		if runtimeErr != nil {
			return taskengine.Actor{}, err
		}
		return taskengine.Actor{
			WorkspaceID: runtimeActor.WorkspaceID,
			PrincipalID: runtimeActor.RuntimeID,
			RuntimeID:   runtimeActor.RuntimeID,
			OwnerEpoch:  runtimeActor.OwnerEpoch,
			AccountIDs:  append([]string(nil), runtimeActor.AccountIDs...),
			Permissions: map[string]bool{"chat.trigger": true, "conversation.write": true},
		}, nil
	}
}

func validCredentialID(value string) bool {
	if strings.TrimSpace(value) != value || len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func fixedHost(next http.Handler, expectedHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != expectedHost || r.URL.Host != "" || r.URL.Scheme != "" {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
