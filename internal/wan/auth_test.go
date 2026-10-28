package wan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type authTestStore struct {
	mu       sync.Mutex
	record   savedCredential
	loadErr  error
	saveErr  error
	eraseErr error
	loads    int
	saves    int
	erases   int
	saveIn   chan struct{}
	saveOut  chan struct{}
	loadIn   chan struct{}
	loadOut  chan struct{}
	eraseIn  chan struct{}
	eraseOut chan struct{}
}

func (*authTestStore) available() bool { return true }
func (s *authTestStore) load() (savedCredential, error) {
	if s.loadIn != nil {
		close(s.loadIn)
		<-s.loadOut
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loadErr != nil {
		return savedCredential{}, s.loadErr
	}
	if s.record.Access == "" {
		return savedCredential{}, errCredentialMissing
	}
	return s.record, nil
}
func (s *authTestStore) save(record savedCredential) error {
	if s.saveIn != nil {
		close(s.saveIn)
		<-s.saveOut
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.record = record // Even a failed native write might have saved its data.
	return s.saveErr
}
func (s *authTestStore) delete() error {
	s.mu.Lock()
	in, out := s.eraseIn, s.eraseOut
	s.eraseIn, s.eraseOut = nil, nil // Only the next erase is held.
	s.mu.Unlock()
	if in != nil {
		close(in)
		<-out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.erases++
	if s.eraseErr != nil {
		return s.eraseErr
	}
	s.record = savedCredential{}
	return nil
}
func (s *authTestStore) snapshot() (savedCredential, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record, s.saves, s.erases
}

type authWaitStep struct {
	d       time.Duration
	release chan struct{}
}

type authTestClock struct {
	nanos atomic.Int64
	steps chan authWaitStep
}

func fakeAuth(t *testing.T, store credentialStore, handle func(*http.Request) (string, error)) (*auth, *authTestClock) {
	t.Helper()
	if store == nil {
		store = memoryCredentialStore{}
	}
	a := newAuth(context.Background(), "wan-own-app", store)
	t.Cleanup(a.CancelJoin)
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State != "loading" })
	clock := &authTestClock{steps: make(chan authWaitStep)}
	clock.nanos.Store(time.Now().UnixNano())
	a.now = func() time.Time { return time.Unix(0, clock.nanos.Load()) }
	a.wait = func(ctx context.Context, d time.Duration) error {
		step := authWaitStep{d: d, release: make(chan struct{})}
		select {
		case clock.steps <- step:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-step.release:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.github = githubDeviceClient{base: "https://github.invalid", client: &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, err := handle(r)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	return a, clock
}

func awaitAuth(t *testing.T, a *auth, predicate func(AuthStatus) bool) AuthStatus {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		status := a.Status()
		if predicate(status) {
			return status
		}
		select {
		case <-deadline.C:
			t.Fatalf("auth state did not arrive: %#v", status)
		default:
			runtime.Gosched()
		}
	}
}

func nextAuthWait(t *testing.T, clock *authTestClock, want time.Duration) authWaitStep {
	t.Helper()
	select {
	case step := <-clock.steps:
		if step.d != want {
			t.Fatalf("poll wait = %s, want %s", step.d, want)
		}
		return step
	case <-time.After(20 * time.Second):
		t.Fatal("auth did not schedule its next poll")
		return authWaitStep{}
	}
}

func startAuth(t *testing.T, a *auth, remember bool) AuthStatus {
	t.Helper()
	s, err := a.Start(remember)
	if err != nil || s.State != "requesting_code" || s.AttemptID == 0 {
		t.Fatalf("start did not register an initial attempt: %#v %v", s, err)
	}
	return s
}

func TestWANAuthRegistrationCancellationAndReplacement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	a, clock := fakeAuth(t, nil, func(r *http.Request) (string, error) {
		if r.URL.Path != "/login/device/code" {
			t.Error("canceled initial request reached token polling")
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-release // Deliberately ignore cancellation to reproduce a late HTTP reply.
		}
		return deviceResponse(), nil
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	first := startAuth(t, a, false)
	waitDone(t, entered)
	if !a.Cancel(first.AttemptID) || a.Status().State != "signed_out" {
		t.Fatal("initial request was not registered before its HTTP response")
	}
	second := startAuth(t, a, false)
	if second.AttemptID == first.AttemptID || a.Cancel(first.AttemptID) {
		t.Fatal("a stale cancel affected the replacement attempt")
	}
	once.Do(func() { close(release) })
	step := nextAuthWait(t, clock, 5*time.Second)
	if s := a.Status(); s.AttemptID != second.AttemptID || s.UserCode != "ABCD-1234" || s.State != "awaiting_authorization" {
		t.Fatalf("late old response replaced the new attempt: %#v", s)
	}
	if !a.Cancel(second.AttemptID) {
		t.Fatal("replacement was not cancelable")
	}
	close(step.release)
	a.CancelJoin()
	if _, err := a.token(); err == nil {
		t.Fatal("a canceled attempt exposed credentials")
	}
}

func TestWANAuthWaitsForPollingAndHonorsSlowDown(t *testing.T) {
	var polls atomic.Int32
	a, clock := fakeAuth(t, nil, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			if err := r.ParseForm(); err != nil || r.Form.Get("scope") != "" || r.Form.Get("client_id") != "wan-own-app" {
				t.Error("sign-in did not use its explicit app and empty least-privilege scope")
			}
			return deviceResponse(), nil
		}
		switch polls.Add(1) {
		case 1:
			return `{"error":"authorization_pending"}`, nil
		case 2:
			return `{"error":"slow_down","interval":1}`, nil
		case 3:
			return `{"error":"slow_down","interval":30}`, nil
		default:
			return `{"access_token":"fake-wan-token","token_type":"bearer","expires_in":3600}`, nil
		}
	})
	startAuth(t, a, false)
	for _, d := range []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second} {
		step := nextAuthWait(t, clock, d)
		if _, err := a.token(); err == nil {
			t.Fatal("credential exposed before authorization completed")
		}
		close(step.release)
	}
	status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_in" })
	if token, err := a.token(); err != nil || token != "fake-wan-token" {
		t.Fatalf("completed memory sign-in is unavailable: %v", err)
	}
	if status.Storage != "memory" || status.StorageAvailable || status.UserCode != "" || status.VerificationURI != "" {
		t.Fatalf("bad completed snapshot: %#v", status)
	}
	data, _ := json.Marshal(status)
	if strings.Contains(string(data), "fake-wan-token") || strings.Contains(string(data), "DISCARDED") {
		t.Fatal("credentials escaped through public status")
	}
	if a.Cancel(status.AttemptID) {
		t.Fatal("cancel of an already completed browser attempt signed the user out")
	}
	if a.tokenExpires() != status.ExpiresAt {
		t.Fatal("host could not read the access-token deadline")
	}
	clock.nanos.Store(status.ExpiresAt.UnixNano())
	if token, err := a.token(); err == nil || token != "" || a.Status().State != "expired" {
		t.Fatal("expired access token remained available")
	}
}

func TestWANAuthTokenSnapshotKeepsDeadlineWhenNextClockReadExpires(t *testing.T) {
	a, clock := fakeAuth(t, nil, func(*http.Request) (string, error) {
		t.Error("credential snapshot attempted network I/O")
		return "", errors.New("unexpected")
	})
	before := time.Unix(0, clock.nanos.Load())
	expires := before.Add(time.Second)
	a.mu.Lock()
	a.status.State = "signed_in"
	a.credential = savedCredential{Version: 1, ClientID: "wan-own-app", Access: "fake-snapshot", ExpiresAt: expires}
	a.mu.Unlock()
	var reads atomic.Int32
	a.now = func() time.Time {
		if reads.Add(1) == 1 {
			return before
		}
		return expires
	}
	token, deadline, err := a.tokenSnapshot()
	if err != nil || token != "fake-snapshot" || !deadline.Equal(expires) {
		t.Fatalf("access and its original expiry were not read atomically: token present=%v expiry=%v error=%v", token != "", deadline, err)
	}
	if token, deadline, err := a.tokenSnapshot(); err == nil || token != "" || !deadline.IsZero() {
		t.Fatal("snapshot returned a token at its expiry boundary")
	}
}

func TestWANAuthExpiryCoversInitialRequestAndPendingPolls(t *testing.T) {
	var clock *authTestClock
	a, c := fakeAuth(t, nil, func(r *http.Request) (string, error) {
		clock.nanos.Add(int64(901 * time.Second))
		return deviceResponse(), nil
	})
	clock = c
	startAuth(t, a, false)
	s := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "error" })
	if !strings.Contains(s.Error, "expired") || s.UserCode != "" {
		t.Fatalf("slow initial response revived an expired device code: %#v", s)
	}

	b, clock2 := fakeAuth(t, nil, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			return deviceResponse(), nil
		}
		t.Error("a poll was submitted after the device deadline")
		return `{"error":"authorization_pending"}`, nil
	})
	startAuth(t, b, false)
	step := nextAuthWait(t, clock2, 5*time.Second)
	clock2.nanos.Add(int64(900 * time.Second))
	close(step.release)
	awaitAuth(t, b, func(s AuthStatus) bool { return s.State == "error" && strings.Contains(s.Error, "expired") })
}

