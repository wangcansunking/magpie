package wan

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

// Only the Go test executable is launched: never Bun, a downloaded package,
// an installed CLI, a login request or a public tunnel.
func TestFakeSDK(t *testing.T) {
	at := -1
	for i, arg := range os.Args {
		if arg == "--wan-fake-sdk" {
			at = i
			break
		}
	}
	if at < 0 {
		return
	}
	mode, folder := os.Args[at+1], os.Args[at+2]
	calls, err := os.OpenFile(filepath.Join(folder, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(20)
	}
	defer calls.Close()
	input := bufio.NewReader(os.Stdin)
	line, err := input.ReadBytes('\n')
	var prepare sdkMessage
	if err != nil || strictJSON(line, &prepare) != nil || prepare.Version != 1 || prepare.Op != "prepare" || len(prepare.Generation) != 32 || prepare.Port < 1 || prepare.Token != "fake-sdk-token" || prepare.ClientID != DefaultGitHubClientID {
		os.Exit(21)
	}
	fmt.Fprintln(calls, "prepare")
	if mode == "failure" || mode == "forbidden" {
		fmt.Fprintln(os.Stderr, "access_token=SECRET-SHOULD-NEVER-LEAK")
		code := "auth"
		if mode == "forbidden" {
			code = "forbidden"
		}
		fmt.Printf("{\"version\":1,\"generation\":%q,\"event\":\"error\",\"code\":%q}\n", prepare.Generation, code)
		os.Exit(22)
	}
	config := tunnelConfig{Version: 2, ID: "actual-id", ClusterID: "usw2", Port: prepare.Port}
	if prepare.Fresh {
		if prepare.Config != nil {
			os.Exit(23)
		}
		fmt.Fprintln(calls, "create")
	} else {
		if prepare.Config == nil || prepare.Config.Port != prepare.Port {
			os.Exit(24)
		}
		fmt.Fprintln(calls, "get")
		if prepare.Config.Version == 2 {
			config = *prepare.Config
		} else if prepare.Config.Version != 1 || !validLegacyID(prepare.Config.ID) {
			os.Exit(25)
		}
	}
	endpoint := fmt.Sprintf("https://%s-%d.%s.devtunnels.ms", config.ID, config.Port, config.ClusterID)
	event := sdkEvent{Version: 1, Generation: prepare.Generation, Event: "prepared", Config: &config, Endpoint: endpoint}
	switch mode {
	case "malformed":
		fmt.Print(strings.Repeat("SECRET", 20000))
		os.Exit(26)
	case "bad-generation":
		event.Generation = strings.Repeat("b", 32)
	case "wrong-port":
		event.Config.Port++
	case "missing":
		event = sdkEvent{Version: 1, Generation: prepare.Generation, Event: "error", Code: "not_found"}
	case "hold-prepare":
		io.Copy(io.Discard, input)
		os.Exit(0)
	}
	encoded, _ := json.Marshal(event)
	fmt.Printf("%s\n", encoded)
	line, err = input.ReadBytes('\n')
	var host sdkControl
	if err != nil || strictJSON(line, &host) != nil || host.Version != 1 || host.Generation != prepare.Generation || host.Op != "host" {
		os.Exit(27)
	}
	// A host command is accepted only after the canonical identity is on disk.
	saved, fresh, err := readTunnel(filepath.Join(folder, "wan.json"))
	if err != nil || fresh || saved != config {
		os.Exit(28)
	}
	fmt.Fprintln(calls, "host")
	if mode == "hold-ready" {
		io.Copy(io.Discard, input)
		os.Exit(0)
	}
	event = sdkEvent{Version: 1, Generation: prepare.Generation, Event: "ready", Endpoint: endpoint}
	encoded, _ = json.Marshal(event)
	if mode == "ready-then-bad" {
		fmt.Printf("%s\n{\"SECRET\":true}\n", encoded)
	} else {
		fmt.Printf("%s\n", encoded)
	}
	if mode == "exit" {
		os.Exit(29)
	}
	// EOF is the helper dispose request. The parent also kills/joins the tree.
	io.Copy(io.Discard, input)
	if mode == "cleanup-connect" {
		proxy, err := url.Parse(prepare.Proxy)
		if err != nil || proxy.User == nil {
			os.Exit(30)
		}
		conn, err := net.DialTimeout("tcp4", proxy.Host, 5*time.Second)
		if err != nil {
			os.Exit(31)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		password, _ := proxy.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(proxy.User.Username() + ":" + password))
		fmt.Fprintf(conn, "CONNECT cleanup.invalid:443 HTTP/1.1\r\nHost: cleanup.invalid:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if err != nil || response.StatusCode != 200 {
			os.Exit(32)
		}
		io.WriteString(conn, "cleanup")
		var ack [2]byte
		if _, err := io.ReadFull(reader, ack[:]); err != nil || string(ack[:]) != "ok" {
			os.Exit(33)
		}
		fmt.Fprintln(calls, "cleanup-connected")
	}
	os.Exit(0)
}

func fakeManager(t *testing.T, mode string, handler http.Handler) (*Manager, string) {
	t.Helper()
	folder := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := New(handler)
	m.runtime = &tunnelRuntime{}
	m.path = filepath.Join(folder, "wan.json")
	m.find = func(context.Context) (string, error) { return executable, nil }
	m.hasRuntime = func(context.Context) (bool, error) { return true, nil }
	m.store = memoryCredentialStore{}
	m.command = func(name string, args ...string) *exec.Cmd {
		for _, arg := range args {
			if strings.Contains(arg, "fake-sdk-token") {
				t.Error("credential entered command arguments")
			}
		}
		return proc.Command(name, "-test.run=^TestFakeSDK$", "--", "--wan-fake-sdk", mode, folder)
	}
	if err := m.Own(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.auth.mu.Lock()
	m.auth.credential = savedCredential{Version: 1, ClientID: DefaultGitHubClientID, Access: "fake-sdk-token"}
	m.auth.status.State = "signed_in"
	m.auth.mu.Unlock()
	t.Cleanup(m.Close)
	return m, folder
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("cleanup did not complete")
	}
}

func TestManagerLifecycleAndStreamingCancellation(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	m, folder := fakeManager(t, "ok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(canceled)
	}))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if status.State != "running" || status.Endpoint == "" || !status.RuntimeAvailable || status.Auth.State != "signed_in" {
		t.Fatalf("status %#v", status)
	}
	config, fresh, err := readTunnel(m.path)
	if err != nil || fresh || config.Version != 2 || config.ClusterID != "usw2" {
		t.Fatalf("saved config: %#v %v fresh %v", config, err, fresh)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(config.Port))
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 20 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + address + "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("stream %q %v", line, err)
	}
	waitDone(t, entered)
	m.cancel()
	waitDone(t, canceled)
	m.Close()
	if s := m.Status(); s.State != "stopped" || s.Endpoint != "" {
		t.Fatalf("after cancel %#v", s)
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("port retained after child cleanup: %v", err)
	}
	listener.Close()
	calls, _ := os.ReadFile(filepath.Join(folder, "calls"))
	if strings.Count(string(calls), "host\n") != 1 {
		t.Fatalf("calls %s", calls)
	}
}

