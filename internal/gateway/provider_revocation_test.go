package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
)

// A stream must not enable a process-global catalog cache for later
// requests. Another process switching the provider off takes effect before
// its next request, without interrupting the stream already admitted.
func TestExternalProviderRevocationDuringStream(t *testing.T) {
	for _, c := range []struct {
		name                  string
		wan, held, unreadable bool
	}{
		{name: "local"},
		{name: "wan", wan: true},
		{name: "wan_gui_hold", wan: true, held: true},
		{name: "wan_gui_hold_unreadable", wan: true, held: true, unreadable: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fresh(t)
			plugin.UseCached(nil)
			var calls atomic.Int64
			s := wanUpstream(t, wanJSONReply)
			h, secret := s.Handler(), ""
			if c.wan {
				_, secret = newWANCaller(t, "external revocation")
				h = s.WANHandler()
			}
			// Prepare the other process's state before opening the stream.
			b, err := os.ReadFile(provider.Path())
			if err != nil {
				t.Fatal(err)
			}
			var f map[string]json.RawMessage
			if err := json.Unmarshal(b, &f); err != nil {
				t.Fatal(err)
			}
			var ps []map[string]any
			if err := json.Unmarshal(f["providers"], &ps); err != nil {
				t.Fatal(err)
			}
			changed := false
			for _, p := range ps {
				if p["id"] == "plan" {
					p["off"], changed = true, true
				}
			}
			if !changed {
				t.Fatal("fixture provider missing")
			}
			f["providers"], err = json.Marshal(ps)
			if err != nil {
				t.Fatal(err)
			}
			revoked, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}

			// The rejected cache lasts one second. Fake time stays fixed while
			// the writer and second handler run, however slow the host is; this
			// regression cannot pass merely because a loaded runner aged it out.
			synctest.Test(t, func(t *testing.T) {
				if c.held {
					// The GUI already holds these process-global caches. WAN
					// admission must not inherit its pre-revocation snapshot.
					defer provider.Hold()()
					defer filememo.Hold()()
				}
				entered, release := make(chan struct{}), make(chan struct{})
				letGo := sync.OnceFunc(func() { close(release) })
				ctx, cancel := context.WithCancel(context.Background())
				transport := s.client.Transport
				s.client.Transport = wanTransport(func(r *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						close(entered)
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					return transport.RoundTrip(r)
				})
				first := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hold"}]}`)).WithContext(ctx)
					r.RemoteAddr = "127.0.0.1:5000"
					if secret != "" {
						r.Header.Set("Authorization", "Bearer "+secret)
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					first <- w
				}()
				defer func() {
					cancel()
					letGo()
					<-first
					s.client.Transport = transport
				}()
				select {
				case <-entered:
				case w := <-first:
					first <- w // cleanup still joins the completed request
					t.Fatalf("first request never reached upstream: %d %s", w.Code, w.Body.String())
				case <-time.After(10 * time.Second):
					t.Fatal("first request never reached in-memory upstream")
				}
				at := time.Now()
				// Deliberately bypass Save/Changed, as an editor or other process does.
				if c.unreadable {
					// A directory is unreadable as a JSON file on every OS;
					// permission bits alone do not reproduce this on Windows.
					if err := os.Remove(provider.Path()); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(provider.Path(), 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(provider.Path(), revoked, 0o600); err != nil {
					t.Fatal(err)
				}
				if !time.Now().Equal(at) {
					t.Fatal("fixture aged the held cache before the second admission")
				}
				second := wanCall(h, "POST", "/v1/chat/completions", `{"model":"plan/m1","messages":[{"role":"user","content":"after revocation"}]}`, secret)
				// An unreadable file is retried by steady.ReadFile on Windows,
				// advancing fake time only after invalidation reached that read.
				// The Off case has no retries and must keep time fixed throughout.
				if !c.unreadable && !time.Now().Equal(at) {
					t.Fatal("fixture advanced fake time during external revocation")
				}
				if calls.Load() != 1 || second.Code != http.StatusNotFound {
					t.Errorf("request spent revoked provider while prior stream remained open: status=%d calls=%d body=%s", second.Code, calls.Load(), second.Body.String())
				}
				letGo()
				w := <-first
				first <- w
				if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") {
					t.Errorf("revocation interrupted admitted stream: %d %s", w.Code, w.Body.String())
				}
			})
		})
	}
}
