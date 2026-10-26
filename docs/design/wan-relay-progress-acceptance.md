# WAN relay recovery progress acceptance — 2026-10-26

A recoverable relay loss previously left the WAN page green and labelled
WAN running until the five-minute recovery deadline expired. The helper now
publishes bounded, generation-validated recovery progress. The owner exposes
reconnecting, actual attempts started, whether a connect is in flight, and
server-relative timing. Success clears recovery and restores running.

The page displays amber Reconnecting WAN, attempt count, and either a local
one-second countdown or Reconnecting now. An elapsed countdown waits for the
next backend status instead of incrementing attempts. Stop stays available;
Start stays hidden, and the endpoint remains visible. Hidden/unmounted pages
cancel their countdown. Recovery/failure clears the progress display. All new
strings are translated in English, Simplified/Traditional Chinese, Japanese
and German.

## Verification

Only changed WAN behavior and its affected GUI lifecycle were selected, per
user request. No repository-wide suite was run. Test HOME/config/temp are
isolated; Go caches are pinned. No live account, desktop instance, public
tunnel or external vendor service was used by these tests.

- Regression: removing the owner's reconnecting projection makes
  TestSDKRecoveryProgressProjectsStatusWithoutRevoking fail with
  "missing reconnect status". The implementation was restored automatically.
- Related Go race run: 50 passing cases, no failures. Covers SDK protocol,
  progress/status, terminal revocation, credentials and recovery lifecycle.
- Fixed SDK Node tests: 17 passing, including actual SDK/SSH relay recovery,
  failed first retry, ordered actual attempt events and success reset.
- Identical fixtures in production Bun 1.4.3 and calibrated Node shim:
  17 passing each, no leaked servers/sockets, external network/DNS or
  unhandled errors. This uses in-memory/loopback transport and injected
  scheduling; it does not interrupt a live public tunnel.
- Chromium/WebKit: 36 passing cases covering WAN setup, Stop, polling/page restoration,
  recovery failure alerts and new countdown behavior. Every new UI behavior
  is checked in all five languages. The browser/backend clocks deliberately
  differ by decades; countdown still starts at 20 seconds, ticks without
  extra HTTP reads, and leaves the attempt count unchanged.
- i18n key and string ownership checks: 3 passing tests.
- Windows and Linux vet: internal/wan and internal/gui passed.
- Changed Go files: gofmt clean; git diff --check clean.

The SDK bundle builds reproducibly with the pinned dependency graph.
SHA-256: 8bff186ecb248caafda28e966a87d113f96cc0165be464d098cc324539947a0d.

Ignored evidence: node_modules/.wan-validation/runs/wan-progress-*,
wan-recovery-five-minute-final-race-20261026-progress,
wan-recovery-vet-*-20261026-progress and bun-fixtures-progress-*-20261026.
The initial unprivileged SDK run could not use sandboxed loopback sockets;
rerunning with loopback permission passed all 17 tests.


## Windows artifact

Source commit: 90998f2c74b02f8926b0867c3ebd69d4d130cf28.
Output: dist/relay-progress/magpie.exe (59,459,584 bytes).
Embedded version: magpie wan-gateway-90998f2c.
Windows x64 PE, GUI subsystem, resources and isolated --version checks passed.
SHA-256: 45461079eab67142c85dc46536da024ee99d95fa6af249f6fa8ad7eaefc1012b.
Build evidence: wan-relay-progress-exe-20261026/build-status.json.
The existing desktop process was left untouched; exit it before running this
separate new executable.
