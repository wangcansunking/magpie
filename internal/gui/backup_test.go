package gui

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agentenv"
	"github.com/yetone/magpie/internal/library"
)

// folderOnly is the app's windows as far as an export needs them.
type folderOnly struct{ Windows }

func (folderOnly) OpenFolder(string) error { return nil }

// sandboxHome gives the test a machine of its own: no agents, no magpie files.
func sandboxHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	// Clear agent overrides first: some are also shared folder variables
	// (XDG_STATE_HOME), which must then point into this test's home.
	for _, v := range agentenv.Vars {
		t.Setenv(v, "")
	}
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(h, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(h, ".local", "state"))
	t.Setenv("PATH", "")
	// os.UserConfigDir requires APPDATA on Windows; empty is not a
	// sandbox there. Both Windows roots belong to this test's home.
	t.Setenv("APPDATA", filepath.Join(h, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(h, "AppData", "Local"))
	return h
}

func TestSandboxHomeKeepsEveryFolderLocal(t *testing.T) {
	home := sandboxHome(t)
	for _, variable := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		value := os.Getenv(variable)
		rel, err := filepath.Rel(home, value)
		if value == "" || !filepath.IsAbs(value) || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%s = %q is not inside the test home", variable, value)
		}
	}
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal("test home has no OS config directory:", err)
	}
	if rel, err := filepath.Rel(home, config); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("OS config directory %q is outside the test home", config)
	}
}

// The Settings page's Export and Import carry the library, and leave it
// when its box is unticked.
func TestBackupLibraryRoundTrip(t *testing.T) {
	h := sandboxHome(t)
	text := "Be brief."
	if _, err := library.SaveInstructions(library.InstructionsChange{Shared: &text}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	backupRoutes(mux, folderOnly{})
	post := func(path string, body any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(string(b))))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	export := func(lib bool) []byte {
		t.Helper()
		out := post("/api/backup/export", map[string]any{"pass": "pw", "keys": true, "library": lib})
		if out["library"] != lib {
			t.Fatalf("export: %v", out)
		}
		p, _ := out["path"].(string)
		data, err := os.ReadFile(filepath.Join(h, strings.TrimPrefix(p, "~")))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	with, without := export(true), export(false)

	sandboxHome(t) // another machine
	enc := base64.StdEncoding.EncodeToString
	if out := post("/api/backup/import", map[string]any{"data": enc(without), "pass": "pw", "agents": true, "library": true}); out["Library"] != false {
		t.Fatalf("a file without it: %v", out)
	}
	if out := post("/api/backup/import", map[string]any{"data": enc(with), "pass": "pw", "agents": true, "library": false}); out["Library"] != false {
		t.Fatalf("unticked: %v", out)
	}
	if iv, _ := library.ReadInstructions(); iv.Shared != "" {
		t.Fatalf("the library came in unticked: %q", iv.Shared)
	}
	if out := post("/api/backup/import", map[string]any{"data": enc(with), "pass": "pw", "agents": true, "library": true}); out["Library"] != true {
		t.Fatalf("import: %v", out)
	}
	if iv, _ := library.ReadInstructions(); iv.Shared != "Be brief." {
		t.Fatalf("instructions: %q", iv.Shared)
	}
}
