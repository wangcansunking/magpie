package wan

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOnlyOneManagerOwnsSavedTunnel(t *testing.T) {
	first, folder := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, _ := fakeManager(t, "ok", first.handler)
	second.path, second.command = first.path, first.command
	if err := second.Start(context.Background()); err == nil {
		t.Fatal("second manager acquired a hosted tunnel")
	}
	first.Stop()
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("lock not released by cleanup: %v", err)
	}
	second.Stop()
	calls, _ := os.ReadFile(filepath.Join(folder, "calls"))
	if strings.Count(string(calls), "host\n") != 2 {
		t.Fatalf("unexpected host count: %s", calls)
	}
}

func TestMissingToolAndCanceledOwnerNeverLaunch(t *testing.T) {
	m, folder := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	m.find = func(context.Context) (string, error) { return "", nil }
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("missing tool started")
	}
	if s := m.Status(); s.State != "failed" || s.RuntimeAvailable || !strings.Contains(s.Error, "runtime") {
		t.Fatal(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Start(ctx); err == nil {
		t.Fatal("canceled owner started")
	}
	if _, err := os.Stat(filepath.Join(folder, "calls")); !os.IsNotExist(err) {
		t.Fatal("missing tool launched")
	}
}

func TestCanceledStartupDoesNotPersistOrHost(t *testing.T) {
	m, folder := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	m.wait = -time.Second // already expired before any subprocess, not a scheduling assertion
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("expired startup succeeded")
	}
	if _, err := os.Stat(filepath.Join(folder, "calls")); !os.IsNotExist(err) {
		t.Fatal("expired startup launched CLI")
	}
	if _, err := os.Stat(m.path); !os.IsNotExist(err) {
		t.Fatal("failed startup persisted identity")
	}
}

type heldSDKTree struct {
	sdkTree
	entered chan struct{}
	release <-chan struct{}
}

func (tree *heldSDKTree) Wait() error {
	err := tree.sdkTree.Wait()
	close(tree.entered)
	<-tree.release
	return err
}

func TestSDKTreeCleanupOwnsSocketUntilJoined(t *testing.T) {
	m, _ := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	stopped := make(chan struct{})
	t.Cleanup(func() { unblock(); m.Stop(); waitDone(t, stopped) })
	m.startTree = func(cmd *exec.Cmd) (sdkTree, error) {
		tree, err := startSDKTree(cmd)
		if err != nil {
			return nil, err
		}
		return &heldSDKTree{sdkTree: tree, entered: entered, release: release}, nil
	}
	if err := m.Start(context.Background()); err != nil {
		close(stopped)
		t.Fatal(err)
	}
	config, _, err := readTunnel(m.path)
	if err != nil {
		close(stopped)
		t.Fatal(err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(config.Port))
	go func() { defer close(stopped); m.Stop() }()
	waitDone(t, entered)
	if s := m.Status(); s.State != "stopping" || s.Endpoint != "" {
		t.Fatalf("cleanup still advertised endpoint: %#v", s)
	}
	select {
	case <-stopped:
		t.Fatal("Stop skipped tree completion")
	default:
	}
	other, err := net.Listen("tcp4", address)
	if err == nil {
		other.Close()
		t.Fatal("socket released before tree cleanup joined")
	}
	unblock()
	waitDone(t, stopped)
	other, err = net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("socket not released after tree cleanup: %v", err)
	}
	other.Close()
}

