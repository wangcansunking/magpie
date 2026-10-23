// Package wan owns the opt-in public tunnel and its separate loopback listener.
// The caller must supply a handler that authenticates every WAN request.
package wan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
)

// DefaultGitHubClientID is the service application's GitHub identity documented
// by the official dev tunnels SDK. It is not a subscription provider's OAuth app.
const DefaultGitHubClientID = "Iv1.e7b89e013f801f03"

// Status is a cached, credential-free snapshot. Endpoint exists only while running.
type Status struct {
	State            string      `json:"state"`
	Endpoint         string      `json:"endpoint"`
	Error            string      `json:"error"`
	RuntimeAvailable bool        `json:"runtimeAvailable"`
	Auth             AuthStatus  `json:"auth"`
	Usage            Usage       `json:"usage"`
	Tunnel           *TunnelInfo `json:"tunnel,omitempty"`
}

// Manager is inert until the actual gateway attaches its lifetime with Own.
// Neither construction nor Status reads a credential, discovers a runtime,
// installs software, signs in or publishes a tunnel.
type Manager struct {
	mu              sync.Mutex
	status          Status
	current         *hosting
	handler         http.Handler
	path            string
	find            func(context.Context) (string, error)
	hasRuntime      func(context.Context) (bool, error)
	install         func(context.Context) (string, error)
	proxy           func(context.Context, func()) (*wanProxy, error)
	command         func(string, ...string) *exec.Cmd
	startTree       func(*exec.Cmd) (sdkTree, error)
	wait            time.Duration
	runtime         *tunnelRuntime
	owner           context.Context
	cancel          context.CancelFunc
	ownerDone       chan struct{}
	auth            *auth
	clientID        string
	store           credentialStore
	blocked         bool // logout/install operation; Start cannot race it
	closed          bool
	runtimeExplicit bool // an explicit start/install supersedes initial discovery
	actions         sync.WaitGroup
	background      sync.WaitGroup
	usage           *usageMonitor
	account         func(context.Context, string) (Account, error)
	tunnels         map[string]tunnelConfig
}

type hosting struct {
	ctx            context.Context
	cancel         context.CancelFunc
	requests       context.Context
	cancelRequests context.CancelFunc
	admitted       atomic.Bool
	done           chan struct{}
	ready          chan struct{}
	err            error // written before ready closes
}

func (r *hosting) revoke() {
	r.admitted.Store(false)
	if r.cancelRequests != nil {
		r.cancelRequests()
	}
}

func New(handler http.Handler) *Manager {
	service := newServiceClient()
	return &Manager{account: service.account, usage: &usageMonitor{request: make(chan struct{}, 1), fetch: service.usage},
		handler: handler, path: filepath.Join(settings.Dir(), "wan.json"),
		find: availableRuntime, hasRuntime: cachedRuntimeAvailable, install: plugin.Bun, proxy: startWANProxy, command: proc.Command, startTree: startSDKTree,
		wait: 90 * time.Second, runtime: &processTunnels, clientID: DefaultGitHubClientID,
		status: Status{State: "stopped", Auth: AuthStatus{State: "signed_out", Storage: "memory"}},
	}
}

// Own attaches exactly once, only after this process owns the gateway listener.
// An owned but idle manager is registered too: Shutdown must join device grants
// and vault restore work even when no hosting generation has ever started.
func (m *Manager) Own(ctx context.Context) error {
	m.runtime.mu.Lock()
	defer m.runtime.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runtime.shutdown != nil || m.closed || ctx == nil || ctx.Err() != nil {
		return errors.New("WAN gateway owner is not running")
	}
	if m.owner != nil {
		return errors.New("WAN gateway already has an owner")
	}
	m.owner, m.cancel = context.WithCancel(ctx)
	m.ownerDone = make(chan struct{})
	m.auth = newAuth(m.owner, m.clientID, m.store)
	m.actions.Add(1)
	go m.discoverRuntime()
	m.background.Add(1)
	go func() { defer m.background.Done(); m.usage.run(m.owner, m.auth) }()
	if m.runtime.owners == nil {
		m.runtime.owners = make(map[*Manager]struct{})
	}
	m.runtime.owners[m] = struct{}{}
	go m.joinOwner()
	return nil
}

