package wan

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func browserCallback(t *testing.T, redirect, query, method, host, origin string) (int, string, http.Header) {
	t.Helper()
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = net.JoinHostPort("127.0.0.1", u.Port())
	u.RawQuery = query
	r, err := http.NewRequest(method, u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = host
	r.Header.Set("Origin", origin)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body), response.Header
}

func TestWANMicrosoftBrowserCallbackRejectsUnboundAndDuplicateReplies(t *testing.T) {
	b, err := newMicrosoftBrowser("https://login.microsoftonline.com/common/oauth2/v2.0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	u, _ := url.Parse(b.redirect)
	for _, tc := range []struct{ query, method, host, origin string }{
		{"code=secret-code", "GET", u.Host, ""},
		{"code=secret-code&state=wrong", "GET", u.Host, ""},
		{"code=secret-code&state=" + b.state + "&state=" + b.state, "GET", u.Host, ""},
		{"code=secret-code&state=" + b.state, "POST", u.Host, ""},
		{"code=secret-code&state=" + b.state, "GET", "attacker.invalid", ""},
		{"code=secret-code&state=" + b.state, "GET", u.Host, "https://attacker.invalid"},
		{"code=secret-code&error=denied&state=" + b.state, "GET", u.Host, ""},
		{"code=secret-code&state=" + b.state + "&extra=1", "GET", u.Host, ""},
	} {
		status, body, _ := browserCallback(t, b.redirect, tc.query, tc.method, tc.host, tc.origin)
		if status != 400 || strings.Contains(body, "secret-code") {
			t.Fatal("unsafe callback response", status, body)
		}
		select {
		case <-b.reply:
			t.Fatal("invalid callback consumed the attempt")
		default:
		}
	}
	status, body, headers := browserCallback(t, b.redirect, "code=secret-code&state="+b.state, "GET", u.Host, "")
	if status != 200 || strings.Contains(body, "secret-code") || strings.Contains(body, b.state) || headers.Get("Cache-Control") != "no-store" || headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("callback exposed authorization material", status, headers)
	}
	if reply := <-b.reply; reply.code != "secret-code" {
		t.Fatal("lost authorization code")
	}
	status, _, _ = browserCallback(t, b.redirect, "code=other-secret&state="+b.state, "GET", u.Host, "")
	if status != 409 {
		t.Fatal("replayed callback was accepted", status)
	}
}

func browserTestAuth(t *testing.T, store credentialStore) (*auth, <-chan string) {
	t.Helper()
	a := newAuth(context.Background(), DefaultGitHubClientID, store)
	t.Cleanup(a.CancelJoin)
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State != "loading" })
	opened := make(chan string, 8)
	a.openBrowser = func(address string) error { opened <- address; return nil }
	return a, opened
}

func browserURL(t *testing.T, opened <-chan string) *url.URL {
	t.Helper()
	select {
	case address := <-opened:
		u, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		return u
	case <-time.After(10 * time.Second):
		t.Fatal("system browser did not open")
		return nil
	}
}

func browserGrantResponse() string {
	return `{"client_info":"` + fixtureMicrosoftInfo() + `","access_token":"opaque-browser-access-secret","refresh_token":"browser-refresh-secret","token_type":"Bearer","expires_in":3600}`
}

func TestWANMicrosoftBrowserDefaultUsesPKCEAndSecureRestore(t *testing.T) {
	store := &authTestStore{}
	a, opened := browserTestAuth(t, store)
	expected := make(chan url.Values, 1)
	a.microsoft.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		q := <-expected
		verifier := r.PostForm.Get("code_verifier")
		hash := sha256.Sum256([]byte(verifier))
		if r.URL.Path != "/common/oauth2/v2.0/token" || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("client_id") != microsoftClientID || r.PostForm.Get("redirect_uri") != q.Get("redirect_uri") || r.PostForm.Get("code") != "browser-code-secret" || len(verifier) != 43 || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) || r.PostForm.Get("scope") != microsoftScope {
			t.Error("incorrect browser authorization exchange")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(browserGrantResponse()))}, nil
	})
	initial, err := a.Start(true, "microsoft")
	if err != nil || initial.Method != "browser" {
		t.Fatal("Microsoft did not default to system browser", initial, err)
	}
	authorize := browserURL(t, opened)
	expected <- authorize.Query()
	q := authorize.Query()
	redirect, _ := url.Parse(q.Get("redirect_uri"))
	if authorize.Scheme != "https" || authorize.Host != "login.microsoftonline.com" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) != 43 || redirect.Hostname() != "localhost" || redirect.Path != "/" || redirect.Port() == "" {
		t.Fatal("did not match CLI browser flow")
	}
	status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "awaiting_browser" })
	data, _ := json.Marshal(status)
	for _, secret := range []string{q.Get("state"), q.Get("code_challenge"), authorize.String(), "browser-code-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("browser authorization details entered status")
		}
	}
	browserCallback(t, redirect.String(), url.Values{"state": {q.Get("state")}, "code": {"browser-code-secret"}}.Encode(), "GET", redirect.Host, "")
	got := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_in" })
	if got.Provider != "microsoft" || got.Storage != "system" || got.Renewal == nil {
		t.Fatal("browser grant lost renewal/storage", got)
	}
	record, saves, _ := store.snapshot()
	if saves != 1 || record.Refresh != "browser-refresh-secret" || !record.RefreshExpiresAt.IsZero() {
		t.Fatal("browser credentials not safely remembered")
	}
	a.CancelJoin()
	restored := newAuth(context.Background(), DefaultGitHubClientID, store)
	defer restored.CancelJoin()
	if got := awaitAuth(t, restored, func(s AuthStatus) bool { return s.State != "loading" }); got.State != "signed_in" || got.Provider != "microsoft" {
		t.Fatal("browser credential failed to restore", got)
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", redirect.Port()), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("callback listener survived shutdown")
	}
}

