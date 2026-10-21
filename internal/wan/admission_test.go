package wan

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// This is the handler installed on the manager's held listener, before it is
// ready and after admission is revoked. No tunnel process is needed to exercise
// the real net/http response/drain path on a single loopback TCP connection.
func TestAdmissionRejectedBodyDoesNotDrain(t *testing.T) {
	for _, state := range []string{"starting", "stopped", "owner-canceled"} {
		for _, body := range []struct{ name, header, prefix string }{
			{"length", "Content-Length: 20", "{"},
			{"chunked", "Transfer-Encoding: chunked", "1\r\n{\r\n"},
		} {
			t.Run(state+"/"+body.name, func(t *testing.T) {
				owner, cancel := context.WithCancel(context.Background())
				defer cancel()
				var admitted, called atomic.Bool
				if state != "starting" {
					admitted.Store(true)
					if state == "stopped" {
						admitted.Store(false)
					} else {
						cancel()
					}
				}
				m := &Manager{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) })}
				server := httptest.NewServer(m.admission(owner, &admitted))
				defer server.Close()
				conn, err := net.DialTimeout("tcp4", server.Listener.Addr().String(), 10*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				// The client sends neither the missing bytes nor the last chunk.
				// The deadline is only a deadlock guard, not a response-time bar.
				if _, err := fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: localhost\r\n%s\r\n\r\n%s", body.header, body.prefix); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(conn)
				resp, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatalf("%s admission waited for unfinished %s body: %v", state, body.name, err)
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != 503 || !resp.Close || string(data) != "WAN gateway is not running\n" {
					t.Fatalf("admission status=%d close=%v body=%q", resp.StatusCode, resp.Close, data)
				}
				if called.Load() {
					t.Fatal("inactive admission reached the protected handler")
				}
				if _, err := reader.ReadByte(); err != io.EOF {
					t.Fatalf("inactive admission left the connection open: %v", err)
				}
			})
		}
	}
}

func TestAdmissionLeavesValidStreamsAlone(t *testing.T) {
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	var admitted atomic.Bool
	admitted.Store(true)
	body := make(chan string, 1)
	m := &Manager{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			body <- err.Error()
			return
		}
		body <- string(data)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})}
	server := httptest.NewUnstartedServer(m.admission(owner, &admitted))
	server.Config.BaseContext = func(net.Listener) context.Context { return owner }
	server.Start()
	defer func() {
		cancel()
		server.Close()
	}()
	conn, err := net.DialTimeout("tcp4", server.Listener.Addr().String(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(conn, "POST /v1/responses HTTP/1.1\r\nHost: localhost\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Close || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("valid stream changed: %v", resp)
	}
	if got := <-body; got != "{}" {
		t.Fatalf("valid body changed: %q", got)
	}
	reader := bufio.NewReader(resp.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("stream first event %q: %v", line, err)
	}
	cancel()
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("owner cancellation did not end the stream: %v", err)
	}
}
