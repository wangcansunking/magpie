# WAN five-minute relay recovery acceptance

## Behavior

The previous helper disabled SDK reconnect and terminated a running generation
as soon as its status left Connected. A brief transport interruption therefore
required another explicit Start, even while WAN sign-in remained valid.

Within an already-running explicit Start, recoverable transport failure now
starts a five-minute budget. It retries immediately and then at least every
20 seconds between attempt starts, at most three per minute and 15 starts in
the complete window. Attempts never overlap. Intermediate statuses and failed
attempts do not reset the deadline. The helper calls the fixed SDK once per
attempt with the SDK's own retry loop disabled, so the SDK cannot retry faster
or give up before our recovery budget. It keeps the existing host and tunnel.

Stop, EOF, logout, owner cancellation and the credential deadline still retire
the generation. Queued retries are canceled and an in-flight connect is joined
before disposal. Authentication, permission, TLS, rate-limit and another host
taking over the tunnel remain terminal. The takeover case now has a specific
message. Recovery exhaustion emits a fixed five-minute failure, revokes parent
admission and requires explicit Start. Late connection events cannot revive it.

The page keeps the error visible and gives one notice per observed failure.
The native desktop observer works independently of page visibility, sends one
localized OS notification per failure and opens WAN when clicked. Repeated
status reads do not spam notifications. Notification delivery depends on the
OS's existing permission; the persistent page error remains available.
The owner retains running state/endpoint during the recovery interval; already
interrupted streams may still fail and callers may need to retry requests.

## Evidence and scope

- A virtual clock proves attempt starts at 0, 20, ..., 280 seconds, no early
  warning at 299.999 seconds, one failure at 300 seconds, and no later restart.
- Successful recovery clears the deadline. A canceled in-flight attempt is
  joined without future retries or an erroneous failure notice.
- The actual pinned V1 SDK is connected to an in-memory relay, receives a real
  SDK SSH connection-lost event, fails one retry, connects a replacement relay,
  and successfully forwards a fresh request on the same tunnel. Only this
  fixture's timer is accelerated; it is not a real public-network outage test.
- The unchanged final fixture runs under Node's test runner and a calibrated
  serial harness under both Node and the production Bun 1.4.3 runtime.
- A separate negative fixture changes the cadence to 10 seconds. The cadence
  assertion fails as expected; the shipping bundle is never mutated.
- GUI tests cover native-notification deduplication, cancellation, all five
  notification languages, and WAN click routing. Chromium and WebKit tests
  cover the page notice and deduplication in all five UI languages.
- No running magpie process was detected during initial inspection, so the
  user's particular disconnect cause was not recovered from live logs. No
  real account, live WAN tunnel, desktop-notification permission or network
  setting was changed for these tests.

Related checks, not a full repository suite:

- WAN module: 240 passing cases including subtests.
- Selected GUI Go notification tests: 6 passing tests.
- SDK host: 16 passing fixtures; calibrated Node and Bun: 16 passing each,
  with no leaked sockets/servers, external DNS/network or unhandled errors.
- Chromium/WebKit page alerts: 10 passing cases, all five languages.
- i18n key/ownership checks: 3 passing tests.
- Windows and Linux vet of internal/wan and internal/gui passed.
- Final related race counts and production artifact are recorded below.

Ignored logs: node_modules/.wan-validation/runs/wan-recovery-final-*,
wan-recovery-five-minute-*, wan-recovery-vet-* and the corresponding
bun-fixtures-five-minute-* snapshots. Native OS popup delivery itself was not
performed by the tests; its existing notification service is reused.

SDK bundle SHA-256: c74535481f3cde7637373cd4b1aa3e7d6e25c706c96d035cb3b4a2ac06933816

## Final race checks and artifact

Final related race run: 45 passing cases across internal/wan and
internal/gui, zero failures. Source commit 61f80939 produced dist/relay-recovery/magpie.exe,
59,441,664 bytes, embedded version magpie wan-gateway-61f80939.
The Windows x64 PE machine, GUI subsystem, embedded resources and isolated
--version smoke checks passed. SHA-256:

d3113bb12d4a4a44d1e9af83f78d17ca610652703c49766a5a0fd5bf34f645fb

Build evidence: wan-relay-recovery-exe-20261025/build-status.json.
Exit the old desktop process before starting this separate new executable.
