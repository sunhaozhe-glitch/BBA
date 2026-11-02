package taskengine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

type searchStored struct {
	Enabled bool        `json:"enabled"`
	Source  string      `json:"source"`
	Key     string      `json:"api_key"`
	Version int64       `json:"version"`
	Proxy   proxyStored `json:"proxy"`
}
type searchProxyWrite struct {
	Enabled  *bool   `json:"enabled"`
	URL      *string `json:"url"`
	Action   string  `json:"secret_action"`
	Username string  `json:"username"`
	Password string  `json:"password"`
}
type searchWrite struct {
	Enabled  *bool            `json:"enabled"`
	Source   string           `json:"source"`
	Expected *int64           `json:"expected_version"`
	Action   string           `json:"secret_action"`
	Key      string           `json:"api_key"`
	Proxy    searchProxyWrite `json:"proxy"`
}
type SearchSettings struct {
	Workspace    string        `json:"workspace_id"`
	Enabled      bool          `json:"enabled"`
	Source       string        `json:"source"`
	HasKey       bool          `json:"has_key"`
	Version      int64         `json:"version"`
	CanConfigure bool          `json:"can_configure"`
	Proxy        ProxySettings `json:"proxy"`
}
type SearchReceipt struct {
	Workspace     string         `json:"workspace_id"`
	RequestID     string         `json:"request_id"`
	ConfigVersion int64          `json:"config_version"`
	Source        string         `json:"source"`
	QueryDigest   string         `json:"query_digest"`
	Status        string         `json:"status"`
	ElapsedMS     int64          `json:"elapsed_ms"`
	Results       []SearchResult `json:"results"`
	At            time.Time      `json:"at"`
}
type searchAudit struct {
	Workspace string    `json:"workspace_id"`
	Principal string    `json:"principal_id"`
	Version   int64     `json:"version"`
	Action    string    `json:"secret_action"`
	At        time.Time `json:"at"`
}
type SearchQuery struct {
	Query string `json:"query"`
}

func validSearchQuery(q string) bool {
	return len(q) <= 512 && strings.TrimSpace(q) != "" && utf8.ValidString(q) && !searchHasControl(q)
}
func validSearch(p searchStored) bool {
	return p.Version >= 0 && (p.Source == "" && !p.Enabled || p.Source == "brave-free" || p.Source == "tavily") && len(p.Key) <= 4096 && !searchHasControl(p.Key) && validProxy(p.Proxy)
}
func searchPublic(ws string, p searchStored, can bool) SearchSettings {
	return SearchSettings{ws, p.Enabled, p.Source, p.Key != "", p.Version, can, proxyPublic(ws, p.Proxy, can)}
}
func (r *ModelRegistry) searchSnapshotLocked(ws string) (searchStored, error) {
	b, e := os.ReadFile(r.path)
	var d modelRegistryDisk
	if e != nil || len(b) > 4<<20 || json.Unmarshal(b, &d) != nil || d.Schema != 1 || d.Workspaces == nil || !validSearch(d.Search[ws]) {
		return searchStored{}, problem(taskCodeStoreUnavailable)
	}
	return d.Search[ws], nil
}
func (r *ModelRegistry) GetSearchSettings(ws string, can bool) (SearchSettings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, e := r.searchSnapshotLocked(ws)
	return searchPublic(ws, p, can), e
}
func (r *ModelRegistry) saveSearch(ws, principal string, w searchWrite) (SearchSettings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, e := r.searchSnapshotLocked(ws)
	if e != nil {
		return SearchSettings{}, e
	}
	if w.Expected == nil || *w.Expected < 0 || w.Enabled == nil || w.Proxy.Enabled == nil || w.Proxy.URL == nil {
		return SearchSettings{}, problem(taskCodeValidation)
	}
	if *w.Expected != old.Version {
		return SearchSettings{}, problem(taskCodeVersionConflict)
	}
	if w.Source != "brave-free" && w.Source != "tavily" {
		return SearchSettings{}, problem("SEARCH_SOURCE_UNSUPPORTED")
	}
	p := old
	p.Enabled = *w.Enabled
	p.Source = w.Source
	p.Version++
	switch w.Action {
	case "keep":
		if w.Key != "" || (old.Source != "" && old.Source != w.Source && old.Key != "") {
			return SearchSettings{}, problem(taskCodeValidation)
		}
	case "replace":
		if strings.TrimSpace(w.Key) == "" {
			return SearchSettings{}, problem(taskCodeValidation)
		}
		p.Key = w.Key
	case "clear":
		if w.Key != "" {
			return SearchSettings{}, problem(taskCodeValidation)
		}
		p.Key = ""
	default:
		return SearchSettings{}, problem(taskCodeValidation)
	}
	p.Proxy.Enabled = *w.Proxy.Enabled
	p.Proxy.URL = *w.Proxy.URL
	p.Proxy.Version = p.Version
	switch w.Proxy.Action {
	case "keep":
		if w.Proxy.Username != "" || w.Proxy.Password != "" {
			return SearchSettings{}, problem(taskCodeValidation)
		}
	case "replace":
		if w.Proxy.Username == "" || w.Proxy.Password == "" {
			return SearchSettings{}, problem(taskCodeValidation)
		}
		p.Proxy.Username = w.Proxy.Username
		p.Proxy.Password = w.Proxy.Password
	case "clear":
		if w.Proxy.Username != "" || w.Proxy.Password != "" {
			return SearchSettings{}, problem(taskCodeValidation)
		}
		p.Proxy.Username = ""
		p.Proxy.Password = ""
	default:
		return SearchSettings{}, problem(taskCodeValidation)
	}
	if !validSearch(p) {
		return SearchSettings{}, problem(taskCodeValidation)
	}
	d := r.cloneConfig()
	if d.Search == nil {
		d.Search = map[string]searchStored{}
	}
	d.Search[ws] = p
	d.SearchAudit = append(d.SearchAudit, searchAudit{ws, principal, p.Version, w.Action, time.Now().UTC()})
	if r.persist(d) != nil {
		return SearchSettings{}, problem(taskCodeStoreUnavailable)
	}
	return searchPublic(ws, p, true), nil
}

