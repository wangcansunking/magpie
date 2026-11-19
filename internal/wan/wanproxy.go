package wan

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
)

// wanProxy is private to one hosting generation. The helper always uses it, so
// each management/relay destination receives netproxy's own bypass decision.
// Its unguessable capability goes through the same private pipe as the token.
// It handles CONNECT only; no credential or untrusted upstream error is logged.
type wanProxy struct {
	ctx     context.Context
	cancel  context.CancelFunc
	listen  net.Listener
	url     string
	auth    string
	done    chan struct{}
	mu      sync.Mutex
	closing bool
	conns   map[net.Conn]struct{}
	workers sync.WaitGroup
	revoke  func()
	choose  func(*http.Request) (*url.URL, error)
	dial    func(context.Context, string, string) (net.Conn, error)
	socks   func(context.Context, *url.URL, string) (net.Conn, error)
	tls     *tls.Config
}

func startWANProxy(owner context.Context, revoke func()) (*wanProxy, error) {
	if owner.Err() != nil {
		return nil, errors.New("WAN proxy owner is not running")
	}
	var capability [32]byte
	if _, err := rand.Read(capability[:]); err != nil {
		return nil, errors.New("Could not generate private WAN proxy access")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("Could not bind the private WAN proxy")
	}
	secret := hex.EncodeToString(capability[:])
	ctx, cancel := context.WithCancel(owner)
	p := &wanProxy{
		ctx: ctx, cancel: cancel, listen: ln, done: make(chan struct{}), conns: make(map[net.Conn]struct{}), revoke: revoke,
		url:    "http://wan:" + secret + "@" + ln.Addr().String(),
		auth:   "Basic " + base64.StdEncoding.EncodeToString([]byte("wan:"+secret)),
		choose: netproxy.Func, dial: (&net.Dialer{Timeout: 15 * time.Second}).DialContext, socks: netproxy.DialSOCKS,
	}
	go p.serve()
	return p, nil
}

func (p *wanProxy) serve() {
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		<-p.ctx.Done()
		if p.revoke != nil {
			p.revoke()
		}
		p.mu.Lock()
		p.closing = true
		_ = p.listen.Close()
		for conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
	}()
	for {
		conn, err := p.listen.Accept()
		if err != nil {
			break
		}
		if !p.track(conn) {
			break
		}
		p.workers.Add(1)
		go func() {
			defer p.workers.Done()
			defer p.forget(conn)
			p.connect(conn)
		}()
	}
	p.cancel()
	<-watchDone
	p.workers.Wait()
	close(p.done)
}

func (p *wanProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing || p.ctx.Err() != nil {
		_ = conn.Close()
		return false
	}
	p.conns[conn] = struct{}{}
	return true
}

func (p *wanProxy) forget(conn net.Conn) {
	_ = conn.Close()
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
}

// Close cancels in-progress proxy choices/dials and joins all tunnel copies,
// including hijacked connections. The dedicated WAN socket is released later.
func (p *wanProxy) Close() {
	p.cancel()
	<-p.done
}

func (p *wanProxy) connect(local net.Conn) {
	_ = local.SetDeadline(time.Now().Add(20 * time.Second))
	reader := bufio.NewReader(local)
	header, err := proxyHeader(reader)
	if err != nil {
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header)))
	if err != nil {
		return
	}
	defer req.Body.Close()
	if len(req.Header.Values("Proxy-Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(req.Header.Get("Proxy-Authorization")), []byte(p.auth)) != 1 {
		_, _ = io.WriteString(local, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"wan\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	if req.Method != http.MethodConnect || req.Host != req.RequestURI || req.ContentLength > 0 || len(req.TransferEncoding) != 0 || req.Header.Get("Expect") != "" || !validProxyTarget(req.RequestURI) {
		_, _ = io.WriteString(local, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
	target := &url.URL{Scheme: "https", Host: req.RequestURI}
	proxy, err := p.choose((&http.Request{URL: target}).WithContext(ctx))
	if err != nil {
		p.refuse(local)
		return
	}
	upstream, err := p.connectTarget(ctx, req.RequestURI, proxy)
	if err != nil {
		p.refuse(local)
		return
	}
	if !p.track(upstream) {
		return
	}
	defer p.forget(upstream)
	if p.ctx.Err() != nil {
		return
	}
	_ = upstream.SetDeadline(time.Time{})
	if _, err := io.WriteString(local, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = local.SetDeadline(time.Time{})
	// Both copies end before this worker returns; cancellation closes both
	// connections. There is no stream lifetime/write deadline after CONNECT.
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, reader); _ = upstream.Close(); close(copied) }()
	_, _ = io.Copy(local, upstream)
	_ = local.Close()
	_ = upstream.Close()
	<-copied
}

func (p *wanProxy) refuse(conn net.Conn) {
	_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

// Only HTTPS DNS destinations are accepted, never an arbitrary local TCP
// forward. The bundled helper additionally restricts service/redirect URLs.
func validProxyTarget(authority string) bool {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || port != "443" || net.ParseIP(host) != nil || strings.ToLower(host) != host || len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	if !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// proxyHeader bounds bytes before HTTP parsing. The original buffered reader
// is retained for any TLS bytes sent immediately after CONNECT.
func proxyHeader(reader *bufio.Reader) ([]byte, error) {
	var header []byte
	for {
		line, err := reader.ReadSlice('\n')
		if len(header)+len(line) > 16<<10 {
			return nil, errors.New("proxy header too large")
		}
		header = append(header, line...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		if bytes.HasSuffix(header, []byte("\r\n\r\n")) {
			return header, nil
		}
	}
}

func (p *wanProxy) connectTarget(ctx context.Context, target string, proxy *url.URL) (net.Conn, error) {
	if proxy == nil {
		return p.dial(ctx, "tcp", target)
	}
	if netproxy.IsSOCKS(proxy) {
		return p.socks(ctx, proxy, target)
	}
	if proxy.Scheme != "http" && proxy.Scheme != "https" || proxy.Hostname() == "" || proxy.RawQuery != "" || proxy.Fragment != "" || proxy.Path != "" && proxy.Path != "/" {
		return nil, errors.New("unsupported proxy")
	}
	port := proxy.Port()
	if port == "" {
		port = "80"
		if proxy.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := p.dial(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	if err != nil {
		return nil, err
	}
	if !p.track(conn) {
		return nil, errors.New("proxy closed")
	}
	owned := conn
	ok := false
	defer func() {
		p.mu.Lock()
		delete(p.conns, owned)
		p.mu.Unlock()
		if !ok {
			_ = owned.Close()
		}
	}()
	if deadline, yes := ctx.Deadline(); yes {
		_ = conn.SetDeadline(deadline)
	}
	if proxy.Scheme == "https" {
		config := &tls.Config{MinVersion: tls.VersionTLS12}
		if p.tls != nil {
			config = p.tls.Clone()
		}
		config.ServerName = proxy.Hostname()
		secure := tls.Client(conn, config)
		if err := secure.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = secure
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		request += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)) + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	header, err := proxyHeader(reader)
	if err != nil {
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header)), &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, errors.New("upstream proxy refused CONNECT")
	}
	ok = true
	return &proxyConn{Conn: conn, reader: reader}, nil
}

type proxyConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *proxyConn) Read(data []byte) (int, error) { return c.reader.Read(data) }
