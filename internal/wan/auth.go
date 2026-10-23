package wan

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

// AuthStatus is a cached, credential-free snapshot. ExpiresAt is the device-code
// deadline while authorizing, then the access-token deadline when signed in.
// A zero signed-in deadline means GitHub issued a non-expiring access token.
// Storage describes this session; StorageAvailable separately enables Remember.
type AuthStatus struct {
	State            string       `json:"state"`
	Provider         string       `json:"provider,omitempty"`
	Error            string       `json:"error"`
	UserCode         string       `json:"userCode"`
	VerificationURI  string       `json:"verificationURI"`
	ExpiresAt        time.Time    `json:"expiresAt"`
	AttemptID        uint64       `json:"attemptID"`
	Storage          string       `json:"storage"`
	StorageAvailable bool         `json:"storageAvailable"`
	Renewal          *AuthRenewal `json:"renewal,omitempty"`
}

// AuthRenewal describes the owner's actual scheduled work, not a browser's
// estimate from access expiry. Transient retries keep their own next attempt.
type AuthRenewal struct {
	State         string    `json:"state"`
	NextAttemptAt time.Time `json:"nextAttemptAt,omitzero"`
}

type authAttempt struct {
	id       uint64
	ctx      context.Context
	cancel   context.CancelFunc
	remember bool
	provider string
}

