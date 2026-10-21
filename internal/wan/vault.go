package wan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	errCredentialMissing = errors.New("WAN credential is not saved")
	errVaultUnavailable  = errors.New("Secure WAN credential storage is unavailable; use a memory-only session")
)

// vaultSandbox matches testenv.Marker without importing testing into production.
// Its equality is checked in TestWANVaultNeverUsesSystemStoreInTestSandbox.
const vaultSandbox = "MAGPIE_TEST_SANDBOX"

// savedCredential belongs only to WAN and the explicit OAuth application. Refresh
// tokens are deliberately discarded: an expired access token needs a new sign-in.
// This record never enters settings, backups, status or the helper's diagnostics.
type savedCredential struct {
	Version   int       `json:"version"`
	ClientID  string    `json:"clientID"`
	Access    string    `json:"access"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

type credentialStore interface {
	available() bool
	load() (savedCredential, error)
	save(savedCredential) error
	delete() error
}

// memoryCredentialStore does not keep a second copy of the token. The auth owner
// holds the only memory session, and remember=true is explicitly unsupported.
type memoryCredentialStore struct{}

func (memoryCredentialStore) available() bool { return false }
func (memoryCredentialStore) load() (savedCredential, error) {
	return savedCredential{}, errCredentialMissing
}
func (memoryCredentialStore) save(savedCredential) error { return errVaultUnavailable }
func (memoryCredentialStore) delete() error              { return nil }

// credentialVault is the current user's OS vault, not a file protected by mode
// bits. Its tiny byte interface lets tests verify serialization without opening
// the real system vault (even when HOME has been isolated).
type credentialVault interface {
	read(string) ([]byte, error)
	write(string, []byte) error
	remove(string) error
}

type systemCredentialStore struct {
	target string
	vault  credentialVault
}

const maxCredentialBlob = 2560 // Windows' CRED_MAX_CREDENTIAL_BLOB_SIZE.

func (s systemCredentialStore) available() bool { return true }
func (s systemCredentialStore) load() (savedCredential, error) {
	data, err := s.vault.read(s.target)
	if err != nil {
		return savedCredential{}, err
	}
	defer clear(data)
	var record savedCredential
	if len(data) == 0 || len(data) > maxCredentialBlob || json.Unmarshal(data, &record) != nil || !validSavedCredential(record) {
		return savedCredential{}, errors.New("Saved WAN sign-in is unreadable; sign out before signing in again")
	}
	return record, nil
}
func (s systemCredentialStore) save(record savedCredential) error {
	if !validSavedCredential(record) {
		return errors.New("WAN credential is invalid")
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > maxCredentialBlob {
		return errors.New("WAN credential does not fit secure storage; use a memory-only session")
	}
	defer clear(data)
	if err := s.vault.write(s.target, data); err != nil {
		return errors.New("Could not save WAN sign-in in secure storage")
	}
	return nil
}
func (s systemCredentialStore) delete() error {
	if err := s.vault.remove(s.target); err != nil && !errors.Is(err, errCredentialMissing) {
		return errors.New("Could not erase saved WAN sign-in; retry sign out")
	}
	return nil
}

func validSavedCredential(record savedCredential) bool {
	return record.Version == 1 && oauthText(record.ClientID, 128) && oauthText(record.Access, 4096) &&
		(record.ExpiresAt.IsZero() || record.ExpiresAt.Year() >= 2020 && record.ExpiresAt.Year() <= 9999)
}

// A distinct settings directory gets a distinct credential. The target contains
// neither the user's folder name nor a token; Windows paths ignore case.
func credentialTarget(dir string, windowsPath bool) string {
	dir = filepath.Clean(dir)
	if windowsPath {
		dir = strings.ToLower(strings.ReplaceAll(dir, "\\", "/"))
	}
	sum := sha256.Sum256([]byte(dir))
	return "magpie/wan/github/v1/" + hex.EncodeToString(sum[:])
}

func defaultCredentialStore(dir string) credentialStore {
	// Never invoke a native vault under testenv, including a test binary that
	// was launched as a child. Changing HOME does not isolate Credential Manager.
	if os.Getenv(vaultSandbox) != "" {
		return memoryCredentialStore{}
	}
	return platformCredentialStore(dir)
}
