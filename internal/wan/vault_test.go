package wan

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

type fakeCredentialVault struct {
	data         []byte
	readErr      error
	writeErr     error
	deleteErr    error
	readTarget   string
	writeTarget  string
	deleteTarget string
	readBuffer   []byte
	writeBuffer  []byte
}

func (v *fakeCredentialVault) read(target string) ([]byte, error) {
	v.readTarget = target
	v.readBuffer = bytes.Clone(v.data)
	return v.readBuffer, v.readErr
}
func (v *fakeCredentialVault) write(target string, data []byte) error {
	v.writeTarget = target
	v.data = bytes.Clone(data)
	v.writeBuffer = data
	return v.writeErr
}
func (v *fakeCredentialVault) remove(target string) error {
	v.deleteTarget = target
	if v.deleteErr == nil {
		v.data = nil
	}
	return v.deleteErr
}

func TestWANVaultNeverUsesSystemStoreInTestSandbox(t *testing.T) {
	if vaultSandbox != testenv.Marker || os.Getenv(testenv.Marker) == "" {
		t.Fatal("WAN vault isolation does not match the testenv marker")
	}
	store := defaultCredentialStore(t.TempDir())
	if store.available() {
		t.Fatal("a test could open the real user's secure credential store")
	}
	if _, err := store.load(); !errors.Is(err, errCredentialMissing) {
		t.Fatal("test sandbox did not have an empty memory-only session")
	}
	if err := store.save(savedCredential{}); !errors.Is(err, errVaultUnavailable) {
		t.Fatal("memory fallback pretended to persist a credential")
	}
	if err := store.delete(); err != nil {
		t.Fatal(err)
	}
}

func TestWANVaultTargetsOnlyItsOwnSettingsDirectory(t *testing.T) {
	dir := t.TempDir()
	first := credentialTarget(filepath.Join(dir, "one"), false)
	second := credentialTarget(filepath.Join(dir, "two"), false)
	if first == second || first != credentialTarget(filepath.Join(dir, "one", "..", "one"), false) {
		t.Fatal("distinct settings directories share a WAN credential or a normalized path changed its identity")
	}
	if !strings.HasPrefix(first, "magpie/wan/github/v1/") || len(first) != len("magpie/wan/github/v1/")+64 || strings.Contains(first, dir) {
		t.Fatal("credential target was not a WAN-specific directory hash")
	}
	if credentialTarget("C:\\Users\\Example\\Magpie", true) != credentialTarget("c:/users/example/magpie", true) {
		t.Fatal("Windows case and path separators duplicated a user's credential")
	}
}

func TestWANVaultRoundTripAndByteBuffersAreCleared(t *testing.T) {
	vault := &fakeCredentialVault{}
	target := credentialTarget(t.TempDir(), false)
	store := systemCredentialStore{target: target, vault: vault}
	record := savedCredential{Version: 1, ClientID: "test-wan-app", Access: "fake-secret", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	if err := store.save(record); err != nil {
		t.Fatal(err)
	}
	if vault.writeTarget != target || !store.available() {
		t.Fatal("secure write used the wrong credential target")
	}
	for _, b := range vault.writeBuffer {
		if b != 0 {
			t.Fatal("temporary encoded credential was retained after secure write")
		}
	}
	var encoded map[string]any
	if err := json.Unmarshal(vault.data, &encoded); err != nil || len(encoded) != 4 || encoded["access"] != "fake-secret" || encoded["clientID"] != "test-wan-app" {
		t.Fatal("vault record includes data beyond the WAN access credential")
	}
	got, err := store.load()
	if err != nil || got != record || vault.readTarget != target {
		t.Fatalf("secure credential round trip failed: %v", err)
	}
	for _, b := range vault.readBuffer {
		if b != 0 {
			t.Fatal("native-read credential buffer was retained after parsing")
		}
	}
	if err := store.delete(); err != nil || vault.deleteTarget != target || vault.data != nil {
		t.Fatal("logout did not erase exactly its own vault record")
	}
}

func TestWANVaultUnreadableRecordIsUnknownNotMissing(t *testing.T) {
	for _, data := range []string{
		"", "null", "{", `{"version":9,"clientID":"test","access":"fake"}`,
		`{"version":1,"clientID":"test","access":"bad token"}`,
		`{"version":1,"clientID":"test","access":"fake","expiresAt":"0001-01-01T01:00:00Z"}`,
		strings.Repeat("x", maxCredentialBlob+1),
	} {
		vault := &fakeCredentialVault{data: []byte(data)}
		store := systemCredentialStore{target: "fake-target", vault: vault}
		if got, err := store.load(); err == nil || errors.Is(err, errCredentialMissing) || got.Access != "" {
			t.Fatal("invalid secure credential was treated as missing or usable")
		}
		if !bytes.Equal(vault.data, []byte(data)) {
			t.Fatal("failed read overwrote an unknown saved credential")
		}
	}
	vault := &fakeCredentialVault{readErr: errCredentialMissing}
	if _, err := (systemCredentialStore{target: "fake-target", vault: vault}).load(); !errors.Is(err, errCredentialMissing) {
		t.Fatal("a missing native record was not distinguished from a corrupt one")
	}
}

func TestWANVaultDoesNotLeakNativeErrorsOrPretendOversizeSaved(t *testing.T) {
	vault := &fakeCredentialVault{writeErr: errors.New("SECRET native write"), deleteErr: errors.New("SECRET native delete")}
	store := systemCredentialStore{target: "fake-target", vault: vault}
	record := savedCredential{Version: 1, ClientID: "test", Access: "fake-secret"}
	if err := store.save(record); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("native secure-write error was exposed")
	}
	if err := store.delete(); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("native secure-delete error was exposed")
	}
	vault.deleteErr = errCredentialMissing
	if err := store.delete(); err != nil {
		t.Fatal("deleting an absent credential was not idempotent")
	}
	vault.writeTarget = ""
	record.Access = strings.Repeat("x", maxCredentialBlob)
	if err := store.save(record); err == nil || vault.writeTarget != "" {
		t.Fatal("oversize credential was truncated or sent to the system store")
	}
}
