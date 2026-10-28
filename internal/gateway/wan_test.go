package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

func newWANCaller(t *testing.T, name string) (access.Identity, string) {
	t.Helper()
	secret, err := access.UpdateWAN("add-key", access.Change{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	who, ok := access.AuthenticateWAN(secret)
	if !ok {
		t.Fatal("created WAN key did not authenticate")
	}
	return who, secret
}

func wanCall(h http.Handler, method, target, body, secret string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5000"
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var wanInferencePaths = []struct{ method, path string }{
	{"GET", "/v1/models"}, {"GET", "/models"}, {"GET", "/v1/models/plan/m1"}, {"GET", "/muse-code/models"}, {"GET", "/v1beta/models"},
	{"POST", "/v1/chat/completions"}, {"POST", "/chat/completions"},
	{"POST", "/v1/responses"}, {"POST", "/responses"}, {"GET", "/v1/responses"}, {"GET", "/responses"},
	{"POST", "/v1/messages"}, {"POST", "/messages"}, {"POST", "/v1/messages/count_tokens"},
	{"POST", "/v1/images/generations"}, {"POST", "/images/generations"}, {"POST", "/v1/images/edits"}, {"POST", "/images/edits"},
	{"POST", "/v1/embeddings"}, {"POST", "/embeddings"}, {"POST", "/v1/rerank"}, {"POST", "/rerank"},
	{"POST", "/v1/audio/transcriptions"}, {"POST", "/audio/transcriptions"}, {"POST", "/v1/audio/translations"}, {"POST", "/audio/translations"},
	{"POST", "/v1/videos"}, {"POST", "/videos"}, {"GET", "/v1/videos/id"}, {"GET", "/videos/id"},
	{"GET", "/v1/videos/id/content"}, {"GET", "/videos/id/content"},
	{"POST", "/v1beta/models/plan/m1:generateContent"}, {"POST", "/v1beta/models/plan/m1:streamGenerateContent"}, {"POST", "/v1beta/models/plan/m1:countTokens"},
}

func TestWANInferenceAlwaysNeedsWANKey(t *testing.T) {
	fresh(t)
	local, err := access.Update("add-key", access.Change{Name: "Local"})
	if err != nil {
		t.Fatal(err)
	}
	who, secret := newWANCaller(t, "Laptop")
	h := New().WANHandler()
	for _, route := range wanInferencePaths {
		for _, key := range []string{"", Token, TokenFor("codex"), local, access.WANPrefix + "invalid"} {
			w := wanCall(h, route.method, route.path, `{}`, key)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s accepts wrong scope or lacks inference route: %d %s", route.method, route.path, w.Code, w.Body.String())
			}
			if len(key) > len(access.WANPrefix) && strings.Contains(w.Body.String(), key) {
				t.Fatal("authentication error exposed credential")
			}
		}
	}
	for _, trust := range []string{"", "1"} {
		t.Setenv("MAGPIE_TRUST_PROXY", trust)
		for _, peer := range []string{"127.0.0.1:4000", "[::1]:4000", "198.51.100.20:4000", ""} {
			for _, header := range append([]string{""}, forwardHeaders...) {
				for _, key := range []string{"", Token, secret} {
					r := httptest.NewRequest("GET", "/v1/models", nil)
					r.RemoteAddr = peer
					r.Header.Set("Authorization", "Bearer "+key)
					if header != "" {
						r.Header.Set(header, "127.0.0.1")
					}
					// A context supplied by an embedding caller is no replacement
					// for authenticating this request's current credential.
					r = r.WithContext(access.WithIdentity(r.Context(), who))
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					want := 401
					if key == secret {
						want = 200
					}
					if w.Code != want {
						t.Fatalf("trust=%q peer=%q forward=%q: %d, want %d", trust, peer, header, w.Code, want)
					}
				}
			}
		}
	}
	for _, action := range []string{"off-key", "rotate-key", "remove-key"} {
		if _, err := access.UpdateWAN(action, access.Change{Key: who.KeyID}); err != nil {
			t.Fatal(err)
		}
		if w := wanCall(h, "GET", "/v1/models", "", secret); w.Code != 401 {
			t.Fatal("WAN lifecycle did not revoke immediately", action, w.Code)
		}
	}
	if s := settings.Load(); s.LAN {
		t.Fatal("test must prove WAN works without LAN sharing")
	}
}

func TestWANRejectsControlAliasesEncodedPathsAndRedirects(t *testing.T) {
	fresh(t)
	_, secret := newWANCaller(t, "Laptop")
	h := New().WANHandler()
	addCallback("wan-live-callback")
	t.Cleanup(func() { dropCallback("wan-live-callback") })
	for _, target := range []string{
		"/", "/v1", "/v1/", "/api/hello", "/api/wan", "/api/providers", "/api/gateway/keys",
		"/mcp/tool", SearchMCPPath, VisionMCPPath, ToolsMCPPath, claudeMCPPath + "wan-live-callback",
		"/v1/magpie/quotas", "/v1/magpie/usage", "/v1/magpie/usage/requests", "/v1/magpie/route", "/v1/magpie/concurrency", "/v1/magpie/limit",
		HeapPath, CPUPath,
		provider.RemoteCardsPath, provider.RemoteRefreshPath, CodexPath, CodexPath + "/responses", CodexCatalogPath,
		"/v1/systemone", "/v1/alpha/search", "/alpha/search", "/responses/compact", "/v1/responses/compact", "/messages/count_tokens",
		"/v1beta/models/m:delete", "/v1beta/models/m", "/v1beta/models/:generateContent",
		"//v1/models", "/v1/./models", "/v1/x/../models", "/v1/models/", "/v1/models/../magpie/quotas",
		"/%61pi/hello", "/v1/%6dodels", "/v1/models%2f..%2fmagpie%2fquotas", "/v1/models/%252e%252e/magpie/quotas",
		"/v1/models/%2e%2e/magpie/quotas", "/v1/models/%5cmagpie", "/v1/models/%00", "/v1/chat/completions/",
	} {
		for _, method := range []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "CONNECT", "TRACE"} {
			w := wanCall(h, method, target, `{}`, secret)
			if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" {
				t.Fatalf("WAN exposed or redirected %s %s: %d %s", method, target, w.Code, w.Body.String())
			}
		}
	}
}

