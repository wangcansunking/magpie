package gateway

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
)

// wanRemote is set only by the WAN ingress, not a forwarding header or an
// exported context key. Even a loopback tunnel under TRUST_PROXY stays
// remote for every downstream privilege and usage decision.
type wanRemote struct{}

func wanRequest(r *http.Request) bool {
	remote, _ := r.Context().Value(wanRemote{}).(bool)
	return remote
}

// WANHandler serves only the inference routes, always with a WAN key. It
// has no dependency on the ordinary listener, sharing, or proxy trust.
// The owner of its listener controls lifetime; stopping a key applies to
// new requests while already authenticated streams finish normally.
func (s *Server) WANHandler() http.Handler {
	mux := http.NewServeMux()
	s.inferenceRoutes(mux)
	return wanSurface(mux, wanGuard(s.apiHandler(mux)))
}

// wanSurface rejects noncanonical paths before ServeMux can redirect or
// unescape them. Membership comes from the actual inference registrations,
// not an allow-list of prefixes that could grow administrative subroutes.
// A listed origin may preflight a supported route without a key; it never
// reaches business logic, and does not establish an authenticated caller.
func wanSurface(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if r.URL.RawPath != "" || p == "" || p != path.Clean(p) || strings.ContainsAny(p, "\\%\x00") {
			wanNotFound(w, r)
			return
		}
		probe := r
		if r.Method == http.MethodOptions {
			probe = r.Clone(r.Context())
			probe.Method = r.Header.Get("Access-Control-Request-Method")
		}
		_, pattern := mux.Handler(probe)
		if pattern == "" {
			wanNotFound(w, r)
			return
		}
		if pattern == "POST /v1beta/models/{call...}" {
			call := strings.TrimPrefix(p, "/v1beta/models/")
			i := strings.LastIndex(call, ":")
			if i <= 0 || (call[i+1:] != "generateContent" && call[i+1:] != "streamGenerateContent" && call[i+1:] != "countTokens") {
				wanNotFound(w, r)
				return
			}
		}
		if r.Method == http.MethodOptions {
			if corsAllowed(r.Header.Get("Origin")) {
				wanCloseRejectedBody(w, r)
				corsGuard(http.NotFoundHandler()).ServeHTTP(w, r)
			} else {
				wanNotFound(w, r)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// A refused request or a preflight has no body consumer. Stop net/http from
// draining an unfinished body before it sends the response, without imposing a
// read or write deadline on any authenticated inference stream.
func wanCloseRejectedBody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Connection", "close")
	controller := http.NewResponseController(w)
	controller.SetReadDeadline(time.Now())
	if r.Body != nil {
		r.Body.Close()
	}
	controller.SetReadDeadline(time.Time{})
}

func wanNotFound(w http.ResponseWriter, r *http.Request) {
	wanCloseRejectedBody(w, r)
	writeError(w, provider.Chat, http.StatusNotFound, "this WAN endpoint serves supported inference routes only")
}

func wanGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A listed browser must be able to read its authentication error as
		// well as its answer. This grants no access or wildcard origin; the
		// shared corsGuard still handles authenticated calls and preflights.
		if origin := r.Header.Get("Origin"); origin != "" && corsAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		// URL credentials are not supported on the public listener: they can
		// be retained in a proxy's access log. All three APIs accept headers.
		q, err := url.ParseQuery(r.URL.RawQuery)
		_, exists := q["key"]
		if err != nil || exists {
			wanCloseRejectedBody(w, r)
			writeError(w, provider.Chat, http.StatusUnauthorized, "send the WAN key in Authorization: Bearer <key>, x-api-key or x-goog-api-key, not in the URL")
			return
		}
		for _, key := range callerKeys(r) {
			if who, ok := access.AuthenticateWAN(key); ok {
				// An overlapping GUI read may hold process-global snapshots.
				// Drop them before policy/provider reads so external changes
				// made before admission are not hidden by that hold.
				provider.Changed()
				ctx := context.WithValue(r.Context(), wanRemote{}, true)
				r = sanitizedCaller(r.WithContext(access.WithIdentity(ctx, who)))
				next.ServeHTTP(w, r)
				return
			}
		}
		// No secret (not even its suffix) is echoed or logged.
		wanCloseRejectedBody(w, r)
		writeError(w, provider.Chat, http.StatusUnauthorized, "send an enabled WAN gateway key in Authorization: Bearer <key>, x-api-key or x-goog-api-key")
	})
}

// localKeysOnly runs before the ordinary Handler's CORS preflight, and
// callerGuard and lanGuard repeat the same check at their own boundaries.
func localKeysOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !refuseWANKey(w, r) {
			next.ServeHTTP(w, r)
		}
	})
}

func refuseWANKey(w http.ResponseWriter, r *http.Request) bool {
	// Inspect every value, not only Header.Get/Query.Get's first: another
	// credential beside a valid local one cannot lend WAN its privileges.
	keys := append([]string(nil), r.URL.Query()["key"]...)
	for _, h := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
		keys = append(keys, r.Header.Values(h)...)
	}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if len(key) >= 7 && strings.EqualFold(key[:7], "Bearer ") {
			key = strings.TrimSpace(key[7:])
		}
		if strings.HasPrefix(key, access.WANPrefix) {
			writeError(w, provider.Chat, http.StatusUnauthorized, "a WAN gateway key works only at its WAN endpoint")
			return true
		}
	}
	return false
}
