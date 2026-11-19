package wan

import (
	"context"
	"errors"
	"sync"
	"time"
)

type sdkCredential struct {
	Version      int    `json:"version"`
	Generation   string `json:"generation"`
	Op           string `json:"op"`
	Token        string `json:"token"`
	AuthProvider string `json:"authProvider,omitempty"`
	Revision     uint64 `json:"revision"`
}

// A lease extends only after the helper acknowledges the new GitHub and host
// credentials. It owns its writer and deadline; Stop cancels and joins both.
type credentialLease struct {
	ready  chan error
	failed chan error
	done   chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	last   string
}

func (l *credentialLease) stop() string {
	l.cancel()
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func keepCredentials(owner context.Context, a *auth, child *sdkProcess, generation, token string, expires time.Time, revoke func()) *credentialLease {
	ctx, cancel := context.WithCancel(owner)
	l := &credentialLease{ready: make(chan error, 1), failed: make(chan error, 1), done: make(chan struct{}), cancel: cancel, last: token}
	go func() {
		defer close(l.done)
		initial := true
		fail := func(err error) {
			revoke()
			if initial {
				l.ready <- err
			}
			l.failed <- err
		}
		var revision uint64
		for {
			next, deadline, changed, err := a.credentialWatch()
			if err != nil {
				status := a.Status()
				if status.Renewal != nil && status.Renewal.State != "required" {
					fail(errors.New("Credentials expired before WAN renewal; start WAN again after sign-in recovers"))
				} else {
					fail(errors.New("WAN sign-in expired; sign in again"))
				}
				return
			}
			if next != token {
				revision++
				ack, err := child.output.credentials(revision)
				if err != nil {
					fail(err)
					return
				}
				until := time.Now().Add(30 * time.Second)
				if !expires.IsZero() && expires.Before(until) {
					until = expires
				}
				if !deadline.IsZero() && deadline.Before(until) {
					until = deadline
				}
				update, finish := context.WithDeadline(ctx, until)
				l.mu.Lock()
				l.last = next
				l.mu.Unlock()
				err = child.send(update, sdkCredential{Version: 1, Generation: generation, Op: "credentials", Token: next, AuthProvider: a.Status().Provider, Revision: revision})
				if err == nil {
					select {
					case <-ack:
					case <-child.output.bad:
						err = child.output.failure()
					case <-update.Done():
						err = sdkError("timeout")
					}
				}
				finish()
				if err != nil {
					fail(err)
					return
				}
				token = next
			}
			expires = deadline
			if initial {
				l.ready <- nil
				initial = false
			}
			var elapsed <-chan time.Time
			var timer *time.Timer
			if !expires.IsZero() {
				timer = time.NewTimer(max(time.Duration(0), expires.Sub(a.now())))
				elapsed = timer.C
			}
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-changed:
			case <-elapsed:
			}
			if timer != nil {
				timer.Stop()
			}
		}
	}()
	return l
}