func TestWANAuthDeviceLifetimeBoundsNetworkContext(t *testing.T) {
	var deadlineDuration time.Duration
	var deadlineCancel context.CancelFunc
	pollEntered := make(chan struct{})
	a, clock := fakeAuth(t, nil, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			return deviceResponse(), nil
		}
		close(pollEntered)
		<-r.Context().Done()
		return "", r.Context().Err()
	})
	deadlineCreated := make(chan struct{})
	a.deadline = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		deadlineDuration = d
		ctx, cancel := context.WithCancel(parent)
		deadlineCancel = cancel
		close(deadlineCreated)
		return ctx, cancel
	}
	startAuth(t, a, false)
	waitDone(t, deadlineCreated)
	if deadlineDuration != 900*time.Second {
		t.Fatalf("device deadline = %s", deadlineDuration)
	}
	step := nextAuthWait(t, clock, 5*time.Second)
	close(step.release)
	waitDone(t, pollEntered)
	// The cancellation stands for the injected lifetime timer firing. It also
	// interrupts a body or request currently held inside the bounded HTTP client.
	deadlineCancel()
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "error" && strings.Contains(s.Error, "expired") })
}

func TestWANAuthPersistenceIsCommittedBeforeUse(t *testing.T) {
	store := &authTestStore{saveIn: make(chan struct{}), saveOut: make(chan struct{})}
	a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			return deviceResponse(), nil
		}
		return `{"access_token":"fake-persisted","token_type":"bearer"}`, nil
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(store.saveOut) }) })
	startAuth(t, a, true)
	step := nextAuthWait(t, clock, 5*time.Second)
	close(step.release)
	waitDone(t, store.saveIn)
	if _, err := a.token(); err == nil || a.Status().State == "signed_in" {
		t.Fatal("token was handed to the host before secure storage completed")
	}
	once.Do(func() { close(store.saveOut) })
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_in" && s.Storage == "system" })
	if token, err := a.token(); err != nil || token != "fake-persisted" {
		t.Fatalf("persisted token was not released: %v", err)
	}
	record, saves, _ := store.snapshot()
	if record.Access != "fake-persisted" || record.ClientID != "wan-own-app" || saves != 1 {
		t.Fatal("wrong independent WAN credential was persisted")
	}
	a.CancelJoin()
	if record, _, _ := store.snapshot(); record.Access != "fake-persisted" {
		t.Fatal("normal shutdown erased a successfully remembered login")
	}
}

