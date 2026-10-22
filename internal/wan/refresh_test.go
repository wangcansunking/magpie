package wan

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const refreshedGrant = `{"access_token":"new-wan-access","refresh_token":"new-wan-refresh","token_type":"bearer","expires_in":3600,"refresh_token_expires_in":86400}`

func seedRefresh(t *testing.T, a *auth, clock *authTestClock, store *authTestStore, remember bool) savedCredential {
	t.Helper()
	record := rotatingCredential(a.clientID, githubGrant{Access: "old-wan-access", Refresh: "old-wan-refresh", ExpiresIn: 3600, RefreshExpiresIn: 86400}, a.now())
	if remember {
		store.mu.Lock()
		store.record = record
		store.mu.Unlock()
	}
	a.refreshWait = a.wait
	a.mu.Lock()
	a.credential = record
	a.status.State, a.status.ExpiresAt = "signed_in", record.ExpiresAt
	if remember {
		a.status.Storage = "system"
	}
	a.startRefreshLocked()
	a.mu.Unlock()
	return record
}

func TestWANRefreshRotatesBothTokensAndSecurelyRestores(t *testing.T) {
	for _, remember := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "system"}[remember], func(t *testing.T) {
			store := &authTestStore{}
			a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
				if err := r.ParseForm(); err != nil {
					return "", err
				}
				if r.URL.RawQuery != "" || len(r.PostForm) != 3 || r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "old-wan-refresh" || r.PostForm.Get("client_id") != "wan-own-app" {
					t.Error("refresh did not use only the device-flow form fields")
				}
				return refreshedGrant, nil
			})
			old := seedRefresh(t, a, clock, store, remember)
			step := nextAuthWait(t, clock, 55*time.Minute)
			clock.nanos.Store(old.RefreshAfter.UnixNano())
			close(step.release)
			nextAuthWait(t, clock, 55*time.Minute) // The next schedule proves commit finished.
			if token, err := a.token(); err != nil || token != "new-wan-access" {
				t.Fatal("new access token was not published", err)
			}
			a.mu.Lock()
			next := a.credential
			a.mu.Unlock()
			if next.Refresh != "new-wan-refresh" || !next.ExpiresAt.After(old.ExpiresAt) {
				t.Fatal("token pair was not rotated")
			}
			status, _ := json.Marshal(a.Status())
			if strings.Contains(string(status), "wan-access") || strings.Contains(string(status), "wan-refresh") {
				t.Fatal("credential leaked into status")
			}
			saved, saves, _ := store.snapshot()
			if !remember {
				if saves != 0 {
					t.Fatal("memory sign-in wrote to secure storage")
				}
				return
			}
			if saves != 1 || saved != next {
				t.Fatal("rotation was not persisted as one credential")
			}
			a.CancelJoin()
			restored := newAuth(context.Background(), a.clientID, store)
			t.Cleanup(restored.CancelJoin)
			got := awaitAuth(t, restored, func(s AuthStatus) bool { return s.State != "loading" })
			if got.State != "signed_in" || got.Storage != "system" {
				t.Fatal("remembered rotation did not restore")
			}
			if token, err := restored.token(); err != nil || token != "new-wan-access" {
				t.Fatal("restored stale access", err)
			}
		})
	}
}

func TestWANRefreshRestoresAnExpiredAccessTokenUsingRefresh(t *testing.T) {
	store := &authTestStore{}
	a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
		if r.URL.Path != "/login/oauth/access_token" {
			t.Error("restore requested a device code")
		}
		return refreshedGrant, nil
	})
	a.refreshWait = a.wait
	record := rotatingCredential(a.clientID, githubGrant{Access: "expired-access", Refresh: "old-refresh", ExpiresIn: 3600, RefreshExpiresIn: 86400}, a.now().Add(-2*time.Hour))
	store.mu.Lock()
	store.record = record
	store.mu.Unlock()
	a.mu.Lock()
	a.status.State = "loading"
	a.workers.Add(1)
	a.mu.Unlock()
	go a.restore()
	step := nextAuthWait(t, clock, 0)
	if _, err := a.token(); err == nil {
		t.Fatal("expired access token was exposed")
	}
	if status := a.Status(); status.State != "loading" || status.Error != "" {
		t.Fatal("restorable session incorrectly asks the user to sign in again")
	}
	close(step.release)
	nextAuthWait(t, clock, 55*time.Minute)
	if token, err := a.token(); err != nil || token != "new-wan-access" {
		t.Fatal("expired remembered session was not renewed", err)
	}
}

