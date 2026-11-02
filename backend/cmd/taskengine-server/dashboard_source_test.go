package main

import (
	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"context"
	"errors"
	"github.com/oapi-codegen/nullable"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type overviewRoundTrip func(*http.Request) (*http.Response, error)

func (f overviewRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDashboardOverviewMappingAndFailures(t *testing.T) {
	actor := taskengine.Actor{WorkspaceID: "ws", PrincipalID: "alice", Permissions: map[string]bool{"operator.read": true}}
	account := contract.Account{Id: "a", SelfId: nullable.NewNullableWithValue("12345")}
	cfg := overviewConfig{Bindings: []overviewBinding{{WorkspaceID: "ws", PrincipalID: "alice", AccountID: "a", InstanceID: "instance1"}}}
	for _, tc := range []struct {
		name, body, reason, status string
		httpStatus                 int
		fail                       bool
	}{
		{name: "online-filtered", body: `{"instances":[{"id":"instance1","uin":"12345","online":true,"token":"DO_NOT_LEAK"},{"id":"foreign","uin":"99999","online":true}]}`, status: "online"},
		{name: "offline", body: `{"instances":[{"id":"instance1","uin":"12345","online":false}]}`, status: "offline"},
		{name: "unknown", body: `{"instances":[{"id":"instance1","uin":"12345"}]}`, reason: "ONLINE_UNKNOWN", status: "abnormal"},
		{name: "identity-mismatch", body: `{"instances":[{"id":"instance1","uin":"99999","online":true}]}`, reason: "QQ_IDENTITY_UNVERIFIED_OR_MISMATCH", status: "abnormal"},
		{name: "duplicate", body: `{"instances":[{"id":"instance1"},{"id":"instance1"}]}`, reason: "DUPLICATE_INSTANCE_ID", status: "abnormal"},
		{name: "missing", body: `{"instances":[]}`, reason: "INSTANCE_MISSING", status: "abnormal"},
		{name: "invalid-online", body: `{"instances":[{"id":"instance1","uin":"12345","online":"yes"}]}`, reason: "INSTANCE_SCHEMA_UNKNOWN", status: "abnormal"},
		{name: "explicit-error", body: `{"instances":[{"id":"instance1","uin":"12345","online":true,"phase":"error"}]}`, reason: "OVERVIEW_EXPLICIT_ERROR", status: "abnormal"},
		{name: "schema", body: `{"new_format":[]}`, reason: "ACCOUNT_SOURCE_SCHEMA_UNKNOWN"},
		{name: "redirect", httpStatus: 302, reason: "ACCOUNT_SOURCE_HTTP_ERROR"},
		{name: "fault", fail: true, reason: "ACCOUNT_SOURCE_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := newOverviewClient()
			client.Transport = overviewRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "http://127.0.0.1:6300/api/overview" || r.Method != "GET" || len(r.Header) != 0 {
					t.Fatalf("unsafe request %v", r)
				}
				if tc.fail {
					return nil, errors.New("fixture network failure")
				}
				status := tc.httpStatus
				if status == 0 {
					status = 200
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"http://attacker.invalid"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			source := fixedOverviewSource(cfg, client)
			items, _, reason, _ := source(context.Background(), actor, []contract.Account{account})
			if tc.status != "" {
				if reason != "" || len(items) != 1 || items[0].Status != tc.status {
					t.Fatalf("items=%+v reason=%s", items, reason)
				}
				if tc.reason != "" && items[0].Reason != tc.reason {
					t.Fatalf("reason %s", items[0].Reason)
				}
			} else if reason != tc.reason {
				t.Fatalf("reason=%s want %s", reason, tc.reason)
			}
			if calls != 1 {
				t.Fatal(calls)
			}
			actorOther := actor
			actorOther.PrincipalID = "bob"
			items, _, reason, _ = source(context.Background(), actorOther, []contract.Account{account})
			if reason != "ACCOUNT_SCOPE_MAPPING_REQUIRED" || len(items) != 0 || calls != 1 {
				t.Fatal("unmapped actor exposed or contacted overview")
			}
		})
	}
}
func TestDashboardSourceRejectsURLsAndMappings(t *testing.T) {
	for _, s := range []string{"http://attacker", "http://127.0.0.1:6300", ""} {
		if _, e := loadDashboardSource(s, ""); e == nil {
			t.Fatal("accepted URL")
		}
	}
	if _, e := loadDashboardSource("local6300", ""); e == nil {
		t.Fatal("missing mappings")
	}
	c := newOverviewClient()
	if c.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("proxy enabled")
	}
	if c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect allowed")
	}
}
func TestProductSurfaceAuthAndStatic(t *testing.T) {
	auth := func(r *http.Request) (taskengine.Actor, error) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			return taskengine.Actor{}, errors.New("denied")
		}
		return taskengine.Actor{WorkspaceID: "ws", PrincipalID: "p", Permissions: map[string]bool{"operator.read": true}}, nil
	}
	h := productSurface(http.NotFoundHandler(), http.NotFoundHandler(), auth)
	for _, c := range []struct {
		path, method, token string
		status              int
	}{{"/product/dashboard", "GET", "", 200}, {"/product/app.js", "GET", "", 200}, {"/product/tools.json", "GET", "", 404}, {"/v1/operator/tools", "GET", "", 401}, {"/v1/operator/tools", "GET", "fixture", 200}, {"/v1/operator/tools", "POST", "fixture", 405}} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.token != "" {
			r.Header.Set("Authorization", "Bearer "+c.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.status {
			t.Fatalf("%s got %d", c.path, w.Code)
		}
	}
}
