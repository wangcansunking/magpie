package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

// Each client leaves its body unfinished and keeps the socket open. A refusal
// must arrive without those bytes, not wait for net/http's implicit body drain.
func TestWANRejectedBodyDoesNotDrain(t *testing.T) {
	fresh(t)
	_, secret := newWANCaller(t, "Rejected body")
	const origin = "https://client.example"
	s := settings.Load()
	s.CORSOrigins = []string{origin}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New().WANHandler())
	defer server.Close()
	for _, body := range []struct{ name, header, prefix string }{
		{"length", "Content-Length: 20", "{"},
		{"chunked", "Transfer-Encoding: chunked", "1\r\n{\r\n"},
	} {
		for _, tc := range []struct {
			name, method, path, key, extra string
			status                         int
			cors                           bool
			message                        string
		}{
			{"missing-key", "POST", "/v1/chat/completions", "", "", 401, true, "send an enabled WAN gateway key"},
			{"bad-key", "POST", "/v1/chat/completions", "invalid-key", "", 401, true, "send an enabled WAN gateway key"},
			{"messages-key", "POST", "/v1/messages", "", "", 401, true, "send an enabled WAN gateway key"},
			{"gemini-key", "POST", "/v1beta/models/m:generateContent", "", "", 401, true, "send an enabled WAN gateway key"},
			{"forbidden-path", "POST", "/api/wan", secret, "", 404, false, "supported inference routes only"},
			{"forbidden-method", "DELETE", "/v1/models", secret, "", 404, false, "supported inference routes only"},
			{"noncanonical-path", "POST", "/v1/./messages", secret, "", 404, false, "supported inference routes only"},
			{"unsupported-gemini-call", "POST", "/v1beta/models/m:delete", secret, "", 404, false, "supported inference routes only"},
			{"invalid-query", "POST", "/v1/messages?key=%zz", secret, "", 401, true, "not in the URL"},
			{"query-key", "POST", "/v1/messages?key=unused", secret, "", 401, true, "not in the URL"},
			{"allowed-options", "OPTIONS", "/v1/messages", "", "Access-Control-Request-Method: POST\r\nAccess-Control-Request-Headers: Authorization\r\n", 204, true, ""},
			{"forbidden-options", "OPTIONS", "/api/wan", "", "Access-Control-Request-Method: POST\r\n", 404, false, "supported inference routes only"},
			{"disallowed-origin-options", "OPTIONS", "/v1/messages", "", "Access-Control-Request-Method: POST\r\n", 404, false, "supported inference routes only"},
		} {
			t.Run(body.name+"/"+tc.name, func(t *testing.T) {
				conn, err := net.DialTimeout("tcp4", server.Listener.Addr().String(), 10*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				// A deadlock guard, not a latency assertion. Never finish the body
				// or close the client's write side to help the server respond.
				if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				auth := ""
				if tc.key != "" {
					auth = "Authorization: Bearer " + tc.key + "\r\n"
				}
				requestOrigin := origin
				if tc.name == "disallowed-origin-options" {
					requestOrigin = "https://unlisted.example"
				}
				if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: localhost\r\nOrigin: %s\r\n%s\r\n%s%s\r\n%s", tc.method, tc.path, requestOrigin, body.header, auth, tc.extra, body.prefix); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(conn)
				resp, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatalf("WAN refusal waited for the unfinished %s body: %v", body.name, err)
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("WAN refusal body did not finish: %v", err)
				}
				if resp.StatusCode != tc.status || !resp.Close || resp.Proto != "HTTP/1.1" {
					t.Fatalf("refusal status=%d close=%v protocol=%s, want %d and closed HTTP/1.1", resp.StatusCode, resp.Close, resp.Proto, tc.status)
				}
				wantOrigin := ""
				if tc.cors {
					wantOrigin = origin
				}
				if got := resp.Header.Get("Access-Control-Allow-Origin"); got != wantOrigin {
					t.Fatalf("ACAO=%q, want %q", got, wantOrigin)
				}
				if tc.status == 204 {
					if len(data) != 0 || !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "POST") {
						t.Fatalf("preflight changed: headers=%v body=%q", resp.Header, data)
					}
				} else if !strings.Contains(string(data), tc.message) || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
					t.Fatalf("error envelope changed: headers=%v body=%q", resp.Header, data)
				}
				if strings.Contains(string(data), secret) || strings.Contains(string(data), "invalid-key") {
					t.Fatal("refusal exposed a credential")
				}
				if _, err := reader.ReadByte(); err != io.EOF {
					t.Fatalf("rejected connection remained open: %v", err)
				}
			})
		}
	}
}
