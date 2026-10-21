//go:build !windows

package wan

// Other platforms explicitly offer memory-only sign-in until a secure native
// store is implemented. There is no plaintext file or external-tool fallback.
func platformCredentialStore(string) credentialStore { return memoryCredentialStore{} }
