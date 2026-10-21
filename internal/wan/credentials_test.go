package wan

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rotateManagerCredential(m *Manager, token string) {
	m.auth.mu.Lock()
	m.auth.credential.Access = token
	m.auth.credential.ExpiresAt = time.Now().Add(2 * time.Hour)
	m.auth.status.ExpiresAt = m.auth.credential.ExpiresAt
	m.auth.signalLocked()
	m.auth.mu.Unlock()
}

func waitCredentialCall(t *testing.T, folder, want string) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		data, _ := os.ReadFile(filepath.Join(folder, "calls"))
		if strings.Contains(string(data), want) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("helper did not receive credential update")
		case <-tick.C:
		}
	}
}

func TestWANCredentialUpdatesKeepEndpointAndActiveStream(t *testing.T) {
	canceled := make(chan struct{})
	m, folder := fakeManager(t, "credentials", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			fmt.Fprint(w, "ok")
			return
		}
		_, fixed := r.Context().Deadline()
		fmt.Fprintf(w, "fixed=%t\n", fixed)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	m.auth.mu.Lock()
	m.auth.credential.ExpiresAt = time.Now().Add(time.Hour)
	m.auth.status.ExpiresAt = m.auth.credential.ExpiresAt
	m.auth.mu.Unlock()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	endpoint := m.Status().Endpoint
	config, _, _ := readTunnel(m.path)
	address := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(config.Port))
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Get(address + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "fixed=false\n" {
		t.Fatal("stream inherited the original token's unextendable deadline", line, err)
	}
	for i := 1; i <= 2; i++ {
		rotateManagerCredential(m, fmt.Sprintf("fake-renewed-%d", i))
		waitCredentialCall(t, folder, fmt.Sprintf("credentials %d", i))
	}
	probe, err := client.Get(address + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()
	if m.Status().Endpoint != endpoint || m.Status().State != "running" {
		t.Fatal("credential rotation restarted the endpoint")
	}
	select {
	case <-canceled:
		t.Fatal("credential update canceled an accepted stream")
	default:
	}
	m.Stop()
	waitDone(t, canceled)
}

func TestWANCredentialUpdateFailureClosesAdmissionAndStopJoinsWriter(t *testing.T) {
	for _, mode := range []string{"credentials-bad-ack", "credentials-auth", "credentials-hold"} {
		t.Run(mode, func(t *testing.T) {
			m, folder := fakeManager(t, mode, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			rotateManagerCredential(m, "fake-renewed-next")
			waitCredentialCall(t, folder, "credentials 1")
			if mode == "credentials-hold" {
				m.Stop()
			} else {
				deadline := time.NewTimer(20 * time.Second)
				defer deadline.Stop()
				for m.Status().State != "failed" {
					select {
					case <-deadline.C:
						t.Fatal("bad credential update kept hosting")
					default:
						time.Sleep(time.Millisecond)
					}
				}
			}
			if m.Status().Endpoint != "" {
				t.Fatal("failed/stopped update kept its endpoint")
			}
			if mode == "credentials-auth" {
				if _, err := m.auth.token(); err == nil {
					t.Fatal("helper-rejected renewed token remained usable")
				}
			}
		})
	}
}
