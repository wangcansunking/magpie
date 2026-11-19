# Microsoft system-browser sign-in — 2026-10-23

## Problem and resulting behavior

An organizational account could use `devtunnel user login` while magpie's
device-code-only flow was refused by Conditional Access. Microsoft now defaults
to authorization code with S256 PKCE and the OS default browser. It uses the
same public Dev Tunnels client/resource and localhost-root redirect as the
official CLI's interactive flow; it does not reuse CLI token caches or automate
the user's browser credentials. GitHub keeps its device flow.

The backend opens the system browser once after holding a random IPv4 loopback
port. No device code is requested for this default. The page shows browser
authorization pending, its deadline, Cancel and an explicit Reopen action.
Reopen accepts only the current attempt ID, not a caller-supplied URL. Reading
status, remounting the page and restarting magpie never open a browser.
OS launch errors remain visible and retryable. Browser errors never silently
switch to device flow. Advanced callers may explicitly select Microsoft's
`method: "device"`.

The callback requires the expected host, root path, GET, absent Origin, exact
random state, unique parameters and exactly one code/error. It is one-use and
bounded. The fixed-length non-reflecting response is flushed before token
exchange can close the callback server. The authorization URL, code, state and
verifier never enter the GUI snapshot or logs. Callback connections are joined
on close. Cancellation/logout close the port even while a canceled exchange
returns late; replacement retires the old callback before opening the next one.
Late credentials cannot resurrect the canceled account.

The resulting Microsoft token pair uses the existing native-vault, restore,
refresh and running-tunnel credential-update paths. Sign-in alone never starts
WAN. The [subsystem reference](../subsystems/wan-gateway.md) owns the contract.

## Focused verification

Only checks related to this change ran, in isolated homes with fake Microsoft
responses and local callback sockets. Native vault access and actual browser
launch are blocked in test processes; a fake launcher records the URL instead.

| Check | Result |
| --- | --- |
| Complete WAN subsystem regression | 213 passing cases including subtests |
| Browser/refresh/credential/shutdown race checks | 31 passing cases, no race |
| WAN GUI/gateway owner APIs and asset contracts | 50 passing cases |
| Related browser UI checks | 24 passing tests; Chromium/WebKit, five languages |
| Translation keys | Three checks passed |
| Negative control | Breaking the OAuth state comparison made the real callback test reject the implementation; exact source restored and positive suites passed |

Coverage includes browser default selection, PKCE challenge/verifier and token
form, secure persistence/restore, response non-disclosure, wrong/duplicate state,
wrong host/origin/method, replay, launch failure/retry, stale attempt IDs,
timeout/refusal and late exchange after cancel/logout/replacement. Existing
GitHub device-flow, Microsoft device fallback and renewal behavior also passed.
No full repository suite was run.

Evidence is under ignored `node_modules/.wan-validation/runs/` in
`wan-browser-final-go-20261023`, `wan-browser-final-race-20261023`,
`wan-browser-api-20261023`, `wan-browser-ui-20261023`,
`wan-browser-i18n-20261023` and `wan-browser-negative-20261023`.

## External compatibility boundary

The public CLI client/resource and actual localhost-root browser redirect were
verified against [Microsoft's CLI diagnostic trace](https://github.com/microsoft/dev-tunnels/issues/500).
The [MSAL system-browser contract](https://learn.microsoft.com/en-us/entra/msal/dotnet/acquiring-tokens/using-web-browsers)
and [Microsoft authorization-code/PKCE contract](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-auth-code-flow)
define the protocol. The earlier device endpoint probe did not validate this
interactive flow and is not counted as browser sign-in proof.

No real organizational-account grant was completed by the tests. Final Entra
policy/device compliance still requires the user to authorize in the new
system-browser flow. Policies requiring a separate Windows broker or blocking
the account/resource can still refuse authorization. This change does not
disable tenant policies. The user's running client and public tunnel were not
restarted for verification.

## Windows artifact

Source commit `2b8b2abb` produced `dist/microsoft-browser/magpie.exe`,
59,405,824 bytes, embedded version `magpie wan-gateway-2b8b2abb`.
The x64 PE machine, GUI subsystem, resource directory and isolated
`--version` checks passed. SHA-256:

```text
645d5437707d0ad4d895e1a997184d69b72caef6446148b871ed122fac7f122a
```

Evidence: `wan-browser-exe-20261023/build-status.json` in the ignored runs
directory. The separate folder preserves the existing executable; the user
must exit the old application before starting this new build.
