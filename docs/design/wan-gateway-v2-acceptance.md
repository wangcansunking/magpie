# WAN Gateway v2 acceptance — 2026-10-22

Base: `f54bf2d30ef0a4c1209b86d7194cde2b0001dc95`.
WAN implementation: `9cb3b6e9bd1a08c555f2f6616cf2ebe6bc40a299`.
Provider sheet completion fix: `92856e5f`.
Modal horizontal-scroll anchor fix: `9d929e79`.
Queued Settings redraw fix: `0111f959`.
Final fixture repairs: `acea4016` and `6bbc4a53`.
Usability follow-up UI: `d9e935be`, `a7827da8` and `3f102890`.
The user approved [this concept](wan-gateway-v2-20261022.png). The implementation
uses magpie's existing shell/theme, menus and translations; the concept's sample
running state and timestamps are not live service observations.

Implemented UI previews: [desktop](wan-gateway-v2-implemented-desktop-20261022.png)
and [narrow](wan-gateway-v2-implemented-mobile-20261022.png). These use isolated
browser fixtures with an invalid example domain and fake masked keys, not live
accounts or a public relay. Light-theme and 360px checks also passed in the
WAN browser matrix.

## Observable changes

- Service state and client connection take priority over completed setup.
  GitHub login shows actual renewal health, next planned renewal/retry and
  storage mode. Current access expiry moves into details. Access-only and
  non-expiring grants never falsely claim an automatic refresh worker.
- Cached, credential-free `auth.renewal` reports `scheduled`, `refreshing`,
  `retrying` or `required`, with the worker's actual `nextAttemptAt`. Callers
  cannot mutate owner state through a returned status value. An access deadline
  missed during recoverable renewal stops WAN and asks for restart after
  recovery, rather than another device login.
- Claude Code/OpenAI and PowerShell/Bash choices produce the appropriate root
  or `/v1` address and safely quoted examples. Model selection reuses Gateway
  eligibility through an independent, cancellable provider read. Only model
  IDs/names are kept. No example fetches or retains a real WAN key.
- Per-key menus keep copy/toggle accessible while rotate/remove retain their
  confirmations. Unknown reads close menus and keep controls locked after copy.
  Late model reads cannot render into hidden, replaced or failed kernel pages.
- Both shared menu types restore focus without scrolling to a moved anchor.
- Closing Add provider settles its dismissed selection immediately. Its late
  animation callback cannot discard an editor opened meanwhile, an import or
  another page. A held-completion regression checks the new editor's owner
  and that its modal has not begun closing.

The owning references are [WAN](../subsystems/wan-gateway.md) and
[GUI shell](../subsystems/gui-shell.md).

## Verification

Go uses 1.26.8, isolated HOME/config folders, pre-pinned caches and offline
module resolution. Browser fixtures intercept requests; native command checks
use fake Claude functions and loopback HTTP. Linux race containers have no
network, drop capabilities, and read final source through a read-only mount.
Evidence is under ignored `node_modules/.wan-validation/runs/`.

