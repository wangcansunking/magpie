package wan

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func fixtureMicrosoftToken() string {
	b, _ := json.Marshal(map[string]string{"aud": microsoftAudience, "tid": "11111111-1111-1111-1111-111111111111", "oid": "22222222-2222-2222-2222-222222222222", "preferred_username": "fixture@example.invalid"})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".fixture"
}

func TestWANUsageUsesServiceNumbersAndKeepsUnknownUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
		used       uint64
		unlimited  bool
	}{
		{"exhausted", `[{"name":"BandwidthPerUser","current":5371017692,"limit":5368709120,"resetTime":1795243853}]`, true, 5371017692, false},
		{"zero", `[{"name":"BandwidthPerUser","current":0,"limit":0}]`, true, 0, false},
		{"unlimited", `[{"name":"BandwidthPerUser","current":12,"limit":null}]`, true, 12, true},
		{"missing-current", `[{"name":"BandwidthPerUser","limit":5368709120}]`, false, 0, false},
		{"missing-allowance", `[]`, false, 0, false},
		{"negative", `[{"name":"BandwidthPerUser","current":-1,"limit":5}]`, false, 0, false},
		{"duplicate", `[{"name":"BandwidthPerUser","current":1},{"name":"BandwidthPerUser","current":2}]`, false, 0, false},
		{"bad-reset", `[{"name":"BandwidthPerUser","current":1,"resetTime":1}]`, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newServiceClient()
			calls := 0
			c.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				body := tc.body
				if r.URL.Host == "api.github.com" {
					body = `{"id":7,"login":"fixture"}`
					if r.Header.Get("Authorization") != "Bearer fake-secret" {
						t.Error("incorrect profile authorization")
					}
				} else if r.URL.Host != "global.rel.tunnels.api.visualstudio.com" || r.URL.Path != "/userlimits" || r.Header.Get("Authorization") != "github fake-secret" {
					t.Error("incorrect service request")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})
			got, err := c.usage(context.Background(), "fake-secret")
			if (err == nil) != tc.ok {
				t.Fatalf("usage validity %v: %+v %v", tc.ok, got, err)
			}
			if tc.ok && (got.Used != tc.used || (got.Limit == nil) != tc.unlimited || got.Account.ID != "github:7" || got.CheckedAt.IsZero()) {
				t.Fatalf("incorrect usage: %+v", got)
			}
			if calls != 2 {
				t.Fatal("unexpected request count", calls)
			}
		})
	}
}

func TestWANUsageMicrosoftIdentityAndNoRedirectOrSecretErrors(t *testing.T) {
	c := newServiceClient()
	token := fixtureMicrosoftToken()
	calls := 0
	c.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "global.rel.tunnels.api.visualstudio.com" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("Microsoft identity borrowed GitHub credentials")
		}
		return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("SECRET")), Header: http.Header{"Location": {"https://attacker.invalid/"}}}, nil
	})
	if _, err := c.usage(context.Background(), token); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("unsafe error", err)
	}
	if calls != 1 {
		t.Fatal("followed redirect")
	}
}

