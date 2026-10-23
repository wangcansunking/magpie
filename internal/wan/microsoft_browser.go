package wan

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

func defaultWANBrowser(address string) error {
	if os.Getenv(vaultSandbox) != "" {
		return errors.New("System browser is disabled in isolated tests")
	}
	return proc.OpenBrowser(address)
}

const microsoftBrowserLifetime = 15 * time.Minute

type browserReply struct{ code, failure string }

// Nothing in this attempt, including its state/verifier/code, enters status.
// The redirect matches the official CLI's localhost root, on a held random port.
type microsoftBrowser struct {
	address, redirect, verifier, state string
	reply                              chan browserReply
	server                             *http.Server
	done                               chan struct{}
	connections                        sync.WaitGroup
	mu                                 sync.Mutex
	claimed                            bool
}

func newMicrosoftBrowser(base string) (*microsoftBrowser, error) {
	var entropy [64]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, errors.New("Could not prepare Microsoft browser sign-in")
	}
	b := &microsoftBrowser{verifier: base64.RawURLEncoding.EncodeToString(entropy[:32]), state: base64.RawURLEncoding.EncodeToString(entropy[32:]), reply: make(chan browserReply, 1), done: make(chan struct{})}
	clear(entropy[:])
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("Could not open the Microsoft sign-in callback port")
	}
	b.redirect = "http://localhost:" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port) + "/"
	challenge := sha256.Sum256([]byte(b.verifier))
	b.address = base + "/authorize?" + url.Values{"client_id": {microsoftClientID}, "response_type": {"code"}, "redirect_uri": {b.redirect}, "response_mode": {"query"}, "scope": {microsoftScope}, "state": {b.state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "prompt": {"select_account"}}.Encode()
	b.server = &http.Server{Handler: http.HandlerFunc(b.callback), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192, ErrorLog: log.New(io.Discard, "", 0), ConnState: func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			b.connections.Add(1)
		}
		if state == http.StateClosed || state == http.StateHijacked {
			b.connections.Done()
		}
	}}
	go func() { defer close(b.done); _ = b.server.Serve(listener) }()
	return b, nil
}

func (b *microsoftBrowser) close() {
	_ = b.server.Close()
	<-b.done
	b.connections.Wait()
}

func (b *microsoftBrowser) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	u, _ := url.Parse(b.redirect)
	if r.Method != http.MethodGet || r.Host != u.Host || r.URL.Path != "/" || r.URL.RawPath != "" || r.Header.Get("Origin") != "" || len(r.URL.RawQuery) > 8192 {
		http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
		return
	}
	for key, values := range q {
		if len(values) != 1 || (key != "code" && key != "state" && key != "error" && key != "error_description" && key != "error_uri" && key != "session_state") {
			http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
			return
		}
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(b.state)) != 1 || (q.Get("code") == "") == (q.Get("error") == "") || (q.Get("code") != "" && !oauthText(q.Get("code"), 4096)) || (q.Get("error") != "" && !oauthText(q.Get("error"), 128)) {
		http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	if b.claimed {
		b.mu.Unlock()
		http.Error(w, "Sign-in callback already received", http.StatusConflict)
		return
	}
	b.claimed = true
	b.mu.Unlock()
	// Finish the fixed-length response before token exchange can close the server.
	// Never echo a code, state or vendor error description to the browser.
	message := "Microsoft authorization received. Return to magpie to check your sign-in. You may close this tab."
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(message)))
	_, _ = io.WriteString(w, message)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	b.reply <- browserReply{code: q.Get("code"), failure: q.Get("error")}
}

func (c microsoftDeviceClient) exchange(ctx context.Context, b *microsoftBrowser, code string) (githubGrant, error) {
	var grant githubGrant
	err := c.post(ctx, "/token", url.Values{"client_id": {microsoftClientID}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {b.redirect}, "code_verifier": {b.verifier}, "scope": {microsoftScope}}, &grant)
	if err != nil {
		return githubGrant{}, err
	}
	if grant.Error != "" {
		return githubGrant{}, errors.New("Microsoft browser sign-in was refused; check account and tenant policies, then sign in again")
	}
	if !validMicrosoftGrant(grant) {
		return githubGrant{}, errors.New("Microsoft returned an unsupported WAN credential")
	}
	return grant, nil
}

func (a *auth) openBrowserAttempt(r *authAttempt) error {
	a.commit.Lock()
	defer a.commit.Unlock()
	a.mu.Lock()
	if !a.activeLocked(r) || r.browser == nil || !a.now().Before(a.status.ExpiresAt) {
		a.mu.Unlock()
		return errors.New("Microsoft browser sign-in is no longer waiting")
	}
	b := r.browser
	b.mu.Lock()
	claimed := b.claimed
	b.mu.Unlock()
	a.mu.Unlock()
	if claimed {
		return errors.New("Microsoft browser sign-in is no longer waiting")
	}
	err := a.openBrowser(b.address)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeLocked(r) {
		a.status.Error = ""
		if err != nil {
			a.status.Error = "Could not open the system browser. Reopen it from the WAN page to continue."
		}
	}
	if err != nil {
		return errors.New("Could not open the system browser. Reopen it from the WAN page to continue.")
	}
	return nil
}

func (a *auth) OpenBrowser(id uint64) error {
	a.mu.Lock()
	r := a.current
	if r == nil || r.id != id || r.method != "browser" {
		a.mu.Unlock()
		return errors.New("Microsoft browser sign-in is no longer waiting")
	}
	a.mu.Unlock()
	return a.openBrowserAttempt(r)
}

func (a *auth) runMicrosoftBrowser(r *authAttempt) {
	ctx, cancel := a.deadline(r.ctx, microsoftBrowserLifetime)
	defer cancel()
	b, err := newMicrosoftBrowser(a.microsoft.base)
	if err != nil {
		a.requestFailed(r, err)
		return
	}
	defer func() {
		a.commit.Lock()
		a.mu.Lock()
		r.browser = nil
		a.mu.Unlock()
		b.close()
		a.commit.Unlock()
	}()
	a.mu.Lock()
	if !a.activeLocked(r) {
		a.mu.Unlock()
		return
	}
	r.browser = b
	a.status.State = "awaiting_browser"
	a.status.ExpiresAt = a.now().Add(microsoftBrowserLifetime)
	a.mu.Unlock()
	_ = a.openBrowserAttempt(r) // A launch failure remains visible and retryable.
	var reply browserReply
	select {
	case reply = <-b.reply:
	case <-ctx.Done():
		if r.ctx.Err() == nil {
			a.fail(r, "Microsoft browser sign-in timed out; start sign-in again", false)
		}
		return
	case <-b.done:
		a.fail(r, "Microsoft sign-in callback stopped; start sign-in again", false)
		return
	}
	if !a.active(r) || ctx.Err() != nil {
		return
	}
	if reply.failure != "" {
		a.fail(r, "Microsoft browser sign-in was refused; check account and tenant policies, then sign in again", false)
		return
	}
	issued := a.now()
	grant, err := a.microsoft.exchange(ctx, b, reply.code)
	if err != nil {
		a.requestFailed(r, err)
		return
	}
	if ctx.Err() != nil {
		return
	}
	a.accept(r, microsoftCredential(grant, issued))
}
