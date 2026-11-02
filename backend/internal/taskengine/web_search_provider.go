package taskengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SearchResult is untrusted metadata, never an instruction or a fetch target.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

const searchResponseLimit = 256 << 10

// Protocol evidence: ARTEX v0.3.24 go.mod pins norma v0.4.3;
// norma/tool/websearch.go:317-430 implements brave-free (GET, subscription
// header, web.results.description) and tavily (POST api_key/query/max_results/
// search_depth, results.content). We deliberately do not inherit its ddgs default,
// deepseek model calls, configurable endpoints, redirects or response limits.
// Current official Tavily docs use Authorization: Bearer, unlike norma v0.4.3
// body api_key; use the documented header and never duplicate the secret.
// https://docs.tavily.com/documentation/api-reference/endpoint/search
func searchProviderCall(ctx context.Context, source, key, query string, proxy proxyStored, network *modelNetwork) ([]SearchResult, error) {
	if source != "brave-free" && source != "tavily" {
		return nil, problem("SEARCH_SOURCE_UNSUPPORTED")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return nil, searchCallError(ctx, ctx.Err())
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(query) == "" || searchHasControl(key) {
		return nil, problem("SEARCH_PROVIDER_FAILURE")
	}
	if !validProxy(proxy) {
		return nil, problem("SEARCH_TRANSPORT_UNAVAILABLE")
	}
	n := defaultModelNetwork()
	if network != nil {
		n = *network
	}
	if n.lookup == nil || n.dial == nil {
		return nil, problem("SEARCH_TRANSPORT_UNAVAILABLE")
	}
	rt, err := proxyTransportNetwork(proxy, nil, n)
	if err != nil {
		return nil, problem("SEARCH_TRANSPORT_UNAVAILABLE")
	}
	var req *http.Request
	if source == "brave-free" {
		q := url.Values{"q": {query}, "count": {"5"}}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.search.brave.com/res/v1/web/search?"+q.Encode(), nil)
	} else {
		body, _ := json.Marshal(struct {
			Query      string `json:"query"`
			MaxResults int    `json:"max_results"`
			Depth      string `json:"search_depth"`
			Answer     bool   `json:"include_answer"`
			Raw        bool   `json:"include_raw_content"`
		}{query, 5, "basic", false, false})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(body))
	}
	if err != nil {
		return nil, problem("SEARCH_PROVIDER_FAILURE")
	}
	req.Header.Set("Accept", "application/json")
	if source == "brave-free" {
		req.Header.Set("X-Subscription-Token", key)
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// Fresh, non-reused transports prevent net/http's reused-connection retry.
	// No redirect (including same-origin), cookie jar, environment proxy or retry.
	client := &http.Client{Transport: rt, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, searchCallError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, problem("SEARCH_PROVIDER_RATE_LIMIT")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, problem("SEARCH_PROVIDER_FAILURE")
	}
	if resp.ContentLength > searchResponseLimit {
		return nil, problem("SEARCH_RESPONSE_TOO_LARGE")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, searchResponseLimit+1))
	if len(raw) > searchResponseLimit {
		return nil, problem("SEARCH_RESPONSE_TOO_LARGE")
	}
	if err != nil {
		return nil, searchCallError(ctx, err)
	}
	if !utf8.Valid(raw) {
		return nil, problem("SEARCH_RESPONSE_INVALID")
	}
	var payload struct {
		Web *struct {
			Results []searchWireResult `json:"results"`
		} `json:"web"`
		Results []searchWireResult `json:"results"`
	}
	if json.Unmarshal(raw, &payload) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, problem("SEARCH_RESPONSE_INVALID")
	}
	rows := payload.Results
	if source == "brave-free" {
		rows = nil
		if payload.Web != nil {
			rows = payload.Web.Results
		}
	}
	out := make([]SearchResult, 0, 5)
	for _, r := range rows {
		// Never truncate a URL: truncation changes its meaning. Discard invalid hits.
		if len(r.URL) > 2048 || !searchPublicURL(r.URL) {
			continue
		}
		snippet := r.Content
		if source == "brave-free" {
			snippet = r.Description
		}
		out = append(out, SearchResult{searchBoundText(r.Title, 512), r.URL, searchBoundText(snippet, 2048)})
		if len(out) == 5 {
			break
		}
	}
	return out, nil
}

type searchWireResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

func searchCallError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return problem("SEARCH_PROVIDER_CANCELLED")
	}
	var ne net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return problem("SEARCH_PROVIDER_TIMEOUT")
	}
	// Never wrap provider, URL, proxy or TLS errors: they can include credentials.
	return problem("SEARCH_PROVIDER_FAILURE")
}

func searchHasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}
func searchBoundText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// Metadata-only validation: do NOT resolve or fetch result hosts. DNS can change
// after validation, so even an accepted URL must never be used as fetch authority.
func searchPublicURL(raw string) bool {
	if raw == "" || searchHasControl(raw) || !utf8.ValidString(raw) {
		return false
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || searchHasControl(decoded) || strings.Contains(raw, "\\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Opaque != "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || strings.Contains(host, "%") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if u.Port() != "" {
		p, e := strconv.Atoi(u.Port())
		if e != nil || p < 1 || p > 65535 {
			return false
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		return !localModelIP(ip)
	}
	if strings.Contains(u.Host, ":") && !strings.Contains(u.Host, "]") && u.Port() == "" {
		return false
	}
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, suffix := range []string{"localhost", "local", "internal", "intranet", "lan", "home", "test", "invalid", "onion", "arpa"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	labels := strings.Split(host, ".")
	numeric := true
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
			if c < '0' || c > '9' {
				numeric = false
			}
		}
	}
	// Reject legacy numeric IPv4 spellings as well as single-label intranet names.
	if numeric {
		return false
	}
	tld := labels[len(labels)-1]
	for _, c := range tld {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return len(tld) >= 2
}
