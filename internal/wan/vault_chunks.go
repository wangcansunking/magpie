package wan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const credentialChunksPrefix = "magpie-wan-parts-v1\n"
const maxCredentialParts = ((32 << 10) + maxCredentialBlob - 1) / maxCredentialBlob

type credentialParts struct {
	Slot   int    `json:"slot"`
	Count  int    `json:"count"`
	Size   int    `json:"size"`
	Digest string `json:"sha256"`
}

func parseCredentialParts(data []byte) (credentialParts, error) {
	var parts credentialParts
	if len(data) > maxCredentialBlob || !bytes.HasPrefix(data, []byte(credentialChunksPrefix)) ||
		strictJSON(data[len(credentialChunksPrefix):], &parts) != nil ||
		parts.Slot < 0 || parts.Slot > 1 || parts.Size <= maxCredentialBlob || parts.Size > 32<<10 ||
		parts.Count != (parts.Size+maxCredentialBlob-1)/maxCredentialBlob || parts.Count > maxCredentialParts ||
		len(parts.Digest) != 64 || len(bytes.Trim([]byte(parts.Digest), "0123456789abcdef")) != 0 {
		return credentialParts{}, errors.New("Saved WAN sign-in is unreadable; sign out before signing in again")
	}
	return parts, nil
}

func (s systemCredentialStore) partTarget(slot, part int) string {
	return fmt.Sprintf("%s/part-v1/%d/%d", s.target, slot, part)
}

func (s systemCredentialStore) readParts(root []byte) ([]byte, error) {
	parts, err := parseCredentialParts(root)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, parts.Size)
	for i := 0; i < parts.Count; i++ {
		part, err := s.vault.read(s.partTarget(parts.Slot, i))
		expected := min(maxCredentialBlob, parts.Size-i*maxCredentialBlob)
		if err != nil || len(part) != expected {
			clear(part)
			clear(data)
			return nil, errors.New("Saved WAN sign-in is unreadable; sign out before signing in again")
		}
		data = append(data, part...)
		clear(part)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != parts.Digest {
		clear(data)
		return nil, errors.New("Saved WAN sign-in is unreadable; sign out before signing in again")
	}
	return data, nil
}

func (s systemCredentialStore) eraseParts(slot int) error {
	var failed bool
	for i := 0; i < maxCredentialParts; i++ {
		if err := s.vault.remove(s.partTarget(slot, i)); err != nil && !errors.Is(err, errCredentialMissing) {
			failed = true
		}
	}
	if failed {
		return errors.New("Could not erase saved WAN sign-in; retry sign out")
	}
	return nil
}

// Write the inactive native-vault slot, then atomically switch its tiny root.
// Fixed bounded slots allow logout to erase interrupted writes too, without
// enumeration, files, or token-bearing target names. The auth commit gate owns
// serialization and handles unknown native write outcomes by erasing the store.
func (s systemCredentialStore) writeParts(data []byte) error {
	if len(data) <= maxCredentialBlob || len(data) > 32<<10 {
		return errors.New("WAN credential does not fit secure storage; use a memory-only session")
	}
	root, err := s.vault.read(s.target)
	defer clear(root)
	if err != nil && !errors.Is(err, errCredentialMissing) {
		return errors.New("Could not save WAN sign-in in secure storage")
	}
	slot := 0
	if bytes.HasPrefix(root, []byte(credentialChunksPrefix)) {
		previous, err := parseCredentialParts(root)
		if err != nil {
			return err
		}
		slot = 1 - previous.Slot
	}
	if err := s.eraseParts(slot); err != nil {
		return err
	}
	count := (len(data) + maxCredentialBlob - 1) / maxCredentialBlob
	for i := 0; i < count; i++ {
		part := data[i*maxCredentialBlob : min(len(data), (i+1)*maxCredentialBlob)]
		if err := s.vault.write(s.partTarget(slot, i), part); err != nil {
			return errors.New("Could not save WAN sign-in in secure storage")
		}
	}
	sum := sha256.Sum256(data)
	manifest, err := json.Marshal(credentialParts{Slot: slot, Count: count, Size: len(data), Digest: hex.EncodeToString(sum[:])})
	if err != nil {
		return errors.New("Could not save WAN sign-in in secure storage")
	}
	encoded := append([]byte(credentialChunksPrefix), manifest...)
	defer clear(encoded)
	if err := s.vault.write(s.target, encoded); err != nil {
		return errors.New("Could not save WAN sign-in in secure storage")
	}
	return s.eraseParts(1 - slot)
}