func TestWANMicrosoftBrowserCancelDiscardsLateExchange(t *testing.T) {
	for _, mode := range []string{"cancel", "logout", "replace"} {
		t.Run(mode, func(t *testing.T) {
			store := &authTestStore{}
			a, opened := browserTestAuth(t, store)
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			a.microsoft.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
				close(entered)
				<-release
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(browserGrantResponse()))}, nil
			})
			initial, err := a.Start(true, "microsoft")
			if err != nil {
				t.Fatal(err)
			}
			u := browserURL(t, opened)
			redirect, _ := url.Parse(u.Query().Get("redirect_uri"))
			browserCallback(t, redirect.String(), url.Values{"state": {u.Query().Get("state")}, "code": {"late-code-secret"}}.Encode(), "GET", redirect.Host, "")
			waitDone(t, entered)
			switch mode {
			case "cancel":
				if !a.Cancel(initial.AttemptID) {
					t.Fatal("could not cancel browser exchange")
				}
			case "logout":
				if browser := a.Logout(); browser != nil {
					browser.close()
				}
				if err := a.Erase(); err != nil {
					t.Fatal(err)
				}
			case "replace":
				replacement, err := a.Start(true, "microsoft")
				if err != nil {
					t.Fatal(err)
				}
				browserURL(t, opened)
				if !a.Cancel(replacement.AttemptID) {
					t.Fatal("could not cancel replacement")
				}
			}

			conn, dialErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", redirect.Port()), time.Second)
			if dialErr == nil {
				conn.Close()
				t.Error("canceled browser callback port remained open during a late exchange")
			}
			close(release)
			a.CancelJoin()
			_, saves, _ := store.snapshot()
			if saves != 0 || a.Status().State != "signed_out" {
				t.Fatal("late browser exchange restored canceled login")
			}

		})
	}
}

func TestWANMicrosoftBrowserOpenFailureCanRetryOnlySameAttempt(t *testing.T) {
	a, _ := browserTestAuth(t, memoryCredentialStore{})
	var calls atomic.Int32
	a.openBrowser = func(string) error {
		if calls.Add(1) == 1 {
			return errors.New("SECRET-launcher-url")
		}
		return nil
	}
	initial, err := a.Start(false, "microsoft")
	if err != nil {
		t.Fatal(err)
	}
	status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "awaiting_browser" && s.Error != "" })
	if strings.Contains(status.Error, "SECRET") {
		t.Fatal("launcher error leaked")
	}
	if a.OpenBrowser(initial.AttemptID+1) == nil || calls.Load() != 1 {
		t.Fatal("stale reopen launched a browser")
	}
	if err := a.OpenBrowser(initial.AttemptID); err != nil || calls.Load() != 2 || a.Status().Error != "" {
		t.Fatal("reopen did not recover", err)
	}
	if !a.Cancel(initial.AttemptID) {
		t.Fatal("cancel failed")
	}
	if a.OpenBrowser(initial.AttemptID) == nil || calls.Load() != 2 {
		t.Fatal("reopened canceled browser login")
	}
}

func TestWANMicrosoftBrowserTimeoutAndRefusalDoNotPersist(t *testing.T) {
	for _, mode := range []string{"timeout", "refused"} {
		t.Run(mode, func(t *testing.T) {
			store := &authTestStore{}
			a, opened := browserTestAuth(t, store)
			expire := make(chan context.CancelFunc, 1)
			a.deadline = func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(parent)
				expire <- cancel
				return ctx, cancel
			}
			if _, err := a.Start(true, "microsoft"); err != nil {
				t.Fatal(err)
			}
			u := browserURL(t, opened)
			if mode == "timeout" {
				(<-expire)()
			} else {
				redirect, _ := url.Parse(u.Query().Get("redirect_uri"))
				browserCallback(t, redirect.String(), url.Values{"state": {u.Query().Get("state")}, "error": {"access_denied"}, "error_description": {"SECRET-vendor-policy"}}.Encode(), "GET", redirect.Host, "")
			}
			status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "error" })
			if strings.Contains(status.Error, "SECRET") {
				t.Fatal("vendor policy details leaked")
			}
			a.CancelJoin()
			_, saves, _ := store.snapshot()
			if saves != 0 {
				t.Fatal("failed browser flow persisted a login")
			}
		})
	}
}
