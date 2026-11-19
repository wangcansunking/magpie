package wan

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestShutdownReapsEveryTunnelAndBlocksStart(t *testing.T) {
	rt := &tunnelRuntime{}
	var managers []*Manager
	var addresses []string
	for i := 0; i < 2; i++ {
		m, _ := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		m.runtime = rt
		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		config, _, err := readTunnel(m.path)
		if err != nil {
			t.Fatal(err)
		}
		managers = append(managers, m)
		addresses = append(addresses, net.JoinHostPort("127.0.0.1", strconv.Itoa(config.Port)))
	}
	var joins sync.WaitGroup
	for i := 0; i < 4; i++ {
		joins.Add(1)
		go func() { defer joins.Done(); rt.stop() }()
	}
	joins.Wait()
	for i, m := range managers {
		if m.Status().State != "stopped" {
			t.Fatal(m.Status())
		}
		if err := m.Start(context.Background()); err == nil {
			t.Fatal("shutdown manager restarted")
		}
		listener, err := net.Listen("tcp4", addresses[i])
		if err != nil {
			t.Fatalf("child cleanup retained listener: %v", err)
		}
		listener.Close()
	}
	late, folder := fakeManager(t, "ok", managers[0].handler)
	late.runtime = rt
	if err := late.Start(context.Background()); err == nil {
		t.Fatal("new manager started after process shutdown")
	}
	if _, err := os.Stat(filepath.Join(folder, "calls")); !os.IsNotExist(err) {
		t.Fatal("post-shutdown Start launched child")
	}
}

func TestShutdownCancelsAllBeforeWaitingForCleanup(t *testing.T) {
	rt := &tunnelRuntime{active: make(map[*hosting]struct{})}
	var runs []*hosting
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		r := &hosting{ctx: ctx, cancel: cancel, done: make(chan struct{})}
		runs = append(runs, r)
		rt.active[r] = struct{}{}
	}
	finished := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			for _, r := range runs {
				r.cancel()
				close(r.done)
			}
		})
	}
	t.Cleanup(func() { release(); waitDone(t, finished) })
	go func() { defer close(finished); rt.stop() }()
	for _, r := range runs {
		waitDone(t, r.ctx.Done())
	}
	select {
	case <-finished:
		t.Fatal("shutdown returned before cleanup")
	default:
	}
	release()
	waitDone(t, finished)
}

func TestShutdownJoinsIdleAuthRestore(t *testing.T) {
	rt := &tunnelRuntime{}
	entered, release := make(chan struct{}), make(chan struct{})
	store := &authTestStore{loadIn: entered, loadOut: release}
	m := New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	m.runtime, m.store = rt, store
	m.hasRuntime = func(context.Context) (bool, error) { return false, nil }
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	stopped := make(chan struct{})
	var launched bool
	t.Cleanup(func() {
		unblock()
		m.Close()
		if launched {
			waitDone(t, stopped)
		}
	})
	if err := m.Own(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, entered)
	launched = true
	go func() { defer close(stopped); rt.stop() }()
	waitDone(t, m.owner.Done())
	select {
	case <-stopped:
		t.Fatal("shutdown returned before idle auth restore joined")
	default:
	}
	if _, err := m.StartAuth(false); err == nil {
		t.Fatal("auth started during process shutdown")
	}
	unblock()
	waitDone(t, stopped)
	if s := m.Status(); s.Auth.State == "signed_in" {
		t.Fatal("late vault restore revived signed in state")
	}
}

func TestShutdownJoinsExplicitRuntimeInstall(t *testing.T) {
	m, _ := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	entered, release := make(chan struct{}), make(chan struct{})
	m.install = func(ctx context.Context) (string, error) {
		close(entered)
		<-release
		return "", errors.New("fake canceled install")
	}
	installed, stopped := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var launchedStop bool
	t.Cleanup(func() {
		unblock()
		m.Close()
		waitDone(t, installed)
		if launchedStop {
			waitDone(t, stopped)
		}
	})
	go func() { defer close(installed); _ = m.InstallRuntime(context.Background()) }()
	waitDone(t, entered)
	launchedStop = true
	go func() { defer close(stopped); m.runtime.stop() }()
	waitDone(t, m.owner.Done())
	select {
	case <-stopped:
		t.Fatal("shutdown returned before runtime installation joined")
	default:
	}
	unblock()
	waitDone(t, installed)
	waitDone(t, stopped)
}

func TestRuntimeDiscoveryHoldsNoManagerOrShutdownLockAndCannotCommitLate(t *testing.T) {
	m := New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	m.runtime, m.store = &tunnelRuntime{}, memoryCredentialStore{}
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m.hasRuntime = func(ctx context.Context) (bool, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return true, nil // Deliberately stale result must not update the snapshot.
	}
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	owned, stopped := make(chan struct{}), make(chan struct{})
	var stopLaunched bool
	t.Cleanup(func() {
		unblock()
		m.Close()
		waitDone(t, owned)
		if stopLaunched {
			waitDone(t, stopped)
		}
	})
	go func() {
		defer close(owned)
		if err := m.Own(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	waitDone(t, entered)
	waitDone(t, owned)
	statusRead := make(chan struct{})
	go func() { defer close(statusRead); _ = m.Status() }()
	waitDone(t, statusRead)
	stopLaunched = true
	go func() { defer close(stopped); m.runtime.stop() }()
	waitDone(t, canceled)
	select {
	case <-stopped:
		t.Fatal("shutdown did not join owned discovery")
	default:
	}
	unblock()
	waitDone(t, stopped)
	if m.Status().RuntimeAvailable {
		t.Fatal("stale runtime discovery committed after shutdown")
	}
}

func TestShutdownDuringStartWaitsItsGeneration(t *testing.T) {
	m, folder := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered after fakeManager's Stop: even a failed assertion first
	// releases discovery, then waits for every goroutine this test starts.
	started := make(chan struct{})
	var stopped chan struct{}
	t.Cleanup(func() {
		unblock()
		m.Stop()
		waitDone(t, started)
		if stopped != nil {
			waitDone(t, stopped)
		}
	})
	find := m.find
	m.find = func(ctx context.Context) (string, error) { close(entered); <-release; return find(ctx) }
	go func() {
		defer close(started)
		if err := m.Start(context.Background()); err == nil {
			t.Error("shutdown startup succeeded")
		}
	}()
	waitDone(t, entered)
	m.mu.Lock()
	r := m.current
	m.mu.Unlock()
	stopped = make(chan struct{})
	go func() { defer close(stopped); m.runtime.stop() }()
	waitDone(t, r.ctx.Done())
	if err := m.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("new start during shutdown: %v", err)
	}
	select {
	case <-stopped:
		t.Fatal("shutdown abandoned starting generation")
	default:
	}
	unblock()
	waitDone(t, stopped)
	waitDone(t, started)
	if _, err := os.Stat(filepath.Join(folder, "calls")); !os.IsNotExist(err) {
		t.Fatal("canceled startup launched child")
	}
}
