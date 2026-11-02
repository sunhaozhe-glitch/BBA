package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Stored only in the existing protected registry (private parent, 0600 file,
// lifetime flock, fsync+rename). Public reads and audit never contain credentials.
type proxyStored struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Version  int64  `json:"version"`
}
type proxyAudit struct {
	Workspace    string    `json:"workspace_id"`
	Principal    string    `json:"principal_id"`
	Version      int64     `json:"version"`
	Enabled      bool      `json:"enabled"`
	SecretAction string    `json:"secret_action"`
	At           time.Time `json:"at"`
}
type ProxySettings struct {
	Workspace      string `json:"workspace_id"`
	Enabled        bool   `json:"enabled"`
	URL            string `json:"url"`
	HasCredentials bool   `json:"has_credentials"`
	Version        int64  `json:"version"`
	CanConfigure   bool   `json:"can_configure"`
}
type proxyWrite struct {
	Enabled  *bool   `json:"enabled"`
	URL      *string `json:"url"`
	Expected *int64  `json:"expected_version"`
	Action   string  `json:"secret_action"`
	Username string  `json:"username"`
	Password string  `json:"password"`
}

func proxyPublic(ws string, p proxyStored, can bool) ProxySettings {
	return ProxySettings{ws, p.Enabled, p.URL, p.Username != "" || p.Password != "", p.Version, can}
}
func validProxy(p proxyStored) bool {
	if p.Version < 0 || len(p.URL) > 1000 || len(p.Username) > 255 || len(p.Password) > 255 || strings.ContainsAny(p.Username+p.Password, "\r\n\x00") {
		return false
	}
	if p.URL == "" {
		return !p.Enabled && p.Username == "" && p.Password == ""
	}
	u, e := url.Parse(p.URL)
	return e == nil && (u.Scheme == "http" || u.Scheme == "socks5") && u.Hostname() != "" && u.Port() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.Contains(u.Hostname(), "%") && validProxyPort(u.Port())
}
func validProxyPort(port string) bool {
	_, e := net.LookupPort("tcp", port)
	if e != nil {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	return port != "0"
}

// Absence is direct. Any protected-file read/decode failure fails closed, including
// configurations for an empty workspace. Never consult process proxy variables.
func (r *ModelRegistry) proxySnapshotLocked(ws string) (proxyStored, error) {
	b, e := os.ReadFile(r.path)
	if e != nil {
		return proxyStored{}, problem(taskCodeStoreUnavailable)
	}
	var d modelRegistryDisk
	if len(b) > 4<<20 || json.Unmarshal(b, &d) != nil || d.Schema != 1 || d.Workspaces == nil {
		return proxyStored{}, problem(taskCodeStoreUnavailable)
	}
	p := d.Proxies[ws]
	if !validProxy(p) {
		return proxyStored{}, problem(taskCodeStoreUnavailable)
	}
	return p, nil
}
func (r *ModelRegistry) proxySnapshot(ws string) (proxyStored, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.proxySnapshotLocked(ws)
}
func (r *ModelRegistry) GetProxySettings(ws string, can bool) (ProxySettings, error) {
	p, e := r.proxySnapshot(ws)
	return proxyPublic(ws, p, can), e
}
func (r *ModelRegistry) saveProxy(ws, principal string, w proxyWrite) (ProxySettings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.Enabled == nil || w.URL == nil || w.Expected == nil || *w.Expected < 0 {
		return ProxySettings{}, problem(taskCodeValidation)
	}
	old, e := r.proxySnapshotLocked(ws)
	if e != nil {
		return ProxySettings{}, e
	}
	if old.Version != *w.Expected {
		return ProxySettings{}, problem(taskCodeVersionConflict)
	}
	p := old
	p.Enabled = *w.Enabled
	p.URL = *w.URL
	p.Version++
	switch w.Action {
	case "keep":
		if w.Username != "" || w.Password != "" {
			return ProxySettings{}, problem(taskCodeValidation)
		}
	case "replace":
		if w.Username == "" || w.Password == "" {
			return ProxySettings{}, problem(taskCodeValidation)
		}
		p.Username = w.Username
		p.Password = w.Password
	case "clear":
		if w.Username != "" || w.Password != "" {
			return ProxySettings{}, problem(taskCodeValidation)
		}
		p.Username = ""
		p.Password = ""
	default:
		return ProxySettings{}, problem(taskCodeValidation)
	}
	if !validProxy(p) {
		return ProxySettings{}, problem(taskCodeValidation)
	}
	d := r.data
	d.Proxies = make(map[string]proxyStored, len(r.data.Proxies)+1)
	for k, v := range r.data.Proxies {
		d.Proxies[k] = v
	}
	d.Proxies[ws] = p
	d.ProxyAudit = append(append([]proxyAudit(nil), r.data.ProxyAudit...), proxyAudit{ws, principal, p.Version, p.Enabled, w.Action, time.Now().UTC()})
	if r.persist(d) != nil {
		return ProxySettings{}, problem(taskCodeStoreUnavailable)
	}
	r.data = d
	return proxyPublic(ws, p, true), nil
}

// Per-call immutable snapshot. New transport per invocation: no shared pools,
// retries, redirect following, environment proxy lookup or DefaultTransport edits.
func (r *ModelRegistry) adapterFor(ctx context.Context, ws string, p modelStoredProfile) (ModelProvider, error) {
	snap, e := r.proxySnapshot(ws)
	if e != nil {
		return nil, e
	}
	provider, e := r.adapter(p)
	if e != nil {
		return nil, e
	}
	return r.proxyProvider(provider, snap)
}
func (r *ModelRegistry) proxyProvider(provider ModelProvider, snap proxyStored) (ModelProvider, error) {
	n := defaultModelNetwork()
	if r.network != nil {
		n = *r.network
	}
	return withProxyNetwork(provider, snap, n)
}
func withProxySnapshot(provider ModelProvider, snap proxyStored) (ModelProvider, error) {
	return withProxyNetwork(provider, snap, defaultModelNetwork())
}
func withProxyNetwork(provider ModelProvider, snap proxyStored, n modelNetwork) (ModelProvider, error) {
	p, ok := provider.(*openAICompatibleProvider)
	if !ok {
		if snap.Enabled {
			return nil, errors.New("configured provider cannot consume proxy")
		}
		return provider, nil
	}
	q := *p
	client := *p.client
	t, e := proxyTransportNetwork(snap, p.client.Transport, n)
	if e != nil {
		return nil, e
	}
	client.Transport = t
	q.client = &client
	return &q, nil
}