func TestWANAuthTokenExpiringDuringVaultWriteIsErasedAndNeverReleased(t *testing.T) {
	store := &authTestStore{saveIn: make(chan struct{}), saveOut: make(chan struct{})}
	a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			return deviceResponse(), nil
		}
		return `{"access_token":"fake-short-token","token_type":"bearer","expires_in":60}`, nil
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(store.saveOut) }) })
	startAuth(t, a, true)
	step := nextAuthWait(t, clock, 5*time.Second)
	close(step.release)
	waitDone(t, store.saveIn)
	clock.nanos.Add(int64(61 * time.Second))
	once.Do(func() { close(store.saveOut) })
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "expired" })
	a.workers.Wait()
	if token, _, err := a.tokenSnapshot(); err == nil || token != "" {
		t.Fatal("token expiring during vault persistence was released")
	}
	if record, _, _ := store.snapshot(); record.Access != "" {
		t.Fatal("unusable newly persisted token was left in secure storage")
	}
}

func TestWANAuthCancelLogoutAndShutdownEraseLatePersistence(t *testing.T) {
	for _, action := range []string{"cancel", "logout", "shutdown", "replace"} {
		t.Run(action, func(t *testing.T) {
			store := &authTestStore{saveIn: make(chan struct{}), saveOut: make(chan struct{})}
			a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
				if r.URL.Path == "/login/device/code" {
					return deviceResponse(), nil
				}
				return `{"access_token":"LATE-SECRET","token_type":"bearer"}`, nil
			})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(store.saveOut) }) })
			first := startAuth(t, a, true)
			step := nextAuthWait(t, clock, 5*time.Second)
			close(step.release)
			waitDone(t, store.saveIn)
			done := make(chan struct{})
			go func() {
				defer close(done)
				switch action {
				case "cancel":
					a.Cancel(first.AttemptID)
				case "logout":
					a.Logout()
				case "shutdown":
					a.CancelJoin()
				case "replace":
					_, _ = a.Start(false)
				}
			}()
			awaitAuth(t, a, func(s AuthStatus) bool { return s.State != "awaiting_authorization" })
			if token, err := a.token(); err == nil || token != "" {
				t.Fatal("invalidated login returned the held token")
			}
			once.Do(func() { close(store.saveOut) })
			waitDone(t, done)
			if action == "logout" {
				if _, err := a.Start(false); err == nil {
					t.Fatal("sign-in reopened before hosting cleanup and credential erasure")
				}
				if err := a.Erase(); err != nil {
					t.Fatal(err)
				}
			}
			if action == "replace" {
				step := nextAuthWait(t, clock, 5*time.Second)
				a.Cancel(a.Status().AttemptID)
				close(step.release)
			}
			a.CancelJoin()
			if record, _, _ := store.snapshot(); record.Access != "" {
				t.Fatal("a canceled late write survived in the vault")
			}
		})
	}
}