func TestWANKeyNeverAcceptedByOrdinaryIngress(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_TRUST_PROXY", "1")
	_, secret := newWANCaller(t, "Laptop")
	local, err := access.Update("add-key", access.Change{Name: "Local"})
	if err != nil {
		t.Fatal(err)
	}
	s := New()
	for name, h := range map[string]http.Handler{"embedded": s.Handler(), "listener": lanGuard(s.Handler()), "caller": callerGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))} {
		for _, key := range []string{secret, access.WANPrefix + "invalid"} {
			for _, field := range []string{"Authorization", "x-api-key", "x-goog-api-key", "query"} {
				for _, method := range []string{"GET", "OPTIONS"} {
					r := httptest.NewRequest(method, "/v1/models", nil)
					r.RemoteAddr = "127.0.0.1:5000"
					r.Header.Set("Authorization", "Bearer "+local)
					if field == "query" {
						r.URL.RawQuery = "key=" + key
					} else if field == "Authorization" {
						r.Header.Set(field, "Bearer "+key)
					} else {
						r.Header.Set(field, key)
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != 401 {
						t.Fatalf("%s accepted WAN via %s on %s: %d", name, field, method, w.Code)
					}
				}
			}
		}
		if w := wanCall(h, "GET", "/v1/models", "", "stale-local-placeholder"); w.Code != 200 {
			t.Fatal("ordinary loopback permissiveness changed", name, w.Code)
		}
	}
}

func TestWANRepeatedCredentialsNeverLeakOrEnterLocal(t *testing.T) {
	fresh(t)
	_, secret := newWANCaller(t, "Laptop")
	for _, field := range []string{"Authorization", "x-api-key", "x-goog-api-key", "query"} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		if field == "query" {
			r.URL.RawQuery = "key=&key=" + secret
		} else {
			r.Header[http.CanonicalHeaderKey(field)] = []string{"", secret}
		}
		w := httptest.NewRecorder()
		New().Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("ordinary ingress accepted repeated WAN credential", field, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header["X-Api-Key"] = []string{"", secret}
	r.Header["X-Goog-Api-Key"] = []string{"", secret}
	called := false
	wanGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		for _, field := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
			if values := r.Header.Values(field); len(values) != 1 || values[0] != Token {
				t.Error("repeated credential survived sanitizing", field)
			}
		}
	})).ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("valid repeated key refused")
	}
}

