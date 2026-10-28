package backup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/settings"
)

func TestWANBackupKeepsScopeNotTunnel(t *testing.T) {
	home(t)
	appdir.UseExecutable("")
	secret, err := access.UpdateWAN("add-key", access.Change{Name: "Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(settings.Dir(), "wan.json")
	original := []byte(`{"id":"magpie-wan-local-only","port":43210}`)
	if err := os.WriteFile(config, original, 0600); err != nil {
		t.Fatal(err)
	}
	for _, withKeys := range []bool{false, true} {
		bundle, err := Collect(withKeys, "test")
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(bundle)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("magpie-wan-local-only")) {
			t.Fatal("backup contains the machine's tunnel identity")
		}
		if bytes.Contains(data, []byte(secret)) != withKeys {
			t.Fatalf("WAN credential inclusion did not follow keys=%v", withKeys)
		}
		if !withKeys {
			continue
		}
		if bundle.GatewayKeys == nil || len(*bundle.GatewayKeys) != 1 {
			t.Fatal("keyed backup omitted WAN key")
		}
		key := (*bundle.GatewayKeys)[0]
		if key.Scope != access.ScopeWAN || key.Secret != "" || key.WANSecret != secret {
			t.Fatal("backup downgraded WAN key into the ordinary credential field")
		}
		if err := access.Restore(nil); err != nil {
			t.Fatal(err)
		}
		if _, err := Restore(bundle, Parts{Settings: true}); err != nil {
			t.Fatal(err)
		}
		if _, ok := access.AuthenticateWAN(secret); !ok {
			t.Fatal("restored WAN key no longer authenticates on WAN")
		}
		if _, ok := access.Authenticate(secret); ok {
			t.Fatal("restored WAN key authenticates as an ordinary gateway key")
		}
		got, err := os.ReadFile(config)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatal("restore changed the machine's own tunnel configuration", err)
		}
	}
}