func (m *Manager) discoverRuntime() {
	defer m.actions.Done()
	config, fresh, configErr := readTunnel(m.path)
	accounts, accountsErr := readTunnelAccounts(m.path)
	available, err := m.hasRuntime(m.owner)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed && m.owner.Err() == nil && err == nil && m.current == nil && !m.blocked && !m.runtimeExplicit {
		m.status.RuntimeAvailable = available
		if configErr == nil && !fresh {
			m.status.Tunnel = tunnelInfo(config)
		}
		if accountsErr == nil {
			m.tunnels = accounts
		}
	}
}

func (m *Manager) joinOwner() {
	<-m.owner.Done()
	m.mu.Lock()
	m.closed = true
	a := m.auth
	if m.current != nil {
		m.current.revoke()
		m.current.cancel()
	}
	m.mu.Unlock()
	// CancelJoin closes the auth gate immediately and joins all prior attempts.
	// No manager lock is held over either child or vault work.
	a.CancelJoin()
	m.Stop()
	m.actions.Wait()
	m.background.Wait()
	m.runtime.mu.Lock()
	delete(m.runtime.owners, m)
	m.runtime.mu.Unlock()
	close(m.ownerDone)
}

// Close joins gateway ownership, including authentication while hosting is idle.
func (m *Manager) Close() {
	m.mu.Lock()
	cancel, done := m.cancel, m.ownerDone
	m.closed = true
	if cancel != nil {
		cancel()
	}
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	status, a := m.status, m.auth
	if status.Tunnel != nil {
		copy := *status.Tunnel
		status.Tunnel = &copy
	}
	m.mu.Unlock()
	if a != nil {
		status.Auth = a.Status()
		status.Usage = m.usage.status(a)
		if status.Usage.Account.ID != "" {
			m.mu.Lock()
			selected, exists := m.tunnels[status.Usage.Account.ID]
			m.mu.Unlock()
			if exists {
				status.Tunnel = tunnelInfo(selected)
			} else if status.Tunnel == nil || status.Tunnel.AccountID != status.Usage.Account.ID {
				status.Tunnel = &TunnelInfo{AccountID: status.Usage.Account.ID, AccountName: status.Usage.Account.Name, New: true}
			}
		}
	}
	return status
}

func (m *Manager) StartAuth(remember bool, providers ...string) (AuthStatus, error) {
	m.runtime.mu.Lock()
	defer m.runtime.mu.Unlock()
	if m.runtime.shutdown != nil {
		return AuthStatus{}, errors.New("WAN authorization cannot start while magpie is shutting down")
	}
	m.mu.Lock()
	if m.auth == nil || m.closed || m.blocked || m.owner.Err() != nil {
		m.mu.Unlock()
		return AuthStatus{}, errors.New("WAN authorization owner is unavailable or busy")
	}
	defer m.mu.Unlock()
	if m.current != nil {
		return m.auth.Status(), errors.New("Stop WAN gateway before replacing its sign-in")
	}
	return m.auth.Start(remember, providers...)
}

func (m *Manager) CancelAuth(id uint64) bool {
	m.mu.Lock()
	if m.closed || m.auth == nil {
		m.mu.Unlock()
		return false
	}
	a := m.auth
	m.actions.Add(1)
	m.mu.Unlock()
	defer m.actions.Done()
	return a.Cancel(id)
}

func (m *Manager) Logout() error {
	m.mu.Lock()
	if m.auth == nil || m.blocked || m.closed || m.owner.Err() != nil {
		m.mu.Unlock()
		return errors.New("WAN sign-in is unavailable or busy")
	}
	m.blocked = true
	m.actions.Add(1)
	defer m.actions.Done()
	a := m.auth
	a.Logout() // Invalidate before stopping, without waiting on the vault.
	m.mu.Unlock()
	m.Stop()
	err := a.Erase()
	m.mu.Lock()
	m.blocked = false
	m.mu.Unlock()
	return err
}