func TestWANRefreshTransientFailureRetriesButDoesNotExtendExpiry(t *testing.T) {
	store := &authTestStore{}
	calls := 0
	a, clock := fakeAuth(t, store, func(*http.Request) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("credential-bearing transport failure")
		}
		return refreshedGrant, nil
	})
	old := seedRefresh(t, a, clock, store, false)
	step := nextAuthWait(t, clock, 55*time.Minute)
	clock.nanos.Store(old.RefreshAfter.UnixNano())
	close(step.release)
	retry := nextAuthWait(t, clock, 30*time.Second)
	status := a.Status()
	if status.Renewal == nil || status.Renewal.State != "retrying" || !status.Renewal.NextAttemptAt.Equal(a.now().Add(30*time.Second)) {
		t.Fatal("transient retry did not publish its actual next attempt")
	}
	if a.tokenExpires() != old.ExpiresAt {
		t.Fatal("failed refresh extended the access deadline")
	}
	clock.nanos.Add(int64(30 * time.Second))
	close(retry.release)
	nextAuthWait(t, clock, 55*time.Minute)
	if token, _ := a.token(); token != "new-wan-access" {
		t.Fatal("retry did not renew")
	}
}

func TestWANRefreshRejectionErasesRememberedPair(t *testing.T) {
	store := &authTestStore{}
	a, clock := fakeAuth(t, store, func(*http.Request) (string, error) {
		return `{"error":"bad_refresh_token","error_description":"SECRET"}`, nil
	})
	old := seedRefresh(t, a, clock, store, true)
	step := nextAuthWait(t, clock, 55*time.Minute)
	clock.nanos.Store(old.RefreshAfter.UnixNano())
	close(step.release)
	status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "error" })
	if status.Renewal == nil || status.Renewal.State != "required" || !status.Renewal.NextAttemptAt.IsZero() {
		t.Fatal("rejected refresh did not require a fresh sign-in")
	}
	if strings.Contains(status.Error, "SECRET") {
		t.Fatal("OAuth error detail leaked")
	}
	a.CancelJoin()
	if saved, _, erases := store.snapshot(); saved.Access != "" || saved.Refresh != "" || erases != 1 {
		t.Fatal("rejected refresh could restore after restart")
	}
}

func TestWANRefreshLogoutJoinsAndErasesALateVaultWrite(t *testing.T) {
	store := &authTestStore{saveIn: make(chan struct{}), saveOut: make(chan struct{})}
	a, clock := fakeAuth(t, store, func(*http.Request) (string, error) { return refreshedGrant, nil })
	old := seedRefresh(t, a, clock, store, true)
	step := nextAuthWait(t, clock, 55*time.Minute)
	clock.nanos.Store(old.RefreshAfter.UnixNano())
	close(step.release)
	waitDone(t, store.saveIn)
	a.Logout()
	closed := make(chan struct{})
	go func() { defer close(closed); _ = a.Erase() }()
	close(store.saveOut)
	waitDone(t, closed)
	a.CancelJoin()
	if saved, _, _ := store.snapshot(); saved.Access != "" || saved.Refresh != "" {
		t.Fatal("late rotation revived signed-out credentials")
	}
}

func TestWANDeviceGrantRetainsRefreshOnlyInSecureRecord(t *testing.T) {
	store := &authTestStore{}
	a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			return deviceResponse(), nil
		}
		return refreshedGrant, nil
	})
	a.refreshWait = a.wait
	startAuth(t, a, true)
	step := nextAuthWait(t, clock, 5*time.Second)
	close(step.release)
	nextAuthWait(t, clock, 55*time.Minute)
	saved, saves, _ := store.snapshot()
	if saves != 1 || saved.Version != 2 || saved.Refresh != "new-wan-refresh" || saved.RefreshAfter.IsZero() {
		t.Fatal("device sign-in discarded its renewal grant")
	}
	status, _ := json.Marshal(a.Status())
	if strings.Contains(string(status), "new-wan-") {
		t.Fatal("device grant leaked into status")
	}
}