func TestWANIdentitySanitizedRemoteAndHeaderOnly(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_TRUST_PROXY", "1")
	who, secret := newWANCaller(t, "Laptop")
	var fetched atomic.Int32
	private := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fetched.Add(1) }))
	defer private.Close()
	var calls int
	h := wanGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !reflect.DeepEqual(access.Caller(r.Context()), who) || local(r) || sharedWith(r) {
			t.Error("WAN caller gained local privilege or lost identity")
		}
		fetch, err := http.NewRequestWithContext(r.Context(), "GET", private.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := publicClient.Do(fetch)
		if response != nil {
			response.Body.Close()
		}
		if !errors.Is(err, errPrivateAddress) || fetched.Load() != 0 {
			t.Error("WAN request reached a private download address", err)
		}
		if r.Header.Get("Authorization") != "Bearer "+Token || r.Header.Get("x-api-key") != Token || r.Header.Get("x-goog-api-key") != Token {
			t.Error("credential was not sanitized")
		}
	}))
	for _, field := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
		r := httptest.NewRequest("GET", "/v1/models?alt=json", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		for _, h := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
			r.Header.Set(h, "placeholder")
		}
		r.Header.Set(field, secret)
		if field == "Authorization" {
			r.Header.Set(field, "Bearer "+secret)
		}
		original := r.Clone(r.Context())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !reflect.DeepEqual(r.Header, original.Header) || *r.URL != *original.URL {
			t.Fatal("WAN authentication changed caller request", field, w.Code)
		}
	}
	if calls != 3 {
		t.Fatal("not every protocol's key header authenticated")
	}
	for _, query := range []string{"key=" + secret, "key=", "key=other&key=" + secret, "key=bad;key=" + secret, "key=%zz"} {
		if w := wanCall(h, "GET", "/v1/models?"+query, "", secret); w.Code != 401 {
			t.Fatal("URL credential reached inference", query, w.Code)
		}
	}
}

func TestWANCORSOnlyPreflightsRegisteredInference(t *testing.T) {
	fresh(t)
	_, secret := newWANCaller(t, "Laptop")
	s := settings.Load()
	s.CORSOrigins = []string{"https://client.example"}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	h := New().WANHandler()
	for _, target := range []string{"/v1/models", "/api/wan", "/v1/magpie/quotas", "/mcp/tool"} {
		r := httptest.NewRequest("OPTIONS", target, nil)
		r.Header.Set("Origin", s.CORSOrigins[0])
		r.Header.Set("Access-Control-Request-Method", "GET")
		r.Header.Set("Access-Control-Request-Headers", "Authorization")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if target == "/v1/models" {
			if w.Code != 204 || w.Body.Len() != 0 || w.Header().Get("Access-Control-Allow-Origin") != s.CORSOrigins[0] {
				t.Fatal("registered origin preflight", w.Code, w.Body.String())
			}
		} else if w.Code != 404 || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("preflight advertised forbidden route", target, w.Code)
		}
	}
	for _, origin := range []string{"https://client.example", "https://foreign.example", "null", "http://localhost"} {
		for _, key := range []string{"", secret} {
			r := httptest.NewRequest("GET", "/v1/models", nil)
			r.RemoteAddr = "127.0.0.1:5000"
			r.Header.Set("Origin", origin)
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 401
			if key == secret {
				want = 200
				if origin == "https://foreign.example" {
					want = 403
				}
			}
			if w.Code != want || w.Header().Get("Access-Control-Allow-Origin") == "*" {
				t.Fatal("WAN changed CORS or key boundary", origin, w.Code)
			}
			if origin == "https://client.example" && w.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("listed browser cannot read its authentication error", w.Code)
			}
		}
	}
}

// A transport, not a test listener: every upstream call stays in memory.
type wanTransport func(*http.Request) (*http.Response, error)

func (f wanTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func wanUpstream(t *testing.T, serve func(*http.Request) *http.Response) *Server {
	t.Helper()
	if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "upstream-secret", Chat: "http://wan-upstream.invalid/v1", Models: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.client.Transport = wanTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "wan-upstream.invalid" {
			t.Errorf("unexpected outbound host: %s", r.URL.Host)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("WAN credential replaced the upstream's own key")
		}
		for _, values := range r.Header {
			for _, v := range values {
				if strings.Contains(v, access.WANPrefix) {
					t.Error("WAN secret leaked to upstream header")
				}
			}
		}
		return serve(r), nil
	})
	return s
}

