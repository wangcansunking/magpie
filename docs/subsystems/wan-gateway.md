# WAN gateway

WAN Gateway publishes a separate, API-only listener through Microsoft Dev
Tunnels. It is opt-in: opening the page, signing in, creating a key, starting
magpie or restoring a backup does not publish anything. The ordinary gateway
and administrative page keep their existing addresses and authentication rules.

## Responsibilities and sources of truth

- [`access.go`](../../internal/access/access.go) stores WAN keys alongside
  ordinary keys in `caller-keys.json`. WAN records have `scope: "wan"`, a
  `wan-` identity and a separate `wanSecret`; the old `secret` stays empty.
  `AuthenticateWAN` accepts only enabled WAN credentials. `ListWAN` masks
  credentials; `UpdateWAN` owns their administrative changes.
- [`wan.go`](../../internal/gateway/wan.go) supplies `Server.WANHandler` and
  authenticates requests before the shared inference routes. Authenticated
  WAN requests count as remote regardless of loopback, forwarding headers
  or `MAGPIE_TRUST_PROXY`.
- [`wan_lifecycle.go`](../../internal/gateway/wan_lifecycle.go) attaches
  `Manager.Own` to the process that acquired the gateway listener. Its
  methods expose cached status and explicit hosting, authentication and
  runtime operations to the local administration API.
- [`manager.go`](../../internal/wan/manager.go) owns the dedicated loopback
  socket and hosting generation. `New` is inert; `Own` attaches the gateway
  lifetime. `wan.json` contains a versioned tunnel identity and port, not a
  credential or an instruction to start automatically.
- [`auth.go`](../../internal/wan/auth.go) owns GitHub and Microsoft device authorization,
  expiry, background renewal, cancellation and the credential commit barrier.
  [`refresh.go`](../../internal/wan/refresh.go) rotates token pairs under that
  barrier; [`credentials.go`](../../internal/wan/credentials.go) leases the
  running helper only after its revision-bound acknowledgement.
  [`github.go`](../../internal/wan/github.go) makes bounded, cancellable
  requests without following redirects. Neither uses provider sign-in,
  account discovery, existing CLI tokens or a browser-owned lifetime.
- [`microsoft.go`](../../internal/wan/microsoft.go) implements Microsoft's
  explicit device grant and refresh against the common v2 endpoint. Personal Microsoft
  and Entra organization accounts use the public Dev Tunnels CLI client and
  the SDK's published Dev Tunnels resource audience. The helper sends `Bearer`
  authorization for Microsoft and `github` authorization for GitHub.
- [`microsoft_browser.go`](../../internal/wan/microsoft_browser.go) owns the
  default Microsoft system-browser flow: authorization code with S256 PKCE,
  a random held IPv4 loopback port and the CLI-compatible
  `http://localhost:<port>/` redirect. The backend launches the OS default
  browser through `proc.OpenBrowser`; embedded/web-page browsers do not own
  the flow. Callback state is random, checked and one-use; its listener and
  accepted connections are joined on completion, cancellation and shutdown.
- [`usage.go`](../../internal/wan/usage.go) reads account identity and actual
  `BandwidthPerUser` values from the Dev Tunnels `userlimits` API. An owner
  worker checks once per minute, cancels old-credential requests and joins on
  shutdown. Status reads only its cached value. Missing/failed measurements
  remain unknown; same-credential failures retain explicitly stale values.
- [`tunnel.go`](../../internal/wan/tunnel.go) stores one selected tunnel per
  account in local-only `wan-tunnels.json`, using GitHub's numeric ID or
  Microsoft's home tenant/user IDs (legacy records use tenant/object IDs).
  Account names are display labels, not keys.
- [`vault.go`](../../internal/wan/vault.go) defines a WAN-specific secure
  credential store. Windows uses the current user's Credential Manager,
  namespaced by settings directory. Other systems currently offer explicit
  memory-only sessions, not plaintext persistence. Tests disable native
  credential access even when HOME has been isolated.
- [`sdk.go`](../../internal/wan/sdk.go) owns the bundled helper process and
  versioned, bounded private pipe protocol. The helper source and build
  instructions live in [`sdk`](../../internal/wan/sdk/README.md). There is
  no devtunnel CLI fallback or runtime npm installation.
