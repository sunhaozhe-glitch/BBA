package taskengine

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

var errProxyDial = errors.New("model network connection failed")

// lookup and dial are per-transport, injectable ONLY from same-package tests.
// Resolve locally before proxy selection; a mixed public/private answer bypasses
// the proxy entirely. Dial the selected numeric address to prevent re-resolution
// (including proxy-side DNS rebinding). TLS still authenticates the original host.
type modelNetwork struct {
	lookup func(context.Context, string) ([]net.IPAddr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

func defaultModelNetwork() modelNetwork {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return modelNetwork{net.DefaultResolver.LookupIPAddr, d.DialContext}
}
func localModelIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() {
		return true
	}
	for _, p := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/32", "2001:db8::/32", "2002::/16"} {
		if netip.MustParsePrefix(p).Contains(a) {
			return true
		}
	}
	return false
}
func proxyTransport(p proxyStored, base http.RoundTripper) (http.RoundTripper, error) {
	return proxyTransportNetwork(p, base, defaultModelNetwork())
}
func proxyTransportNetwork(p proxyStored, base http.RoundTripper, n modelNetwork) (http.RoundTripper, error) {
	var t *http.Transport
	if base == nil {
		t = &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	} else if b, ok := base.(*modelTransport); ok {
		t = b.transport.Clone()
	} else if b, ok := base.(*http.Transport); ok {
		t = b.Clone()
	} else {
		if p.Enabled {
			return nil, errProxyDial
		}
		return base, nil
	}
	if t.TLSClientConfig != nil && t.TLSClientConfig.InsecureSkipVerify {
		return nil, errProxyDial
	}
	t.Proxy = nil
	t.DialTLSContext = nil
	t.DialTLS = nil
	t.DisableKeepAlives = true
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, errProxyDial
		}
		ips, e := n.lookup(ctx, host)
		if e != nil || len(ips) == 0 {
			return nil, errProxyDial
		}
		bypass := !p.Enabled || port == "6300" || port == "5432" || port == "6099" || port == "6199" || port == "6210" || port == "18421"
		for _, ip := range ips {
			if localModelIP(ip.IP) {
				bypass = true
			}
		}
		target := net.JoinHostPort(ips[0].IP.String(), port)
		if bypass {
			c, e := n.dial(ctx, "tcp", target)
			if e != nil {
				return nil, errProxyDial
			}
			return c, nil
		}
		u, e := url.Parse(p.URL)
		if e != nil {
			return nil, errProxyDial
		}
		c, e := n.dial(ctx, "tcp", u.Host)
		if e != nil {
			return nil, errProxyDial
		}
		deadline := time.Now().Add(10 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = c.SetDeadline(deadline)
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { _ = c.Close(); close(done) })
		if u.Scheme == "http" {
			e = connectHTTP(c, target, p)
		} else if u.Scheme == "socks5" {
			e = connectSOCKS(c, ips[0].IP, port, p)
		} else {
			e = errProxyDial
		}
		if !stop() {
			<-done
		}
		if e != nil || ctx.Err() != nil {
			c.Close()
			return nil, errProxyDial
		}
		_ = c.SetDeadline(time.Time{})
		return c, nil
	}
	return &modelTransport{t}, nil
}

// net/http detaches DialContext cancellation for connection reuse. Bind the
// tunnel handshake explicitly to this request, so cancellation closes it now.
type modelTransport struct{ transport *http.Transport }

func (m *modelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t := m.transport.Clone()
	dial := t.DialContext
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(req.Context(), cancel)
		defer func() { stop(); cancel() }()
		return dial(ctx, network, address)
	}
	defer t.CloseIdleConnections()
	return t.RoundTrip(req)
}
func connectHTTP(c net.Conn, target string, p proxyStored) error {
	req := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
	if p.Username != "" {
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.Username+":"+p.Password)))
	}
	if req.Write(c) != nil {
		return errProxyDial
	}
	reader := bufio.NewReader(io.LimitReader(c, 16384))
	resp, e := http.ReadResponse(reader, req)
	if e != nil || resp.StatusCode != 200 || reader.Buffered() != 0 {
		return errProxyDial
	}
	return nil
}
func connectSOCKS(c net.Conn, ip net.IP, port string, p proxyStored) error {
	method := byte(0)
	if p.Username != "" {
		method = 2
	}
	if _, e := c.Write([]byte{5, 1, method}); e != nil {
		return errProxyDial
	}
	b := make([]byte, 2)
	if _, e := io.ReadFull(c, b); e != nil || b[0] != 5 || b[1] != method {
		return errProxyDial
	}
	if method == 2 {
		auth := append([]byte{1, byte(len(p.Username))}, []byte(p.Username)...)
		auth = append(auth, byte(len(p.Password)))
		auth = append(auth, []byte(p.Password)...)
		if _, e := c.Write(auth); e != nil {
			return errProxyDial
		}
		if _, e := io.ReadFull(c, b); e != nil || b[0] != 1 || b[1] != 0 {
			return errProxyDial
		}
	}
	req := []byte{5, 1, 0}
	if v := ip.To4(); v != nil {
		req = append(req, 1)
		req = append(req, v...)
	} else {
		req = append(req, 4)
		req = append(req, ip.To16()...)
	}
	pn, e := strconv.Atoi(port)
	if e != nil {
		return errProxyDial
	}
	req = append(req, byte(pn>>8), byte(pn))
	if _, e = c.Write(req); e != nil {
		return errProxyDial
	}
	b = make([]byte, 4)
	if _, e = io.ReadFull(c, b); e != nil || b[0] != 5 || b[1] != 0 || b[2] != 0 {
		return errProxyDial
	}
	size := 0
	switch b[3] {
	case 1:
		size = 4
	case 4:
		size = 16
	case 3:
		b = make([]byte, 1)
		if _, e = io.ReadFull(c, b); e != nil {
			return errProxyDial
		}
		size = int(b[0])
	default:
		return errProxyDial
	}
	_, e = io.ReadFull(c, make([]byte, size+2))
	return e
}
