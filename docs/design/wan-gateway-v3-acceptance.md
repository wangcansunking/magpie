# WAN account, quota and tunnel acceptance — 2026-10-23

## Observable changes

- WAN authorization offers GitHub and Microsoft (personal or Entra). It uses
  independent tunnel-service credentials, never a provider subscription's login.
- The page separates connection state, account identity/renewal, bandwidth,
  tunnel facts, client setup and WAN keys. Current token expiry is visible with
  its time zone and is explicitly distinguished from a fixed session expiry.
- Actual account bandwidth, allowance, reset time and last successful check
  come from Dev Tunnels `userlimits`. Failed reads remain unknown or explicitly
  stale. A measured zero is displayed as zero. No Microsoft allowance is assumed.
- Current exhausted usage explains Microsoft's refusal and blocks Start until
  reset. A disconnected relay retains its distinct error; it no longer says
  “Ready to start.” A redundant generic 429 is suppressed only when the known
  exhausted quota is already explained above it.
- One saved tunnel is selected per stable account identity. First use creates
  a tunnel; returning to an account reuses its selection. Explicit replacement
  affects that account only and creates the new address on the next Start.
  WAN keys and other accounts' selections remain intact. No remote tunnel is
  deleted automatically.
- Old unbound tunnel settings cannot establish ownership. The first Start
  after upgrade creates a bound tunnel, which can change the client's address.
  Broken settings fail closed. Catalogs and credentials remain local to the
  machine and do not enable hosting on restart.
- Microsoft rotating credentials use the existing native-vault commit barrier.
  The refresh-token deadline remains unknown. Oversized records are compressed
  within Windows Credential Manager; records exceeding its limit still fail
  closed and offer an explicit memory session, never plaintext persistence.

The owning contract and source links are in
[WAN gateway](../subsystems/wan-gateway.md).

## Design and visual inspection

The approved [concept](wan-gateway-v3-20261023.png) was generated with the
built-in image generation tool, not the fallback CLI. Prompt brief: redesign
the existing dark magpie WAN gateway as a polished desktop dashboard; charcoal
cards, lavender actions, amber quota failure; a prominent bandwidth-exhausted
banner with reset time; two aligned account/renewal and actual bandwidth cards;
a full-width tunnel identity/region/port/account card with manual replacement;
client setup and independent keys below. Use readable aligned facts, a quota
meter and a clear current-token deadline, avoiding dense paragraphs, secrets
or claims of unlimited service. Example identity is fictional. The implemented
Microsoft login choice extends that approved layout.

Final browser-fixture captures were inspected at 1400 and 360 pixels:

- [Desktop implementation](wan-gateway-v3-implemented-desktop-20261023.png).
- [Mobile implementation](wan-gateway-v3-implemented-mobile-20261023.png).

These are simulated quota/account states, not screenshots of a real user's
credentials or proof that a public tunnel is serving. On narrow screens the
quota card precedes account details so the failure and reset are easy to find.

## Focused verification

The user explicitly requested checks of our changes instead of a repository-wide
suite. Tests ran with isolated HOME/config/temp paths and pinned caches. Native
Credential Manager access is disabled in test processes. Network-facing tests
use local fixtures or an in-memory pinned SDK relay.

| Check | Result |
| --- | --- |
| Complete `internal/wan` package | 203 passing cases including subtests; 9.177 s |
| Targeted WAN race detection | 43 passing cases; 18.638 s; no race |
| WAN administrative API, gateway, backup and GUI asset checks | 50 passing cases across three packages |
| WAN page and translation checks | 101 passing tests, zero failures/skips; Chromium and WebKit, five languages |
| Final layout, reset boundary and Microsoft device-page subset | 22 passing tests in both engines |
| Pinned SDK helper tests | 12 passing tests, including provider-specific actual HTTP adapter authorization and uninterrupted streams during renewal |
| Negative control | Replacing reported bandwidth with zero failed the exhausted-usage test; exact source restored and WAN suite passed |

Go commands used `-tags nogui -count=1` under the isolated runner:

```text
go test ./internal/wan
go test -race -run='TestWAN(Usage|Microsoft|Account|Refresh|Credential)|TestManagerLifecycleAndStreamingCancellation|TestStopDuringStartupAndStatusDoNotLaunch|TestShutdown' ./internal/wan
go test -run='TestWAN|TestCoreScriptBudget|TestAssetTypesHoldAgainstTheRegistry|TestPurposeAssetMatchesUsage' ./internal/gui ./internal/gateway ./internal/backup
node --test --test-timeout=120000 internal/wan/sdk/host.test.cjs
node --test internal/gui/tests/wan-gateway.test.cjs internal/gui/tests/i18n-keys.test.cjs
```

Evidence is retained under ignored `node_modules/.wan-validation/runs/`:
`wan-v3-verified-go-20261023`, `wan-v3-verified-race-20261023`,
`wan-v3-final-api-20261023`, `wan-v3-final-ui-20261023`,
`wan-v3-final-boundary-ui-20261023` and `wan-v3-negative-20261023`.

The first race run caught the new restart fixture changing its settings path
before runtime discovery finished. The fixture now joins discovery before
changing the path. A stricter credential fixture also needed to normalize the
legacy omitted GitHub discriminator, matching the real helper. Translation
checks caught shared/duplicate keys; shared terms now live in core i18n.
Those failures were fixed and rerun, not waived.

## Actual service evidence and remaining limits

Read-only diagnostics of the existing service found an account at
5,371,017,692 / 5,368,709,120 bytes with reset Unix time 1795243853.
The public relay returned 429 before magpie authentication, whereas the local
WAN listener correctly returned 401 without a key. The minute-rate bucket
still had capacity; this was monthly bandwidth exhaustion. A different GitHub
identity reported 0 / 5,368,709,120 bytes. Reading publicly visible tunnel
metadata does not establish ownership, so a new account is not bound to that
old address based on metadata visibility.

The official Microsoft device endpoint accepted the chosen public client and
resource scope with HTTP 200 and returned
`https://login.microsoft.com/device`, a 900-second deadline and a 5-second
poll interval. No device code/token was included in this report. No real
Microsoft account grant, refresh or public hosting was completed in this
revision; tenant/account policies and native-vault token sizes still require
real-account acceptance. Offline fixtures do not prove those external steps.
The user's running client and public tunnel were not restarted for these checks.

## Windows artifact

The production Windows GUI artifact is delivered as `dist/magpie.exe`, with
`dist/magpie.exe.sha256`. Build metadata records its source commit, embedded
version, SHA-256, x64 PE machine, GUI subsystem and resource directory; the
isolated `--version` check must pass before delivery. The build does not start
a gateway or replace a running executable.