- [`endpoint.go`](../../internal/wan/endpoint.go) accepts only a Dev Tunnels
  HTTPS inference URL for the owned port, never an inspection URL or a URL
  carrying credentials.
- [`wan.go`](../../internal/gui/wan.go) serves `/api/wan` and explicit POST
  operations on the administrative handler, not the public listener. The
  [Gateway plugin](../../plugins/gateway/gateway.go) owns its second page,
  drawn by [`wan.js`](../../internal/gui/assets/plugins/gateway/wan.js).

## Authentication and runtime

The official SDK documents a production Dev Tunnels GitHub App Client ID for
client applications authenticating tunnel users. WAN uses that service app,
not Copilot's or VS Code's identity, and does not require registering a magpie
OAuth App. The device request omits OAuth scopes: GitHub App user tokens use
the app's registered permissions intersected with the user's access. No
repository scope or client secret is requested. See the
[SDK service-app contract](https://github.com/microsoft/dev-tunnels/blob/b73b0eb774b86a5b519c300f2f41c9072a66f663/ts/src/contracts/tunnelServiceProperties.ts)
and [GitHub App device flow](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app#using-the-device-flow-to-generate-a-user-access-token).

Microsoft authorization uses `common`, the public CLI client ID
`c0df98ca-23b4-4bce-bb9f-72039b28d3a5`, and the published resource
`46da2f7e-b5ef-422a-88d4-2a7f9de6a0b2/.default` with `offline_access`,
`openid` and `profile`. Microsoft defaults to system-browser authorization
code/PKCE, matching the CLI's interactive flow rather than its `-d` option.
The browser callback uses the localhost root and a random port, as shown in
the [official CLI diagnostic trace](https://github.com/microsoft/dev-tunnels/issues/500).
Its code, state, verifier and authorization URL never enter the GUI snapshot,
settings or logs. A fixed, non-reflecting callback page acknowledges receipt;
the WAN page reports the token exchange's result separately. Wrong host,
origin, state, duplicate parameters and replayed callbacks are refused.

Device flow remains an explicit `method: "device"` option; no browser failure
silently falls back to it. Its supported verification pages are
`https://login.microsoft.com/device` and `https://microsoft.com/devicelogin`.
Account and tenant policies can refuse a grant. See the
[official CLI authentication options](https://learn.microsoft.com/azure/developer/dev-tunnels/cli-commands).
Provider subscriptions remain independent of this tunnel-service login.

Sign in registers an attempt before preparing a browser callback or device code.
Microsoft browser preparation retains the existing `requesting_code` state
with `method: "browser"`, then reports `awaiting_browser` with a 15-minute
deadline. It opens the system browser once, without waiting for a page poll.
An OS launch failure stays visible and allows an explicit
`POST /api/wan/auth/open` retry bound to the exact `attemptID`; the request
accepts no URL. Polling, page restoration and ordinary startup never reopen
a browser. Cancellation and logout close its callback port even while a late
token exchange remains in flight. Replacement retires the old callback before
opening the next account's browser. Late grants cannot commit after retirement.

For device flow, only the
verification URI, user code, deadline and attempt identity reach the page;
the device code and access token stay in the backend. The user opens the chosen
service explicitly. The waiting view offers code copying, its validity time, the service page
and cancellation without signed-in management details. A missing runtime is
a prerequisite alert above the connection controls; the alert disappears after
installation. Device polling respects the response interval, slow-down and expiry.
Cancel targets the exact current attempt. Replaced/canceled attempts cannot
commit late credentials. A slow secure-store operation does not hold the
cached-status lock.

The login storage picker offers an explicit memory session or remembering on
this computer; it defaults to memory and supports keyboard selection. Remember
is available only with secure system storage. Selecting a memory
session first erases an earlier remembered login, so a restart cannot restore
an account the user replaced. Failed reads and erases are unknown and block
sign-in until explicit sign-out succeeds; they are not empty stores. An
unreadable Windows vault requires a usable Windows user session; it does not
silently downgrade to memory, which could restore an old account on the next
normal launch. Neither access nor refresh tokens enter settings, backup, sync,
status, command arguments, environment or logs. A remembered version-2 secure
record contains the rotating pair and its access/refresh deadlines. Version-1
access-only records still load; they require one fresh device sign-in to acquire
a refresh token. Non-expiring access-only grants retain their existing behavior.
Microsoft uses a version-4 secure record with an explicit provider and the
stable home-account identity from the TLS token response's requested
`client_info` (`uid`/`utid`), matching MSAL's account model. Access tokens
are opaque; their JWT shape, audience and claims never decide whether a new
grant is accepted or which provider receives it. Profile lookup for Microsoft
never sends its token to GitHub. Missing/invalid account metadata fails sign-in
with a fixed message. Refresh responses may omit metadata; renewal retains the
existing identity, and refuses a different or malformed identity. Existing
version-3 vault records still load using their legacy identity reader, only for
migration; a guest account whose new home identity differs must sign in again.
Display names may come from the same response's optional ID token, bounded and
checked for the application audience; labels never select an account. ID tokens
are not persisted. Version-4 identities use the home tenant; guest accounts may
therefore create a
new account-specific tunnel on their first new sign-in. Its
refresh deadline remains unknown because the token response does not report it.
Records exceeding Windows Credential Manager's 2560-byte blob limit are
compressed inside the native vault, with bounded decompression and the same
validation on restore. If a Microsoft record still exceeds one blob, bounded
native-vault parts hold the payload and a small root stores its slot, size,
count and SHA-256 digest. Each part remains within the native limit. The owner
writes the inactive of two fixed slots, switches the root only after all parts
succeed, then erases the previous slot. Missing, mixed or corrupt parts are
unknown credentials, never a partial session or an empty store. Logout erases
both bounded slots even after an interrupted write or a missing root; a failed
erasure blocks sign-in until retry. There are no token files or plaintext
fallbacks. The existing small/compressed and GitHub records remain readable.
Development builds log only bounded response metadata (lengths, booleans,
numeric error codes) and fixed browser stages; credential contents, callback
parameters and vendor error descriptions never enter these diagnostics.

The gateway owner renews independently of the page, before access expiry (up to
five minutes early, or one quarter of a short grant's lifetime). Each successful
GitHub refresh invalidates both old tokens, so the replacement pair is saved as
one secure record before publication. Failed/unknown vault writes clear the
session and block further sign-in until explicit sign-out; there is no plaintext
fallback. Cancellation, replacement and logout join the credential commit gate;
a late write is erased and cannot resurrect a signed-out account. Transient
network/service failures retry with bounded backoff without extending the last
access deadline. A refused or expired refresh token erases remembered credentials
and requires a fresh device sign-in. Device-flow refresh needs the client ID and
refresh token, never a client secret ([GitHub refresh contract](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens)).

On restart, an expired access token with a usable saved refresh token is renewed
in the background; the page shows loading while access is unavailable. Restoring
or renewing sign-in never hosts a tunnel. A memory session renews only until the
owner exits and is never remembered implicitly.

`AuthStatus.renewal` is an optional, credential-free snapshot of the owner's
actual work. Its states are `scheduled`, `refreshing`, `retrying` and `required`.
`nextAttemptAt` is present for scheduled work and transient retries, and absent
while a request is in flight or a fresh login is required. It is not calculated
by the browser from access expiry. Status returns a value copy; reading it does
no I/O. Access-only credentials have no renewal worker and no renewal snapshot.
Logout/replacement clears the snapshot; a refused rotating credential reports
`required`. An expired access token that is still being renewed stops hosting
with a recovery/restart message rather than asking for another device login.

The login card makes renewal health primary, with next planned attempt and
memory/system storage shown separately. Access-token expiry and remaining time
are secondary details with an explicit local time zone, not a fixed login-session
deadline. The backend's zero signed-in expiry means a non-expiring access-only
grant; invalid or missing dates remain unknown. The page does not invent a
refresh-token lifetime or show healthy renewal from `signed_in` alone.

Login does not host. Stop does not sign out. Replacing a sign-in while hosting
is refused until Stop. Sign out invalidates authentication and blocks Start,
then stops and joins hosting before erasing the local credential. Local sign
out does not claim to revoke the GitHub grant upstream. An SDK authentication
rejection invalidates only the matching credential, not a later replacement;
a remembered rejected login is erased through the same tracked commit barrier.
A failed erase remains blocked until explicit sign-out succeeds. Permission or
policy refusals (403) do not invalidate authentication.

The helper uses the existing verified Bun runtime facility, independently of
plugin enablement. Owner attachment starts a tracked, cancellable cache probe
outside the status and process-wide lifecycle locks. Status reads do not install
or execute a runtime. Start rechecks the cache without downloading, even if
cached availability is false because another component installed Bun later.
An explicit Start or runtime installation supersedes the initial cache probe;
a late initial result cannot overwrite the newer availability snapshot.
Missing runtime installation is a separate explicit operation; Start refuses a
missing runtime rather than downloading silently. The helper preserves an
explicit extra-CA file or reuses the system-trust export used by the plugin
runtime; preload and proxy environment variables remain excluded.

**Real-service validation remains separate.** Public documentation establishes
the service-app contract and device-flow procedure, not the current enabled
setting of that particular app or a successful sign-in from magpie. A
`device_flow_disabled` response is a service-app support problem, not advice
to edit an app the user does not own. No real GitHub grant or public tunnel
has been verified as part of the offline checks.

## Hosting lifecycle

1. The user signs in explicitly and creates independent named WAN keys.
   A name describes an intended device; it is not a verified device identity.
2. Start requires a serving owner, current authentication and an available
   runtime. It acquires the run lock and dedicated TCP4 `127.0.0.1` socket
   before invoking the bundled helper.
3. The helper prepares a new dedicated tunnel or resolves the saved tunnel.
   It does not select the user's default tunnel. Canonical identity and the
   sole HTTP port are validated and saved atomically before hosting starts.
   Management requests explicitly retrieve access-control metadata before the
   helper checks the tunnel and nested port ACLs.
4. Pipe messages carry version and generation. Unknown, oversized, stale or
   out-of-order output fails closed. Credentials and proxy credentials go
   through stdin, never argv, environment, status, traces or diagnostics.
   Ready is accepted only for a validated endpoint of the held port.
5. Only then does admission open. A client sends a WAN key using
   `Authorization: Bearer`, `x-api-key` or `x-goog-api-key` as its protocol
   requires. Existing model/account restrictions, budgets, routing and usage
   identity apply; external credentials are removed before upstream dispatch.
6. Stop revokes admission and active request contexts, disposes/stops and
   joins the child tree, closes the server, then releases socket and lock.
   A disconnect or child failure takes the offline cleanup path; it does not
   automatically reopen admission or create another tunnel.

Running credential rotation uses a strictly ordered, generation-bound private
pipe command. The helper switches the management credential, requests fresh host
access metadata, validates the canonical identity, sole port, ACLs, inspection
and forwarding options, and unchanged public endpoint, then replaces the SDK
host's tunnel metadata without reconnecting existing streams. Only a matching
revision acknowledgement extends the hosting lease. Unknown, duplicate or stale
acknowledgements fail closed. Credentials remain on stdin; stdout carries only
the revision. The original access deadline is not a fixed deadline on accepted
streams: the lease revokes admission and request contexts if renewal or helper
update fails, the current deadline expires, or Stop occurs. Stop cancels and
joins the update writer before disposing the child.

`wan.json` version 2 stores the active canonical `id`, `clusterId`, `port` and
optional account binding. `wan-tunnels.json` version 1 is the per-account
catalog. Start resolves the authenticated identity, selects that account's
saved entry, or creates its first tunnel. Returning to an account reuses its
selection. Old unbound files, including `{id,port}`, cannot establish ownership:
the first explicit Start creates an account-bound tunnel and may change the
address. Corrupt files refuse hosting rather than becoming an empty catalog.
Known bound tunnels that expire or disappear report an error; they are not
silently replaced or deleted. WAN client keys are independent of identity.

The explicit `POST /api/wan/tunnel/new` operation requires stopped WAN and
records replacement intent for the current account only. It creates nothing
remotely until the next Start. Other accounts' selections, existing remote
tunnels and WAN keys are retained. The page confirms the address change.
`POST /api/wan/auth/start` accepts optional `provider: "github" | "microsoft"`
and `method: "browser" | "device"`. Omission preserves GitHub device flow;
Microsoft defaults to browser flow. Browser flow is refused for GitHub. One WAN login is active at a
time; selecting a different account requires its own authorization.

`POST /api/wan/usage/refresh` coalesces checks with a 15-second cooldown.
Usage reports bytes, a nullable limit, the service's Unix reset timestamp and
last successful check time. No missing field becomes a fabricated zero.
Fresh exhausted measurements block Start until the reported reset; stale or
unavailable measurements do not invent a refusal. Service connection errors
still remain visible. GitHub and Microsoft quotas are queried for the current
identity; Microsoft is not assumed to have a larger or unlimited allowance.
See the [service limits](https://github.com/MicrosoftDocs/azure-docs/blob/main/includes/dev-tunnels/dev-tunnels-service-limits.md).

The SDK's automatic local-port forwarding is disabled. The helper uses its
explicit V1 protocol selection and manually handles streams, dialing only
the pinned port on `127.0.0.1`; unknown ports are rejected, not dialed. This
is intentionally not based on an assumption that the V2 stream event behaves
like the V1 sample. The exact SDK manual-stream, protocol-selection, proxy and
cleanup behavior still requires the local SDK fixtures and authorized runtime
checks before claiming the external-management boundary verified.

Management and relay transports receive an explicit proxy agent pointing to
[`wanProxy`](../../internal/wan/wanproxy.go), an authenticated TCP4 loopback
CONNECT bridge owned by that hosting generation. Its random capability travels
only through stdin. Each HTTPS destination is passed to `netproxy.Func`, so
host-specific environment and system bypass decisions are not reused from the
global management address. The bridge accepts canonical DNS destinations on
443, not literal IPs, localhost or `.local` names. It supports direct, SOCKS
and HTTP/HTTPS upstream proxies; HTTPS proxy certificates are verified and a
failed or unsupported proxy never falls back to direct. Headers and handshakes
are bounded, not the stream lifetime. Stop revokes admission immediately and
keeps the proxy alive only for bounded helper disposal, then closes its
accepted/upstream connections and joins both copy directions before the held
WAN socket is released. A bridge failure revokes admission itself. OAuth uses
`netproxy` directly.
Offline fixtures verify per-destination routing and proxy transport; these do
not establish compatibility with every real system proxy or PAC deployment.

[`Shutdown`](../../internal/wan/shutdown.go) closes the process-wide gate and
cancels every hosting generation and idle authentication owner before joining
cleanup. Desktop exit, web quit/reexec, CLI return and caught termination
signals use it. A normal Stop remains restartable. Child cleanup uses
[`proc.Tree`](../../internal/proc/tree.go); Wait joins its cleanup sweep and
any earlier Kill sweep. On Unix, the existing bounded process-group sweep does
not prove every descendant exited in all circumstances. Native Windows
ordering coverage is not native Unix execution proof.

The page polls only while visible. Page/visibility/action generations keep
late responses from replacing restored state. Leaving the page stops reads,
not hosting or backend sign-in. Remount cleanup removes lifecycle hooks;
opening, polling, changing language and back/forward restoration never invoke
Start, sign-in or installation. A failed key-store read returns `keys: null`
and a separate `keysError` alongside owner/runtime/auth status. It disables
Start and key mutations, not Stop or sign-out; unknown keys are never displayed
as a successfully empty store.

Stop does not delete the service tunnel. Local admission and requests are
revoked immediately; the generation's private proxy remains available only
through bounded helper cleanup. EOF requests SDK disposal before forced
process-tree stopping, and proxy connections are closed and joined before the
held local socket is released. Remote endpoint deletion is still best-effort:
a failed service request, expired credential or forced termination can prevent
it. An offline SDK fixture completing endpoint deletion does not prove a real
service delete. This does not preserve access to the held local listener. The
Endpoint may survive between starts, but Microsoft can expire inactive tunnels
and a deleted tunnel needs explicit recovery. Restarting magpie never hosts
automatically.

## Public surface and authentication

Only shared inference routes are registered: model catalogs, Chat
Completions, Responses over HTTP, Messages and token counting, Gemini,
embeddings, rerank, images, audio and videos, with supported aliases. WAN
serves no GUI assets, `/api/*`, `/v1/magpie/*` diagnostics, `/mcp/*`, internal
callbacks, System One, standalone search or general Codex backend subtree.
A WAN key is not an administrator credential.

The ordinary listener rejects the WAN namespace even on loopback; local
agent tokens and ordinary gateway keys do not authenticate on WAN. Invalid,
disabled and revoked keys fail closed. Unreadable/corrupt key files do not
become permissive empty configuration. CORS retains the existing policy;
publication adds no wildcard or trusted browser origin. WAN rejects query
credentials even beside a valid header, and noncanonical or percent-encoded
paths. Clients use canonical paths and header credentials.

Early route/authentication refusals, preflights and not-running responses
close incomplete request bodies without waiting indefinitely for the sender.
Their temporary read deadline applies only to short-circuited requests, not
authenticated inference or streaming. Header and idle timeouts remain
separate; there is no whole-stream write timeout.

WAN secrets stay outside the legacy `secret` field so old readers cannot
accept them as ordinary keys. An old writer can discard unknown fields,
making a WAN key unusable; its stable `wan-` identity prevents conversion to
a local key by current versions. Rotate such a key. Do not run an older
version as a WAN server.

Disable, rotate and remove apply to subsequent requests; they do not undo
upstream work or end an already accepted stream. Stop closes all WAN requests.

## What a key does not prove

**Endpoint + API key proves possession of a secret, not a physical device.**
Assign a separate key per device and revoke independently, but a copied key
can work on another computer. There is no hardware identifier, fingerprint,
IP lease or enforced one-machine binding; a device name is only a label.

The maintainer [reports that the service does not expose the original client
IP](https://github.com/microsoft/dev-tunnels/issues/466#issuecomment-2297086080).
No forwarding header or User-Agent is evidence of exclusivity.

Microsoft terminates TLS and can process request contents and API keys.
HTTPS is not end-to-end TLS directly to magpie. Dev Tunnels remains a public
preview without a production SLA; see its
[security description](https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/security).

## Ownership, persistence and verification

Only the serving gateway process controls WAN. A GUI observing another
`magpie serve` process shows unknown status and unavailable controls; it does
not start a second gateway or guess the other process's state. No new
cross-process administrative API is exposed.

WAN keys follow the existing encrypted gateway-key backup rules. Tunnel
identity, secure-store credentials, runtime state and public enablement do
not travel in backups or sync. Restoring keys cannot publish a tunnel.

Go tests use isolated homes, fake pipe children and local HTTP servers; no
real GitHub/Microsoft login, public tunnel or provider account is permitted.
Run the repository's sandboxed Go runner with `-timeout 45m`, then the full
checks in [code standards](../code-standards.md). Tests of the former CLI do
not prove the SDK path. GUI source cases cover both engines and five
languages. The full browser sweep and repaired-file passes are recorded in
[the prior acceptance report](../design/wan-gateway-acceptance.md); focused backend
checks alone are not a browser acceptance result. Renewal tests use injected
clocks, fake secure stores and fake helper children; the SDK fixture exercises
its actual host-token setter and rejects changed metadata. Run renewal and
lifecycle regressions with `-race -count=20` as well as the complete WAN package.

A real-tunnel check requires separate approval: publish only an isolated
gateway with a fake upstream, then check Endpoint+Key clients, SSE, revoked
keys, blocked administration routes, unknown ports and Stop from outside the
host. Static SDK inspection, builds and local fakes do not prove real public
connectivity, native platform behavior or service authentication.

## Client setup page

The WAN page puts service state and actionable failure first, then account and
bandwidth cards, tunnel facts, client setup and independent WAN keys. A quota
meter shows actual usage and reset time; unavailable and stale data are labeled.
Account renewal, storage, next attempt and current access-token expiry remain
visible, with local time zones. A missing runtime is a required-warning banner
above connection controls, with an explicit installation button. The entire
banner disappears once the runtime is available; installed status is not a
permanent account-card row. Device authorization remains explicit. Provider sign-in supplies
models; the separate GitHub or Microsoft login owns the tunnel. Stop/sign-out effects and
public-relay details remain available without dominating the working view.
[The approved v3 concept](../design/wan-gateway-v3-20261023.png) is a visual
reference. [V3 acceptance](../design/wan-gateway-v3-acceptance.md) distinguishes
offline fixtures, actual service probes and unverified real-account flows.
The [visual follow-up](../design/wan-gateway-ux-polish-acceptance.md) records the
aligned metadata, compact renewal health, visible outlined actions, light/dark
button contrast and focus checks, and responsive prerequisite placement.

Client tabs distinguish Claude Code's root Endpoint from an OpenAI-compatible
`/v1` base. PowerShell/Bash examples quote values literally. An explicit model
picker uses a separately cancellable `GET /api/providers` read and the existing
Gateway plugin's `gatewayModels(state)` eligibility rules. Only public model IDs
and labels are retained, not the account snapshot. Empty and unreadable model
lists are different; failures and held reads cannot block Stop or sign-out.
Changing/hiding/remounting the page invalidates late model reads. The kernel's
error placeholder is not a mounted WAN view and cannot receive async renders.

Examples always contain `YOUR_WAN_KEY`; they never fetch a secret implicitly.
The selected model must remain eligible, and copying a runnable example requires
a fresh running Endpoint. A missing address/model remains a placeholder. Key
copying stays explicit, while rotate/remove move into a per-key menu and retain
their existing confirmations. Unknown key reads close menus and lock mutations;
copy completion cannot re-enable controls after a returned key-read error.

SDK startup failures return fixed HTTP/network/TLS/timeout/runtime categories.
No exception message, remote response body, headers, URL or credential enters
the status channel. Unknown exceptions retain the generic failure.

With the `dev` build tag, each explicit Start reads the rebuilt helper from
`internal/wan/sdk/host.cjs`, bounded to 4 MiB and copied into the private run
folder. An unavailable/empty/oversized file fails closed. A rebuild does not
change a running generation. This permits SDK fixes without restarting the
backend or discarding memory-only sign-in. Release builds always use the
embedded bundle and have no helper-path override.

The service's `TunnelOptions.IsInspectionEnabled` is a non-nullable bool
serialized with `WhenWritingDefault`, so a missing field represents false.
The helper accepts omitted or explicit false inspection at both port and
tunnel level, and rejects true, null and other types. Host/Origin preservation
and the explicit zero request timeout must still be present. This matches
the [service contract](https://github.com/microsoft/dev-tunnels/blob/main/cs/src/Contracts/TunnelOptions.cs),
not an assumption that every omitted field is safe.

## Relay transport recovery

Within an already-running explicit Start, RelayRecovery asks the fixed SDK to
reconnect the same host and tunnel after a recoverable transport failure. The
first attempt is immediate, then starts are at least 20 seconds apart (at most
three per minute), without overlapping attempts. A single five-minute deadline
allows at most 15 starts. Repeated connection statuses and failed attempts do
not extend it. One SDK connect call is one attempt; its internal retry/reconnect
loops remain disabled so the policy controls the cadence even when the SDK
would give up early. Initial Start still fails on its first failed connection.
Authentication, permission, rate-limit, TLS and host-takeover failures are
terminal, rather than retrying a credential/policy problem for five minutes.

The owner retains its endpoint and credential lease during recovery. Cached
status exposes `state: reconnecting` and a credential-free `recovery` snapshot:
actual attempts started (0–15), whether a connect is in flight, the next retry
time while waiting, and server time for a clock-skew-free browser countdown.
Validated helper progress belongs to its current hosting generation; malformed
or terminal output cannot publish recovery or revive admission. The internal
lifecycle remains running so recovery never triggers a second Start. Successful
SDK connection clears progress and restores public `running` status. Existing
streams may fail and clients may need to retry requests. Recovery never
extends the parent's credential lease or creates another tunnel. Stop, EOF,
logout and credential expiry cancel recovery, clear both timers, and join the
in-flight connect before disposal. Failure at five full minutes emits the fixed
reconnect_timeout error and revokes admission; late events cannot revive the
generation. Startup after process exit never auto-publishes.

The WAN page shows an amber reconnecting state, the actual attempt count, and
a locally ticking next-retry countdown (or an in-flight label). It retains Stop
and the existing endpoint, hides Start, and never increments attempts or starts
a connection from its timer. At zero it waits for authoritative status rather
than inventing another attempt. Hidden/unmounted pages cancel the countdown;
returning reads fresh backend progress. Successful recovery and terminal states
clear the progress display. The page displays persistent failure and a one-shot
in-page notice.
The desktop independently watches cached owner status and sends one native
notification per recovery failure, even when the WAN page is closed. Clicking
the native notification opens WAN. Repeated polls do not repeat it; a later
independent failure is notified again. OS notification permissions still apply.
