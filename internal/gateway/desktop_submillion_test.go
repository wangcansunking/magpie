package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// Desktop 5.11492 gives its managed Claude Code a 200K host cap. The
// long-window id must bypass that cap without advertising a larger backend.
func TestDesktopSubmillionWindow(t *testing.T) {
	setup(t, provider.Anthropic, &fake{})
	p := provider.Provider{ID: "long", Name: "Long", Key: "k", Chat: "https://example.test/v1", Models: []string{"gpt-long"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "gpt-long", Context: 272000, MaxContext: 922000}}); err != nil {
		t.Fatal(err)
	}
	s := settings.Load()
	s.DesktopLongest = true
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/models?limit=1000", nil)
	r.Header.Set("x-api-key", TokenFor("claude-desktop"))
	r.Header.Set("User-Agent", "Mozilla/5.0 Claude/5.11492")
	w := httptest.NewRecorder()
	New().Handler().ServeHTTP(w, r)
	var out struct {
		Data []struct {
			ID          string `json:"id"`
			Max         int    `json:"max_input_tokens"`
			Description string `json:"description"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Data {
		if DesktopCatalogID(strings.TrimSuffix(m.ID, "[1m]")) != "long/gpt-long" {
			continue
		}
		if !strings.HasSuffix(m.ID, "[1m]") || m.Max != 922000 || !strings.Contains(m.Description, "922K") {
			t.Fatalf("922K Desktop row cannot escape its 200K host cap accurately: %+v", m)
		}
		if err := SetDesktopTier("sonnet", "long/gpt-long"); err != nil {
			t.Fatal(err)
		}
		list, ok := DesktopInferenceModels()
		if !ok {
			t.Fatal("could not read Desktop model list")
		}
		for _, entry := range list {
			if entry["name"] == m.ID {
				if entry["supports1m"] == true || entry["prefer1m"] == true {
					t.Fatalf("922K profile claims a real 1M backend: %v", entry)
				}
				return
			}
		}
		t.Fatalf("static profile lost compatibility alias %s: %v", m.ID, list)
	}
	t.Fatal("922K model missing from Desktop discovery")
}

func TestDesktopSubmillionBoundaries(t *testing.T) {
	setup(t, provider.Anthropic, &fake{})
	for _, tc := range []struct {
		model  string
		window int
		marked bool
	}{
		{"gpt-long", 922000, true},
		{"gpt-small", 200000, false},
		{"gpt-unknown", 0, false},
		{"claude-sonnet-4-6", 256000, false},
		{"claude-sonnet-4-6", 1000000, true},
	} {
		e := provider.Entry{Model: tc.model, Context: tc.window}
		if got := desktopListed1M(e, true); got != tc.marked {
			t.Errorf("%s %d: marked %v, want %v", tc.model, tc.window, got, tc.marked)
		}
		if desktopListed1M(e, false) {
			t.Errorf("%s %d: marked with setting off", tc.model, tc.window)
		}
	}
}
