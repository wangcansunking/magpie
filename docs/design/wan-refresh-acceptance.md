# WAN refresh acceptance — 2026-10-22

Scope: WAN's own GitHub device-flow credentials, secure restoration and live
helper credential replacement. Base: `b62d044b453996483982f7e71fd75df3d14d5554`.
Implementation: `9eb441e8d599d09dc8a33b8100ce6bffc371db3c`, following the
separate full-suite fixture repair `db6cc788`.
The owning design reference is [WAN gateway](../subsystems/wan-gateway.md).

The OAuth exchange follows GitHub's
[refresh-token documentation](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens):
device-flow grants do not require a client secret, and each successful refresh
replaces both the old access token and the old refresh token.

## Behavior

- Device-flow grants retain and rotate both tokens. Renewal starts before the
  access deadline, independently of the page and of whether hosting is enabled.
  Transient failures retry without extending the old access deadline.
- Remembered sessions store the complete replacement pair in the existing
  current-user Windows Credential Manager entry before publishing it. There is
  no plaintext fallback. Memory-only sessions never write a credential record.
  Rejected refresh credentials are erased; unknown vault outcomes fail closed.
- Restart restores the saved pair. Expired access with a usable refresh token
  waits for renewal rather than requesting another device code. Existing v1
  access-only records remain readable, but cannot recover a discarded refresh
  token: sign in once after upgrading and select Remember login.
- A running helper receives only the new access token, with a generation-bound,
  monotonic revision. It fetches and validates the same tunnel, endpoint, port,
  ACL and host token before acknowledging. Only that ACK extends the lease.
  The SDK updates host credentials without reconnecting active streams. Stop,
  logout, refusal, expiry and a stalled update revoke admission and join work.

## Verification

All Go checks use Go 1.26.8, isolated HOME/config folders, pre-pinned Go caches,
and offline module resolution. Tests do not use a live account or native vault.
Linux race checks run in disposable Docker containers with network disabled.
Detailed local evidence is retained under ignored
`node_modules/.wan-validation/runs/`.

| Check | Result / evidence directory |
| --- | --- |
| Windows full repository `go test -json -count=1 -tags nogui -timeout 90m -p 2 ./...` | Completed: 68 packages passed, 19 had no tests, one testenv failure repaired below; `wan-refresh-repository-20261022` |
| Repaired Windows testenv full package and WSL regression `-count=20` | Both PASS; `wan-refresh-testenv-package-20261022`, `wan-refresh-testenv-fixed-20261022` |
| Repaired WSL regression, Linux `-race -count=20` | PASS, 22.958s; offline disposable container, read-only source mount |
| Linux WAN `go test -race -tags nogui -count=1 ./internal/wan` | PASS; `wan-refresh-linux-final-20261022/wan-race-all.log` |
| Linux renewal/concurrency regressions `go test -race -tags nogui -count=20 -run 'TestWANRefresh\|TestWANDeviceGrant\|TestWANCredential\|TestWANVaultRotating' ./internal/wan` | PASS; `wan-refresh-linux-final-20261022/race20.log` |
| Windows `go vet ./...`; Linux `go vet -tags nogui ./...` | Both PASS, repeated after the fixture repair; `wan-refresh-final-static-20261022` |
| Windows normal/dev builds; Linux nogui build | All PASS, repeated after the fixture repair; `wan-refresh-final-static-20261022` |
| WAN Playwright tests, Chromium and WebKit, all five supported languages | 48 passed, zero failed/cancelled/skipped; `wan-refresh-browser-20261022` |
| `node --test internal/wan/sdk/host.test.cjs` | 11 passed, zero failed/cancelled/skipped; `wan-refresh-linux-final-20261022/sdk-tests.log` |
| Changed Go `gofmt -l`, helper/test JS syntax, owned-source whitespace | PASS |

The SDK suite uses the pinned 1.3.6 SDK's actual V1 in-memory relay and proves
the same forwarded stream can send/receive before and after host-token renewal.
Go regressions cover device-flow retention, saved-pair rotation/restoration,
expired-access recovery, transient retry, rejection/erasure, secure-write
failure, logout during a held write, repeated helper updates, invalid ACK,
helper authorization failure and Stop during a held update.

The full run's sole failure was
`TestWSLStandInBlocksInstalledExecutable`: a copied sentinel executable was
killed by the negative control's unrelated ten-second startup deadline.
Commit `db6cc788` retains the executable-selection assertions and lets the test
context own the child. An eleven-second startup-delay overlay makes the old
test fail and the repaired test pass. Windows repeat-20, Linux race repeat-20
and the complete repaired package pass. Evidence:
`wan-refresh-testenv-delay-{negative,fixed}-20261022`.
The initial full-run exit code remains one in its retained log; there was no
second monolithic repository run. Every package has a passing result after the
focused repair, rather than the initial failure being called a flake or hidden.

## Negative regression proofs

Separate overlays/in-memory module transformations break one guarantee at a
time; tracked source and the production bundle remain intact. Each check fails
with the named behavior, not a compilation error:

| Deliberate break | Failing check |
| --- | --- |
| Disable renewal scheduling | `TestWANDeviceGrantRetainsRefreshOnlyInSecureRecord`: next renewal was not scheduled |
| Bypass secure replacement save | `TestWANRefreshRotatesBothTokensAndSecurelyRestores/system`: rotated pair was not persisted |
| Restore initial access deadline on HTTP request contexts | `TestWANCredentialUpdatesKeepEndpointAndActiveStream`: stream retained a fixed deadline |
| Remove SDK host credential setter | SDK renewal test: host access token stayed at its old value |

Evidence: `wan-refresh-negative-20261022` plus the separate
`wan-refresh-negative-{worker,store,deadline}-20261022` Go logs. The Go wrapper
itself returns zero even for failed jobs; the recorded nested Go exit codes are
one and the JSON test events explicitly name these failures.

## Limits and release artifact

No live GitHub refresh rotation, eight-hour soak, Microsoft public-relay renewal,
or real-account Windows Credential Manager round trip was performed. Secure
serialization uses a fake vault; native storage continues through the existing
Windows implementation. The prior public WAN smoke is historical evidence,
not validation of this renewal change. The dev server and public WAN remain
stopped. The whole GUI suite was not repeated for this backend/helper change;
the WAN page suite ran in both engines.

Windows x64 GUI production artifact, built after acceptance from implementation
commit `9eb441e8`:

- Local output: `dist/magpie-windows-amd64-wan-gateway-9eb441e8.exe`
- Size: 59,196,416 bytes
- SHA-256: `6a87da4d603844cf8988b6afb884c7af4dd7e8c790918f1108e88f2e85007390`
- `--version`: `magpie wan-gateway-9eb441e8`
- PE machine `0x8664`, GUI subsystem `2`, nonempty embedded resources.
- Flags: `-buildvcs=false -tags production -trimpath`, with
  `-s -w -X main.version=wan-gateway-9eb441e8 -H windowsgui`.
- Unsigned; generated output is local and ignored by Git. No dev server was
  launched; the smoke ran only `--version` under isolated configuration.

Build evidence: `wan-refresh-windows-exe-20261022/build-status.json`. The later
acceptance-documentation commit changes no executable source.