// InstallRuntime is explicit; only this method may ask Bun to download. The
// gateway owner, never the browser's request, owns cancellation and shutdown.
func (m *Manager) InstallRuntime(owner context.Context) error {
	m.runtime.mu.Lock()
	if m.runtime.shutdown != nil {
		m.runtime.mu.Unlock()
		return errors.New("WAN runtime cannot install while magpie is shutting down")
	}
	m.mu.Lock()
	if m.owner == nil || m.closed || m.blocked || m.owner.Err() != nil || owner == nil || owner.Err() != nil {
		m.mu.Unlock()
		m.runtime.mu.Unlock()
		return errors.New("WAN runtime owner is unavailable or busy")
	}
	m.blocked = true
	m.runtimeExplicit = true
	ctx := m.owner
	m.actions.Add(1)
	defer m.actions.Done()
	m.mu.Unlock()
	m.runtime.mu.Unlock()
	path, err := m.install(ctx)
	m.mu.Lock()
	m.status.RuntimeAvailable = err == nil && path != ""
	m.blocked = false
	m.mu.Unlock()
	if err != nil || path == "" {
		return errors.New("Could not install the WAN runtime")
	}
	return nil
}

// Start joins a concurrent start, but not a stop/auth operation. ctx is the
// gateway owner's lifetime, not a single administrative request's context.
func (m *Manager) Start(ctx context.Context) error {
	m.runtime.mu.Lock()
	if m.runtime.shutdown != nil {
		m.runtime.mu.Unlock()
		return errors.New("WAN gateway cannot start while magpie is shutting down")
	}
	m.mu.Lock()
	if m.closed || m.blocked || m.owner == nil || m.owner.Err() != nil || ctx == nil || ctx.Err() != nil {
		m.mu.Unlock()
		m.runtime.mu.Unlock()
		return errors.New("WAN gateway owner is not running or is busy")
	}
	if m.current != nil {
		r := m.current
		if m.status.State == "stopping" {
			m.mu.Unlock()
			m.runtime.mu.Unlock()
			return errors.New("WAN gateway is stopping; try again after it stops")
		}
		m.mu.Unlock()
		m.runtime.mu.Unlock()
		<-r.ready
		return r.err
	}
	r := &hosting{done: make(chan struct{}), ready: make(chan struct{})}
	m.runtimeExplicit = true
	r.ctx, r.cancel = context.WithCancel(m.owner)
	r.requests, r.cancelRequests = context.WithCancel(r.ctx)
	m.current = r
	if m.runtime.active == nil {
		m.runtime.active = make(map[*hosting]struct{})
	}
	m.runtime.active[r] = struct{}{}
	m.status.State, m.status.Endpoint, m.status.Error = "starting", "", ""
	m.mu.Unlock()
	m.runtime.mu.Unlock()
	go m.run(r)
	<-r.ready
	return r.err
}

// Stop revokes admission immediately, joins the helper's tree, then releases
// the listener and lock. Authentication remains signed in and restartable.
func (m *Manager) Stop() {
	m.mu.Lock()
	r := m.current
	if r != nil {
		m.status.State, m.status.Endpoint = "stopping", ""
		r.revoke()
		r.cancel()
	}
	m.mu.Unlock()
	if r != nil {
		<-r.done
	}
}