| Check | Result / evidence |
| --- | --- |
| Windows `go test -count=1 -tags nogui -timeout 90m -p 2 ./...` | PASS: 69 packages, 19 with no tests, zero failures; `wan-v2-go-20261022` |
| Final WAN + GUI Go packages after implementation freeze | PASS; `wan-v2-final-packages-20261022` |
| Linux exact-source renewal `-race -count=20` and full WAN `-race -count=1` | PASS; `wan-v2-source-race-20261022` |
| Windows/Linux vet; Windows normal/dev and Linux nogui builds | PASS; `wan-v2-post-pending-static-20261022` |
| WAN browser states, five languages, themes/widths; i18n ownership; both shared menus | 93 passed, zero failed/cancelled/skipped; `wan-v2-final-browser-20261022` |
| Gateway model/key/connection and cross-page click-scroll regressions, both engines | 260 passed, zero failed/cancelled/skipped; `wan-v2-gateway-browser-20261022` |
| Actual PowerShell/Bash Claude arguments and OpenAI loopback requests | All four PASS, including literal quoted model IDs and placeholder-only keys; `wan-v2-native-examples-20261022` |
| Full GUI suite | COMPLETE: 830 files, 11,791 tests, 11,762 passed; 29 failed nodes (21 top-level cases plus eight nested failure nodes); zero cancelled/skipped. Every affected file repaired and rerun separately; original nonzero result preserved. `wan-v2-full-browser-glob-20261022` |
| Full-suite fixture repairs: Built-ins, Codex descriptions, currency | 20 passed, zero failed/cancelled/skipped; `wan-v2-suite-repairs-20261022` |
| Held restart state and routing-group save completion | 16 passed, zero failed/cancelled/skipped; `wan-v2-suite-saves-20261022` |
| Phone/touch, desktop pixel comparisons, reader pointer guards and Codex settings | 36 passed, zero failed/cancelled/skipped; `wan-v2-mobile-repair-20261022` |
| Settings save completion, Discover update cards and GitHub filter loading | 48 passed, zero failed/cancelled/skipped; `wan-v2-plugin-settings-repairs-20261022` |
| Held Add-sheet completion / sign-in sheet cleanup, both engines | 12 passed, zero failed/cancelled/skipped; `wan-v2-sheet-close-fixed-20261022` |
| Final GUI Go package, including loopback API tests | 366 test PASS; `wan-v2-final-gui-loopback-20261022` |
| Modal horizontal/vertical anchor, morph and click-scroll | 50 passed; `wan-v2-modal-horizontal-fixed-20261022` |
| Original Detect APIs draft preservation | 22 passed; `wan-v2-modal-original-detect-20261022` |
| Final WAN/i18n/menu matrix after modal repair | 93 passed; `wan-v2-final-wan-after-modal-20261022` |
| Quota curves/pace and routing folds/rest | 88 passed; `wan-v2-quota-routing-repairs-20261022` |
| Final pace / held routing-rest / held library regroup | 4 / 22 / 22 passed; `wan-v2-pace-final-20261022`, `wan-v2-routing-rest-held-final-20261022`, `wan-v2-library-regroup-final-20261022` |
| Held Settings save, search keys, mouse/touch redraw matrix | 82 passed; `wan-v2-pending-settings-fixed-20261022` |
| Menu bar logos / automatic updates save completion | 16 passed; `wan-v2-last-settings-final-20261022` |
| Full privacy masking matrix after navigation barrier | 20 passed, zero failed/cancelled/skipped; `wan-v2-window-mask-final-20261022` |
| Gofmt, JS syntax, owned-source whitespace | PASS, no formatting/whitespace output or syntax errors |

The repository run started before the final UI/error-message refinements and
the added recovery test; the affected final Go packages were rerun rather than
presenting the broad run as a test of a later source snapshot.

The first GUI iterations exposed string ownership conflicts, Escape focus
scrolling and a WebKit remount race. These were repaired. The final matrix
includes a held model response released while the kernel error placeholder is
visible, then its actual retry/remount path. Earlier logs remain intact.
Docker Desktop stopped between Linux runs; the failed infrastructure attempt
is retained and final checks passed after starting the engine again.

The broad GUI run also exposed four fixture synchronization defects, repaired
in `86589ab0`: an unconditional wheel with only a bottom-edge check, implicit
scrollIntoView in a pointer/no-scroll assertion, reading a cost before saved
currency arrived, and treating menu closure as completion of a key save.
The latter two now hold their API requests deliberately and observe the returned
state/badge before asserting or acting again. Original failures remain in the
broad log; the repaired files passed separately in both engines. These repairs
do not alter application code.

The phone fixture also used Playwright's frame-dependent/implicit-scroll clicks;
it now uses real horizontal wheel input and hit-tested native pointer input.
The pointer helper refuses disabled (including aria-disabled), covered and
offscreen targets and sends trusted pointer events. Neither desktop pixel
tolerance nor form position/scroll assertions is relaxed. Settings tests hold
an optimistic save until released; plugin update cards explicitly choose
Discover after a known update opens Installed; GitHub filter tests distinguish
loading ghost cards from real `data-pkg` cards.

The moved-provider sign-in failure exposed an application race, not just a
fixture race. Holding Add-sheet animation completion, opening Cursor's editor,
then releasing it deterministically reset `editing` to null and started closing
the new modal on the old code in both engines/languages. `92856e5f` fixes that
completion ownership. The first visual-only probe was insufficient because a
closing modal remains visible during its outgoing animation; the final test
checks the owner and outgoing state as well as visibility. Counterexample:
`wan-v2-sheet-close-counterexample-20261022` (six FAIL nodes including parents).
Final pass: `wan-v2-sheet-close-fixed-20261022`.

The modal failure was a real application defect: horizontal scroll clamping
incorrectly replaced the vertical anchor during hint reflow. The new listener
updates that anchor only when scrollTop changes, including movement back to zero.
The old listener fails the controlled 17.25px-growth regression in both engines
(`wan-v2-modal-horizontal-counterexample-final-20261022`).

