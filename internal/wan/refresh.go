package wan

import (
	"context"
	"errors"
	"time"
)

func rotatingCredential(clientID string, grant githubGrant, issued time.Time) savedCredential {
	life := time.Duration(grant.ExpiresIn) * time.Second
	lead := min(5*time.Minute, life/4)
	return savedCredential{Version: 2, ClientID: clientID, Access: grant.Access,
		ExpiresAt: issued.Add(life), Refresh: grant.Refresh,
		RefreshExpiresAt: issued.Add(time.Duration(grant.RefreshExpiresIn) * time.Second),
		RefreshAfter:     issued.Add(life - lead)}
}

func (a *auth) signalLocked() {
	if a.changed != nil {
		close(a.changed)
	}
	a.changed = make(chan struct{})
}

func (a *auth) stopRefreshLocked() {
	if a.refreshCancel != nil {
		a.refreshCancel()
		a.refreshCancel = nil
	}
	a.renewal = AuthRenewal{}
}

func (a *auth) startRefreshLocked() {
	a.stopRefreshLocked()
	if a.closed || a.blocked || a.owner.Err() != nil || a.credential.Refresh == "" {
		return
	}
	ctx, cancel := context.WithCancel(a.owner)
	a.refreshCancel = cancel
	record, remember := a.credential, a.status.Storage == "system"
	next := record.RefreshAfter
	if next.Before(a.now()) {
		next = a.now()
	}
	a.renewal = AuthRenewal{State: "scheduled", NextAttemptAt: next}
	a.workers.Add(1)
	go a.refreshLoop(ctx, record, remember)
}

func (a *auth) refreshActiveLocked(ctx context.Context, record savedCredential) bool {
	return ctx.Err() == nil && !a.closed && !a.blocked && a.owner.Err() == nil &&
		a.credential.Access == record.Access && a.credential.Refresh == record.Refresh
}

func (a *auth) publishRenewal(ctx context.Context, record savedCredential, state string, next time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.refreshActiveLocked(ctx, record) {
		return false
	}
	a.renewal = AuthRenewal{State: state, NextAttemptAt: next}
	return true
}

func (a *auth) refreshLoop(ctx context.Context, record savedCredential, remember bool) {
	defer a.workers.Done()
	delay := max(time.Duration(0), record.RefreshAfter.Sub(a.now()))
	var retry time.Duration
	for {
		if err := a.refreshWait(ctx, delay); err != nil {
			return
		}
		if !record.RefreshExpiresAt.IsZero() && !a.now().Before(record.RefreshExpiresAt) {
			a.refreshRejected(ctx, record)
			return
		}
		issued := a.now()
		if !a.publishRenewal(ctx, record, "refreshing", time.Time{}) {
			return
		}
		var grant githubGrant
		var err error
		if record.Provider == "microsoft" {
			grant, err = a.microsoft.refresh(ctx, record.Refresh)
		} else {
			grant, err = a.github.refresh(ctx, a.clientID, record.Refresh)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errRefreshRejected) {
				a.refreshRejected(ctx, record)
				return
			}
			// Bounded retries retain the last access deadline. Hosting still
			// closes if that deadline passes; network trouble never extends it.
			retry = min(5*time.Minute, max(30*time.Second, retry*2))
			delay = retry
			if !record.RefreshExpiresAt.IsZero() {
				delay = min(retry, max(time.Duration(0), record.RefreshExpiresAt.Sub(a.now())))
			}
			if !a.publishRenewal(ctx, record, "retrying", a.now().Add(delay)) {
				return
			}
			continue
		}
		next := rotatingCredential(a.clientID, grant, issued)
		if record.Provider == "microsoft" {
			next = microsoftCredential(grant, issued)
		}
		if !a.commitRefresh(ctx, record, next, remember) {
			return
		}
		record = next
		retry = 0
		delay = max(time.Duration(0), record.RefreshAfter.Sub(a.now()))
	}
}

func (a *auth) commitRefresh(ctx context.Context, old, next savedCredential, remember bool) bool {
	a.commit.Lock()
	defer a.commit.Unlock()
	a.mu.Lock()
	active := a.refreshActiveLocked(ctx, old)
	a.mu.Unlock()
	if !active {
		return false
	}
	if remember {
		if err := a.store.save(next); err != nil {
			// The rotation already invalidated both old tokens. A partial
			// vault write must not be mistaken for a safely retained session.
			if err := a.store.delete(); err != nil {
				a.storageFailed()
			} else {
				a.mu.Lock()
				a.blocked = true
				a.stopRefreshLocked()
				a.credential = savedCredential{}
				a.status.State, a.status.Error = "error", "Could not safely save WAN sign-in; sign out and retry, or use a memory-only session"
				a.signalLocked()
				a.mu.Unlock()
			}
			return false
		}
	}
	a.mu.Lock()
	active = a.refreshActiveLocked(ctx, old)
	if active {
		a.credential = next
		a.renewal = AuthRenewal{State: "scheduled", NextAttemptAt: next.RefreshAfter}
		a.status.State, a.status.Error, a.status.ExpiresAt = "signed_in", "", next.ExpiresAt
		a.signalLocked()
	}
	a.mu.Unlock()
	if !active && remember {
		if err := a.store.delete(); err != nil {
			a.storageFailed()
		}
	}
	return active
}

func (a *auth) refreshRejected(ctx context.Context, record savedCredential) {
	a.mu.Lock()
	active := a.refreshActiveLocked(ctx, record)
	a.mu.Unlock()
	if active {
		a.Reject(record.Access)
	}
}

// credentialWatch atomically observes the snapshot and its change notification.
// A refresh between separate reads cannot leave hosting on an obsolete token.
func (a *auth) credentialWatch() (string, time.Time, <-chan struct{}, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked()
	if a.closed || a.blocked || a.owner.Err() != nil || a.status.State != "signed_in" || a.credential.Access == "" {
		return "", time.Time{}, a.changed, errors.New("Sign in to GitHub for WAN before starting the gateway")
	}
	return a.credential.Access, a.credential.ExpiresAt, a.changed, nil
}

// hostToken joins an already-due renewal before preparing a tunnel. A restored
// grant must not be rotated out from under the helper's initial management call.
func (a *auth) hostToken(ctx context.Context) (string, time.Time, error) {
	for {
		a.mu.Lock()
		a.expireLocked()
		record, changed := a.credential, a.changed
		pending := !a.closed && !a.blocked && a.owner.Err() == nil && record.Refresh != "" && !a.now().Before(record.RefreshAfter) &&
			(a.status.State == "signed_in" || a.status.State == "expired" || a.status.State == "loading")
		a.mu.Unlock()
		if !pending {
			return a.tokenSnapshot()
		}
		select {
		case <-ctx.Done():
			return "", time.Time{}, ctx.Err()
		case <-changed:
		}
	}
}
