package wan

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Public device-flow client used by the official Dev Tunnels CLI; the resource
// audience is published in the SDK's TunnelServiceProperties contract.
const microsoftClientID = "c0df98ca-23b4-4bce-bb9f-72039b28d3a5"
const microsoftAudience = "46da2f7e-b5ef-422a-88d4-2a7f9de6a0b2"
const microsoftScope = microsoftAudience + "/.default offline_access openid profile"

type microsoftDeviceClient struct {
	client *http.Client
	base   string
}

func newMicrosoftDeviceClient() microsoftDeviceClient {
	return microsoftDeviceClient{client: newGitHubDeviceClient().client, base: "https://login.microsoftonline.com/common/oauth2/v2.0"}
}

func (c microsoftDeviceClient) post(ctx context.Context, path string, values url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(values.Encode()))
	if err != nil {
		return errors.New("Could not prepare Microsoft WAN authorization")
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Could not reach Microsoft for WAN authorization; check network and proxy settings")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 400 {
		return errors.New("Microsoft WAN authorization is temporarily unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (32<<10)+1))
	if err != nil || len(b) > 32<<10 || json.Unmarshal(b, out) != nil {
		return errors.New("Microsoft returned an unreadable WAN authorization response")
	}
	return ctx.Err()
}

func (c microsoftDeviceClient) code(ctx context.Context) (githubDeviceCode, error) {
	var out struct {
		githubDeviceCode
		Error string `json:"error"`
	}
	if err := c.post(ctx, "/devicecode", url.Values{"client_id": {microsoftClientID}, "scope": {microsoftScope}}, &out); err != nil {
		return githubDeviceCode{}, err
	}
	if out.Error != "" {
		return githubDeviceCode{}, microsoftAuthorizationError(out.Error)
	}
	if (out.VerificationURI != "https://microsoft.com/devicelogin" && out.VerificationURI != "https://login.microsoft.com/device") || !oauthText(out.DeviceCode, 4096) || !githubUserCode(out.UserCode) || out.ExpiresIn < 1 || out.ExpiresIn > 3600 || out.Interval < 0 || out.Interval > 60 {
		return githubDeviceCode{}, errors.New("Microsoft returned an unsupported WAN device code response")
	}
	if out.Interval == 0 {
		out.Interval = 5
	}
	return out.githubDeviceCode, nil
}
func (c microsoftDeviceClient) poll(ctx context.Context, code string) (githubGrant, error) {
	var out githubGrant
	if err := c.post(ctx, "/token", url.Values{"client_id": {microsoftClientID}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {code}}, &out); err != nil {
		return out, err
	}
	if out.Error != "" && out.Error != "authorization_pending" && out.Error != "slow_down" {
		return out, microsoftAuthorizationError(out.Error)
	}
	if out.Error == "" && !validMicrosoftGrant(out) {
		return out, errors.New("Microsoft returned an unsupported WAN credential")
	}
	return out, nil
}
func (c microsoftDeviceClient) refresh(ctx context.Context, refresh string) (githubGrant, error) {
	var out githubGrant
	if err := c.post(ctx, "/token", url.Values{"client_id": {microsoftClientID}, "grant_type": {"refresh_token"}, "refresh_token": {refresh}, "scope": {microsoftScope}}, &out); err != nil {
		return out, err
	}
	if out.Error == "temporarily_unavailable" || out.Error == "server_error" || out.Error == "slow_down" {
		return githubGrant{}, errors.New("Microsoft WAN renewal is temporarily unavailable; automatic retry is scheduled")
	}
	if out.Error != "" || !validMicrosoftGrant(out) {
		return githubGrant{}, errRefreshRejected
	}
	return out, nil
}
func validMicrosoftGrant(g githubGrant) bool {
	_, ok := microsoftAccount(g.Access)
	return ok && oauthText(g.Access, 8192) && oauthText(g.Refresh, 8192) && strings.EqualFold(g.Type, "bearer") && g.ExpiresIn > 0 && g.ExpiresIn <= 86400
}
func microsoftAuthorizationError(code string) error {
	switch code {
	case "authorization_declined", "access_denied":
		return errors.New("Microsoft WAN authorization was denied")
	case "expired_token":
		return errors.New("Microsoft device code for WAN expired; start sign-in again")
	case "invalid_client", "unauthorized_client":
		return errors.New("Microsoft refused the Dev Tunnels device login client")
	case "invalid_grant", "interaction_required":
		return errRefreshRejected
	default:
		return errors.New("Microsoft refused WAN authorization; check account and tenant policies")
	}
}

var microsoftGUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// These labels are decoded only from a token received by our TLS OAuth flow or
// secure store. They are not an authentication decision; the service verifies
// the bearer token on every operation. Never accept a caller-supplied JWT here.
func microsoftAccount(token string) (Account, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 8192 {
		return Account{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Account{}, false
	}
	var claims struct {
		Audience string `json:"aud"`
		Tenant   string `json:"tid"`
		Object   string `json:"oid"`
		Name     string `json:"preferred_username"`
		UPN      string `json:"upn"`
		Unique   string `json:"unique_name"`
	}
	if json.Unmarshal(b, &claims) != nil || claims.Audience != microsoftAudience || !microsoftGUID.MatchString(claims.Tenant) || !microsoftGUID.MatchString(claims.Object) {
		return Account{}, false
	}
	name := claims.Name
	if name == "" {
		name = claims.UPN
	}
	if name == "" {
		name = claims.Unique
	}
	if !oauthText(name, 254) {
		name = "Microsoft account"
	}
	return Account{ID: "microsoft:" + strings.ToLower(claims.Tenant) + ":" + strings.ToLower(claims.Object), Name: name, Provider: "microsoft"}, true
}

func microsoftCredential(g githubGrant, issued time.Time) savedCredential {
	end := issued.Add(time.Duration(g.ExpiresIn) * time.Second)
	return savedCredential{Version: 3, Provider: "microsoft", ClientID: microsoftClientID, Access: g.Access, Refresh: g.Refresh, ExpiresAt: end, RefreshAfter: end.Add(-min(5*time.Minute, time.Duration(g.ExpiresIn)*time.Second/4))}
}