func TestWANAuthShutdownJoinsLateInitialReply(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a, _ := fakeAuth(t, nil, func(r *http.Request) (string, error) {
		close(entered)
		<-release
		return deviceResponse(), nil
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	startAuth(t, a, false)
	waitDone(t, entered)
	done := make(chan struct{})
	go func() { a.CancelJoin(); close(done) }()
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_out" })
	select {
	case <-done:
		t.Fatal("shutdown returned before its held initial request joined")
	default:
	}
	if _, err := a.Start(false); err == nil {
		t.Fatal("shutdown did not close the authorization start gate")
	}
	once.Do(func() { close(release) })
	waitDone(t, done)
	if a.Status().State != "signed_out" {
		t.Fatal("a late initial device code revived shutdown")
	}
}

func TestWANAuthOAuthFailuresClearDisplayAndNeverEchoSecrets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial string
		poll    string
		message string
	}{
		{"initial denied", `{"error":"access_denied","error_description":"SECRET"}`, "", "denied"},
		{"poll denied", "", `{"error":"access_denied","error_description":"SECRET"}`, "denied"},
		{"poll expired", "", `{"error":"expired_token","error_description":"SECRET"}`, "expired"},
		{"disabled", `{"error":"device_flow_disabled","error_description":"SECRET"}`, "", "unavailable"},
		{"transport", "transport", "", "network"},
		{"huge lifetime", "", `{"access_token":"SECRET","token_type":"bearer","expires_in":2147483647}`, "lifetime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, clock := fakeAuth(t, nil, func(r *http.Request) (string, error) {
				if r.URL.Path == "/login/device/code" {
					if tc.initial == "transport" {
						return "", errors.New("SECRET transport URL")
					}
					if tc.initial != "" {
						return tc.initial, nil
					}
					return deviceResponse(), nil
				}
				return tc.poll, nil
			})
			first := startAuth(t, a, false)
			if tc.initial == "" {
				step := nextAuthWait(t, clock, 5*time.Second)
				close(step.release)
			}
			status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "error" })
			if !strings.Contains(status.Error, tc.message) || strings.Contains(status.Error, "SECRET") || status.UserCode != "" || status.VerificationURI != "" {
				t.Fatalf("unsafe or incorrect OAuth failure: %#v", status)
			}
			if _, err := a.token(); err == nil || a.Cancel(first.AttemptID) {
				t.Fatal("failed attempt could be used or canceled after completion")
			}
		})
	}
}