// auth has an immutable owner and application identity. mu guards cached state
// only; commit serializes vault I/O without holding mu, so Status and Logout
// remain responsive when the OS vault is stalled. Every commit rechecks its
// generation, and a canceled write is erased before releasing the commit gate.
type auth struct {
	mu            sync.Mutex
	commit        sync.Mutex
	workers       sync.WaitGroup
	owner         context.Context
	cancel        context.CancelFunc
	clientID      string
	store         credentialStore
	status        AuthStatus
	credential    savedCredential
	current       *authAttempt
	refreshCancel context.CancelFunc
	renewal       AuthRenewal
	changed       chan struct{}
	refreshWait   func(context.Context, time.Duration) error
	rejection     uint64 // owns pending erasure of a rejected remembered credential
	next          uint64
	closed        bool
	blocked       bool
	github        githubDeviceClient
	microsoft     microsoftDeviceClient
	now           func() time.Time
	wait          func(context.Context, time.Duration) error
	deadline      func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

// newAuth does not request a code, borrow another login, install a runtime or
// publish anything. Only an available, WAN-specific OS store is read, once, in
// the background. A nil store selects the platform store (memory-only in tests).
func newAuth(owner context.Context, clientID string, store credentialStore) *auth {
	if owner == nil {
		owner, cancel := context.WithCancel(context.Background())
		cancel()
		return newAuth(owner, clientID, store)
	}
	if store == nil {
		store = defaultCredentialStore(settings.Dir())
	}
	ctx, cancel := context.WithCancel(owner)
	a := &auth{
		owner: ctx, cancel: cancel, clientID: clientID, store: store,
		github: newGitHubDeviceClient(), microsoft: newMicrosoftDeviceClient(), now: time.Now, changed: make(chan struct{}), refreshWait: waitAuth,
		wait: waitAuth, deadline: context.WithTimeout,
		status: AuthStatus{State: "signed_out", Storage: "memory", StorageAvailable: store.available()},
	}
	if store.available() && ctx.Err() == nil {
		a.status.State = "loading"
		a.workers.Add(1)
		go a.restore()
	}
	return a
}

func waitAuth(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func (a *auth) restore() {
	defer a.workers.Done()
	a.commit.Lock()
	defer a.commit.Unlock()
	record, err := a.store.load()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.owner.Err() != nil || a.blocked || a.status.State != "loading" {
		return
	}
	if errors.Is(err, errCredentialMissing) {
		a.status.State = "signed_out"
		return
	}
	if err != nil || !validSavedCredential(record) || (record.ClientID != a.clientID && !(record.Provider == "microsoft" && record.ClientID == microsoftClientID)) {
		a.blocked = true // An unknown saved record is not an empty store.
		a.status.State, a.status.Error = "error", "Could not read saved WAN sign-in for this application; sign out before signing in again"
		return
	}
	a.status.Provider = record.Provider
	a.status.Storage = "system"
	a.status.ExpiresAt = record.ExpiresAt
	if !record.ExpiresAt.IsZero() && !a.now().Before(record.ExpiresAt) && record.Refresh == "" {
		a.status.State, a.status.Error = "expired", "WAN sign-in expired; sign in again"
		return
	}
	a.credential = record
	a.status.State = "signed_in"
	if !record.ExpiresAt.IsZero() && !a.now().Before(record.ExpiresAt) {
		a.status.State = "loading"
	}
	a.signalLocked()
	a.startRefreshLocked()
}

func (a *auth) expireLocked() {
	if a.status.State == "signed_in" && !a.credential.ExpiresAt.IsZero() && !a.now().Before(a.credential.ExpiresAt) {
		if a.credential.Refresh == "" {
			a.credential = savedCredential{}
			a.status.State, a.status.Error = "expired", "WAN sign-in expired; sign in again"
		} else {
			a.status.State, a.status.Error = "loading", ""
		}
		a.signalLocked()
	}
}

// Status performs no network, disk or native-vault work and never waits on the
// commit gate. Reading it cannot revive a canceled attempt or publish a tunnel.
func (a *auth) Status() AuthStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked()
	out := a.status
	if !a.closed && a.renewal.State != "" {
		renewal := a.renewal
		out.Renewal = &renewal
	}
	return out
}

// Start registers the attempt before any request and returns immediately. The
// owner, not an HTTP request or browser page, owns its cancellation. Replacing an
// attempt also removes the old cached token; it cannot remain usable meanwhile.
func (a *auth) Start(remember bool, providers ...string) (AuthStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.owner.Err() != nil {
		return a.status, errors.New("WAN authorization owner is not running")
	}
	if a.blocked || a.status.State == "loading" {
		return a.status, errors.New("WAN sign-in is loading or signing out; finish that operation first")
	}
	provider := "github"
	if len(providers) > 0 && providers[0] != "" {
		provider = providers[0]
	}
	if provider != "github" && provider != "microsoft" {
		return a.status, errors.New("Unsupported WAN login provider")
	}
	if !oauthText(a.clientID, 128) {
		return a.status, errors.New("A valid WAN GitHub OAuth Client ID is required")
	}
	if remember && !a.status.StorageAvailable {
		return a.status, errVaultUnavailable
	}
	a.stopRefreshLocked()
	if a.current != nil {
		a.current.cancel()
	}
	a.next++
	ctx, cancel := context.WithCancel(a.owner)
	r := &authAttempt{id: a.next, ctx: ctx, cancel: cancel, remember: remember, provider: provider}
	a.current, a.credential = r, savedCredential{}
	a.signalLocked()
	a.status = AuthStatus{State: "requesting_code", Provider: provider, AttemptID: r.id, Storage: "memory", StorageAvailable: a.status.StorageAvailable}
	if remember {
		a.status.Storage = "system"
	}
	a.workers.Add(1) // Under mu, before CancelJoin closes the start gate.
	go a.run(r)
	return a.status, nil
}

func (a *auth) activeLocked(r *authAttempt) bool {
	return !a.closed && !a.blocked && a.current == r && r.ctx.Err() == nil && a.owner.Err() == nil
}

func (a *auth) active(r *authAttempt) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activeLocked(r)
}

