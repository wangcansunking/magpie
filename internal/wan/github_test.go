package wan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func localGitHub(t *testing.T, handle http.HandlerFunc) githubDeviceClient {
	t.Helper()
	s := httptest.NewServer(handle)
	t.Cleanup(s.Close)
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	c := newGitHubDeviceClient()
	c.base, c.client.Transport = s.URL, transport
	return c
}

func deviceResponse() string {
	return `{"device_code":"fake-device-code","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`
}

func TestWANDeviceGrantUsesOnlyItsOwnClientID(t *testing.T) {
	var calls atomic.Int32
	c := localGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("device request did not use the documented JSON form exchange")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("device request borrowed credentials")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		want := url.Values{"client_id": {"own-app-id"}}
		switch r.URL.Path {
		case "/login/device/code":
			fmt.Fprint(w, deviceResponse())
		case "/login/oauth/access_token":
			want.Set("device_code", "fake-device-code")
			want.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
			fmt.Fprint(w, `{"access_token":"fake-wan-token","token_type":"bearer","scope":""}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		if r.Form.Encode() != want.Encode() {
			t.Errorf("request included something beyond the explicit app grant: %v", r.Form)
		}
	})
	code, err := c.code(context.Background(), "own-app-id", "")
	if err != nil || code.UserCode != "ABCD-1234" || code.Interval != 5 || code.ExpiresIn != 900 {
		t.Fatalf("device code: %#v %v", code, err)
	}
	grant, err := c.poll(context.Background(), "own-app-id", code.DeviceCode)
	if err != nil || grant.Access != "fake-wan-token" || grant.Scope != "" || grant.Refresh != "" {
		t.Fatalf("grant: %#v %v", grant, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("requested %d times, want the two explicit calls only", calls.Load())
	}
}

func TestWANDeviceCodeClassifiesInitialRefusal(t *testing.T) {
	for code, message := range map[string]string{
		"device_flow_disabled":         "unavailable",
		"incorrect_client_credentials": "Client ID",
		"unknown-SECRET":               "refused",
	} {
		t.Run(code, func(t *testing.T) {
			c := localGitHub(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"error":%q,"error_description":"SECRET"}`, code)
			})
			out, err := c.code(context.Background(), "own-app-id", "")
			if err == nil || !strings.Contains(err.Error(), message) || strings.Contains(err.Error(), "SECRET") || out.DeviceCode != "" {
				t.Fatalf("incorrect initial authorization refusal: %#v %v", out, err)
			}
		})
	}
}