func wanJSONReply(r *http.Request) *http.Response {
	var ask struct{ Stream bool }
	json.NewDecoder(r.Body).Decode(&ask)
	if ask.Stream {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")), Request: r}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)), Request: r}
}

func TestWANProtocolsKeepModelConstraintsAndUsage(t *testing.T) {
	fresh(t)
	var calls atomic.Int64
	s := wanUpstream(t, func(r *http.Request) *http.Response { calls.Add(1); return wanJSONReply(r) })
	who, secret := newWANCaller(t, "Laptop")
	if _, err := access.UpdateWAN("models-key", access.Change{Key: who.KeyID, Models: []string{"plan/m1"}}); err != nil {
		t.Fatal(err)
	}
	h := s.WANHandler()
	for _, route := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"plan/MODEL","messages":[{"role":"user","content":"hi"}]}`},
		{"/chat/completions", `{"model":"plan/MODEL","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"plan/MODEL","input":"hi"}`},
		{"/responses", `{"model":"plan/MODEL","input":"hi"}`},
		{"/v1/messages", `{"model":"plan/MODEL","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{"/messages", `{"model":"plan/MODEL","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1beta/models/plan/MODEL:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	} {
		before := calls.Load()
		path := strings.ReplaceAll(route.path, "MODEL", "m2")
		body := strings.ReplaceAll(route.body, "MODEL", "m2")
		if w := wanCall(h, "POST", path, body, secret); w.Code != 403 || calls.Load() != before {
			t.Fatalf("%s did not hold model: %d %s", path, w.Code, w.Body.String())
		}
		path = strings.ReplaceAll(route.path, "MODEL", "m1")
		body = strings.ReplaceAll(route.body, "MODEL", "m1")
		if w := wanCall(h, "POST", path, body, secret); w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
			t.Fatalf("%s did not answer: %d %s", path, w.Code, w.Body.String())
		}
		if rec := lastUsage(t); rec.CallerKeyID != who.KeyID || rec.Local {
			t.Fatal("inference lost remote caller attribution")
		}
	}
	for _, path := range []string{"/v1/messages/count_tokens", "/v1beta/models/plan/m2:countTokens"} {
		before := calls.Load()
		w := wanCall(h, "POST", path, `{"model":"plan/m2","messages":[{"role":"user","content":"hi"}],"contents":[{"parts":[{"text":"hi"}]}]}`, secret)
		if w.Code != 403 || calls.Load() != before {
			t.Fatal("counting bypassed model restriction", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/v1/models", "/models", "/v1beta/models"} {
		w := wanCall(h, "GET", path, "", secret)
		if w.Code != 200 || strings.Contains(w.Body.String(), "m2") || !strings.Contains(w.Body.String(), "m1") {
			t.Fatal("WAN model discovery bypassed model restrictions", path, w.Code, w.Body.String())
		}
	}
	b, err := os.ReadFile(usage.Path())
	if err != nil || strings.Contains(string(b), secret) {
		t.Fatal("WAN secret in usage or unreadable usage", err)
	}
}

func TestWANMediaAndRetrievalKeepModelAndAccountConstraints(t *testing.T) {
	fresh(t)
	var calls atomic.Int64
	s := wanUpstream(t, func(r *http.Request) *http.Response { calls.Add(1); return wanJSONReply(r) })
	who, secret := newWANCaller(t, "Held")
	if _, err := access.UpdateWAN("models-key", access.Change{Key: who.KeyID, Models: []string{"plan/m1"}}); err != nil {
		t.Fatal(err)
	}
	h := s.WANHandler()
	for _, endpoint := range []string{"/v1/images/generations", "/images/generations", "/v1/images/edits", "/images/edits", "/v1/videos", "/videos", "/v1/embeddings", "/embeddings", "/v1/rerank", "/rerank"} {
		body := `{"model":"plan/m2","prompt":"hi","input":"hi","query":"hi","documents":["hi"],"image":"data:image/png;base64,aGk="}`
		if w := wanCall(h, "POST", endpoint, body, secret); w.Code != 403 || calls.Load() != 0 {
			t.Fatal("media/retrieval bypassed model hold", endpoint, w.Code, w.Body.String())
		}
	}
	for _, endpoint := range []string{"/v1/audio/transcriptions", "/audio/transcriptions", "/v1/audio/translations", "/audio/translations"} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if err := form.WriteField("model", "plan/m2"); err != nil {
			t.Fatal(err)
		}
		file, err := form.CreateFormFile("file", "hello.wav")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("fake audio")); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", endpoint, &body)
		r.Header.Set("Content-Type", form.FormDataContentType())
		r.Header.Set("Authorization", "Bearer "+secret)
		r.RemoteAddr = "127.0.0.1:5000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 || calls.Load() != 0 {
			t.Fatal("audio bypassed model hold", endpoint, w.Code, w.Body.String())
		}
	}
	// A formerly valid account restriction becomes unmatched after the
	// upstream key changes; it must not turn into unrestricted WAN access.
	if _, err := access.UpdateWAN("accounts-key", access.Change{Key: who.KeyID, Accounts: []string{"plan/" + provider.KeyID("upstream-secret")}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "different-upstream-secret", Chat: "http://wan-upstream.invalid/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	if w := wanCall(h, "POST", "/v1/chat/completions", chatReq, secret); w.Code != 403 || calls.Load() != 0 {
		t.Fatal("WAN ignored account restriction", w.Code, w.Body.String())
	}
}

func TestWANBudgetSurvivesRestartAndIsPerKey(t *testing.T) {
	fresh(t)
	budget.Forget()
	now := time.Now()
	limitClock = func() time.Time { return now }
	t.Cleanup(func() { limitClock = time.Now })
	var calls atomic.Int64
	s := wanUpstream(t, func(r *http.Request) *http.Response { calls.Add(1); return wanJSONReply(r) })
	a, secret := newWANCaller(t, "Capped")
	_, other := newWANCaller(t, "Other")
	if _, err := access.UpdateWAN("limit-key", access.Change{Key: a.KeyID, Limit: &access.Limit{Period: "day", Tokens: 35}}); err != nil {
		t.Fatal(err)
	}
	h := s.WANHandler()
	if w := wanCall(h, "POST", "/v1/chat/completions", chatReq, secret); w.Code != 200 {
		t.Fatal("within budget", w.Code, w.Body.String())
	}
	for range 2 {
		if w := wanCall(h, "POST", "/v1/chat/completions", chatReq, secret); w.Code != 429 || calls.Load() != 1 {
			t.Fatal("spent WAN key reached provider", w.Code, w.Body.String())
		}
		budget.Forget()
		h = s.WANHandler()
	}
	if w := wanCall(h, "POST", "/v1/chat/completions", chatReq, other); w.Code != 200 || calls.Load() != 2 {
		t.Fatal("A's budget held B", w.Code, w.Body.String())
	}
}

// flushNotice observes the first streamed write while the fake upstream is
// held open. Waiting for state, not elapsed time, proves forwarding is live.
type wanFlushNotice struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func (w *wanFlushNotice) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.flushed) })
}