func TestWANUsageWorkerCancelsAccountChangeAndJoinsShutdown(t *testing.T) {
	a := newAuth(context.Background(), DefaultGitHubClientID, memoryCredentialStore{})
	defer a.CancelJoin()
	set := func(token string) {
		a.mu.Lock()
		a.credential = savedCredential{Version: 1, ClientID: DefaultGitHubClientID, Access: token}
		a.status.State = "signed_in"
		a.signalLocked()
		a.mu.Unlock()
	}
	set("account-a")
	entered, canceled := make(chan struct{}), make(chan struct{})
	u := &usageMonitor{request: make(chan struct{}, 1), fetch: func(ctx context.Context, token string) (Usage, error) {
		if token == "account-a" {
			close(entered)
			<-ctx.Done()
			close(canceled)
			return Usage{State: "ready", Used: 999}, nil
		}
		return Usage{State: "ready", Account: Account{ID: "github:2"}, Used: 5, CheckedAt: time.Now()}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); u.run(ctx, a) }()
	t.Cleanup(func() { cancel(); waitDone(t, done) })
	waitDone(t, entered)
	set("account-b")
	waitDone(t, canceled)
	deadline := time.After(5 * time.Second)
	for {
		got := u.status(a)
		if got.State == "ready" {
			if got.Used != 5 {
				t.Fatal("old account usage committed", got)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("new account was not checked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	u.mu.Lock()
	u.value.State = "stale"
	u.value.Error = "check failed"
	u.mu.Unlock()
	if got := u.status(a); got.Used != 5 || got.State != "stale" {
		t.Fatal("failed read lost last known data", got)
	}
	a.Logout()
	if got := u.status(a); got.State != "signed_out" || got.Used != 0 {
		t.Fatal("logout leaked old usage", got)
	}
}

func TestWANMicrosoftDeviceGrantAndRefresh(t *testing.T) {
	c := newMicrosoftDeviceClient()
	calls := 0
	token := fixtureMicrosoftToken()
	c.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		r.ParseForm()
		if r.PostForm.Get("client_id") != microsoftClientID {
			t.Fatal("wrong client")
		}
		body := `{"device_code":"fake-device","user_code":"ABCD-EFGH","verification_uri":"https://login.microsoft.com/device","expires_in":900,"interval":5}`
		if strings.HasSuffix(r.URL.Path, "/token") {
			body = `{"access_token":"` + token + `","refresh_token":"fake-refresh","token_type":"Bearer","expires_in":3600}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	code, err := c.code(context.Background())
	if err != nil || code.VerificationURI != "https://login.microsoft.com/device" {
		t.Fatal(code, err)
	}
	grant, err := c.poll(context.Background(), code.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	record := microsoftCredential(grant, time.Now())
	if !validSavedCredential(record) || !record.RefreshExpiresAt.IsZero() {
		t.Fatal("invented Microsoft refresh lifetime or invalid record")
	}
	if _, err := c.refresh(context.Background(), record.Refresh); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}

func TestWANMicrosoftTransientRefreshRemainsRetryable(t *testing.T) {
	for _, code := range []string{"temporarily_unavailable", "server_error", "slow_down", "invalid_grant"} {
		t.Run(code, func(t *testing.T) {
			c := newMicrosoftDeviceClient()
			c.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"` + code + `","error_description":"SECRET"}`))}, nil
			})
			_, err := c.refresh(context.Background(), "fake-refresh")
			if err == nil || errors.Is(err, errRefreshRejected) != (code == "invalid_grant") || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("incorrect refresh recovery", err)
			}
		})
	}
}