func (m *Manager) run(r *hosting) {
	var failure error
	var authOwner *auth
	var lease *credentialLease
	var snapshotToken string
	var listener *heldListener
	var server *http.Server
	var served chan error
	var child *sdkProcess
	var proxy *wanProxy
	var cancelProxy context.CancelFunc
	var unlock func()
	ready := false
	defer func() {
		if r.ctx.Err() != nil {
			failure = nil
		}
		m.mu.Lock()
		if m.current == r {
			m.status.State, m.status.Endpoint = "stopping", ""
		}
		m.mu.Unlock()
		r.revoke()
		r.cancel()
		if lease != nil {
			snapshotToken = lease.stop()
		}
		if child != nil {
			if child.output.authWasRejected() && authOwner != nil {
				authOwner.Reject(snapshotToken)
			}
			child.stop()
		}
		if cancelProxy != nil {
			cancelProxy()
		}
		if proxy != nil {
			proxy.Close()
		}
		if server != nil {
			_ = server.Close()
			<-served
		}
		if listener != nil {
			_ = listener.TCPListener.Close()
		}
		if unlock != nil {
			unlock()
		}
		m.runtime.mu.Lock()
		delete(m.runtime.active, r)
		m.mu.Lock()
		if m.current == r {
			m.current = nil
			m.status.Endpoint, m.status.Error, m.status.State = "", "", "stopped"
			if failure != nil {
				m.status.State, m.status.Error = "failed", failure.Error()
			}
		}
		if !ready {
			r.err = failure
			if r.err == nil {
				r.err = errors.New("WAN gateway startup was canceled")
			}
			close(r.ready)
		}
		close(r.done)
		m.mu.Unlock()
		m.runtime.mu.Unlock()
	}()
	startup, cancel := context.WithTimeout(r.ctx, m.wait)
	defer cancel()
	if startup.Err() != nil {
		failure = errors.New("WAN gateway startup deadline expired")
		return
	}
	m.mu.Lock()
	a, clientID := m.auth, m.clientID
	m.mu.Unlock()
	token, expires, err := a.hostToken(startup)
	if err != nil {
		failure = err
		return
	}
	authOwner, snapshotToken = a, token
	if a.Status().Provider == "microsoft" {
		clientID = microsoftClientID
	}
	var expired <-chan time.Time
	if !expires.IsZero() {
		credentialStartup, cancelCredential := context.WithDeadline(startup, expires)
		defer cancelCredential()
		startup = credentialStartup
		timer := time.NewTimer(time.Until(expires))
		defer timer.Stop()
		expired = timer.C
	}
	tool, err := m.find(startup)
	m.mu.Lock()
	m.status.RuntimeAvailable = err == nil && tool != ""
	m.mu.Unlock()
	if err != nil || tool == "" {
		failure = errors.New("WAN runtime is unavailable; install the runtime before starting")
		return
	}
	if m.handler == nil {
		failure = errors.New("WAN gateway handler is unavailable")
		return
	}
	var held bool
	unlock, held = filelock.Lock(m.path+".lock", 0)
	if !held {
		failure = errors.New("Another backend owns WAN gateway, or its runtime lock is unavailable")
		return
	}
	account, err := m.account(startup, snapshotToken)
	if err != nil {
		failure = err
		return
	}
	usage := m.usage.status(a)
	if usage.State == "ready" && usage.Limit != nil && usage.Used >= *usage.Limit && (usage.ResetAt.IsZero() || time.Now().Before(usage.ResetAt)) {
		failure = errors.New("Dev Tunnels account bandwidth is exhausted; wait for the reset or request a limit increase")
		return
	}
	config, fresh, err := readTunnel(m.path)
	if err != nil {
		failure = err
		return
	}
	accounts, err := readTunnelAccounts(m.path)
	if err != nil {
		failure = err
		return
	}
	if selected, ok := accounts[account.ID]; ok {
		config = selected
		fresh = false
	}
	if config.AccountID != account.ID || config.New {
		fresh = true
		config = tunnelConfig{}
	}
	config.AccountID, config.AccountName = account.ID, account.Name
	m.mu.Lock()
	m.status.Usage.Account = account
	m.mu.Unlock()
	wireConfig := config
	wireConfig.AccountID, wireConfig.AccountName, wireConfig.New = "", "", false
	raw, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(config.Port)))
	if err != nil {
		failure = errors.New("WAN gateway's saved loopback port is unavailable; no tunnel was hosted")
		return
	}
	listener = &heldListener{TCPListener: raw.(*net.TCPListener)}
	config.Port = raw.Addr().(*net.TCPAddr).Port
	wireConfig.Port = config.Port
	requests := r.requests
	server = &http.Server{
		Handler: m.admission(requests, &r.admitted), BaseContext: func(net.Listener) context.Context { return requests },
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	served = make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		failure = errors.New("Could not generate a private WAN hosting generation")
		return
	}
	// Keep the helper's transport alive only through its bounded dispose grace.
	// Admission and business requests are canceled first; remote cleanup is
	// best-effort and cannot extend the helper/tree lifetime without bound.
	proxyOwner, stopProxy := context.WithCancel(context.WithoutCancel(requests))
	cancelProxy = stopProxy
	proxy, err = m.proxy(proxyOwner, r.revoke)
	if err != nil {
		failure = errors.New("WAN proxy configuration is unavailable")
		return
	}
	child, err = startSDK(startup, tool, m.command, m.startTree, hex.EncodeToString(generation[:]), wireConfig, r.revoke)
	if err != nil {
		failure = err
		return
	}
	request := sdkMessage{Version: 1, Generation: hex.EncodeToString(generation[:]), Op: "prepare", Token: token, ClientID: clientID, Config: &wireConfig, Fresh: fresh, Proxy: proxy.url, AuthProvider: account.Provider}
	if fresh {
		request.Config = nil
	}
	request.Port = config.Port
	if err := child.send(startup, request); err != nil {
		failure = err
		return
	}
	token, request.Token = "", ""
	var prepared sdkEvent
	select {
	case prepared = <-child.output.prepared:
	case <-child.output.bad:
		failure = child.output.failure()
		return
	case <-child.done:
		failure = errors.New("WAN SDK helper exited before preparation")
		return
	case <-startup.Done():
		failure = errors.New("WAN gateway startup deadline expired")
		return
	case <-proxy.done:
		failure = errors.New("WAN proxy stopped unexpectedly; WAN gateway is offline")
		return
	case <-expired:
		failure = errors.New("WAN sign-in expired; sign in again")
		return
	case err := <-served:
		served <- err
		failure = errors.New("WAN listener stopped unexpectedly")
		return
	}
	if startup.Err() != nil || child.output.failure() != nil {
		failure = errors.New("WAN gateway preparation was canceled or failed")
		return
	}
	// Save the exact validated canonical identity before permitting host. A failed
	// migration never overwrites the old identity or silently creates a new one.
	saved := *prepared.Config
	saved.AccountID, saved.AccountName = account.ID, account.Name
	data, _ := json.Marshal(saved)
	accounts[account.ID] = saved
	if err := writeTunnelAccounts(m.path, accounts); err != nil {
		failure = err
		return
	}
	if err := edit.WriteAtomic(m.path, append(data, '\n')); err != nil {
		failure = errors.New("Could not save WAN tunnel identity; no tunnel was hosted")
		return
	}
	m.mu.Lock()
	m.status.Tunnel = tunnelInfo(saved)
	m.tunnels = accounts
	m.mu.Unlock()
	if err := child.output.host(); err != nil {
		failure = err
		return
	}
	if startup.Err() != nil || !expires.IsZero() && !time.Now().Before(expires) {
		failure = errors.New("WAN startup or sign-in expired before hosting")
		return
	}
	if err := child.send(startup, sdkControl{Version: 1, Generation: request.Generation, Op: "host"}); err != nil {
		failure = err
		return
	}
	var endpoint string
	select {
	case endpoint = <-child.output.ready:
	case <-child.output.bad:
		failure = child.output.failure()
		return
	case <-child.done:
		failure = errors.New("WAN SDK helper exited before readiness")
		return
	case <-startup.Done():
		failure = errors.New("WAN gateway startup deadline expired")
		return
	case <-proxy.done:
		failure = errors.New("WAN proxy stopped unexpectedly; WAN gateway is offline")
		return
	case <-expired:
		failure = errors.New("WAN sign-in expired; sign in again")
		return
	case err := <-served:
		served <- err
		failure = errors.New("WAN listener stopped unexpectedly")
		return
	}
	lease = keepCredentials(r.ctx, a, child, request.Generation, snapshotToken, expires, r.revoke)
	select {
	case err := <-lease.ready:
		if err != nil {
			failure = err
			return
		}
	case <-startup.Done():
		failure = errors.New("WAN gateway startup deadline expired")
		return
	}
	expires = a.tokenExpires()
	child.output.mu.Lock()
	m.mu.Lock()
	if child.output.failed != nil || startup.Err() != nil || proxy.ctx.Err() != nil || r.requests.Err() != nil || m.status.State == "stopping" || !expires.IsZero() && !time.Now().Before(expires) {
		failure = child.output.failed
		if failure == nil {
			failure = errors.New("WAN startup or sign-in expired before readiness")
		}
		m.mu.Unlock()
		child.output.mu.Unlock()
		return
	}
	r.admitted.Store(true)
	m.status.State, m.status.Endpoint = "running", endpoint
	ready = true
	close(r.ready)
	m.mu.Unlock()
	child.output.mu.Unlock()
	cancel()
	select {
	case <-r.ctx.Done():
	case <-child.output.bad:
		failure = child.output.failure()
	case <-child.done:
		failure = errors.New("WAN SDK helper stopped unexpectedly; WAN gateway is offline")
	case <-proxy.done:
		failure = errors.New("WAN proxy stopped unexpectedly; WAN gateway is offline")
		return
	case err := <-lease.failed:
		failure = err
	case err := <-served:
		served <- err
		failure = errors.New("WAN listener stopped unexpectedly")
	}
}

