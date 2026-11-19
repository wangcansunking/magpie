package wan

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
)

type TunnelInfo struct {
	ID          string `json:"id"`
	Region      string `json:"region"`
	Port        int    `json:"port"`
	AccountID   string `json:"accountID,omitempty"`
	AccountName string `json:"accountName,omitempty"`
	New         bool   `json:"new"`
}

func tunnelInfo(c tunnelConfig) *TunnelInfo {
	return &TunnelInfo{ID: c.ID, Region: c.ClusterID, Port: c.Port, AccountID: c.AccountID, AccountName: c.AccountName, New: c.New}
}

func (m *Manager) RefreshUsage() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.blocked || m.owner == nil || m.owner.Err() != nil {
		return errors.New("WAN gateway owner is not running or is busy")
	}
	if m.auth == nil || m.auth.Status().State != "signed_in" {
		return errors.New("Sign in before checking Dev Tunnels usage")
	}
	if !m.usage.refresh() {
		return errors.New("Usage was just checked; try again shortly")
	}
	return nil
}

// RequestNewTunnel records explicit intent, not an instruction to host at boot.
// Creation happens only on Start. Existing remote tunnels and WAN keys stay intact.
func (m *Manager) RequestNewTunnel() error {
	m.mu.Lock()
	if m.closed || m.blocked || m.owner == nil || m.owner.Err() != nil || m.current != nil {
		m.mu.Unlock()
		return errors.New("Stop WAN before choosing a new tunnel")
	}
	m.blocked = true
	m.actions.Add(1)
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.blocked = false; m.mu.Unlock(); m.actions.Done() }()
	unlock, held := filelock.Lock(m.path+".lock", 0)
	if !held {
		return errors.New("WAN tunnel settings are busy; try again")
	}
	defer unlock()
	token, _, err := m.auth.tokenSnapshot()
	if err != nil {
		return err
	}
	account, err := m.account(m.owner, token)
	if err != nil {
		return err
	}
	accounts, err := readTunnelAccounts(m.path)
	if err != nil {
		return err
	}
	c, exists := accounts[account.ID]
	if !exists {
		return errors.New("The next Start already creates a new tunnel")
	}
	c.New = true
	accounts[account.ID] = c
	if err := writeTunnelAccounts(m.path, accounts); err != nil {
		return err
	}
	m.mu.Lock()
	m.status.Tunnel = tunnelInfo(c)
	m.tunnels = accounts
	m.mu.Unlock()
	return nil
}

// validAccountBinding keeps the settings boundary bounded, including old files
// without an owner. Empty ownership deliberately causes fresh creation on Start.
func validAccountBinding(c tunnelConfig) bool {
	return (c.AccountID == "" || oauthText(c.AccountID, 160)) && len(c.AccountName) <= 254 && !containsControl(c.AccountName)
}
func containsControl(s string) bool {
	for _, r := range s {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}

func accountTunnelsPath(active string) string {
	return filepath.Join(filepath.Dir(active), "wan-tunnels.json")
}
func readTunnelAccounts(active string) (map[string]tunnelConfig, error) {
	path := accountTunnelsPath(active)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]tunnelConfig{}, nil
	}
	if err != nil {
		return nil, errors.New("Could not read saved account tunnels; refusing to replace them")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	var out struct {
		Version  int                     `json:"version"`
		Accounts map[string]tunnelConfig `json:"accounts"`
	}
	if err != nil || len(b) > 128<<10 || strictJSON(b, &out) != nil || out.Version != 1 || out.Accounts == nil || len(out.Accounts) > 100 {
		return nil, errors.New("Saved account tunnels are unreadable; refusing to replace them")
	}
	for id, c := range out.Accounts {
		if id == "" || c.AccountID != id || !validCanonicalConfig(c) || !validAccountBinding(c) {
			return nil, errors.New("Saved account tunnels are unreadable; refusing to replace them")
		}
	}
	return out.Accounts, nil
}
func writeTunnelAccounts(active string, accounts map[string]tunnelConfig) error {
	b, err := json.Marshal(struct {
		Version  int                     `json:"version"`
		Accounts map[string]tunnelConfig `json:"accounts"`
	}{1, accounts})
	if err != nil || len(b) > 128<<10 || len(accounts) > 100 {
		return errors.New("Account tunnel settings are too large")
	}
	if edit.WriteAtomic(accountTunnelsPath(active), append(b, '\n')) != nil {
		return errors.New("Could not save account tunnel identity; no tunnel was hosted")
	}
	return nil
}