func TestWANAuthSafeFailureAndUnknownStorageStayClosed(t *testing.T) {
	store := &authTestStore{saveErr: errors.New("SECRET native error")}
	a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			return deviceResponse(), nil
		}
		return `{"access_token":"SECRET","token_type":"bearer"}`, nil
	})
	startAuth(t, a, true)
	step := nextAuthWait(t, clock, 5*time.Second)
	close(step.release)
	status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "error" })
	if strings.Contains(status.Error, "SECRET") {
		t.Fatal("native storage error exposed credential material")
	}
	if record, _, _ := store.snapshot(); record.Access != "" {
		t.Fatal("a possibly successful failed write was not erased")
	}
	if _, err := a.token(); err == nil {
		t.Fatal("failed persistence exposed its credential")
	}
	if _, err := a.Start(false); err == nil {
		t.Fatal("unknown storage was silently treated as empty")
	}
	a.Logout()
	store.mu.Lock()
	store.eraseErr = errors.New("SECRET delete")
	store.mu.Unlock()
	if err := a.Erase(); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("failed erase was not safely reported")
	}
	if _, err := a.Start(false); err == nil {
		t.Fatal("failed logout reopened the start gate")
	}
	store.mu.Lock()
	store.eraseErr = nil
	store.mu.Unlock()
	if err := a.Erase(); err != nil || a.Status().State != "signed_out" {
		t.Fatal("successful logout retry did not unblock sign-in")
	}
}

func TestWANAuthRestoresOnlyItsOwnUnexpiredCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record savedCredential
		err    error
		state  string
	}{
		{"missing", savedCredential{}, nil, "signed_out"},
		{"valid", savedCredential{Version: 1, ClientID: "wan-own-app", Access: "fake-saved"}, nil, "signed_in"},
		{"expired", savedCredential{Version: 1, ClientID: "wan-own-app", Access: "fake-saved", ExpiresAt: time.Now().Add(-time.Hour)}, nil, "expired"},
		{"different app", savedCredential{Version: 1, ClientID: "other-app", Access: "fake-saved"}, nil, "error"},
		{"invalid", savedCredential{Version: 9, ClientID: "wan-own-app", Access: "fake-saved"}, nil, "error"},
		{"unknown", savedCredential{}, errors.New("SECRET vault read"), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &authTestStore{record: tc.record, loadErr: tc.err}
			a := newAuth(context.Background(), "wan-own-app", store)
			t.Cleanup(a.CancelJoin)
			s := awaitAuth(t, a, func(s AuthStatus) bool { return s.State != "loading" })
			if s.State != tc.state || !s.StorageAvailable || strings.Contains(s.Error, "SECRET") {
				t.Fatalf("bad restore classification: %#v", s)
			}
			token, err := a.token()
			if tc.state == "signed_in" {
				if err != nil || token != "fake-saved" {
					t.Fatal("own valid credential was not restored")
				}
			} else if err == nil || token != "" {
				t.Fatal("invalid saved credential became available")
			}
		})
	}
}

func TestWANAuthHeldRestoreNeverBlocksStatusOrRevivesLogout(t *testing.T) {
	store := &authTestStore{record: savedCredential{Version: 1, ClientID: "wan-own-app", Access: "LATE-SAVED"}, loadIn: make(chan struct{}), loadOut: make(chan struct{})}
	a := newAuth(context.Background(), "wan-own-app", store)
	t.Cleanup(a.CancelJoin)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(store.loadOut) }) })
	waitDone(t, store.loadIn)
	if a.Status().State != "loading" {
		t.Fatal("held restore was not visible as cached loading")
	}
	a.Logout()
	if a.Status().State != "signing_out" {
		t.Fatal("a held vault read blocked immediate logout")
	}
	once.Do(func() { close(store.loadOut) })
	if err := a.Erase(); err != nil {
		t.Fatal(err)
	}
	if token, err := a.token(); err == nil || token != "" || a.Status().State != "signed_out" {
		t.Fatal("late stored credential revived logout")
	}
}