func (a *auth) run(r *authAttempt) {
	defer a.workers.Done()
	defer r.cancel()
	defer func() {
		a.mu.Lock()
		if a.current == r {
			a.current = nil
		}
		a.mu.Unlock()
	}()
	// An explicit new sign-in replaces the remembered account, including when
	// the user chooses memory-only. A later restart must not restore the old one.
	a.commit.Lock()
	if !a.active(r) {
		a.commit.Unlock()
		return
	}
	err := a.store.delete()
	a.commit.Unlock()
	if err != nil {
		a.fail(r, "Could not erase the previous WAN sign-in; sign out and retry", true)
		return
	}
	if !a.active(r) {
		return
	}
	requested := a.now()
	var code githubDeviceCode
	if r.provider == "microsoft" {
		code, err = a.microsoft.code(r.ctx)
	} else {
		code, err = a.github.code(r.ctx, a.clientID, "")
	}
	if err != nil {
		a.requestFailed(r, err)
		return
	}
	expires := requested.Add(time.Duration(code.ExpiresIn) * time.Second)
	remaining := expires.Sub(a.now())
	if remaining <= 0 {
		a.fail(r, "GitHub device code for WAN expired; start sign-in again", false)
		return
	}
	ctx, cancel := a.deadline(r.ctx, remaining)
	defer cancel()
	a.mu.Lock()
	if !a.activeLocked(r) {
		a.mu.Unlock()
		return
	}
	a.status.State, a.status.UserCode = "awaiting_authorization", code.UserCode
	a.status.VerificationURI, a.status.ExpiresAt = code.VerificationURI, expires
	a.mu.Unlock()
	interval := time.Duration(code.Interval) * time.Second
	for {
		if err := a.wait(ctx, interval); err != nil {
			if r.ctx.Err() == nil {
				a.fail(r, "GitHub device code for WAN expired; start sign-in again", false)
			}
			return
		}
		if !a.active(r) {
			return
		}
		if !a.now().Before(expires) {
			a.fail(r, "GitHub device code for WAN expired; start sign-in again", false)
			return
		}
		polled := a.now()
		var grant githubGrant
		var err error
		if r.provider == "microsoft" {
			grant, err = a.microsoft.poll(ctx, code.DeviceCode)
		} else {
			grant, err = a.github.poll(ctx, a.clientID, code.DeviceCode)
		}
		if err != nil {
			if ctx.Err() != nil && r.ctx.Err() == nil {
				a.fail(r, "GitHub device code for WAN expired; start sign-in again", false)
			} else {
				a.requestFailed(r, err)
			}
			return
		}
		if !a.now().Before(expires) || ctx.Err() != nil {
			a.fail(r, "GitHub device code for WAN expired; start sign-in again", false)
			return
		}
		switch grant.Error {
		case "authorization_pending":
			if next := time.Duration(grant.Interval) * time.Second; next > interval {
				interval = next
			}
			continue
		case "slow_down":
			interval += 5 * time.Second // RFC 8628: add five for all later polls.
			if next := time.Duration(grant.Interval) * time.Second; next > interval {
				interval = next
			}
			continue
		}
		// Do not multiply an unbounded vendor integer into time.Duration.
		if grant.ExpiresIn > 366*24*60*60 {
			a.fail(r, "GitHub returned an unsupported WAN token lifetime", false)
			return
		}
		record := savedCredential{Version: 1, ClientID: a.clientID, Access: grant.Access}
		if grant.ExpiresIn > 0 {
			record.ExpiresAt = polled.Add(time.Duration(grant.ExpiresIn) * time.Second)
		}
		if grant.Refresh != "" && grant.ExpiresIn > 0 && grant.RefreshExpiresIn > grant.ExpiresIn && grant.RefreshExpiresIn <= 2*366*24*60*60 {
			record = rotatingCredential(a.clientID, grant, polled)
		}
		if r.provider == "microsoft" {
			record = microsoftCredential(grant, polled)
		}
		a.accept(r, record)
		return
	}
}

func (a *auth) requestFailed(r *authAttempt, err error) {
	if r.ctx.Err() != nil {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		a.fail(r, "GitHub WAN authorization request timed out; try again", false)
		return
	}
	// githubDeviceClient returns only bounded, fixed messages, never a response
	// body, URL, access token or transport's original error.
	a.fail(r, err.Error(), false)
}

