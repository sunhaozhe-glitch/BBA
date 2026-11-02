package taskengine

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Full TLS consumer over a controlled pipe tunnel: the public numeric IPv6
// target is observed on the wire, but the only socket connects to our TLS server.
func TestB5IPv6TLSConsumerControlledTunnel(t *testing.T) {
	const public = "2606:4700:4700::1111"
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			var hits, lookups, dials atomic.Int32
			srv, client := b5TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("proxy credential leaked")
				}
				b5CloseoutReply(w)
			}))
			targets := make(chan string, 1)
			n := modelNetwork{
				lookup: func(context.Context, string) ([]net.IPAddr, error) {
					if lookups.Add(1) != 1 {
						return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
					}
					return []net.IPAddr{{IP: net.ParseIP(public)}}, nil
				},
				dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					dials.Add(1)
					if address != "127.0.0.1:17890" {
						return nil, errProxyDial
					}
					a, b := net.Pipe()
					go func() {
						defer b.Close()
						b.SetDeadline(time.Now().Add(3 * time.Second))
						if scheme == "http" {
							req, e := http.ReadRequest(bufio.NewReader(b))
							if e != nil {
								targets <- "invalid"
								return
							}
							targets <- req.Host
							if _, e = io.WriteString(b, "HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil {
								return
							}
						} else {
							g := make([]byte, 3)
							if _, e := io.ReadFull(b, g); e != nil {
								return
							}
							b.Write([]byte{5, 0})
							wire := make([]byte, 22)
							if _, e := io.ReadFull(b, wire); e != nil {
								return
							}
							if wire[3] != 4 {
								targets <- "invalid"
								return
							}
							targets <- net.JoinHostPort(net.IP(wire[4:20]).String(), "443")
							if _, e := b.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); e != nil {
								return
							}
						}
						up, e := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", srv.Listener.Addr().String())
						if e != nil {
							return
						}
						defer up.Close()
						done := make(chan struct{})
						go func() { io.Copy(up, b); up.Close(); close(done) }()
						io.Copy(b, up)
						b.Close()
						<-done
					}()
					return a, nil
				},
			}
			p, e := NewOpenAICompatibleProvider(b5CloseoutConfig("https://model.bba.example/v1"), client)
			if e != nil {
				t.Fatal(e)
			}
			p, e = withProxyNetwork(p, proxyStored{Enabled: true, URL: scheme + "://127.0.0.1:17890"}, n)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = p.Complete(context.Background(), ModelCall{}); e != nil {
				t.Fatal(e)
			}
			if target := <-targets; target != "["+public+"]:443" {
				t.Fatal("IPv6 not pinned", target)
			}
			if hits.Load() != 1 || lookups.Load() != 1 || dials.Load() != 1 {
				t.Fatal("resolution repeated or TLS consumer missing")
			}
			t.Log("public IPv6 numeric tunnel + verified TLS provider=1; DNS once; only real socket is owned loopback; no public network")
		})
	}
}

func TestB5ProxyAuthRefusalAndHandshakeTimeoutNoProvider(t *testing.T) {
	for _, mode := range []string{"http407", "socks-auth-reject", "http-timeout", "explicit-cancel"} {
		t.Run(mode, func(t *testing.T) {
			var hits, direct, proxy atomic.Int32
			srv, client := b5TLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); b5CloseoutReply(w) }))
			_ = srv
			n := modelNetwork{lookup: func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
			}, dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr != "127.0.0.1:17890" {
					direct.Add(1)
					return nil, errProxyDial
				}
				proxy.Add(1)
				a, b := net.Pipe()
				go func() {
					defer b.Close()
					b.SetDeadline(time.Now().Add(time.Second))
					if mode == "socks-auth-reject" {
						g := make([]byte, 3)
						if _, e := io.ReadFull(b, g); e != nil {
							return
						}
						b.Write([]byte{5, 2})
						v := make([]byte, 2)
						if _, e := io.ReadFull(b, v); e != nil {
							return
						}
						io.CopyN(io.Discard, b, int64(v[1]))
						if _, e := io.ReadFull(b, v[:1]); e != nil {
							return
						}
						io.CopyN(io.Discard, b, int64(v[0]))
						b.Write([]byte{1, 1})
						return
					}
					if _, e := http.ReadRequest(bufio.NewReader(b)); e != nil {
						return
					}
					if mode == "http407" {
						io.WriteString(b, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
						return
					}
					io.Copy(io.Discard, b)
				}()
				return a, nil
			}}
			scheme := "http"
			if mode == "socks-auth-reject" {
				scheme = "socks5"
			}
			p, e := NewOpenAICompatibleProvider(b5CloseoutConfig("https://model.bba.example/v1"), client)
			if e != nil {
				t.Fatal(e)
			}
			p, e = withProxyNetwork(p, proxyStored{Enabled: true, URL: scheme + "://127.0.0.1:17890", Username: "fixture-auth-user", Password: "fixture-auth-password"}, n)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			if mode == "explicit-cancel" {
				time.AfterFunc(20*time.Millisecond, cancel)
			}
			_, e = p.Complete(ctx, ModelCall{})
			if e == nil {
				t.Fatal("proxy failure hidden")
			}
			for _, secret := range []string{"fixture-auth-user", "fixture-auth-password", "Proxy-Authorization", "127.0.0.1:17890"} {
				if strings.Contains(e.Error(), secret) {
					t.Fatal("error leaked proxy credential/address")
				}
			}
			if hits.Load() != 0 || direct.Load() != 0 || proxy.Load() != 1 {
				t.Fatal("failure reached provider or direct fallback")
			}
			t.Log("failure provider_delta=0 direct=0 proxy=1; stable redacted error")
		})
	}
}
