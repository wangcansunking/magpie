package wan

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
)

// Account identifies the authenticated service user, never an API credential.
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// Usage retains the last successful measurement on failed reads of the same
// credential. State distinguishes unknown/stale data from a real zero usage.
type Usage struct {
	State     string    `json:"state"`
	Account   Account   `json:"account"`
	Used      uint64    `json:"used"`
	Limit     *uint64   `json:"limit"`
	ResetAt   time.Time `json:"resetAt,omitzero"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
}

type serviceClient struct {
	client          *http.Client
	github, service string
}

func newServiceClient() serviceClient {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = netproxy.Func
	return serviceClient{client: &http.Client{Transport: netproxy.Dispatch(t), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, github: "https://api.github.com", service: "https://global.rel.tunnels.api.visualstudio.com"}
}

func (c serviceClient) get(ctx context.Context, address, authorization string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("WAN service request could not be prepared")
	}
	r.Header.Set("Authorization", authorization)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "magpie-wan/1")
	resp, err := c.client.Do(r)
	if err != nil {
		return errors.New("Could not read Dev Tunnels usage; check network and proxy settings")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case 401:
			return errors.New("Dev Tunnels could not verify this sign-in; usage is unavailable")
		case 403:
			return errors.New("Dev Tunnels refused access to account usage")
		case 429:
			return errors.New("Dev Tunnels usage requests are rate limited; automatic checks will retry")
		default:
			return errors.New("Dev Tunnels usage is temporarily unavailable")
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(b) > 64<<10 || json.Unmarshal(b, out) != nil {
		return errors.New("Dev Tunnels returned unreadable account information")
	}
	return ctx.Err()
}

func (c serviceClient) account(ctx context.Context, token string, identity ...Account) (Account, error) {
	if len(identity) > 0 && identity[0].Provider == "microsoft" {
		if !validMicrosoftAccount(identity[0]) {
			return Account{}, errors.New("Could not identify the WAN account; sign in again")
		}
		return identity[0], nil
	}
	var user struct {
		ID    uint64 `json:"id"`
		Login string `json:"login"`
	}
	if err := c.get(ctx, c.github+"/user", "Bearer "+token, &user); err != nil {
		return Account{}, err
	}
	if user.ID == 0 || !oauthText(user.Login, 100) {
		return Account{}, errors.New("Could not identify the WAN account; the saved tunnel was not replaced")
	}
	return Account{ID: "github:" + strconv.FormatUint(user.ID, 10), Name: user.Login, Provider: "github"}, nil
}

func (c serviceClient) usage(ctx context.Context, token string, identity ...Account) (Usage, error) {
	a, err := c.account(ctx, token, identity...)
	if err != nil {
		return Usage{}, err
	}
	scheme := "github "
	if a.Provider == "microsoft" {
		scheme = "Bearer "
	}
	var rates []struct {
		Name    string  `json:"name"`
		Current *uint64 `json:"current"`
		Limit   *uint64 `json:"limit"`
		Reset   *int64  `json:"resetTime"`
	}
	if err := c.get(ctx, c.service+"/userlimits?api-version=2023-09-27-preview", scheme+token, &rates); err != nil {
		return Usage{Account: a}, err
	}
	var found *Usage
	for _, r := range rates {
		if r.Name != "BandwidthPerUser" {
			continue
		}
		if found != nil || r.Current == nil {
			return Usage{Account: a}, errors.New("Dev Tunnels returned unreadable account information")
		}
		u := Usage{State: "ready", Account: a, Used: *r.Current, Limit: r.Limit, CheckedAt: time.Now().UTC()}
		if r.Reset != nil && *r.Reset != 0 {
			if *r.Reset < 1577836800 || *r.Reset > 253402300799 {
				return Usage{Account: a}, errors.New("Dev Tunnels returned unreadable account information")
			}
			u.ResetAt = time.Unix(*r.Reset, 0).UTC()
		}
		found = &u
	}
	if found == nil {
		return Usage{Account: a}, errors.New("Dev Tunnels did not report a bandwidth allowance")
	}
	return *found, nil
}

type usageMonitor struct {
	mu         sync.Mutex
	value      Usage
	credential [32]byte
	request    chan struct{}
	startedAt  time.Time
	fetch      func(context.Context, string) (Usage, error)
}

func (u *usageMonitor) refresh() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if time.Since(u.startedAt) < 15*time.Second {
		return false
	}
	select {
	case u.request <- struct{}{}:
	default:
	}
	return true
}

func (u *usageMonitor) status(a *auth) Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	u.mu.Lock()
	defer u.mu.Unlock()
	if a.closed || a.blocked || a.status.State != "signed_in" || a.credential.Access == "" {
		return Usage{State: "signed_out"}
	}
	if u.credential != sha256.Sum256([]byte(a.credential.Access)) {
		return Usage{State: "loading"}
	}
	out := u.value
	if out.Limit != nil {
		limit := *out.Limit
		out.Limit = &limit
	}
	return out
}

// A single owner worker coalesces manual checks and joins every request before
// shutdown. Credential changes cancel/discard old-account results immediately.
func (u *usageMonitor) run(ctx context.Context, a *auth) {
	for ctx.Err() == nil {
		token, _, changed, err := a.credentialWatch()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		hash := sha256.Sum256([]byte(token))
		u.mu.Lock()
		u.startedAt = time.Now()
		if u.credential != hash {
			u.credential, u.value = hash, Usage{State: "loading"}
		}
		u.mu.Unlock()
		request, cancel := context.WithTimeout(ctx, 20*time.Second)
		type result struct {
			value Usage
			err   error
		}
		done := make(chan result, 1)
		go func() { v, e := u.fetch(request, token); done <- result{v, e} }()
		var got result
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case <-changed:
			cancel()
			<-done
			continue
		case got = <-done:
		}
		cancel()
		u.mu.Lock()
		if got.err == nil {
			u.value = got.value
		} else {
			if u.value.CheckedAt.IsZero() {
				u.value = Usage{State: "unavailable", Account: got.value.Account}
			} else {
				u.value.State = "stale"
			}
			u.value.Error = got.err.Error()
		}
		u.mu.Unlock()
		timer := time.NewTimer(time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
		case <-u.request:
			timer.Stop()
		case <-timer.C:
		}
	}
}