func (a *auth) fail(r *authAttempt, message string, blocked bool) {
	if r.provider == "microsoft" {
		message = strings.ReplaceAll(message, "GitHub", "Microsoft")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeLocked(r) {
		a.current = nil
		a.status.State, a.status.Error = "error", message
		a.status.UserCode, a.status.VerificationURI = "", ""
		a.status.ExpiresAt = time.Time{}
		a.credential = savedCredential{}
		a.blocked = blocked
	}
}

func (a *auth) accept(r *authAttempt, record savedCredential) {
	a.commit.Lock()
	defer a.commit.Unlock()
	if !a.active(r) {
		return
	}
	if r.remember {
		if err := a.store.save(record); err != nil {
			// A failed OS write is unknown, not proof it wrote nothing. Even a
			// canceled attempt must block later sign-ins if cleanup also fails.
			if err := a.store.delete(); err != nil {
				a.storageFailed()
			} else {
				a.fail(r, "Could not safely save WAN sign-in; sign out and retry, or use a memory-only session", true)
			}
			return
		}
	}
	a.mu.Lock()
	active := a.activeLocked(r)
	expired := !record.ExpiresAt.IsZero() && !a.now().Before(record.ExpiresAt)
	if !active || expired {
		if active && expired {
			a.current = nil
			a.status.State, a.status.Error = "expired", "WAN sign-in expired; sign in again"
			a.status.UserCode, a.status.VerificationURI = "", ""
			a.status.ExpiresAt = record.ExpiresAt
		}
		a.mu.Unlock()
		if r.remember {
			if err := a.store.delete(); err != nil {
				a.storageFailed()
			}
		}
		return
	}
	a.current = nil // A completed login is not a cancelable browser attempt.
	a.credential = record
	a.status.State, a.status.Error = "signed_in", ""
	a.status.UserCode, a.status.VerificationURI = "", ""
	a.status.ExpiresAt = record.ExpiresAt
	a.signalLocked()
	a.startRefreshLocked()
	a.mu.Unlock()
}

// storageFailed blocks later sign-ins after an unerasable late write. It never
// changes the current generation's status to signed in, or exposes its token.
func (a *auth) storageFailed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.blocked = true
	a.stopRefreshLocked()
	a.signalLocked()
	a.credential = savedCredential{}
	a.status.State, a.status.Error = "error", "Could not erase saved WAN sign-in; retry sign out"
	a.status.UserCode, a.status.VerificationURI = "", ""
}

// Cancel acts only on the matching in-progress attempt. It invalidates cached
// state before waiting for a possible vault write, then joins that commit barrier.
func (a *auth) Cancel(attemptID uint64) bool {
	a.mu.Lock()
	if a.current == nil || a.current.id != attemptID || a.blocked {
		a.mu.Unlock()
		return false
	}
	r := a.current
	a.current = nil
	r.cancel()
	a.credential = savedCredential{}
	a.status = AuthStatus{State: "signed_out", AttemptID: r.id, Storage: "memory", StorageAvailable: a.status.StorageAvailable}
	a.mu.Unlock()
	a.commit.Lock()
	a.commit.Unlock()
	return true
}

// Reject invalidates only the credential supplied to this helper generation.
// A service authentication refusal is not a denial of tunnel permissions: the
// caller must invoke this only for the helper's typed authentication failure.
// Remembered credentials are erased by an owner-tracked worker before another
// sign-in can start, so a restart cannot restore the token just rejected.
func (a *auth) Reject(snapshotToken string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.blocked || a.owner.Err() != nil || (a.status.State != "signed_in" && a.status.State != "expired" && !(a.status.State == "loading" && a.credential.Refresh != "")) || snapshotToken == "" || a.credential.Access != snapshotToken {
		return false
	}
	renewable := a.credential.Refresh != ""
	a.stopRefreshLocked()
	if renewable {
		a.renewal = AuthRenewal{State: "required"}
	}
	a.signalLocked()
	remembered := a.status.Storage == "system"
	a.credential = savedCredential{}
	a.status.State, a.status.Error = "error", "GitHub refused WAN sign-in; sign in again"
	a.status.UserCode, a.status.VerificationURI = "", ""
	a.status.ExpiresAt = time.Time{}
	if remembered {
		a.next++
		id := a.next
		a.rejection, a.blocked = id, true
		a.workers.Add(1) // Under mu, before CancelJoin closes the owner gate.
		go a.eraseRejected(id)
	}
	return true
}

