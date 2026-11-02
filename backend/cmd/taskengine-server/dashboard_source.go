package main

import (
	"bba.local/taskengine/internal/contract"
	"bba.local/taskengine/internal/taskengine"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// There is no configurable URL or credential field. Bindings are trusted
// server-side configuration, never supplied by the requesting browser.
type overviewBinding struct {
	WorkspaceID string `json:"workspace_id"`
	PrincipalID string `json:"principal_id"`
	AccountID   string `json:"account_id"`
	InstanceID  string `json:"instance_id"`
}
type overviewConfig struct {
	Bindings []overviewBinding `json:"bindings"`
}

func loadDashboardSource(mode, path string) (taskengine.DashboardAccountSource, error) {
	switch mode {
	case "projection":
		if path != "" {
			return nil, errors.New("mapping file only valid with local6300")
		}
		return taskengine.ProjectionAccounts, nil
	case "local6300":
		var cfg overviewConfig
		if path == "" {
			return nil, errors.New("local6300 requires protected explicit identity mapping")
		}
		if err := readProtectedJSON(path, &cfg); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, b := range cfg.Bindings {
			key := b.WorkspaceID + "\x00" + b.PrincipalID + "\x00" + b.InstanceID
			if !validCredentialID(b.WorkspaceID) || !validCredentialID(b.PrincipalID) || !validCredentialID(b.AccountID) || !validCredentialID(b.InstanceID) || seen[key] {
				return nil, errors.New("invalid or duplicate dashboard instance binding")
			}
			seen[key] = true
		}
		return fixedOverviewSource(cfg, newOverviewClient()), nil
	default:
		return nil, errors.New("dashboard account source must be projection or local6300")
	}
}
func newOverviewClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, DisableKeepAlives: true, ResponseHeaderTimeout: time.Second}}
}
func fixedOverviewSource(cfg overviewConfig, client *http.Client) taskengine.DashboardAccountSource {
	return func(ctx context.Context, a taskengine.Actor, accounts []contract.Account) ([]taskengine.DashboardAccount, string, string, error) {
		allowed := map[string]contract.Account{}
		for _, v := range accounts {
			allowed[v.Id] = v
		}
		bindings := []overviewBinding{}
		for _, b := range cfg.Bindings {
			if b.WorkspaceID == a.WorkspaceID && b.PrincipalID == a.PrincipalID {
				if _, ok := allowed[b.AccountID]; ok {
					bindings = append(bindings, b)
				}
			}
		}
		if len(bindings) == 0 {
			return nil, "local6300", "ACCOUNT_SCOPE_MAPPING_REQUIRED", nil
		}
		req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:6300/api/overview", nil)
		if err != nil {
			return nil, "local6300", "ACCOUNT_SOURCE_UNAVAILABLE", err
		}
		// No inbound headers, Authorization, Cookie or proxy state are forwarded.
		response, err := client.Do(req)
		if err != nil {
			return nil, "local6300", "ACCOUNT_SOURCE_UNAVAILABLE", err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return nil, "local6300", "ACCOUNT_SOURCE_HTTP_ERROR", errors.New("overview unavailable")
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
		if err != nil || len(raw) > 1<<20 {
			return nil, "local6300", "ACCOUNT_SOURCE_INVALID", errors.New("overview exceeds limit")
		}
		// Only the known overview envelope and explicitly whitelisted typed fields
		// are projected. Unknown payload fields can never be returned to a browser.
		var payload struct {
			Instances []json.RawMessage `json:"instances"`
		}
		if json.Unmarshal(raw, &payload) != nil || payload.Instances == nil {
			return nil, "local6300", "ACCOUNT_SOURCE_SCHEMA_UNKNOWN", nil
		}
		instances := map[string][]json.RawMessage{}
		for _, item := range payload.Instances {
			var id struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(item, &id) == nil && id.ID != "" {
				instances[id.ID] = append(instances[id.ID], item)
			}
		}
		now := time.Now().UTC()
		out := []taskengine.DashboardAccount{}
		for _, b := range bindings {
			v := taskengine.DashboardAccount{ID: b.AccountID, InstanceID: b.InstanceID, Status: "abnormal", Reason: "INSTANCE_MISSING", ObservedAt: &now}
			rows := instances[b.InstanceID]
			if len(rows) > 1 {
				v.Reason = "DUPLICATE_INSTANCE_ID"
			} else if len(rows) == 1 {
				var x struct {
					UIN    string `json:"uin"`
					Online *bool  `json:"online"`
					Phase  string `json:"phase"`
					Flag   string `json:"flag"`
				}
				if json.Unmarshal(rows[0], &x) != nil {
					v.Reason = "INSTANCE_SCHEMA_UNKNOWN"
				} else {
					account := allowed[b.AccountID]
					expected := ""
					if account.SelfId.IsSpecified() && !account.SelfId.IsNull() {
						expected = account.SelfId.MustGet()
					}
					if expected == "" || x.UIN != expected {
						v.Reason = "QQ_IDENTITY_UNVERIFIED_OR_MISMATCH"
					} else {
						v.SelfID = x.UIN
						if x.Online == nil {
							v.Reason = "ONLINE_UNKNOWN"
						} else if *x.Online {
							v.Status = "online"
							v.Reason = "OVERVIEW_ONLINE"
						} else {
							v.Status = "offline"
							v.Reason = "OVERVIEW_OFFLINE"
						}
						if x.Phase == "error" || x.Phase == "failed" || x.Flag == "error" {
							v.Status = "abnormal"
							v.Reason = "OVERVIEW_EXPLICIT_ERROR"
						}
					}
				}
			}
			out = append(out, v)
		}
		return out, "local6300", "", nil
	}
}
