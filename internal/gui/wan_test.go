package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/wan"
)

type fakeWANController struct {
	status                        wan.Status
	starts, stops                 int
	err                           error
	authStarts, logouts, installs int
	remember                      bool
	canceled                      uint64
	provider                      string
	method                        string
	browserOpened                 uint64
	usageChecks, tunnelChanges    int
}

func (f *fakeWANController) WANStatus() wan.Status { return f.status }
func (f *fakeWANController) StartWAN() error {
	f.starts++
	if f.err == nil {
		f.status.State = "starting"
	}
	return f.err
}
func (f *fakeWANController) StopWAN() error {
	f.stops++
	if f.err == nil {
		f.status.State = "stopping"
	}
	return f.err
}

func (f *fakeWANController) StartWANAuth(remember bool, provider string, methods ...string) (wan.AuthStatus, error) {
	f.authStarts++
	f.provider = provider
	if len(methods) > 0 {
		f.method = methods[0]
	}
	f.remember = remember
	if f.err == nil {
		f.status.Auth = wan.AuthStatus{State: "requesting_code", AttemptID: 7}
	}
	return f.status.Auth, f.err
}

func (f *fakeWANController) OpenWANAuthBrowser(id uint64) error { f.browserOpened = id; return f.err }
func (f *fakeWANController) CancelWANAuth(id uint64) error {
	f.canceled = id
	return f.err
}
func (f *fakeWANController) LogoutWAN() error {
	f.logouts++
	return f.err
}
func (f *fakeWANController) InstallWANRuntime() error {
	f.installs++
	return f.err
}

func wanRequest(t *testing.T, h http.Handler, method, path, body string, code int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	if w.Code != code {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, w.Code, code, w.Body)
	}
	if code != http.StatusMethodNotAllowed && w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s: response can be cached", path)
	}
	return w
}

