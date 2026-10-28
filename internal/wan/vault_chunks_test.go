package wan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWANMicrosoftBrowserLargeRememberedGrantRestores(t *testing.T) {
	v := &mapCredentialVault{values: map[string][]byte{}}
	s := systemCredentialStore{target: "browser-own-scope", vault: v}
	a, opened := browserTestAuth(t, s)
	record := largeMicrosoftCredential()
	a.microsoft.client.Transport = githubRoundTrip(func(*http.Request) (*http.Response, error) {
		body, _ := json.Marshal(githubGrant{Access: record.Access, Refresh: record.Refresh, Type: "Bearer", ExpiresIn: 3600, ClientInfo: fixtureMicrosoftInfo()})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	if _, err := a.Start(true, "microsoft"); err != nil {
		t.Fatal(err)
	}
	u := browserURL(t, opened)
	redirect, _ := url.Parse(u.Query().Get("redirect_uri"))
	browserCallback(t, redirect.String(), url.Values{"state": {u.Query().Get("state")}, "code": {"large-browser-code"}}.Encode(), "GET", redirect.Host, "")
	if status := awaitAuth(t, a, func(s AuthStatus) bool { return s.State == "signed_in" || s.State == "error" }); status.State != "signed_in" || status.Storage != "system" {
		t.Fatal("large browser grant could not be remembered", status.State)
	}
	a.CancelJoin()
	restored := newAuth(context.Background(), DefaultGitHubClientID, s)
	defer restored.CancelJoin()
	if status := awaitAuth(t, restored, func(s AuthStatus) bool { return s.State != "loading" }); status.State != "signed_in" || status.Storage != "system" {
		t.Fatal("large Microsoft grant did not restore")
	}
	if token, err := restored.token(); err != nil || token != record.Access {
		t.Fatal("restored a different or incomplete access token", err)
	}
}

type mapCredentialVault struct {
	values       map[string][]byte
	failWrite    func(string) bool
	failRemove   func(string) bool
	readBuffers  [][]byte
	writeBuffers [][]byte
}

func (v *mapCredentialVault) read(target string) ([]byte, error) {
	data, ok := v.values[target]
	if !ok {
		return nil, errCredentialMissing
	}
	b := bytes.Clone(data)
	v.readBuffers = append(v.readBuffers, b)
	return b, nil
}
func (v *mapCredentialVault) write(target string, data []byte) error {
	if len(data) > maxCredentialBlob {
		return errors.New("oversized native blob")
	}
	if v.failWrite != nil && v.failWrite(target) {
		return errors.New("SECRET native write error")
	}
	v.values[target] = bytes.Clone(data)
	v.writeBuffers = append(v.writeBuffers, data)
	return nil
}
func (v *mapCredentialVault) remove(target string) error {
	if v.failRemove != nil && v.failRemove(target) {
		return errors.New("SECRET native delete error")
	}
	if _, ok := v.values[target]; !ok {
		return errCredentialMissing
	}
	delete(v.values, target)
	return nil
}

func incompressibleFixture(label string, size int) string {
	var b []byte
	for i := 0; len(b) < size; i++ {
		sum := sha256.Sum256(fmt.Appendf(nil, "%s-%d", label, i))
		b = append(b, sum[:]...)
	}
	return base64.RawURLEncoding.EncodeToString(b)[:size]
}

func largeMicrosoftCredential() savedCredential {
	return microsoftCredential(githubGrant{ClientInfo: fixtureMicrosoftInfo(), Access: incompressibleFixture("access", 2748), Refresh: incompressibleFixture("refresh", 1753), ExpiresIn: 3600}, time.Now().UTC().Truncate(time.Second))
}

func TestWANVaultLargeMicrosoftRecordRestoresAndRotates(t *testing.T) {
	v := &mapCredentialVault{values: map[string][]byte{}}
	s := systemCredentialStore{target: "own-scope", vault: v}
	record := largeMicrosoftCredential()
	for _, size := range []string{"large", "large-again", "small", "large-after-small"} {
		t.Run(size, func(t *testing.T) {
			if size == "small" {
				record.Access = "small-access"
				record.Refresh = "small-refresh"
			} else {
				record = largeMicrosoftCredential()
				record.Access = incompressibleFixture(size, 2748)
			}
			if err := s.save(record); err != nil {
				t.Fatal(err)
			}
			loaded, err := s.load()
			if err != nil || loaded != record {
				t.Fatal("native multi-blob round trip lost the token pair or identity", err)
			}
			if size != "small" {
				p, err := parseCredentialParts(v.values[s.target])
				if err != nil || len(v.values) != p.Count+1 {
					t.Fatal("large credential was not committed or old parts remain", err)
				}
			} else if len(v.values) != 1 {
				t.Fatal("old large credential fragments remain")
			}
			for target, b := range v.values {
				if !strings.HasPrefix(target, s.target) || len(b) > maxCredentialBlob {
					t.Fatal("native store scope or size escaped")
				}
			}
		})
	}
	if err := s.delete(); err != nil || len(v.values) != 0 {
		t.Fatal("logout did not erase all credential fragments", err)
	}
	for _, b := range append(v.readBuffers, v.writeBuffers...) {
		for _, c := range b {
			if c != 0 {
				t.Fatal("temporary credential byte buffer retained")
			}
		}
	}
}

func TestWANVaultIncompleteLargeWriteKeepsCommittedPair(t *testing.T) {
	for _, phase := range []string{"first-part", "second-part", "root"} {
		t.Run(phase, func(t *testing.T) {
			v := &mapCredentialVault{values: map[string][]byte{}}
			s := systemCredentialStore{target: "own-scope", vault: v}
			old := largeMicrosoftCredential()
			if err := s.save(old); err != nil {
				t.Fatal(err)
			}
			parts, _ := parseCredentialParts(v.values[s.target])
			slot := 1 - parts.Slot
			bad := s.target
			if phase == "first-part" {
				bad = s.partTarget(slot, 0)
			}
			if phase == "second-part" {
				bad = s.partTarget(slot, 1)
			}
			v.failWrite = func(target string) bool { return target == bad }
			next := old
			next.Refresh = incompressibleFixture("new-refresh", 1753)
			if err := s.save(next); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("partial native write reported success or leaked an error")
			}
			if loaded, err := s.load(); err != nil || loaded != old {
				t.Fatal("failed write damaged the committed credential", err)
			}
			v.failWrite = nil
			if err := s.delete(); err != nil || len(v.values) != 0 {
				t.Fatal("logout retained interrupted write fragments", err)
			}
		})
	}
}

func TestWANVaultMissingOrTamperedPartsAreUnknown(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "oversized", "manifest-count", "manifest-slot", "manifest-path"} {
		t.Run(damage, func(t *testing.T) {
			v := &mapCredentialVault{values: map[string][]byte{}}
			s := systemCredentialStore{target: "own-scope", vault: v}
			if err := s.save(largeMicrosoftCredential()); err != nil {
				t.Fatal(err)
			}
			parts, _ := parseCredentialParts(v.values[s.target])
			target := s.partTarget(parts.Slot, 0)
			switch damage {
			case "missing":
				delete(v.values, target)
			case "corrupt":
				v.values[target][0] ^= 1
			case "oversized":
				v.values[target] = append(v.values[target], 0)
			default:
				if damage == "manifest-count" {
					parts.Count = 1000000
				}
				if damage == "manifest-slot" {
					parts.Slot = 2
				}
				if damage == "manifest-path" {
					parts.Digest = "../../another-account"
				}
				b, _ := json.Marshal(parts)
				v.values[s.target] = append([]byte(credentialChunksPrefix), b...)
			}
			if record, err := s.load(); err == nil || errors.Is(err, errCredentialMissing) || record.Access != "" {
				t.Fatal("damaged secure credential was usable or treated as missing")
			}
		})
	}
}

func TestWANVaultLogoutRetriesFragmentsAfterRootRemoval(t *testing.T) {
	v := &mapCredentialVault{values: map[string][]byte{}}
	s := systemCredentialStore{target: "own-scope", vault: v}
	if err := s.save(largeMicrosoftCredential()); err != nil {
		t.Fatal(err)
	}
	parts, _ := parseCredentialParts(v.values[s.target])
	bad := s.partTarget(parts.Slot, 0)
	v.failRemove = func(target string) bool { return target == bad }
	if err := s.delete(); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("partial erasure reported success or leaked native data")
	}
	if _, exists := v.values[s.target]; exists {
		t.Fatal("logout left a restorable root")
	}
	v.failRemove = nil
	if err := s.delete(); err != nil || len(v.values) != 0 {
		t.Fatal("retry could not erase orphaned fragments", err)
	}
}