func TestWANMicrosoftHostingCarriesProviderThroughCredentialUpdates(t *testing.T) {
	m, folder := fakeManager(t, "credentials", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	m.account = func(context.Context, string) (Account, error) {
		return Account{ID: "microsoft:fixture", Name: "fixture@example.invalid", Provider: "microsoft"}, nil
	}
	m.auth.mu.Lock()
	m.auth.status.Provider = "microsoft"
	m.auth.mu.Unlock()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	endpoint := m.Status().Endpoint
	rotateManagerCredential(m, "fake-renewed-microsoft")
	waitCredentialCall(t, folder, "credentials 1")
	if got := m.Status(); got.State != "running" || got.Endpoint != endpoint {
		t.Fatal("Microsoft credential update interrupted hosting", got)
	}
	m.Stop()
}

func TestWANAccountTunnelCatalogFailsClosed(t *testing.T) {
	path := t.TempDir() + "/wan.json"
	one := tunnelConfig{Version: 2, ID: "one-tunnel", ClusterID: "usw2", Port: 41234, AccountID: "github:1", AccountName: "first"}
	two := one
	two.ID = "two-tunnel"
	two.AccountID = "github:2"
	two.AccountName = "second"
	if err := writeTunnelAccounts(path, map[string]tunnelConfig{one.AccountID: one, two.AccountID: two}); err != nil {
		t.Fatal(err)
	}
	got, err := readTunnelAccounts(path)
	if err != nil || got["github:1"].ID != "one-tunnel" || got["github:2"].ID != "two-tunnel" {
		t.Fatal(got, err)
	}
	if err := os.WriteFile(accountTunnelsPath(path), []byte(`{"version":1,"accounts":{"github:2":{"version":2,"id":"one-tunnel","clusterId":"usw2","port":41234,"accountID":"github:1"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTunnelAccounts(path); err == nil {
		t.Fatal("accepted another account's tunnel")
	}
	if err := os.WriteFile(accountTunnelsPath(path), []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTunnelAccounts(path); err == nil {
		t.Fatal("corrupt catalog became empty")
	}
}

func TestWANAccountsReuseTheirTunnelAndReplaceOnlySelectedAccount(t *testing.T) {
	m, folder := fakeManager(t, "account-tunnels", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	selectAccount := func(id string) {
		m.account = func(context.Context, string) (Account, error) {
			return Account{ID: "github:" + id, Name: "account-" + id, Provider: "github"}, nil
		}
	}
	start := func() string {
		t.Helper()
		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		id := m.Status().Tunnel.ID
		m.Stop()
		return id
	}
	selectAccount("1")
	first := start()
	selectAccount("2")
	second := start()
	if first == second {
		t.Fatal("new account reused another account's tunnel")
	}
	selectAccount("1")
	if again := start(); again != first {
		t.Fatal("returning account did not reuse its tunnel")
	}
	if err := m.RequestNewTunnel(); err != nil {
		t.Fatal(err)
	}
	accounts, err := readTunnelAccounts(m.path)
	if err != nil || !accounts["github:1"].New || accounts["github:2"].New {
		t.Fatal("replacement affected another account", accounts, err)
	}
	third := start()
	if third == first || third == second {
		t.Fatal("explicit replacement reused the old tunnel")
	}
	selectAccount("2")
	if again := start(); again != second {
		t.Fatal("replacement changed another account's default tunnel")
	}
	calls, _ := os.ReadFile(folder + "/calls")
	if strings.Count(string(calls), "create\n") != 3 || strings.Count(string(calls), "get\n") != 2 {
		t.Fatal("incorrect create/reuse operations", string(calls))
	}
	m.Close()
	restarted, _ := fakeManager(t, "account-tunnels", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	restarted.actions.Wait() // Join discovery before replacing the fixture's settings path.
	restarted.path = m.path
	restarted.command = m.command
	restarted.account = m.account
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.Status().Tunnel.ID != second {
		t.Fatal("restart forgot the account tunnel")
	}
}

func TestWANMicrosoftLoginRestoreAndSecureRecord(t *testing.T) {
	store := &authTestStore{}
	a := newAuth(context.Background(), DefaultGitHubClientID, store)
	defer a.CancelJoin()
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_out" })
	a.microsoft.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"device_code":"fake-device","user_code":"ABCD-EFGH","verification_uri":"https://microsoft.com/devicelogin","expires_in":900,"interval":5}`
		if strings.HasSuffix(r.URL.Path, "/token") {
			body = `{"access_token":"` + fixtureMicrosoftToken() + `","refresh_token":"fake-refresh","token_type":"Bearer","expires_in":3600}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	a.wait = func(context.Context, time.Duration) error { return nil }
	if _, err := a.Start(true, "microsoft", "device"); err != nil {
		t.Fatal(err)
	}
	got := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_in" })
	if got.Provider != "microsoft" || got.Renewal == nil || got.Storage != "system" {
		t.Fatal("Microsoft grant lost renewal or provider metadata", got)
	}
	a.CancelJoin()
	restored := newAuth(context.Background(), DefaultGitHubClientID, store)
	defer restored.CancelJoin()
	got = awaitAuth(t, restored, func(s AuthStatus) bool { return s.State == "signed_in" })
	if got.Provider != "microsoft" || got.Storage != "system" {
		t.Fatal("Microsoft remembered login did not restore", got)
	}
	vault := &fakeCredentialVault{}
	secure := systemCredentialStore{target: "fixture", vault: vault}
	record := microsoftCredential(githubGrant{Access: fixtureMicrosoftToken(), Refresh: strings.Repeat("r", 2600), ExpiresIn: 3600}, time.Now().UTC())
	if err := secure.save(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := secure.load()
	if err != nil || loaded.Access != record.Access || loaded.Refresh != record.Refresh {
		t.Fatal("secure compressed Microsoft record failed to round-trip", err)
	}
}