A second controlled regression holds an Off save while unrelated Settings draws
run. Old code resets the optimistic thumb; the repaired renderSettings defers
these draws until queued saves settle. Both engines fail on old code and the
82-case save/redraw matrix passes on the fix
(`wan-v2-pending-segs-counterexample-20261022`).

Runner permissions mattered. Disposable AppContainer OS-temp paths denied atomic
rename (EPERM); the identical workspace-temp rename probe passed 20/20, and the
Zen test then passed count=20. Loopback-denied GUI tests were rerun with isolated
HOME/config and local fake servers outside that restriction; all 366 passed.
These failed infrastructure attempts remain in the logs and were not addressed
by changing application persistence/network logic or skipping tests.

## Negative proofs

Separate Go overlays and in-memory browser source mutations leave production
files intact. All five yield named behavioral FAIL events:

1. Hide renewal metadata: `TestWANRenewalStatusTracksScheduledAndInFlightWork`.
2. Restore the old login-again message on recoverable expiry:
   `TestWANRenewalRecoveryDoesNotAskForAnotherDeviceLogin`.
3. Restore bare menu anchor focus: the moved-anchor menu regression scrolls.
4. Treat the kernel error placeholder as mounted: held-response boundary test
   records an uncaught missing-control error.
5. Show a healthy badge while retrying: the WAN health test expects retry state.

Evidence: `wan-v2-negatives-20261022` and the separate
`wan-v2-negative-{status,expiry,focus,boundary,health}-20261022` logs. Negative
runs are not acceptance passes; their expected nonzero job codes are checked.
Removing the pointer helper's guards also yields a named behavioral failure
(`Missing expected rejection`, not an import/compile failure) in
`wan-v2-negative-pointer-20261022`. The provider-sheet counterexample above uses
the real old application code and a controlled completion, without changing
production files for the negative run.

## Limits and release artifact

No real GitHub/Microsoft login, live refresh rotation, eight-hour soak or public
WAN relay was exercised. Native Credential Manager and live Claude Code were
not invoked. Browser screenshots and command requests use test data. Dev and
the public WAN remain stopped; restarting/restoring sign-in still does not host.

Initial v2 Windows candidate built from `6bbc4a53`, superseded by the
follow-up artifact below:

- `dist/magpie-windows-amd64-wan-gateway-6bbc4a53.exe`
- `--version`: `magpie wan-gateway-6bbc4a53`
- 59,243,008 bytes; x64 PE, GUI subsystem, embedded Windows resources.
- SHA-256: `0fedf7e8c8b15e478802393ca95e5cc95a538d50aeea9d18467ad91c84c4c597`
- Build evidence: `wan-v2-windows-exe-release-20261022/build-status.json`.
- Final source audit: 29 JavaScript files syntax-checked, five Go files gofmt
  clean, and owned-source whitespace check passed.

The executable has not been started as a dev server or public WAN host.
The broad GUI run began at `9cb3b6e9` and continued during repairs; it is not a
single all-green run of final HEAD. Acceptance combines its complete repository
coverage with the final passing affected-file matrices above. The new modal and
pointer regression files were added after glob expansion and passed separately.
No failing case is waived or skipped.

## Follow-up usability review

The follow-up reviews WAN interactions rather than only its signed-in screenshot.

| Journey | Review and change |
| --- | --- |
| First visit | No implicit installation, login or hosting. At narrow widths authentication/setup appears before the long client examples. Empty key lists omit their table headings. |
| Runtime | An installed runtime is a quiet status with a green indicator and no empty disclosure. Missing runtime shows its install action and download explanation; the existing busy/error controls remain. |
| Storage choice | Replace the small checkbox with explicit memory/remember choices. Memory remains the default; the entire button is clickable, checked state is announced, arrow/Home/End keys select and move focus without scrolling. Text explains the selected choice in future tense. Unavailable secure storage still fails closed to the existing explicit memory-only behavior. |
| GitHub authorization | Keep the code selectable; add Copy with the shared clipboard success/fallback behavior. Show its validity deadline beside authorization, not inside signed-in credential management. Open GitHub is the primary external action and remains explicit. Cancel stays available during a held request. |
| Session management | Waiting authorization does not show Sign out or credential/renewal details. Signed-in management is visible outside the credential disclosure, with its stop/erase explanation. Once polling confirms sign-in, management is available even if an older auth/start reply is held; sign-out must reject that late reply. Renewal state is ignored outside session/recovery states. Empty metadata groups have no border or space. |
| Start and stop | Disabled actions retain the existing owner/auth/key-read safeguards. Start alone publishes; Stop leaves sign-in intact. Starting can be interrupted; refresh/hide/remount cannot resurrect old action results. |
| Client connection | Claude/OpenAI and shell/model choices remain explicit. Copy address/example uses the selected protocol and shell. Examples contain placeholders, never an implicit key read. |
| Key management | Named separate keys, masked values, explicit copy/enable/disable, and confirmed rotate/remove remain. Failed reads retain prior rows and lock writes rather than claiming an empty list. |
| Recovery and accessibility | Existing retry/required renewal states, unknown-status locking, keyboard menus, hit-tested input, focus/scroll preservation, browser remount and narrow layout remain in the regression matrix. |