func TestWANRoutesFollowServingOwner(t *testing.T) {
	sandboxHome(t)
	mux := http.NewServeMux()
	var owner wanController
	wanRoutes(mux, func() wanController { return owner })
	read := func() wanJSON {
		t.Helper()
		w := wanRequest(t, mux, "GET", "/api/wan", "", 200)
		var out wanJSON
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := read(); out.Controllable || out.State != "unknown" || out.Endpoint != "" {
		t.Fatalf("non-owner guessed another process's status: %+v", out)
	}
	for _, path := range []string{"/api/wan/start", "/api/wan/stop", "/api/wan/keys/add-key"} {
		wanRequest(t, mux, "POST", path, `{"name":"Laptop"}`, 409)
	}
	if keys, err := access.ListWAN(); err != nil || len(keys) != 0 {
		t.Fatalf("non-owner wrote keys: %+v %v", keys, err)
	}
	first := &fakeWANController{status: wan.Status{State: "stopped", RuntimeAvailable: true}}
	owner = first
	wanRequest(t, mux, "POST", "/api/wan/start", "{}", 200)
	if out := read(); !out.Controllable || out.State != "starting" || first.starts != 1 {
		t.Fatalf("start: %+v, starts=%d", out, first.starts)
	}
	second := &fakeWANController{status: wan.Status{State: "running", Endpoint: "https://example.invalid", RuntimeAvailable: true}}
	owner = second
	if out := read(); out.Endpoint != second.status.Endpoint || out.State != "running" {
		t.Fatalf("read stale owner: %+v", out)
	}
	wanRequest(t, mux, "POST", "/api/wan/stop", "{}", 200)
	if second.stops != 1 || first.stops != 0 {
		t.Fatalf("stopped stale owner: first=%d second=%d", first.stops, second.stops)
	}
	second.err = errors.New("Sign in to GitHub before starting WAN")
	w := wanRequest(t, mux, "POST", "/api/wan/start", "{}", 400)
	if !strings.Contains(w.Body.String(), second.err.Error()) {
		t.Fatal("missing start error", w.Body)
	}
	owner = nil
	wanRequest(t, mux, "POST", "/api/wan/stop", "{}", 409)
	wanRequest(t, mux, "GET", "/api/wan/start", "", 405)
	if first.starts != 1 || second.starts != 1 || second.stops != 1 {
		t.Fatal("refused request changed controller")
	}
}

func TestWANAuthRoutesAreExplicitAndOwnerOnly(t *testing.T) {
	sandboxHome(t)
	mux := http.NewServeMux()
	var owner wanController
	wanRoutes(mux, func() wanController { return owner })
	paths := []string{"auth/start", "auth/cancel", "auth/logout", "runtime/install"}
	for _, path := range paths {
		wanRequest(t, mux, "POST", "/api/wan/"+path, `{}`, 409)
	}
	controller := &fakeWANController{status: wan.Status{State: "stopped", Auth: wan.AuthStatus{State: "signed_out"}}}
	owner = controller
	wanRequest(t, mux, "GET", "/api/wan", "", 200)
	if controller.authStarts+controller.installs+controller.logouts != 0 {
		t.Fatal("reading status started authorization or installed a runtime")
	}
	w := wanRequest(t, mux, "POST", "/api/wan/auth/start", `{"remember":true}`, 200)
	var out wanJSON
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !controller.remember || controller.authStarts != 1 || out.Auth.State != "requesting_code" || out.Auth.AttemptID != 7 || controller.starts != 0 {
		t.Fatal("authorization did not remain independent of hosting", out)
	}
	for _, body := range []string{`{}`, `{"attemptID":0}`, `{"attemptID":-1}`, `{"attemptID":7,"token":"SECRET"}`, `{"attemptID":7}{}`, `{"attemptID":7,"padding":"` + strings.Repeat("x", 8192) + `"}`} {
		wanRequest(t, mux, "POST", "/api/wan/auth/cancel", body, 400)
	}
	if controller.canceled != 0 {
		t.Fatal("invalid cancellation reached the owner")
	}
	wanRequest(t, mux, "POST", "/api/wan/auth/cancel", `{"attemptID":7}`, 200)
	if controller.canceled != 7 {
		t.Fatal("cancel did not carry the displayed attempt identity")
	}
	for _, path := range paths {
		wanRequest(t, mux, "GET", "/api/wan/"+path, "", 405)
	}
	wanRequest(t, mux, "POST", "/api/wan/auth/logout", `{}`, 200)
	wanRequest(t, mux, "POST", "/api/wan/runtime/install", `{}`, 200)
	if controller.logouts != 1 || controller.installs != 1 || controller.stops != 0 {
		t.Fatal("explicit auth/runtime actions invoked the wrong owner operation")
	}
	controller.err = errors.New("WAN sign-in attempt is no longer current")
	wanRequest(t, mux, "POST", "/api/wan/auth/cancel", `{"attemptID":6}`, 400)
	owner = nil
	wanRequest(t, mux, "POST", "/api/wan/auth/logout", `{}`, 409)
	if controller.logouts != 1 {
		t.Fatal("handover retained authority over the old owner")
	}
}

func TestWANOwnerUsesCurrentServedGateway(t *testing.T) {
	previous := served.Load()
	t.Cleanup(func() { served.Store(previous) })
	served.Store(nil)
	if wanOwner() != nil {
		t.Fatal("no serving gateway, but WAN can be controlled")
	}
	// No listener or tunnel starts: this checks which owner the routes resolve.
	first, next := new(gateway.Server), new(gateway.Server)
	served.Store(first)
	if wanOwner() != first {
		t.Fatal("not the serving gateway")
	}
	served.Store(next)
	if wanOwner() != next {
		t.Fatal("stale gateway after handover")
	}
}

func TestWANKeyRoutesStaySeparateAndMasked(t *testing.T) {
	sandboxHome(t)
	mux := http.NewServeMux()
	controller := &fakeWANController{status: wan.Status{State: "stopped"}}
	wanRoutes(mux, func() wanController { return controller })
	callerKeyRoutes(mux)
	local, err := access.Update("add-key", access.Change{Name: "Local"})
	if err != nil {
		t.Fatal(err)
	}
	post := func(action, body string) wanJSON {
		t.Helper()
		w := wanRequest(t, mux, "POST", "/api/wan/keys/"+action, body, 200)
		var out wanJSON
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, k := range out.Keys {
			if k.Scope != access.ScopeWAN || k.Secret != "" || k.WANSecret != "" || k.Masked == "" {
				t.Fatalf("wrong key scope or secret in row: %+v", k)
			}
		}
		return out
	}
	first := post("add-key", `{"name":"Laptop"}`)
	if len(first.Keys) != 1 || first.Secret == "" {
		t.Fatalf("add: %+v", first)
	}
	id, secret := first.Keys[0].ID, first.Secret
	second := post("add-key", `{"name":"Phone"}`)
	if len(second.Keys) != 2 || second.Secret == secret {
		t.Fatal("keys not independent")
	}
	w := wanRequest(t, mux, "GET", "/api/wan", "", 200)
	for _, forbidden := range []string{local, secret, second.Secret, `"secret"`, `"wanSecret"`} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("list leaked %q", forbidden)
		}
	}
	body := `{"key":"` + id + `"}`
	if out := post("copy-key", body); out.Secret != secret {
		t.Fatal("explicit copy did not return the key")
	}
	if out := post("rename-key", `{"key":"`+id+`","name":"Work laptop"}`); out.Keys[0].Name != "Work laptop" || out.Secret != "" {
		t.Fatal("rename", out)
	}
	if out := post("off-key", body); !out.Keys[0].Off || out.Keys[1].Off {
		t.Fatal("off changed wrong key", out)
	}
	if out := post("on-key", body); out.Keys[0].Off {
		t.Fatal("on", out)
	}
	rotated := post("rotate-key", body)
	if rotated.Secret == "" || rotated.Secret == secret || rotated.Keys[0].ID != id {
		t.Fatal("rotation", rotated)
	}
	for _, action := range []string{"copy-key", "rotate-key", "remove-key", "off-key"} {
		wanRequest(t, mux, "POST", "/api/caller-keys/"+action, body, 400)
	}
	locals, err := access.ListLocal()
	if err != nil || len(locals) != 1 {
		t.Fatalf("local keys: %+v %v", locals, err)
	}
	for _, action := range []string{"copy-key", "rotate-key", "remove-key", "off-key"} {
		wanRequest(t, mux, "POST", "/api/wan/keys/"+action, `{"key":"`+locals[0].ID+`"}`, 400)
	}
	w = wanRequest(t, mux, "GET", "/api/caller-keys", "", 200)
	if strings.Contains(w.Body.String(), id) || strings.Contains(w.Body.String(), second.Keys[1].ID) {
		t.Fatal("WAN keys shown in ordinary editor", w.Body)
	}
	w = wanRequest(t, mux, "POST", "/api/caller-keys/rename-key", `{"key":"`+locals[0].ID+`","name":"Local renamed"}`, 200)
	if strings.Contains(w.Body.String(), id) {
		t.Fatal("ordinary mutation listed WAN keys", w.Body)
	}
	if out := post("remove-key", body); len(out.Keys) != 1 || out.Keys[0].ID != second.Keys[1].ID {
		t.Fatal("removal changed other key", out)
	}
	wanRequest(t, mux, "POST", "/api/wan/keys/add-key", "{broken", 400)
	if err := os.WriteFile(access.Path(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	w = wanRequest(t, mux, "GET", "/api/wan", "", 200)
	var unknown wanJSON
	if err := json.Unmarshal(w.Body.Bytes(), &unknown); err != nil {
		t.Fatal(err)
	}
	if !unknown.Controllable || unknown.Keys != nil || unknown.KeysError == "" {
		t.Fatalf("unknown keys hid owner controls or became an empty list: %+v", unknown)
	}
	wanRequest(t, mux, "POST", "/api/wan/keys/add-key", `{"name":"Lost"}`, 400)
}

func TestWANBrokenKeysStillPermitStopAndSignOut(t *testing.T) {
	sandboxHome(t)
	mux := http.NewServeMux()
	controller := &fakeWANController{status: wan.Status{State: "running", Auth: wan.AuthStatus{State: "signed_in"}}}
	wanRoutes(mux, func() wanController { return controller })
	if _, err := access.UpdateWAN("add-key", access.Change{Name: "Laptop"}); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{broken secret-value-never-rendered")
	if err := os.WriteFile(access.Path(), broken, 0600); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ method, path, body string }{
		{"GET", "/api/wan", ""},
		{"POST", "/api/wan/stop", "{}"},
		{"POST", "/api/wan/auth/logout", "{}"},
	} {
		w := wanRequest(t, mux, request.method, request.path, request.body, 200)
		var out wanJSON
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.Controllable || out.Keys != nil || out.KeysError == "" || strings.Contains(w.Body.String(), "secret-value") {
			t.Fatalf("broken keys disabled lifecycle controls or leaked file contents: %s", w.Body)
		}
	}
	if controller.stops != 1 || controller.logouts != 1 {
		t.Fatalf("lifecycle actions were lost: stops=%d logouts=%d", controller.stops, controller.logouts)
	}
	wanRequest(t, mux, "POST", "/api/wan/keys/add-key", `{"name":"Lost"}`, 400)
	data, err := os.ReadFile(access.Path())
	if err != nil || string(data) != string(broken) {
		t.Fatalf("unknown keys were overwritten: %q %v", data, err)
	}
}

func (f *fakeWANController) RefreshWANUsage() error { f.usageChecks++; return f.err }
func (f *fakeWANController) NewWANTunnel() error    { f.tunnelChanges++; return f.err }
func TestWANAccountDiagnosticsAndExplicitTunnelActions(t *testing.T) {
	sandboxHome(t)
	limit := uint64(5368709120)
	controller := &fakeWANController{status: wan.Status{State: "failed", Usage: wan.Usage{State: "ready", Used: 5371017692, Limit: &limit, Account: wan.Account{ID: "github:1", Name: "fixture"}}, Tunnel: &wan.TunnelInfo{ID: "fixture-tunnel", Region: "usw2", Port: 50188, AccountID: "github:1"}}}
	mux := http.NewServeMux()
	wanRoutes(mux, func() wanController { return controller })
	w := wanRequest(t, mux, "GET", "/api/wan", "", 200)
	var out wanJSON
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Usage.Used != 5371017692 || out.Tunnel == nil || out.Tunnel.ID != "fixture-tunnel" {
		t.Fatal("diagnostics missing", w.Body)
	}
	if controller.starts != 0 || controller.usageChecks != 0 || controller.tunnelChanges != 0 {
		t.Fatal("read had an action side effect")
	}
	wanRequest(t, mux, "POST", "/api/wan/auth/start", `{"remember":false,"provider":"microsoft"}`, 200)
	if controller.provider != "microsoft" {
		t.Fatal("Microsoft selection was lost")
	}
	wanRequest(t, mux, "POST", "/api/wan/usage/refresh", `{}`, 200)
	wanRequest(t, mux, "POST", "/api/wan/tunnel/new", `{}`, 200)
	if controller.usageChecks != 1 || controller.tunnelChanges != 1 || controller.starts != 0 {
		t.Fatal("explicit management action published a tunnel")
	}
	wanRequest(t, mux, "POST", "/api/wan/tunnel/new", `{"id":"foreign-tunnel"}`, 400)
}

func TestWANBrowserAuthReopenIsExplicitAndOwnerOnly(t *testing.T) {
	sandboxHome(t)
	controller := &fakeWANController{}
	var owner wanController = controller
	mux := http.NewServeMux()
	wanRoutes(mux, func() wanController { return owner })
	wanRequest(t, mux, "POST", "/api/wan/auth/start", `{"remember":false,"provider":"microsoft","method":"browser"}`, 200)
	if controller.method != "browser" {
		t.Fatal("browser method was dropped")
	}
	wanRequest(t, mux, "GET", "/api/wan/auth/open", "", 405)
	for _, body := range []string{`{}`, `{"attemptID":0}`, `{"attemptID":-1}`, `{"attemptID":7,"url":"https://attacker.invalid"}`} {
		wanRequest(t, mux, "POST", "/api/wan/auth/open", body, 400)
	}
	if controller.browserOpened != 0 {
		t.Fatal("read or invalid input opened browser")
	}
	wanRequest(t, mux, "POST", "/api/wan/auth/open", `{"attemptID":7}`, 200)
	if controller.browserOpened != 7 || controller.canceled != 0 || controller.starts != 0 {
		t.Fatal("reopen changed wrong operation")
	}
	owner = nil
	wanRequest(t, mux, "POST", "/api/wan/auth/open", `{"attemptID":7}`, 409)
}
