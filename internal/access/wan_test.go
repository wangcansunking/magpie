package access

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestWANKeysSeparateScopesAndLifecycle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	local, err := Update("add-key", Change{Name: "Local"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := UpdateWAN("add-key", Change{Name: "Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := UpdateWAN("add-key", Change{Name: "Phone"})
	if err != nil {
		t.Fatal(err)
	}
	wa, oka := AuthenticateWAN(a)
	wb, okb := AuthenticateWAN(b)
	if !oka || !okb || wa.KeyID == wb.KeyID || a == b || !strings.HasPrefix(a, WANPrefix) {
		t.Fatal("WAN keys lack distinct authenticated identities")
	}
	for _, secret := range []string{"", "magpie", "magpie-codex", local, WANPrefix + "invalid", a + "x"} {
		if _, ok := AuthenticateWAN(secret); ok {
			t.Fatal("WAN accepted a non-WAN credential")
		}
	}
	for _, secret := range []string{a, b} {
		if _, ok := Authenticate(secret); ok {
			t.Fatal("ordinary authentication accepted a WAN key")
		}
		if !Named(secret) {
			t.Fatal("WAN namespace must never fall back to anonymous local access")
		}
	}
	all, err := List()
	if err != nil || len(all) != 3 {
		t.Fatal("accounting must keep all keys", err)
	}
	wans, err := ListWAN()
	if err != nil || len(wans) != 2 {
		t.Fatal("WAN list", err)
	}
	locals, err := ListLocal()
	if err != nil || len(locals) != 1 || locals[0].Name != "Local" {
		t.Fatal("local list", err)
	}
	for _, keys := range [][]Key{all, wans, locals} {
		encoded, err := json.Marshal(keys)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{local, a, b} {
			if strings.Contains(string(encoded), secret) {
				t.Fatal("a list exposed a credential")
			}
		}
		for _, k := range keys {
			if k.Secret != "" || k.WANSecret != "" || k.Masked == "" {
				t.Fatal("list must mask both secret fields")
			}
		}
	}
	for _, action := range []string{"rename-key", "rotate-key", "off-key", "on-key", "remove-key", "copy-key", "models-key", "accounts-key", "quotas-key", "limit-key", "reset-limit-key"} {
		if _, err := Update(action, Change{Key: wa.KeyID, Name: "Changed"}); err == nil {
			t.Fatal("ordinary editor changed WAN key", action)
		}
		if _, err := UpdateWAN(action, Change{Key: locals[0].ID, Name: "Changed"}); err == nil {
			t.Fatal("WAN editor changed local key", action)
		}
	}
	if _, err := UpdateWAN("add-key", Change{Name: "Own", Secret: "user-key-value"}); err == nil {
		t.Fatal("WAN accepted a user-supplied unscoped credential")
	}
	if _, err := UpdateWAN("rename-key", Change{Key: wa.KeyID, Name: "Travel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateWAN("models-key", Change{Key: wa.KeyID, Models: []string{"fixture/model"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "fixture", Name: "Fixture", Chat: "http://127.0.0.1:1/v1", Key: "fixture-key"}); err != nil {
		t.Fatal(err)
	}
	account := "fixture/" + provider.KeyID("fixture-key")
	if _, err := UpdateWAN("accounts-key", Change{Key: wa.KeyID, Accounts: []string{account}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateWAN("limit-key", Change{Key: wa.KeyID, Limit: &Limit{Period: "day", Tokens: 100}}); err != nil {
		t.Fatal(err)
	}
	index := 1
	if _, err := UpdateWAN("limit-key", Change{Key: wa.KeyID, Index: &index, Limit: &Limit{Period: "week", Cost: 1}}); err != nil {
		t.Fatal(err)
	}
	wa, _ = AuthenticateWAN(a)
	if wa.KeyName != "Travel" || wa.Limit == nil || wa.Limit.Tokens != 100 || len(wa.More) != 1 || !reflect.DeepEqual(wa.Models, []string{"fixture/model"}) || !reflect.DeepEqual(wa.Accounts, []string{account}) {
		t.Fatal("WAN identity lost constraints")
	}
	checkB := func() {
		t.Helper()
		got, ok := AuthenticateWAN(b)
		if !ok || !reflect.DeepEqual(got, wb) {
			t.Fatal("editing A changed B")
		}
		if _, ok := Authenticate(local); !ok {
			t.Fatal("editing WAN changed local key")
		}
	}
	for _, action := range []string{"off-key", "on-key"} {
		if _, err := UpdateWAN(action, Change{Key: wa.KeyID}); err != nil {
			t.Fatal(err)
		}
		if _, ok := AuthenticateWAN(a); ok != (action == "on-key") {
			t.Fatal("toggle was not immediate", action)
		}
		checkB()
	}
	rotated, err := UpdateWAN("rotate-key", Change{Key: wa.KeyID})
	if err != nil || rotated == a || !strings.HasPrefix(rotated, WANPrefix) {
		t.Fatal("rotation", err)
	}
	if _, ok := AuthenticateWAN(a); ok {
		t.Fatal("rotated credential still authenticates")
	}
	if got, ok := AuthenticateWAN(rotated); !ok || !reflect.DeepEqual(got, wa) {
		t.Fatal("rotation lost identity or constraints")
	}
	if copy, err := UpdateWAN("copy-key", Change{Key: wa.KeyID}); err != nil || copy != rotated {
		t.Fatal("copy did not return the WAN credential", err)
	}
	checkB()
	if _, err := UpdateWAN("remove-key", Change{Key: wa.KeyID}); err != nil {
		t.Fatal(err)
	}
	if _, ok := AuthenticateWAN(rotated); ok {
		t.Fatal("removed WAN key authenticates")
	}
	checkB()
}

func TestWANExportRestoreAndOlderWriterFailClosed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	secret, err := UpdateWAN("add-key", Change{Name: "Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	who, ok := AuthenticateWAN(secret)
	if !ok {
		t.Fatal("created key did not authenticate")
	}
	backup, err := Export()
	if err != nil || len(backup) != 1 || backup[0].Secret != "" || backup[0].WANSecret != secret || backup[0].Scope != ScopeWAN {
		t.Fatal("backup lost scope or exposed WAN to an older reader", err)
	}
	if err := Restore(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := AuthenticateWAN(secret); ok {
		t.Fatal("empty store retained WAN key")
	}
	if err := Restore(backup); err != nil {
		t.Fatal(err)
	}
	if got, ok := AuthenticateWAN(secret); !ok || got.KeyID != who.KeyID {
		t.Fatal("restored WAN key changed identity")
	}
	// These are the fields an older version reads and writes. It neither
	// learns the WAN secret nor drops ID, so the record cannot be rotated
	// by the current ordinary editor after the round-trip.
	var old []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Secret string `json:"secret,omitempty"`
	}
	b, err := os.ReadFile(Path())
	if err != nil || json.Unmarshal(b, &old) != nil || len(old) != 1 || old[0].Secret != "" {
		t.Fatal("old reader learned WAN credential", err)
	}
	b, err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := AuthenticateWAN(secret); ok {
		t.Fatal("older writer did not revoke missing scope")
	}
	if _, ok := Authenticate(secret); ok {
		t.Fatal("older writer downgraded WAN to local")
	}
	if _, err := Update("rotate-key", Change{Key: who.KeyID}); err == nil {
		t.Fatal("ordinary rotation downgraded a damaged WAN record")
	}
	if keys, err := ListWAN(); err != nil || len(keys) != 1 || keys[0].Masked != "" {
		t.Fatal("damaged WAN record cannot be managed", err)
	}
	if keys, err := ListLocal(); err != nil || len(keys) != 0 {
		t.Fatal("damaged WAN record shown as local", err)
	}
	if _, err := UpdateWAN("copy-key", Change{Key: who.KeyID}); err == nil {
		t.Fatal("copy pretended damaged credential exists")
	}
	repaired, err := UpdateWAN("rotate-key", Change{Key: who.KeyID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := AuthenticateWAN(repaired); !ok {
		t.Fatal("explicit WAN rotation did not repair credential")
	}
}

func TestWANCorruptAndUnreadableStoreFailsClosed(t *testing.T) {
	for _, corrupt := range []bool{true, false} {
		t.Run(map[bool]string{true: "corrupt", false: "unreadable"}[corrupt], func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			secret, err := UpdateWAN("add-key", Change{Name: "Laptop"})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				err = os.WriteFile(Path(), []byte("[{\"scope\":\"wan\""), 0o600)
			} else {
				if err := os.Remove(Path()); err != nil {
					t.Fatal(err)
				}
				err = os.Mkdir(Path(), 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := AuthenticateWAN(secret); ok {
				t.Fatal("unknown credentials authenticated")
			}
			if _, err := ListWAN(); err == nil {
				t.Fatal("unknown credentials read as empty")
			}
			if _, err := UpdateWAN("add-key", Change{Name: "New"}); err == nil {
				t.Fatal("unknown credentials written over")
			}
		})
	}
}

func TestWANMalformedScopeCannotUseLegacyMirror(t *testing.T) {
	for _, k := range []Key{
		{ID: "wan-1", Scope: ScopeWAN, Secret: "legacy-scoped-secret"},
		{ID: "unknown", Scope: "future", Secret: "legacy-scoped-secret"},
		{ID: "wan-1", WANSecret: "legacy-scoped-secret"},
	} {
		t.Run(k.ID+"-"+k.Scope, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			s := settings.Load()
			s.LAN, s.LANKey, s.LANKeyID = true, "legacy-scoped-secret", ""
			if err := settings.Save(s); err != nil {
				t.Fatal(err)
			}
			if err := Restore([]Key{k}); err != nil {
				t.Fatal(err)
			}
			if _, ok := Authenticate(s.LANKey); ok || LANSecret() != "" {
				t.Fatal("rejected scope reentered through the legacy mirror")
			}
			if err := MigrateLegacyLANKey(); err == nil {
				t.Fatal("legacy migration downgraded a rejected scope")
			}
			keys, err := Export()
			if err != nil || !reflect.DeepEqual(keys, []Key{k}) {
				t.Fatal("legacy migration changed the scoped store", err)
			}
		})
	}
}

func TestWANMalformedScopeNeverAuthenticatesLocally(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	secret := WANPrefix + strings.Repeat("a", 48)
	for _, key := range []Key{
		{ID: "wan-1", Secret: secret},
		{ID: "1", Scope: ScopeWAN, Secret: secret},
		{ID: "1", WANSecret: secret},
		{ID: "1", Scope: "future", Secret: "custom-secret"},
		{ID: "1", Scope: ScopeWAN, WANSecret: secret, Secret: "custom-secret"},
		{ID: "wan-1", Scope: ScopeWAN, WANSecret: secret, LAN: true},
		{Scope: ScopeWAN, WANSecret: secret},
		{ID: "wan-", Scope: ScopeWAN, WANSecret: secret},
	} {
		if err := Restore([]Key{key}); err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{secret, "custom-secret"} {
			if _, ok := Authenticate(value); ok {
				t.Fatal("malformed scoped key authenticated locally")
			}
			if _, ok := AuthenticateWAN(value); ok {
				t.Fatal("malformed scoped key authenticated on WAN")
			}
		}
	}
}