func TestAuthReplacementRefusesHostingAndLogoutJoinsTreeBeforeErase(t *testing.T) {
	m, _ := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	loggedOut := make(chan struct{})
	var launched bool
	t.Cleanup(func() {
		unblock()
		m.Close()
		if launched {
			waitDone(t, loggedOut)
		}
	})
	m.startTree = func(cmd *exec.Cmd) (sdkTree, error) {
		tree, err := startSDKTree(cmd)
		if err != nil {
			return nil, err
		}
		return &heldSDKTree{sdkTree: tree, entered: entered, release: release}, nil
	}
	store := &authTestStore{}
	m.auth.store = store // No restore/worker runs for fakeManager's memory store.
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartAuth(false); err == nil || m.Status().State != "running" || m.Status().Auth.State != "signed_in" {
		t.Fatal("sign-in replacement did not refuse an active helper")
	}
	launched = true
	go func() {
		defer close(loggedOut)
		if err := m.Logout(); err != nil {
			t.Error(err)
		}
	}()
	waitDone(t, entered)
	if _, _, erases := store.snapshot(); erases != 0 {
		t.Fatal("credential erased before hosted tree completed cleanup")
	}
	if _, err := m.auth.token(); err == nil {
		t.Fatal("logout left credential usable while cleanup waited")
	}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("logout allowed a replacement generation during cleanup")
	}
	unblock()
	waitDone(t, loggedOut)
	if _, _, erases := store.snapshot(); erases != 1 || m.Status().Auth.State != "signed_out" {
		t.Fatal("logout did not erase the credential after cleanup")
	}
}

func TestProxyFailureDuringStartupNeverPersistsOrAdmits(t *testing.T) {
	for _, mode := range []string{"hold-prepare", "hold-ready"} {
		t.Run(mode, func(t *testing.T) {
			m, folder := fakeManager(t, mode, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("failed proxy admitted a request") }))
			var bridge *wanProxy
			commandEntered := make(chan struct{})
			m.proxy = func(ctx context.Context, revoke func()) (*wanProxy, error) {
				var err error
				bridge, err = startWANProxy(context.Background(), revoke)
				return bridge, err
			}
			command := m.command
			m.command = func(name string, args ...string) *exec.Cmd { close(commandEntered); return command(name, args...) }
			started := make(chan struct{})
			var startErr error
			go func() { defer close(started); startErr = m.Start(context.Background()) }()
			t.Cleanup(func() {
				if bridge != nil {
					bridge.Close()
				}
				m.Stop()
				waitDone(t, started)
			})
			waitDone(t, commandEntered)
			if mode == "hold-ready" {
				// A local fake-helper host callback acknowledges the manager has
				// persisted and entered readiness, rather than sleeping for it.
				deadline := time.After(20 * time.Second)
				for {
					data, _ := os.ReadFile(filepath.Join(folder, "calls"))
					if strings.Contains(string(data), "host\n") {
						break
					}
					select {
					case <-deadline:
						t.Fatal("fake helper did not reach host")
					default:
						time.Sleep(time.Millisecond)
					}
				}
			}
			bridge.Close()
			waitDone(t, started)
			if startErr == nil || m.Status().State != "failed" || m.Status().Endpoint != "" {
				t.Fatalf("proxy failure escaped startup: %v %#v", startErr, m.Status())
			}
			if mode == "hold-prepare" {
				if _, err := os.Stat(m.path); !os.IsNotExist(err) {
					t.Fatal("failed preparation wrote a null tunnel identity")
				}
			}
		})
	}
}

func TestHelper401InvalidatesMatchingLoginBut403DoesNot(t *testing.T) {
	for _, mode := range []string{"failure", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := fakeManager(t, mode, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			if err := m.Start(context.Background()); err == nil {
				t.Fatal("refused helper succeeded")
			}
			_, err := m.auth.token()
			if mode == "failure" {
				if err == nil || m.Status().Auth.State == "signed_in" {
					t.Fatal("401 left rejected login signed in")
				}
			} else if err != nil || m.Status().Auth.State != "signed_in" {
				t.Fatal("403 incorrectly invalidated the login")
			}
		})
	}
}