func TestWANAuthCancelOfFailedVaultCleanupCannotClearItsError(t *testing.T) {
	store := &authTestStore{saveIn: make(chan struct{}), saveOut: make(chan struct{})}
	a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			return deviceResponse(), nil
		}
		return `{"access_token":"LATE-SECRET","token_type":"bearer"}`, nil
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(store.saveOut) }) })
	status := startAuth(t, a, true)
	step := nextAuthWait(t, clock, 5*time.Second)
	close(step.release)
	waitDone(t, store.saveIn)
	store.mu.Lock()
	store.saveErr = errors.New("SECRET save")
	store.eraseErr = errors.New("SECRET cleanup")
	store.mu.Unlock()
	done := make(chan struct{})
	go func() { a.Cancel(status.AttemptID); close(done) }()
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_out" })
	once.Do(func() { close(store.saveOut) })
	waitDone(t, done)
	if status := a.Status(); status.State != "error" || strings.Contains(status.Error, "SECRET") {
		t.Fatal("unerasable canceled save was not reported safely")
	}
	if a.Cancel(status.AttemptID) {
		t.Fatal("a second cancel cleared the failed storage barrier")
	}
	if _, err := a.Start(false); err == nil {
		t.Fatal("an unerasable late credential was treated as an empty vault")
	}
	store.mu.Lock()
	store.eraseErr = nil
	store.mu.Unlock()
	a.Logout()
	if err := a.Erase(); err != nil {
		t.Fatal(err)
	}
}

func TestWANAuthRejectMatchesOnlyTheCurrentSignedInSnapshot(t *testing.T) {
	var calls atomic.Int32
	a, _ := fakeAuth(t, nil, func(*http.Request) (string, error) {
		calls.Add(1)
		return deviceResponse(), nil
	})
	a.mu.Lock()
	a.status.State = "signed_in"
	a.credential = savedCredential{Version: 1, ClientID: "wan-own-app", Access: "current-fake-token"}
	a.mu.Unlock()
	if a.Reject("older-fake-token") || a.Reject("") {
		t.Fatal("a late previous helper rejected the replacement credential")
	}
	if token, err := a.token(); err != nil || token != "current-fake-token" {
		t.Fatal("a mismatching helper refusal invalidated the current login")
	}
	if !a.Reject("current-fake-token") || a.Reject("current-fake-token") {
		t.Fatal("a matching current credential was not invalidated exactly once")
	}
	status := a.Status()
	if status.State != "error" || !strings.Contains(status.Error, "sign in again") || strings.Contains(status.Error, "current-fake-token") {
		t.Fatalf("rejected credential did not enable safe re-login: %#v", status)
	}
	if token, _, err := a.tokenSnapshot(); err == nil || token != "" {
		t.Fatal("service-rejected token remained usable")
	}
	if calls.Load() != 0 {
		t.Fatal("rejecting a credential attempted OAuth network I/O")
	}
	startAuth(t, a, false)
	a.Cancel(a.Status().AttemptID)
}