func TestDuplicateStartAndStopOwnOneGeneration(t *testing.T) {
	m, folder := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := m.Start(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() { defer group.Done(); m.Stop() }()
	}
	group.Wait()
	if m.Status().State != "stopped" || m.Status().Auth.State != "signed_in" {
		t.Fatal(m.Status())
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Stop()
	calls, _ := os.ReadFile(filepath.Join(folder, "calls"))
	if strings.Count(string(calls), "host\n") != 2 || strings.Count(string(calls), "create\n") != 1 || strings.Count(string(calls), "get\n") != 1 {
		t.Fatalf("duplicate generation commands %s", calls)
	}
}

func TestStartupFailureIsSafeAndReleases(t *testing.T) {
	for _, mode := range []string{"failure", "wrong-port", "malformed", "bad-generation", "ready-then-bad", "exit"} {
		t.Run(mode, func(t *testing.T) {
			m, folder := fakeManager(t, mode, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			err := m.Start(context.Background())
			if err == nil {
				m.mu.Lock()
				run := m.current
				m.mu.Unlock()
				if run != nil {
					waitDone(t, run.done)
				}
			}
			status := m.Status()
			if status.State != "failed" || status.Endpoint != "" || strings.Contains(status.Error, "SECRET") {
				t.Fatalf("unsafe failure: %#v err %v", status, err)
			}
			if mode == "wrong-port" || mode == "bad-generation" {
				calls, _ := os.ReadFile(filepath.Join(folder, "calls"))
				if strings.Contains(string(calls), "host\n") {
					t.Fatalf("hosted invalid metadata: %s", calls)
				}
			}
		})
	}
}

func TestStopDuringStartupAndStatusDoNotLaunch(t *testing.T) {
	m, _ := fakeManager(t, "hold-prepare", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	started := make(chan struct{})
	command := m.command
	m.command = func(name string, args ...string) *exec.Cmd {
		close(started)
		return command(name, args...)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := m.Start(context.Background()); err == nil {
			t.Error("canceled startup succeeded")
		}
	}()
	t.Cleanup(func() { m.Stop(); waitDone(t, done) })
	waitDone(t, started)
	for i := 0; i < 100; i++ {
		if s := m.Status(); s.State != "starting" {
			t.Fatalf("status %#v", s)
		}
	}
	m.Stop()
	waitDone(t, done)
	if m.Status().State != "stopped" {
		t.Fatal(m.Status())
	}
}

func TestCorruptConfigAndOccupiedPortFailClosed(t *testing.T) {
	for _, data := range []string{"", "null", `{`, `{"id":"user-tunnel","port":3425}`, `{"version":2,"id":"x-y","clusterId":"evil","port":3425}`, `{"version":2,"id":"x-y","clusterId":"usw2","port":3425,"token":"SECRET"}`, `{"version":2,"version":2,"id":"x-y","clusterId":"usw2","port":3425}`} {
		m, folder := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		if err := os.WriteFile(m.path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := m.Start(context.Background()); err == nil {
			t.Fatalf("accepted config %q", data)
		}
		if _, err := os.Stat(filepath.Join(folder, "calls")); !os.IsNotExist(err) {
			t.Fatal("corrupt config launched helper")
		}
		read, _ := os.ReadFile(m.path)
		if string(read) != data {
			t.Fatal("corrupt config overwritten")
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	m, folder := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	config := tunnelConfig{Version: 2, ID: "actual-id", ClusterID: "usw2", Port: listener.Addr().(*net.TCPAddr).Port}
	data, _ := json.Marshal(config)
	os.WriteFile(m.path, data, 0600)
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("hosted occupied port")
	}
	if _, err := os.Stat(filepath.Join(folder, "calls")); !os.IsNotExist(err) {
		t.Fatal("occupied port launched helper")
	}
}

func TestHeldListenerDoesNotReleasePort(t *testing.T) {
	raw, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	held := &heldListener{raw.(*net.TCPListener)}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Accept(); err == nil {
		t.Fatal("held listener still admitted connections")
	}
	second, err := net.Listen("tcp4", raw.Addr().String())
	if err == nil {
		second.Close()
		t.Fatal("port reusable before child reaped")
	}
}

func TestLegacyConfigResolvedBeforeHostAndNeverReplacedOnMissing(t *testing.T) {
	for _, mode := range []string{"ok", "missing"} {
		t.Run(mode, func(t *testing.T) {
			m, folder := fakeManager(t, mode, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			ln.Close()
			original := fmt.Sprintf(`{"id":"magpie-wan-%s","port":%d}`, strings.Repeat("a", 32), port)
			if err := os.WriteFile(m.path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			err = m.Start(context.Background())
			data, _ := os.ReadFile(m.path)
			calls, _ := os.ReadFile(filepath.Join(folder, "calls"))
			if strings.Contains(string(calls), "create\n") {
				t.Fatal("legacy tunnel was silently replaced")
			}
			if mode == "missing" {
				if err == nil || string(data) != original || strings.Contains(string(calls), "host\n") {
					t.Fatalf("missing legacy identity: %v %s %s", err, data, calls)
				}
			} else {
				config, _, readErr := readTunnel(m.path)
				if err != nil || readErr != nil || config.Version != 2 || config.ID != "actual-id" || config.ClusterID != "usw2" || config.Port != port {
					t.Fatalf("canonical migration: %#v %v %v", config, err, readErr)
				}
			}
		})
	}
}
