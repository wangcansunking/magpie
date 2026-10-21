package wan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
)

// githubDeviceClient speaks only the device grant protocol. Its construction
// does not request a code, discover an account or start a browser or tunnel.
// The auth owner must check its attempt again before committing a returned token.
type githubDeviceClient struct {
	client *http.Client
	base   string
}

func newGitHubDeviceClient() githubDeviceClient {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = netproxy.Func
	return githubDeviceClient{
		base: "https://github.com",
		client: &http.Client{
			Transport: netproxy.Dispatch(t),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type githubDeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// githubGrant stays backend-only, including any refresh token. No instance of
// it is part of Status, a GUI response or the on-disk tunnel configuration.
type githubGrant struct {
	Access           string `json:"access_token"`
	Type             string `json:"token_type"`
	Scope            string `json:"scope"`
	Refresh          string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_token_expires_in"`
	Error            string `json:"error"`
	Interval         int    `json:"interval"`
}

func (c githubDeviceClient) code(ctx context.Context, clientID, scope string) (githubDeviceCode, error) {
	var out githubDeviceCode
	if !oauthText(clientID, 128) || len(scope) > 128 || strings.ContainsAny(scope, "\r\n\x00") {
		return out, errors.New("WAN GitHub OAuth Client ID or scope is invalid")
	}
	values := url.Values{"client_id": {clientID}}
	if scope != "" {
		values.Set("scope", scope)
	}
	var response struct {
		githubDeviceCode
		Error string `json:"error"`
	}
	if err := c.post(ctx, "/login/device/code", values, &response); err != nil {
		return githubDeviceCode{}, err
	}
	if response.Error != "" {
		return githubDeviceCode{}, githubAuthorizationError(response.Error)
	}
	out = response.githubDeviceCode
	// A verification address is shown to the user, not followed by this client.
	// Keep it on GitHub's device page, without query parameters or credentials.
	if out.VerificationURI != "https://github.com/login/device" ||
		!oauthText(out.DeviceCode, 4096) || !githubUserCode(out.UserCode) ||
		out.ExpiresIn < 1 || out.ExpiresIn > 3600 || out.Interval < 0 || out.Interval > 60 {
		return githubDeviceCode{}, errors.New("GitHub returned an unsupported WAN device code response")
	}
	if out.Interval == 0 {
		out.Interval = 5 // RFC 8628's default when the server omits interval.
	}
	return out, nil
}

func (c githubDeviceClient) poll(ctx context.Context, clientID, deviceCode string) (githubGrant, error) {
	var out githubGrant
	if !oauthText(clientID, 128) || !oauthText(deviceCode, 4096) {
		return out, errors.New("WAN GitHub device grant is invalid")
	}
	err := c.post(ctx, "/login/oauth/access_token", url.Values{
		"client_id": {clientID}, "device_code": {deviceCode},
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
	}, &out)
	if err != nil {
		return githubGrant{}, err
	}
	if out.Error != "" {
		if out.Access != "" || out.Refresh != "" {
			return githubGrant{}, errors.New("GitHub returned an ambiguous WAN token response")
		}
		switch out.Error {
		case "authorization_pending", "slow_down":
			if out.Interval < 0 || out.Interval > 3600 {
				return githubGrant{}, errors.New("GitHub returned an unsupported WAN polling interval")
			}
			return githubGrant{Error: out.Error, Interval: out.Interval}, nil
		default:
			return githubGrant{}, githubAuthorizationError(out.Error)
		}
	}
	if !oauthText(out.Access, 4096) || !strings.EqualFold(out.Type, "bearer") ||
		len(out.Scope) > 4096 || strings.ContainsAny(out.Scope, "\r\n\x00") ||
		out.ExpiresIn < 0 || out.RefreshExpiresIn < 0 ||
		(out.Refresh != "" && !oauthText(out.Refresh, 4096)) {
		return githubGrant{}, errors.New("GitHub returned an unsupported WAN token response")
	}
	return out, nil
}

func (c githubDeviceClient) post(ctx context.Context, path string, values url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(values.Encode()))
	if err != nil {
		return errors.New("Could not prepare WAN GitHub authorization request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Transport errors can quote a proxy URL or request data. Only the
		// bounded, fixed classification crosses the administrative boundary.
		return errors.New("Could not reach GitHub for WAN authorization; check the network and proxy")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("GitHub refused the WAN authorization request")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (32<<10)+1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(data) > 32<<10 || json.Unmarshal(data, out) != nil {
		return errors.New("GitHub returned an unreadable WAN authorization response")
	}
	return ctx.Err()
}

func githubAuthorizationError(code string) error {
	switch code {
	case "access_denied":
		return errors.New("GitHub authorization for WAN was denied")
	case "expired_token", "token_expired":
		return errors.New("GitHub device code for WAN expired; start sign-in again")
	case "device_flow_disabled":
		return errors.New("GitHub device authorization is unavailable for the Dev Tunnels service app")
	case "incorrect_client_credentials":
		return errors.New("GitHub refused the WAN OAuth Client ID")
	default:
		return errors.New("GitHub refused the WAN device grant")
	}
}

func oauthText(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func githubUserCode(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