// Search admission and immutable snapshot share one registry lock. The caller
// already passed database principal/task authorization and local model budget.
func (r *ModelRegistry) startSearchLocked(ws string, agentAllowed bool) (searchStored, error) {
	p, e := r.searchSnapshotLocked(ws)
	if e != nil {
		return p, e
	}
	if !agentAllowed {
		return p, problem("SEARCH_AGENT_DISABLED")
	}
	if !p.Enabled {
		return p, problem("SEARCH_DISABLED")
	}
	if p.Source != "brave-free" && p.Source != "tavily" {
		return p, problem("SEARCH_SOURCE_UNSUPPORTED")
	}
	if strings.TrimSpace(p.Key) == "" {
		return p, problem("SEARCH_KEY_MISSING")
	}
	if r.searchActive[ws] >= 2 {
		return p, problem("SEARCH_BUDGET_EXCEEDED")
	}
	r.searchActive[ws]++
	return p, nil
}
func (r *ModelRegistry) consumeSearch(ctx context.Context, ws, id, query string, p searchStored) (SearchReceipt, error) {
	defer func() { r.mu.Lock(); r.searchActive[ws]--; r.mu.Unlock() }()
	start := time.Now()
	qd, _ := digest(query)
	out := SearchReceipt{Workspace: ws, RequestID: id, ConfigVersion: p.Version, Source: p.Source, QueryDigest: qd, Status: "started", Results: []SearchResult{}, At: start.UTC()}
	record := func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		d := r.cloneConfig()
		if d.SearchCalls == nil {
			d.SearchCalls = map[string]SearchReceipt{}
		}
		d.SearchCalls[configKey(ws, id)] = out
		return r.persist(d)
	}
	if record() != nil {
		return out, problem(taskCodeStoreUnavailable)
	}
	rows, e := searchProviderCall(ctx, p.Source, p.Key, query, p.Proxy, r.network)
	out.ElapsedMS = time.Since(start).Milliseconds()
	if e != nil {
		out.Status = Code(e)
	} else {
		out.Results = rows
		out.Status = "success"
		if len(rows) == 0 {
			out.Status = "no_results"
		}
	}
	if record() != nil {
		return out, problem(taskCodeStoreUnavailable)
	}
	return out, e
}