Preview and tests use fixtures; this is not a claim that every possible user or
real-service condition has been exercised. Scoped verification results follow.

Follow-up previews (fixture data only): [authorization](wan-gateway-v2-authorization-usability-20261022.png) and [narrow setup](wan-gateway-v2-setup-usability-20261022.png).

The 300 no-key and 100 populated-key responsive-state combinations passed in
`wan-v2-usability-layout-matrix-20261022` and
`wan-v2-usability-key-layout-20261022`. The old UI fails code-copy availability,
explicit storage choices and empty-runtime-disclosure checks in both engines;
the new UI passes (`wan-v2-usability-counterexamples-20261022`).

### Follow-up verification and artifact

- GUI Go package: PASS, `wan-v2-usability-go-20261022`.
- Final embedded asset budget/registry and WAN API checks: PASS,
  `wan-v2-usability-final-assets-20261022`.
- WAN/i18n/menu matrix: 93 PASS, zero failed/cancelled/skipped,
  `wan-v2-usability-final-snapshot-20261022`.
- Final storage/authorization/touch/held-logout actions: 12 PASS,
  `wan-v2-usability-touch-controls-20261022`. These assert selected-storage help
  after expiry, unknown-validity disclosure suppression, safe clearing,
  clipboard request completion and actual >=44px narrow button targets.
- Old pre-touch CSS (`98b13a85`) fails the 44px geometry check in both engines;
  final CSS passes (`wan-v2-usability-touch-counterexamples-20261022`).
- The preceding WAN iteration retained one WebKit German resize-measurement
  failure. The viewport helper now waits for the requested resize event and
  frame before measuring; it does not wait for overflow to disappear or relax
  the <=1px assertion. The final 93-case matrix passed.
- The 832-file GUI run was stopped at the user's request. It is incomplete
  and is not an acceptance requirement; original logs and failures remain in
  `wan-v2-usability-full-20261022`. No full-suite pass is claimed.
- Final copy-feedback smoke: 4 PASS (Chromium/WebKit, English/Chinese),
  `wan-v2-usability-copy-feedback-20261022`. Verifies localized success text,
  copied code and accessible description for the final two-line UI adjustment.

The current executable is built from the final UI source commit `3f102890`:

- `dist/magpie-windows-amd64-wan-gateway-3f102890.exe`
- `--version`: `magpie wan-gateway-3f102890`
- 59,246,592 bytes; x64 GUI PE with embedded Windows resources.
- SHA-256: `0aa07c1afc47f4cb5a629ff88bf391edc2649fb8cbf63f656763becf34754555`
- Build evidence: `wan-v2-usability-copy-release-exe-20261022/build-status.json`.

Known broad-run failures so far are three fixture issues (live account-row
measurement and two context wait/keyboard flows) and two loopback permission
failures. The repaired account file passed 12 cases
(`wan-v2-usability-account-models-20261022`), the context files passed 112
(`wan-v2-usability-context-fixtures-20261022`), and the unchanged real streaming
reply tests passed both engines with loopback permitted
(`wan-v2-usability-lifecycle-loopback-20261022`). No tests were skipped, no
geometry/animation/content tolerances were relaxed, and the real HTTP streaming
test was not replaced by route interception. Syntax/whitespace audit of all six
changed JavaScript files passed at `a8eb354f`
(`wan-v2-usability-source-audit-20261022`); this follow-up changes no Go source.

Final acceptance scope follows the user's instruction: validate our changes,
without completing full-repository tests. The final WAN script passes syntax
and whitespace checks. The final executable passes PE/resource validation and
an isolated --version smoke; no dev server or public WAN was started.