// admission refuses before readiness and during tree cleanup, without draining
// an untrusted incomplete request body. Running streams have no total deadline.
func (m *Manager) admission(owner context.Context, admitted *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !admitted.Load() || owner.Err() != nil {
			w.Header().Set("Connection", "close")
			controller := http.NewResponseController(w)
			controller.SetReadDeadline(time.Now())
			if r.Body != nil {
				r.Body.Close()
			}
			controller.SetReadDeadline(time.Time{})
			http.Error(w, "WAN gateway is not running", http.StatusServiceUnavailable)
			return
		}
		m.handler.ServeHTTP(w, r)
	})
}

// Serve may interrupt Accept but cannot release the socket before relay cleanup.
type heldListener struct{ *net.TCPListener }

func (l *heldListener) Close() error { return l.SetDeadline(time.Now()) }

func (l *heldListener) Accept() (net.Conn, error) {
	c, err := l.TCPListener.Accept()
	if err != nil {
		return nil, net.ErrClosed
	}
	return c, nil
}

type tunnelConfig struct {
	Version     int    `json:"version"`
	ID          string `json:"id"`
	ClusterID   string `json:"clusterId,omitempty"`
	Port        int    `json:"port"`
	AccountID   string `json:"accountID,omitempty"`
	AccountName string `json:"accountName,omitempty"`
	New         bool   `json:"new,omitempty"`
}

