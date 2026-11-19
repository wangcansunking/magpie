package wan

import (
	"bytes"
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fakeWindowsVaultCalls struct {
	record  windowsCredential
	blob    []byte
	target  *uint16
	readErr error
	freed   bool
	written windowsCredential
	data    []byte
	removed string
}

func (v *fakeWindowsVaultCalls) read(target *uint16) (*windowsCredential, error) {
	if v.readErr != nil {
		return nil, v.readErr
	}
	return &v.record, nil
}
func (v *fakeWindowsVaultCalls) write(record *windowsCredential) error {
	v.written = *record
	v.data = append([]byte(nil), unsafe.Slice(record.CredentialBlob, int(record.CredentialBlobSize))...)
	return nil
}
func (v *fakeWindowsVaultCalls) remove(target *uint16) error {
	v.removed = windows.UTF16PtrToString(target)
	return nil
}
func (v *fakeWindowsVaultCalls) free(*windowsCredential) { v.freed = true }

func TestWANWindowsVaultGenericCurrentUserContractWithoutNativeCalls(t *testing.T) {
	const target = "magpie/wan/github/v1/fake-settings-hash"
	calls := &fakeWindowsVaultCalls{}
	vault := windowsCredentialVault{calls}
	data := []byte("fake-utf8-credential")
	if err := vault.write(target, data); err != nil {
		t.Fatal(err)
	}
	written := calls.written
	if written.Type != 1 || written.Persist != 2 || written.Flags != 0 || written.AttributeCount != 0 || written.Attributes != 0 || written.TargetAlias != nil ||
		windows.UTF16PtrToString(written.TargetName) != target || windows.UTF16PtrToString(written.UserName) != "magpie WAN GitHub" || !bytes.Equal(calls.data, data) {
		t.Fatal("CredWriteW contract was not a current-user generic, locally persistent WAN record")
	}
	calls.blob = append([]byte(nil), data...)
	calls.target, _ = windows.UTF16PtrFromString(target)
	calls.record = windowsCredential{Type: 1, TargetName: calls.target, CredentialBlobSize: uint32(len(calls.blob)), CredentialBlob: &calls.blob[0]}
	read, err := vault.read(target)
	if err != nil || !bytes.Equal(read, data) || !calls.freed {
		t.Fatalf("CredReadW did not copy and free the returned record: %v", err)
	}
	if !bytes.Equal(calls.blob, make([]byte, len(calls.blob))) {
		t.Fatal("native returned credential blob was not cleared before freeing")
	}
	if err := vault.remove(target); err != nil || calls.removed != target {
		t.Fatal("CredDeleteW used a different WAN target")
	}
}

func TestWANWindowsVaultRejectsWrongOrOversizeNativeRecord(t *testing.T) {
	for _, corrupt := range []func(*fakeWindowsVaultCalls){
		func(v *fakeWindowsVaultCalls) { v.record.Type = 2 },
		func(v *fakeWindowsVaultCalls) { v.record.TargetName, _ = windows.UTF16PtrFromString("another-target") },
		func(v *fakeWindowsVaultCalls) { v.record.CredentialBlobSize = maxCredentialBlob + 1 },
		func(v *fakeWindowsVaultCalls) { v.record.CredentialBlob = nil },
		func(v *fakeWindowsVaultCalls) { v.record.CredentialBlobSize = 0 },
	} {
		calls := &fakeWindowsVaultCalls{blob: []byte("fake")}
		calls.target, _ = windows.UTF16PtrFromString("own-target")
		calls.record = windowsCredential{Type: 1, TargetName: calls.target, CredentialBlobSize: uint32(len(calls.blob)), CredentialBlob: &calls.blob[0]}
		corrupt(calls)
		if data, err := (windowsCredentialVault{calls}).read("own-target"); err == nil || len(data) != 0 || !calls.freed {
			t.Fatal("invalid CREDENTIALW was read as a usable WAN credential or leaked its allocation")
		}
	}
	if err := windowsVaultError(windows.ERROR_NOT_FOUND); !errors.Is(err, errCredentialMissing) {
		t.Fatal("native missing credential was not distinguished from failure")
	}
}

func TestWANWindowsNativeVaultRefusesTestSandboxBeforeAnySystemCall(t *testing.T) {
	// TestMain set the marker. These methods must refuse even when someone
	// bypasses the factory; no user's Credential Manager is opened by this test.
	calls := nativeWindowsVault{}
	if _, err := calls.read(nil); !errors.Is(err, errVaultUnavailable) {
		t.Fatal("native vault read escaped the test sandbox")
	}
	if err := calls.write(nil); !errors.Is(err, errVaultUnavailable) {
		t.Fatal("native vault write escaped the test sandbox")
	}
	if err := calls.remove(nil); !errors.Is(err, errVaultUnavailable) {
		t.Fatal("native vault delete escaped the test sandbox")
	}
}
