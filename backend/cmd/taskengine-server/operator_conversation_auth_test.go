package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"bba.local/taskengine/internal/taskengine"
)

const (
	fixtureOperatorToken = "fixture-operator-token"
	fixtureRuntimeToken  = "fixture-runtime-token"
)

func digestOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TestResolverGrantConversationPermissionsOnRealFile walks the production
// loader path end to end: a resolver file on disk, tokens never stored in the
// file, and the operator credential resolving with the conversation and
// notification permissions the operator conversation handler requires.
func TestResolverGrantConversationPermissionsOnRealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolver.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	err = encoder.Encode(resolverFile{Credentials: []credentialRecord{{
		TokenSHA256: digestOf(fixtureOperatorToken),
		WorkspaceID: "b7fix-ws", PrincipalID: "op-b7fix",
		Permissions: []string{"conversation.read", "conversation.write", "notification.read"},
	}, {
		TokenSHA256: digestOf(fixtureRuntimeToken),
		WorkspaceID: "b7fix-ws", RuntimeID: "rt-b7fix", OwnerEpoch: 1,
		AccountIDs: []string{"acct-qq-1", "acct-qq-2"},
	}}})
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := loadResolver(path)
	if err != nil {
		t.Fatalf("loadResolver rejected the conversation permissions: %v", err)
	}
	runtimeAuth, err := loadRuntimeResolver(path)
	if err != nil {
		t.Fatalf("loadRuntimeResolver failed: %v", err)
	}
	combined := operatorConversationAuth(auth, runtimeAuth)
	request, _ := http.NewRequest(http.MethodGet, "/v1/operator/conversations", nil)
	request.Header.Set("Authorization", "Bearer "+fixtureOperatorToken)
	actor, err := combined(request)
	if err != nil {
		t.Fatalf("resolver lookup failed: %v", err)
	}
	if actor.WorkspaceID != "b7fix-ws" || actor.PrincipalID != "op-b7fix" ||
		!actor.Permissions["conversation.read"] || !actor.Permissions["conversation.write"] || !actor.Permissions["notification.read"] {
		t.Fatalf("operator identity mapping wrong: %+v", actor)
	}
	request, _ = http.NewRequest(http.MethodPost, "/v1/operator/qq-inbox/events", nil)
	request.Header.Set("Authorization", "Bearer "+fixtureRuntimeToken)
	actor, err = combined(request)
	if err != nil {
		t.Fatalf("runtime bridge lookup failed: %v", err)
	}
	if actor.RuntimeID != "rt-b7fix" || actor.PrincipalID != "rt-b7fix" || !actor.Permissions["chat.trigger"] {
		t.Fatalf("runtime bridge mapping wrong: %+v", actor)
	}
	if len(actor.AccountIDs) != 2 {
		t.Fatalf("runtime bridge lost the account scope: %+v", actor)
	}
	request, _ = http.NewRequest(http.MethodGet, "/v1/operator/conversations", nil)
	request.Header.Set("Authorization", "Bearer wrong-token")
	if _, err := combined(request); err == nil || err.(*taskengine.Error).Code != unauthenticatedCode {
		t.Fatalf("unknown token must stay unauthenticated, got %v", err)
	}
}

func TestOperatorConversationAuthNilRuntimeFallsBackToOperator(t *testing.T) {
	sentinel := taskengine.Actor{WorkspaceID: "w", PrincipalID: "p", Permissions: map[string]bool{"conversation.read": true}}
	auth := operatorConversationAuth(func(*http.Request) (taskengine.Actor, error) { return sentinel, nil }, nil)
	actor, err := auth(httptest.NewRequest(http.MethodGet, "/v1/operator/conversations", nil))
	if err != nil || actor.PrincipalID != "p" {
		t.Fatalf("nil runtime resolver must pass operator auth through: %+v %v", actor, err)
	}
}