func TestWANDeviceCodeRejectsUntrustedDisplayData(t *testing.T) {
	for name, body := range map[string]string{
		"foreign URL":          strings.ReplaceAll(deviceResponse(), "https://github.com/login/device", "https://attacker.example/login"),
		"URL query":            strings.ReplaceAll(deviceResponse(), "https://github.com/login/device", "https://github.com/login/device?token=secret"),
		"URL credentials":      strings.ReplaceAll(deviceResponse(), "https://github.com/login/device", "https://secret@github.com/login/device"),
		"missing expiry":       strings.ReplaceAll(deviceResponse(), `"expires_in":900`, `"expires_in":0`),
		"huge expiry":          strings.ReplaceAll(deviceResponse(), `"expires_in":900`, `"expires_in":2147483647`),
		"negative interval":    strings.ReplaceAll(deviceResponse(), `"interval":5`, `"interval":-1`),
		"invalid code":         strings.ReplaceAll(deviceResponse(), "ABCD-1234", "<script>"),
		"missing private code": strings.ReplaceAll(deviceResponse(), "fake-device-code", ""),
		"large response":       strings.TrimSuffix(deviceResponse(), "}") + `,"padding":"` + strings.Repeat("x", 32<<10) + `"}`,
		"trailing JSON":        deviceResponse() + `{}`,
		"null":                 "null",
	} {
		t.Run(name, func(t *testing.T) {
			c := localGitHub(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			if code, err := c.code(context.Background(), "own-app-id", ""); err == nil || code.DeviceCode != "" {
				t.Fatalf("accepted untrusted code response: %#v %v", code, err)
			}
		})
	}
}

func TestWANDevicePollClassifiesWithoutEchoingSecrets(t *testing.T) {
	for _, response := range []struct {
		body    string
		pending string
		message string
	}{
		{`{"error":"authorization_pending","error_description":"SECRET"}`, "authorization_pending", ""},
		{`{"error":"slow_down","interval":10,"error_description":"SECRET"}`, "slow_down", ""},
		{`{"error":"access_denied","error_description":"SECRET"}`, "", "denied"},
		{`{"error":"expired_token","error_description":"SECRET"}`, "", "expired"},
		{`{"error":"device_flow_disabled","error_description":"SECRET"}`, "", "unavailable"},
		{`{"error":"incorrect_client_credentials","error_description":"SECRET"}`, "", "Client ID"},
		{`{"error":"unknown-SECRET","error_description":"SECRET"}`, "", "refused"},
		{`{"error":"authorization_pending","access_token":"SECRET"}`, "", "ambiguous"},
		{`{"error":"slow_down","refresh_token":"SECRET"}`, "", "ambiguous"},
		{`{"error":"slow_down","interval":-1}`, "", "interval"},
		{`{"access_token":"SECRET","token_type":"basic"}`, "", "unsupported"},
		{`{"access_token":"SECRET","token_type":"bearer","expires_in":-1}`, "", "unsupported"},
		{`{"access_token":"SECRET\n","token_type":"bearer"}`, "", "unsupported"},
		{`{"access_token":"SECRET","token_type":"bearer","refresh_token":"bad token"}`, "", "unsupported"},
		{`{}`, "", "unsupported"},
	} {
		t.Run(response.pending+response.message+response.body, func(t *testing.T) {
			c := localGitHub(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response.body) })
			grant, err := c.poll(context.Background(), "own-app-id", "fake-device-code")
			if response.pending != "" {
				if err != nil || grant.Error != response.pending || grant.Access != "" || grant.Refresh != "" {
					t.Fatalf("pending outcome: %#v %v", grant, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), response.message) || strings.Contains(err.Error(), "SECRET") || grant.Access != "" || grant.Refresh != "" {
				t.Fatalf("unsafe or incorrect error: %#v %v", grant, err)
			}
		})
	}
}

func TestWANDeviceRequestDoesNotFollowRedirect(t *testing.T) {
	var followed atomic.Bool
	c := localGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/capture" {
			followed.Store(true)
			fmt.Fprint(w, deviceResponse())
			return
		}
		w.Header().Set("Location", "/capture")
		w.WriteHeader(http.StatusTemporaryRedirect)
		fmt.Fprint(w, "SECRET")
	})
	if _, err := c.poll(context.Background(), "own-app-id", "fake-device-code"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("redirect accepted or leaked: %v", err)
	}
	if followed.Load() {
		t.Fatal("device grant was submitted to a redirected endpoint")
	}
}

func TestWANDeviceRequestCancellationClosesHeldBody(t *testing.T) {
	entered, closed := make(chan struct{}), make(chan struct{})
	c := localGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(closed)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	t.Cleanup(func() { cancel(); waitDone(t, closed) })
	go func() { _, err := c.poll(ctx, "own-app-id", "fake-device-code"); done <- err }()
	waitDone(t, entered)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled response was not classified as cancellation: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("canceled device request did not finish")
	}
	waitDone(t, closed)
}

type githubRoundTrip func(*http.Request) (*http.Response, error)

func (f githubRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWANDeviceRequestsHaveDeadlineAndSafeTransportErrors(t *testing.T) {
	c := githubDeviceClient{base: "https://github.invalid", client: &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("device request has no deadline")
		}
		return nil, errors.New("proxy https://SECRET@proxy.invalid:1")
	})}}
	if _, err := c.code(context.Background(), "own-app-id", ""); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe transport error: %v", err)
	}
	c.client.Transport = githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.ReplaceAll(deviceResponse(), `,"interval":5`, "")))}, nil
	})
	code, err := c.code(context.Background(), "own-app-id", "")
	if err != nil || code.Interval != 5 {
		t.Fatalf("missing interval should use documented default: %#v %v", code, err)
	}
}