func TestWANStreamFinishesAfterRevocationAndCancels(t *testing.T) {
	fresh(t)
	who, secret := newWANCaller(t, "Laptop")
	reader, writer := io.Pipe()
	release := make(chan struct{})
	var releaseOnce sync.Once
	doneWriting := make(chan struct{})
	s := wanUpstream(t, func(r *http.Request) *http.Response {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: r}
	})
	h := s.WANHandler()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("Authorization", "Bearer "+secret)
	w := &wanFlushNotice{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, r) }()
	go func() {
		defer close(doneWriting)
		defer writer.Close()
		io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		<-release
		io.WriteString(writer, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
	}()
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		reader.Close()
		writer.Close()
		<-done
		<-doneWriting
	})
	select {
	case <-w.flushed:
	case <-done:
		t.Fatal("stream ended before its first flushed event")
	case <-time.After(10 * time.Second):
		t.Fatal("first event buffered until upstream EOF")
	}
	if _, err := access.UpdateWAN("off-key", access.Change{Key: who.KeyID}); err != nil {
		t.Fatal(err)
	}
	if next := wanCall(h, "GET", "/v1/models", "", secret); next.Code != 401 {
		t.Fatal("revocation allowed new request", next.Code)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not finish")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), "first") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("revocation broke an authenticated stream", w.Code, w.Body.String())
	}
	if rec := lastUsage(t); rec.CallerKeyID != who.KeyID || rec.Local {
		t.Fatal("stream lost WAN attribution")
	}
	// Cancellation is preserved at the ingress without starting a listener.
	cancel()
	var downstream context.Context
	h2 := wanGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downstream = r.Context() }))
	if _, err := access.UpdateWAN("on-key", access.Change{Key: who.KeyID}); err != nil {
		t.Fatal(err)
	}
	h2.ServeHTTP(httptest.NewRecorder(), r)
	if downstream == nil || downstream.Err() != context.Canceled {
		t.Fatal("WAN ingress lost request cancellation")
	}
}

func TestWANCorruptCredentialsRefusedByHandler(t *testing.T) {
	fresh(t)
	_, secret := newWANCaller(t, "Laptop")
	h := New().WANHandler()
	if err := os.WriteFile(access.Path(), []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := wanCall(h, "GET", "/v1/models", "", secret); w.Code != 401 {
		t.Fatal("corrupt store authenticated at ingress", w.Code)
	}
}

func TestWANModelResponseIsJSON(t *testing.T) {
	fresh(t)
	_, secret := newWANCaller(t, "Laptop")
	w := wanCall(New().WANHandler(), "GET", "/v1/models", "", secret)
	var body map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["object"] != "list" {
		t.Fatal("WAN did not reuse standard models API", w.Code, w.Body.String())
	}
}