func readTunnel(path string) (tunnelConfig, bool, error) {
	var config tunnelConfig
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, true, nil
	}
	if err != nil {
		return config, false, errors.New("Could not read wan.json; refusing to replace the saved tunnel")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 || strictJSON(data, &config) != nil {
		return tunnelConfig{}, false, errors.New("Invalid wan.json; refusing to replace the saved tunnel")
	}
	if config.Version == 0 && config.ClusterID == "" && validLegacyID(config.ID) && config.Port > 0 && config.Port <= 65535 {
		config.Version = 1
		return config, false, nil
	}
	if !validCanonicalConfig(config) || !validAccountBinding(config) {
		return tunnelConfig{}, false, errors.New("Invalid wan.json; refusing to replace the saved tunnel")
	}
	return config, false, nil
}

// Anchored from @microsoft/dev-tunnels-contracts 1.3.6 TunnelConstraints.
var canonicalCluster = regexp.MustCompile(`^(([a-z]{3,4}[0-9]{1,3})|asse|aue|brs|euw|use)$`)

func validCanonicalConfig(config tunnelConfig) bool {
	return config.Version == 2 && validTunnelID(config.ID) && canonicalCluster.MatchString(config.ClusterID) && config.Port > 0 && config.Port <= 65535
}

func validTunnelID(id string) bool {
	if len(id) < 3 || len(id) > 60 || strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validLegacyID(id string) bool {
	const prefix = "magpie-wan-"
	if len(id) != len(prefix)+32 || !strings.HasPrefix(id, prefix) {
		return false
	}
	_, err := hex.DecodeString(id[len(prefix):])
	return err == nil
}