func TestWANRefreshFailedSaveNeverPublishesOrRestoresRotatedTokens(t *testing.T) {
	store := &authTestStore{saveErr: errors.New("native write unknown")}
	a, clock := fakeAuth(t, store, func(*http.Request) (string, error) { return refreshedGrant, nil })
	old := seedRefresh(t, a, clock, store, true)
	step := nextAuthWait(t, clock, 55*time.Minute)
	clock.nanos.Store(old.RefreshAfter.UnixNano())
	close(step.release)
	status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "error" })
	if !strings.Contains(status.Error, "safely save") {
		t.Fatal("failed write was mislabeled")
	}
	if token, _ := a.token(); token != "" {
		t.Fatal("unsaved rotated token was published")
	}
	a.CancelJoin()
	if saved, _, _ := store.snapshot(); saved.Access != "" || saved.Refresh != "" {
		t.Fatal("failed write left a restorable pair")
	}
}

func TestWANRefreshTerminalExpiryErasesAnAlreadyExpiredSession(t *testing.T) {
	store := &authTestStore{}
	a, clock := fakeAuth(t, store, func(*http.Request) (string, error) {
		t.Error("expired refresh reached GitHub")
		return "", errors.New("unexpected")
	})
	old := seedRefresh(t, a, clock, store, true)
	step := nextAuthWait(t, clock, 55*time.Minute)
	clock.nanos.Store(old.RefreshExpiresAt.UnixNano())
	if _, err := a.token(); err == nil {
		t.Fatal("expired access was accepted")
	}
	close(step.release)
	status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "error" })
	if strings.Contains(status.Error, "SECRET") {
		t.Fatal("refresh detail leaked")
	}
	a.CancelJoin()
	if saved, _, _ := store.snapshot(); saved.Access != "" {
		t.Fatal("expired pair survived restart")
	}
}

func TestWANRenewalStatusTracksScheduledAndInFlightWork(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	store := &authTestStore{}
	a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
		close(entered)
		select {
		case <-release:
			return refreshedGrant, nil
		case <-r.Context().Done():
			return "", r.Context().Err()
		}
	})
	old := seedRefresh(t, a, clock, store, false)
	step := nextAuthWait(t, clock, 55*time.Minute)
	before := a.Status()
	if before.Renewal == nil || before.Renewal.State != "scheduled" || before.Renewal.NextAttemptAt != old.RefreshAfter {
		t.Fatal("saved renewal schedule was not exposed")
	}
	before.Renewal.State = "caller mutation"
	if a.Status().Renewal.State != "scheduled" {
		t.Fatal("status returned mutable owner state")
	}
	clock.nanos.Store(old.RefreshAfter.UnixNano())
	close(step.release)
	waitDone(t, entered)
	during := a.Status()
	if during.Renewal == nil || during.Renewal.State != "refreshing" || !during.Renewal.NextAttemptAt.IsZero() {
		t.Fatal("in-flight renewal was reported as a future scheduled attempt")
	}
	close(release)
	nextAuthWait(t, clock, 55*time.Minute)
	after := a.Status()
	if after.Renewal == nil || after.Renewal.State != "scheduled" || !after.Renewal.NextAttemptAt.After(old.RefreshAfter) {
		t.Fatal("rotation did not replace the published schedule")
	}
	data, _ := json.Marshal(after)
	if strings.Contains(string(data), "wan-access") || strings.Contains(string(data), "wan-refresh") {
		t.Fatal("renewal metadata exposed a credential")
	}
	a.Logout()
	if a.Status().Renewal != nil {
		t.Fatal("logout retained scheduled renewal metadata")
	}
}

func TestWANAccessOnlyLoginNeverClaimsAutomaticRenewal(t *testing.T) {
	a, _ := fakeAuth(t, &authTestStore{}, func(*http.Request) (string, error) { return "", nil })
	a.mu.Lock()
	a.credential = savedCredential{Version: 1, ClientID: a.clientID, Access: "legacy-access", ExpiresAt: a.now().Add(time.Hour)}
	a.status.State = "signed_in"
	a.mu.Unlock()
	if a.Status().Renewal != nil {
		t.Fatal("legacy access-only credential claimed a refresh worker")
	}
}
