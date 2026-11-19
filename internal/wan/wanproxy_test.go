package wan

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeWANProxy(t *testing.T) *wanProxy {
	t.Helper()
	p, err := startWANProxy(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func proxyClient(t *testing.T, p *wanProxy, authority, auth string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", p.listen.Addr().String(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", authority, authority, auth); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return conn, reader, response
}

// Every destination and upstream is a net.Pipe fixture, so even a mutation of
// proxy selection cannot reach DNS, a real proxy or any external service.
func TestWANProxySelectsActualDestinationAndCarriesBothWays(t *testing.T) {
	p := fakeWANProxy(t)
	var mu sync.Mutex
	var selected, dialed []string
	upstreamProxy, _ := url.Parse("http://proxyuser:proxypass@proxy.invalid:8080")
	p.choose = func(req *http.Request) (*url.URL, error) {
		mu.Lock()
		selected = append(selected, req.URL.String())
		mu.Unlock()
		if req.URL.Hostname() == "management.invalid" {
			return nil, nil // NO_PROXY/system bypass decision for this host only.
		}
		return upstreamProxy, nil
	}
	var peers sync.WaitGroup
	defer peers.Wait()
	p.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, address)
		mu.Unlock()
		local, remote := net.Pipe()
		peers.Add(1)
		go func() {
			defer peers.Done()
			defer remote.Close()
			remote.SetDeadline(time.Now().Add(10 * time.Second))
			if address == "proxy.invalid:8080" {
				req, err := http.ReadRequest(bufio.NewReader(remote))
				if err != nil {
					t.Error(err)
					return
				}
				if req.RequestURI != "relay.invalid:443" || req.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("proxyuser:proxypass")) {
					t.Error("upstream CONNECT target/auth was not separately synthesized")
					return
				}
				io.WriteString(remote, "HTTP/1.1 200 Connection Established\r\n\r\n")
			}
			io.Copy(remote, remote)
		}()
		return local, nil
	}
	for _, host := range []string{"management.invalid:443", "relay.invalid:443"} {
		conn, reader, response := proxyClient(t, p, host, p.auth)
		if response.StatusCode != 200 {
			t.Fatalf("CONNECT %s: %d", host, response.StatusCode)
		}
		if _, err := io.WriteString(conn, "SSE-data-through-tunnel"); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, len("SSE-data-through-tunnel"))
		if _, err := io.ReadFull(reader, data); err != nil || string(data) != "SSE-data-through-tunnel" {
			t.Fatalf("duplex transfer %q %v", data, err)
		}
		conn.Close()
	}
	p.Close()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(selected, ",") != "https://management.invalid:443,https://relay.invalid:443" || strings.Join(dialed, ",") != "management.invalid:443,proxy.invalid:8080" {
		t.Fatalf("per-host decisions selected=%v dialed=%v", selected, dialed)
	}
}

func TestWANProxyAuthenticationAndDestinationFailBeforeDial(t *testing.T) {
	p := fakeWANProxy(t)
	p.choose = func(*http.Request) (*url.URL, error) {
		t.Error("refused request reached proxy selection")
		return nil, nil
	}
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("refused request dialed")
		return nil, fmt.Errorf("SECRET")
	}
	for _, test := range []struct {
		target, auth string
		status       int
	}{
		{"relay.invalid:443", "", 407},
		{"relay.invalid:443", "Basic wrong-generation", 407},
		{"localhost:443", p.auth, 403}, {"localhost.evil:80", p.auth, 403},
		{"127.0.0.1:443", p.auth, 403}, {"[::1]:443", p.auth, 403},
		{"host.local:443", p.auth, 403}, {"relay.invalid:444", p.auth, 403},
		{"UPPER.invalid:443", p.auth, 403}, {"relay.invalid.:443", p.auth, 403},
	} {
		conn, _, response := proxyClient(t, p, test.target, test.auth)
		if response.StatusCode != test.status {
			t.Fatalf("%s auth=%v status=%d", test.target, test.auth != "", response.StatusCode)
		}
		conn.Close()
	}
}

func TestWANProxySOCKSUsesDestinationAndNeverFallsBack(t *testing.T) {
	p := fakeWANProxy(t)
	chosen, _ := url.Parse("socks5://user:SECRET@proxy.invalid:1080")
	p.choose = func(req *http.Request) (*url.URL, error) { return chosen, nil }
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("SOCKS fell back to direct")
		return nil, fmt.Errorf("SECRET")
	}
	p.socks = func(ctx context.Context, got *url.URL, target string) (net.Conn, error) {
		if got != chosen || target != "relay.invalid:443" {
			t.Error("SOCKS target changed")
		}
		return nil, fmt.Errorf("SOCKS SECRET failure")
	}
	conn, reader, response := proxyClient(t, p, "relay.invalid:443", p.auth)
	if response.StatusCode != 502 {
		t.Fatal(response.StatusCode)
	}
	data, _ := io.ReadAll(reader)
	if strings.Contains(string(data), "SECRET") {
		t.Fatal("proxy error leaked credentials")
	}
	conn.Close()
}

func TestWANProxyCloseJoinsAcceptedAndUpstreamConnections(t *testing.T) {
	p := fakeWANProxy(t)
	p.choose = func(*http.Request) (*url.URL, error) { return nil, nil }
	remoteDone := make(chan struct{})
	p.dial = func(context.Context, string, string) (net.Conn, error) {
		local, remote := net.Pipe()
		go func() { defer close(remoteDone); defer remote.Close(); io.Copy(io.Discard, remote) }()
		return local, nil
	}
	conn, reader, response := proxyClient(t, p, "relay.invalid:443", p.auth)
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	p.Close()
	waitDone(t, remoteDone)
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("proxy close retained client connection")
	}
	conn.Close()
	p.mu.Lock()
	count := len(p.conns)
	p.mu.Unlock()
	if count != 0 {
		t.Fatalf("%d unjoined connections", count)
	}
	second, err := net.Listen("tcp4", p.listen.Addr().String())
	if err != nil {
		t.Fatalf("proxy socket retained after close: %v", err)
	}
	second.Close()
}

func TestWANProxyHTTPSUpstreamValidatesCertificate(t *testing.T) {
	for _, trust := range []bool{false, true} {
		t.Run(fmt.Sprint(trust), func(t *testing.T) {
			p := fakeWANProxy(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.RequestURI != "relay.invalid:443" {
					t.Error("unexpected TLS proxy request")
				}
				w.WriteHeader(200)
			}))
			defer server.Close()
			upstream, _ := url.Parse(server.URL)
			p.choose = func(*http.Request) (*url.URL, error) { return upstream, nil }
			if trust {
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				p.tls = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			conn, _, response := proxyClient(t, p, "relay.invalid:443", p.auth)
			want := 502
			if trust {
				want = 200
			}
			if response.StatusCode != want {
				t.Fatalf("trusted=%v CONNECT=%d", trust, response.StatusCode)
			}
			conn.Close()
		})
	}
}