func TestStopKeepsCleanupProxyUntilHelperJoinsButRevokesRequests(t *testing.T) {
	m, folder := fakeManager(t, "cleanup-connect", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("stopped generation admitted business request") }))
	var bridge *wanProxy
	cleanupConnected := make(chan struct{})
	peerDone := make(chan struct{})
	m.proxy = func(ctx context.Context, revoke func()) (*wanProxy, error) {
		var err error
		bridge, err = startWANProxy(ctx, revoke)
		if err != nil {
			return nil, err
		}
		bridge.choose = func(*http.Request) (*url.URL, error) { return nil, nil }
		bridge.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != "cleanup.invalid:443" {
				t.Error("unexpected cleanup target")
			}
			local, remote := net.Pipe()
			go func() {
				defer close(peerDone)
				defer remote.Close()
				remote.SetDeadline(time.Now().Add(10 * time.Second))
				var data [7]byte
				if _, err := io.ReadFull(remote, data[:]); err != nil || string(data[:]) != "cleanup" {
					t.Error("helper did not send cleanup bytes")
					return
				}
				close(cleanupConnected)
				io.WriteString(remote, "ok")
			}()
			return local, nil
		}
		return bridge, nil
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	run := m.current
	m.mu.Unlock()
	m.Stop()
	waitDone(t, cleanupConnected)
	waitDone(t, peerDone)
	if run.admitted.Load() || run.requests.Err() == nil {
		t.Fatal("cleanup grace retained business admission")
	}
	if bridge.ctx.Err() == nil {
		t.Fatal("cleanup proxy remained after helper joined")
	}
	select {
	case <-bridge.done:
	default:
		t.Fatal("Stop did not join cleanup proxy")
	}
	calls, _ := os.ReadFile(filepath.Join(folder, "calls"))
	if !strings.Contains(string(calls), "cleanup-connected\n") {
		t.Fatal("helper could not use proxy during dispose grace")
	}
}

func TestSDKHelperReceivesUserCAWithoutInheritedPreloads(t *testing.T) {
	m, _ := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Setenv("NODE_EXTRA_CA_CERTS", "wan-relative-ca.pem")
	t.Setenv("NODE_OPTIONS", "--require=do-not-load.cjs")
	t.Setenv("BUN_OPTIONS", "--preload=do-not-load.cjs")
	want, err := filepath.Abs("wan-relative-ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	startTree := m.startTree
	m.startTree = func(cmd *exec.Cmd) (sdkTree, error) {
		found := false
		for _, item := range cmd.Env {
			key, value, _ := strings.Cut(item, "=")
			if strings.EqualFold(key, "NODE_OPTIONS") || strings.EqualFold(key, "BUN_OPTIONS") {
				t.Error("inherited runtime preload enabled")
			}
			if strings.EqualFold(key, "NODE_EXTRA_CA_CERTS") {
				actual := value
				if !filepath.IsAbs(actual) {
					actual = filepath.Join(cmd.Dir, actual)
				}
				if !strings.EqualFold(filepath.Clean(actual), want) {
					t.Errorf("user CA resolved against helper cwd: %q want %q", actual, want)
				}
				found = true
			}
		}
		if !found {
			t.Error("helper lost user configured CA")
		}
		return startTree(cmd)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredCredentialSnapshotCannotLaunchPrepare(t *testing.T) {
	m, folder := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// Auth's read clock is deliberately before its deadline, while the actual
	// helper deadline is already past. This forces the snapshot/launch boundary
	// without relying on a sleep or on how fast discovery returns.
	expires := time.Now().Add(-time.Hour)
	m.auth.mu.Lock()
	m.auth.now = func() time.Time { return expires.Add(-time.Hour) }
	m.auth.credential.ExpiresAt = expires
	m.auth.mu.Unlock()
	m.find = func(ctx context.Context) (string, error) {
		if ctx.Err() == nil {
			t.Error("runtime discovery was not bounded by credential deadline")
		}
		return "fake-tool", nil
	}
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("expired snapshot started hosting")
	}
	if _, err := os.Stat(filepath.Join(folder, "calls")); !os.IsNotExist(err) {
		t.Fatal("expired credential reached helper prepare")
	}
}

func TestStatusCopiesSnapshotOnly(t *testing.T) {
	m, _ := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	m.find = func(context.Context) (string, error) { t.Error("status performed discovery"); return "", nil }
	m.command = func(string, ...string) *exec.Cmd { t.Error("status launched a child"); return nil }
	for i := 0; i < 100; i++ {
		if s := m.Status(); s.State != "stopped" {
			t.Fatal(s)
		}
	}
}