func TestWANAuthRejectRememberedBlocksReplacementUntilEraseAndJoinsShutdown(t *testing.T) {
	store := &authTestStore{record: savedCredential{Version: 1, ClientID: "wan-own-app", Access: "rejected-fake-token"}, eraseIn: make(chan struct{}), eraseOut: make(chan struct{})}
	entered, release := store.eraseIn, store.eraseOut
	a, _ := fakeAuth(t, store, func(*http.Request) (string, error) {
		t.Error("rejected-token cleanup attempted OAuth network I/O")
		return "", errors.New("unexpected")
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	if !a.Reject("rejected-fake-token") {
		t.Fatal("remembered service-refused token was not rejected")
	}
	waitDone(t, entered)
	if a.Status().State != "error" {
		t.Fatal("held secure erase blocked cached auth status")
	}
	if _, err := a.Start(false); err == nil {
		t.Fatal("replacement started before rejected remembered credential was erased")
	}
	joined := make(chan struct{})
	go func() { a.CancelJoin(); close(joined) }()
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_out" })
	select {
	case <-joined:
		t.Fatal("owner shutdown did not join rejected credential erasure")
	default:
	}
	once.Do(func() { close(release) })
	waitDone(t, joined)
	if record, _, erased := store.snapshot(); record.Access != "" || erased != 1 {
		t.Fatal("a restart could restore the rejected remembered credential")
	}
}

func TestWANAuthRejectedEraseCannotOverwriteLogoutOrEraseReplacement(t *testing.T) {
	store := &authTestStore{record: savedCredential{Version: 1, ClientID: "wan-own-app", Access: "rejected-fake-token"}, eraseIn: make(chan struct{}), eraseOut: make(chan struct{})}
	entered, release := store.eraseIn, store.eraseOut
	a, clock := fakeAuth(t, store, func(r *http.Request) (string, error) {
		if r.URL.Path == "/login/device/code" {
			return deviceResponse(), nil
		}
		return `{"access_token":"replacement-fake-token","token_type":"bearer"}`, nil
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	if !a.Reject("rejected-fake-token") {
		t.Fatal("matching remembered credential was not rejected")
	}
	waitDone(t, entered)
	a.Logout()
	if a.Status().State != "signing_out" {
		t.Fatal("held rejected-token erase blocked immediate logout")
	}
	erased := make(chan error, 1)
	go func() { erased <- a.Erase() }()
	once.Do(func() { close(release) })
	select {
	case err := <-erased:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("logout did not join rejected-token cleanup")
	}
	startAuth(t, a, true)
	step := nextAuthWait(t, clock, 5*time.Second)
	close(step.release)
	awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_in" })
	a.workers.Wait()
	if a.Reject("rejected-fake-token") {
		t.Fatal("late helper refusal matched a replacement credential")
	}
	if token, err := a.token(); err != nil || token != "replacement-fake-token" {
		t.Fatal("rejection cleanup invalidated the replacement login")
	}
	if record, _, _ := store.snapshot(); record.Access != "replacement-fake-token" {
		t.Fatal("stale rejected-token erase deleted the newly remembered credential")
	}
}

func TestWANAuthRejectedEraseFailureStaysClosedUntilExplicitLogoutRetry(t *testing.T) {
	store := &authTestStore{record: savedCredential{Version: 1, ClientID: "wan-own-app", Access: "rejected-fake-token"}, eraseErr: errors.New("SECRET native erase")}
	a, _ := fakeAuth(t, store, func(*http.Request) (string, error) { return deviceResponse(), nil })
	if !a.Reject("rejected-fake-token") {
		t.Fatal("matching remembered credential was not rejected")
	}
	a.workers.Wait()
	status := a.Status()
	if status.State != "error" || !strings.Contains(status.Error, "retry sign out") || strings.Contains(status.Error, "SECRET") {
		t.Fatalf("failed rejection cleanup did not remain safely blocked: %#v", status)
	}
	if _, err := a.Start(false); err == nil {
		t.Fatal("failed rejected-token erase reopened sign-in")
	}
	store.mu.Lock()
	store.eraseErr = nil
	store.mu.Unlock()
	a.Logout()
	if err := a.Erase(); err != nil || a.Status().State != "signed_out" {
		t.Fatal("explicit logout retry did not clear rejected saved credential")
	}
}

func TestWANAuthMemoryFallbackIsExplicitAndRemovesOldRememberedLogin(t *testing.T) {
	a, _ := fakeAuth(t, nil, func(*http.Request) (string, error) {
		t.Error("unsupported Remember request made a network request")
		return "", errors.New("unexpected")
	})
	if _, err := a.Start(true); !errors.Is(err, errVaultUnavailable) || a.Status().State != "signed_out" {
		t.Fatal("Remember silently downgraded to memory-only")
	}
	store := &authTestStore{record: savedCredential{Version: 1, ClientID: "wan-own-app", Access: "old-remembered"}}
	b, clock := fakeAuth(t, store, func(*http.Request) (string, error) { return deviceResponse(), nil })
	status := startAuth(t, b, false)
	step := nextAuthWait(t, clock, 5*time.Second)
	if record, _, erased := store.snapshot(); record.Access != "" || erased != 1 {
		t.Fatal("memory-only replacement left an older remembered login restorable")
	}
	b.Cancel(status.AttemptID)
	close(step.release)
}
