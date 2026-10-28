package wan

import (
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsCredential is CREDENTIALW. CRED_TYPE_GENERIC and local-machine persist
// mean a credential in the current user's vault, not one shared with other users.
type windowsCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// Native calls have their own sandbox check, not only the store factory's. Tests
// inject this narrow interface and never call Credential Manager on the machine.
type windowsVaultCalls interface {
	read(*uint16) (*windowsCredential, error)
	write(*windowsCredential) error
	remove(*uint16) error
	free(*windowsCredential)
}

type windowsCredentialVault struct{ calls windowsVaultCalls }

func platformCredentialStore(dir string) credentialStore {
	return systemCredentialStore{target: credentialTarget(dir, true), vault: windowsCredentialVault{nativeWindowsVault{}}}
}

func (v windowsCredentialVault) read(target string) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return nil, errors.New("Invalid WAN credential target")
	}
	record, err := v.calls.read(name)
	runtime.KeepAlive(name)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, errors.New("Secure storage returned an unreadable WAN credential")
	}
	defer v.calls.free(record)
	if record.Type != 1 || record.TargetName == nil || windows.UTF16PtrToString(record.TargetName) != target ||
		record.CredentialBlob == nil || record.CredentialBlobSize == 0 || record.CredentialBlobSize > maxCredentialBlob {
		return nil, errors.New("Secure storage returned an unreadable WAN credential")
	}
	blob := unsafe.Slice(record.CredentialBlob, int(record.CredentialBlobSize))
	defer clear(blob)
	return append([]byte(nil), blob...), nil
}

func (v windowsCredentialVault) write(target string, data []byte) error {
	if len(data) == 0 || len(data) > maxCredentialBlob {
		return errors.New("WAN credential does not fit secure storage")
	}
	name, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return errors.New("Invalid WAN credential target")
	}
	user, _ := windows.UTF16PtrFromString("magpie WAN GitHub")
	record := windowsCredential{
		Type: 1, TargetName: name, UserName: user,
		CredentialBlobSize: uint32(len(data)), CredentialBlob: &data[0],
		Persist: 2, // CRED_PERSIST_LOCAL_MACHINE, current user only.
	}
	err = v.calls.write(&record)
	runtime.KeepAlive(name)
	runtime.KeepAlive(user)
	runtime.KeepAlive(data)
	return err
}

func (v windowsCredentialVault) remove(target string) error {
	name, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return errors.New("Invalid WAN credential target")
	}
	err = v.calls.remove(name)
	runtime.KeepAlive(name)
	return err
}

type nativeWindowsVault struct{}

var (
	wanAdvapi32   = windows.NewLazySystemDLL("advapi32.dll")
	wanCredRead   = wanAdvapi32.NewProc("CredReadW")
	wanCredWrite  = wanAdvapi32.NewProc("CredWriteW")
	wanCredDelete = wanAdvapi32.NewProc("CredDeleteW")
	wanCredFree   = wanAdvapi32.NewProc("CredFree")
)

func (nativeWindowsVault) read(target *uint16) (*windowsCredential, error) {
	if os.Getenv(vaultSandbox) != "" {
		return nil, errVaultUnavailable
	}
	var record *windowsCredential
	ok, _, callErr := wanCredRead.Call(uintptr(unsafe.Pointer(target)), 1, 0, uintptr(unsafe.Pointer(&record)))
	runtime.KeepAlive(target)
	if ok == 0 {
		return nil, windowsVaultError(callErr)
	}
	return record, nil
}

func (nativeWindowsVault) write(record *windowsCredential) error {
	if os.Getenv(vaultSandbox) != "" {
		return errVaultUnavailable
	}
	ok, _, callErr := wanCredWrite.Call(uintptr(unsafe.Pointer(record)), 0)
	runtime.KeepAlive(record)
	if ok == 0 {
		return windowsVaultError(callErr)
	}
	return nil
}

func (nativeWindowsVault) remove(target *uint16) error {
	if os.Getenv(vaultSandbox) != "" {
		return errVaultUnavailable
	}
	ok, _, callErr := wanCredDelete.Call(uintptr(unsafe.Pointer(target)), 1, 0)
	runtime.KeepAlive(target)
	if ok == 0 {
		return windowsVaultError(callErr)
	}
	return nil
}

func (nativeWindowsVault) free(record *windowsCredential) {
	if os.Getenv(vaultSandbox) == "" {
		wanCredFree.Call(uintptr(unsafe.Pointer(record)))
	}
}

func windowsVaultError(err error) error {
	if errors.Is(err, windows.ERROR_NOT_FOUND) {
		return errCredentialMissing
	}
	return errors.New("WAN secure credential storage could not complete the operation")
}
