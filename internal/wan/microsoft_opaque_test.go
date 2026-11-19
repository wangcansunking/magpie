package wan

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWANMicrosoftOpaqueCredentialReachesOnlyMicrosoft(t *testing.T) {
	grant := githubGrant{Access: "opaque-access-secret", Refresh: "opaque-refresh-secret", Type: "Bearer", ExpiresIn: 3600, ClientInfo: fixtureMicrosoftInfo()}
	if !validMicrosoftGrant(grant) {
		t.Fatal("valid opaque Microsoft credential rejected")
	}
	record := microsoftCredential(grant, time.Now().UTC())
	vault := systemCredentialStore{target: "opaque-fixture", vault: &fakeCredentialVault{}}
	if err := vault.save(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := vault.load()
	if err != nil || loaded != record {
		t.Fatal("opaque credential or account metadata lost on restart", err)
	}
	a := newAuth(context.Background(), DefaultGitHubClientID, memoryCredentialStore{})
	defer a.CancelJoin()
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State != "loading" })
	a.mu.Lock()
	a.credential, a.status.State = loaded, "signed_in"
	a.mu.Unlock()
	m := &Manager{auth: a}
	identity, err := m.credentialAccount(grant.Access)
	if err != nil || identity.ID != fixtureMicrosoftAccount().ID {
		t.Fatal("opaque credential lost stable account identity", err)
	}
	c := newServiceClient()
	calls := 0
	c.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "global.rel.tunnels.api.visualstudio.com" || r.Header.Get("Authorization") != "Bearer "+grant.Access {
			t.Error("Microsoft access token sent outside its service")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[{"name":"BandwidthPerUser","current":1,"limit":2}]`))}, nil
	})
	u, err := c.usage(context.Background(), grant.Access, identity)
	if err != nil || calls != 1 || u.Account.ID != identity.ID || u.Used != 1 {
		t.Fatal("opaque credential could not read usage", err)
	}
	if _, err := m.credentialAccount("obsolete-access-secret"); err == nil {
		t.Fatal("stale credential accepted")
	}
}

func TestWANMicrosoftOpaqueRefreshKeepsAccount(t *testing.T) {
	different := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"33333333-3333-3333-3333-333333333333","utid":"11111111-1111-1111-1111-111111111111"}`))
	for _, tc := range []struct {
		name, metadata string
		reject         bool
	}{{"omitted", "", false}, {"same", fixtureMicrosoftInfo(), false}, {"different", different, true}, {"malformed", "invalid-client-info", true}} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := tc.metadata
			store := &authTestStore{}
			a, clock := fakeAuth(t, store, func(*http.Request) (string, error) { return "", nil })
			a.microsoft.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
				r.ParseForm()
				if r.PostForm.Get("client_info") != "1" || r.PostForm.Get("refresh_token") != "old-refresh" {
					t.Error("missing Microsoft renewal metadata request")
				}
				body := `{"access_token":"renewed-opaque-access","refresh_token":"renewed-refresh","token_type":"Bearer","expires_in":3600,"client_info":"` + metadata + `"}`
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			old := microsoftCredential(githubGrant{Access: "old-opaque-access", Refresh: "old-refresh", ExpiresIn: 3600, ClientInfo: fixtureMicrosoftInfo()}, a.now())
			a.refreshWait = a.wait
			a.mu.Lock()
			a.credential = old
			a.status.State, a.status.Provider, a.status.Storage = "signed_in", "microsoft", "system"
			a.startRefreshLocked()
			a.mu.Unlock()
			step := nextAuthWait(t, clock, 55*time.Minute)
			clock.nanos.Store(old.RefreshAfter.UnixNano())
			close(step.release)
			if tc.reject {
				awaitAuth(t, a, func(s AuthStatus) bool { return s.State != "signed_in" })
				_, saves, _ := store.snapshot()
				if saves != 0 {
					t.Fatal("different or invalid account committed")
				}
				return
			}
			nextAuthWait(t, clock, 55*time.Minute)
			saved, saves, _ := store.snapshot()
			if saves != 1 || saved.Access != "renewed-opaque-access" || saved.Account != old.Account || !validSavedCredential(saved) {
				t.Fatal("renewal failed to securely retain account identity")
			}
		})
	}
}

func TestWANMicrosoftGrantErrorsDoNotExposeSecrets(t *testing.T) {
	base := githubGrant{Access: "opaque-access-secret", Refresh: "refresh-secret", Type: "Bearer", ExpiresIn: 3600}
	for _, field := range []string{"access", "refresh", "type", "lifetime"} {
		g := base
		switch field {
		case "access":
			g.Access = strings.Repeat("s", 8193)
		case "refresh":
			g.Refresh = ""
		case "type":
			g.Type = "secret-type"
		case "lifetime":
			g.ExpiresIn = 0
		}
		err := microsoftGrantError(g)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("missing or unsafe credential error", field)
		}
	}
}

func TestWANMicrosoftLegacyRecordStillLoads(t *testing.T) {
	record := microsoftCredential(githubGrant{Access: fixtureMicrosoftToken(), Refresh: "legacy-refresh", ExpiresIn: 3600}, time.Now().UTC())
	record.Version = 3
	record.Account = Account{}
	store := systemCredentialStore{target: "legacy-fixture", vault: &fakeCredentialVault{}}
	if err := store.save(record); err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.load(); err != nil || loaded != record {
		t.Fatal("legacy Microsoft record no longer loads", err)
	}
}
