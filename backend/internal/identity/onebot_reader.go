package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxLoginInfoResponseBytes = 64 << 10

type HTTPLoginInfoReader struct {
	runtimeID string
	endpoint  string
	token     string
	client    *http.Client
}

// NewHTTPLoginInfoReader accepts one explicit loopback OneBot runtime and fixes
// the only action to the read-only get_login_info endpoint.
func NewHTTPLoginInfoReader(runtimeID, endpoint, token string) (*HTTPLoginInfoReader, error) {
	if strings.TrimSpace(runtimeID) == "" || strings.TrimSpace(token) == "" {
		return nil, errors.New("runtime id and access token are required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("OneBot endpoint must be an HTTP(S) loopback origin without path, userinfo, query, or fragment")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("OneBot endpoint must use a loopback IP or localhost")
	}
	transport := &http.Transport{
		Proxy:       nil,
		DialContext: loopbackDialContext,
	}
	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &HTTPLoginInfoReader{runtimeID: runtimeID, endpoint: strings.TrimRight(endpoint, "/"), token: token, client: client}, nil
}

func loopbackDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid loopback endpoint")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("loopback endpoint did not resolve")
	}
	for _, resolved := range ips {
		if !resolved.IP.IsLoopback() {
			return nil, errors.New("loopback endpoint resolved outside loopback")
		}
	}
	dialer := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, resolved := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, errors.New("loopback connection failed: " + lastErr.Error())
}

func (r *HTTPLoginInfoReader) ReadLoginInfo(ctx context.Context, binding Binding) (LoginInfo, error) {
	if binding.RuntimeID != r.runtimeID {
		return LoginInfo{}, &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("reader is bound to another runtime")}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint+"/get_login_info", bytes.NewReader([]byte("{}")))
	if err != nil {
		return LoginInfo{}, &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("cannot construct self-info request")}
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return LoginInfo{}, &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("self-info request failed")}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return LoginInfo{}, &RegistryError{Code: ErrRuntimeUnavailable, Err: errors.New("self-info endpoint returned a non-200 status")}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxLoginInfoResponseBytes+1))
	if err != nil || len(body) > maxLoginInfoResponseBytes {
		return LoginInfo{}, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("self-info response exceeds size limit or cannot be read")}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var envelope struct {
		Status  string                     `json:"status"`
		RetCode json.Number                `json:"retcode"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	if err := decoder.Decode(&envelope); err != nil {
		return LoginInfo{}, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("invalid self-info response")}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return LoginInfo{}, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("self-info response contains trailing content")}
	}
	if envelope.Status != "ok" || envelope.RetCode.String() != "0" {
		return LoginInfo{}, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("self-info action was not successful")}
	}
	encodedID, ok := envelope.Data["user_id"]
	if !ok {
		return LoginInfo{}, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("self-info response has no user_id")}
	}
	var userID any
	idDecoder := json.NewDecoder(bytes.NewReader(encodedID))
	idDecoder.UseNumber()
	if err := idDecoder.Decode(&userID); err != nil {
		return LoginInfo{}, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("self-info user_id is invalid")}
	}
	var selfID string
	switch value := userID.(type) {
	case json.Number:
		selfID = value.String()
	case string:
		selfID = value
	default:
		return LoginInfo{}, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("self-info user_id must be a decimal string or integer")}
	}
	if !validQQID(selfID) {
		return LoginInfo{}, &RegistryError{Code: ErrIdentityUnverified, Err: errors.New("self-info user_id is not a QQ identifier")}
	}
	return LoginInfo{SelfID: selfID, Source: SourceGetLoginInfo}, nil
}