func (a *auth) eraseRejected(id uint64) {
	defer a.workers.Done()
	a.commit.Lock()
	defer a.commit.Unlock()
	a.mu.Lock()
	if a.rejection != id {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	// Do not skip this erase when the owner is canceled: it is precisely the
	// persisted rejected token that must not be restored on the next start.
	err := a.store.delete()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rejection != id {
		return // Logout's own Erase now owns the visible result and storage gate.
	}
	a.rejection = 0
	if err != nil {
		a.blocked = true
		a.status.State, a.status.Error = "error", "Could not erase rejected WAN sign-in; retry sign out"
		return
	}
	a.blocked = false
}

// Logout immediately invalidates tokens and attempts. The manager must then stop
// and join hosting, then call Erase. Local logout does not revoke GitHub's grant.
func (a *auth) Logout() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopRefreshLocked()
	a.blocked = true
	a.signalLocked()
	a.rejection = 0 // Explicit logout takes over any pending rejected-token erase.
	if a.current != nil {
		a.current.cancel()
		a.current = nil
	}
	a.credential = savedCredential{}
	a.status = AuthStatus{State: "signing_out", Storage: "memory", StorageAvailable: a.status.StorageAvailable}
}

func (a *auth) Erase() error {
	a.commit.Lock()
	defer a.commit.Unlock()
	a.mu.Lock()
	if !a.blocked {
		a.mu.Unlock()
		return errors.New("Invalidate WAN sign-in before erasing its saved credential")
	}
	a.mu.Unlock()
	err := a.store.delete()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.blocked = true
		a.status.State, a.status.Error = "error", "Could not erase saved WAN sign-in; retry sign out"
		return errors.New(a.status.Error)
	}
	a.blocked = false
	a.rejection = 0 // A stale rejection worker cannot erase a later sign-in.
	a.status = AuthStatus{State: "signed_out", Storage: "memory", StorageAvailable: a.status.StorageAvailable}
	return nil
}

// tokenSnapshot returns access and its original deadline under the same lock.
// Separate reads could let an expiry between them look like a non-expiring token.
func (a *auth) tokenSnapshot() (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked()
	if a.closed || a.blocked || a.owner.Err() != nil || a.status.State != "signed_in" || a.credential.Access == "" {
		return "", time.Time{}, errors.New("Sign in to GitHub for WAN before starting the gateway")
	}
	return a.credential.Access, a.credential.ExpiresAt, nil
}

func (a *auth) token() (string, error) {
	token, _, err := a.tokenSnapshot()
	return token, err
}

func (a *auth) tokenExpires() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked()
	if a.closed || a.blocked || a.owner.Err() != nil || a.status.State != "signed_in" {
		return time.Time{}
	}
	return a.credential.ExpiresAt
}

// CancelJoin permanently closes the start gate and waits for every worker,
// including initial code requests, previous replaced attempts and idle restore.
// It preserves a successfully remembered login, but never a canceled late write.
func (a *auth) CancelJoin() {
	a.mu.Lock()
	a.closed = true
	a.signalLocked()
	a.cancel()
	if a.current != nil {
		a.current.cancel()
		a.current = nil
	}
	a.credential = savedCredential{}
	a.status = AuthStatus{State: "signed_out", Storage: "memory", StorageAvailable: a.status.StorageAvailable}
	a.mu.Unlock()
	a.workers.Wait()
}
