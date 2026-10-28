# WAN gateway acceptance — 2026-10-21

Base: upstream `origin/main` at `62a3e79b7b33d7cade6dace4cbef0fbce4ed0ffd`.
Branch: `codex/wan-gateway`; destination: `wangcansunking/magpie`.
Go 1.26.8, Node 24.18.0, Bun 1.4.3; Windows amd64 and isolated Docker Linux.

## Repairs found during repository acceptance

- Overlapping provider catalog holds now invalidate builds made before an external
  provider/settings write. A deterministic held-build test fails with the old
  publication path; the streaming external-revocation test and race repeats pass.
- The Linux cgroup parser walks virtual paths with Linux slash semantics, even
  on a Windows test host. Previously the Windows fixture walked its root forever.
  The original test timed out with the old code and passes 20 times with the fix;
  Linux parsing and Windows' unbounded default retain their existing behavior.
- Routing's removable rest-action line names its account row as the shell's hold
  anchor. Previously removing Try now moved the page by 20px. The regression checks
  the unchanged scroll offset and account-row screen position, including both
  engines, five languages and wide/narrow windows. Historical trace data remains.
- Test fixtures now join actual request arrival/body consumption instead of
  optimistic DOM changes, distinguish skeletons from provider rows, await fonts
  and layout before geometry checks, and hold animations explicitly when sampling
  their middle. Offscreen WebKit frames poll bridge readiness with timers.
  Sidebar drags wait for the deferred gateway footer: it shortens the scroll
  box by 29px after the initial layout. Title-bar layouts own separate pages
  instead of cancelling startup requests while navigating a reused page.
  Usage route callbacks own their timers and are drained after page closure,
  preventing an unhandled rejection from cancelling the next test.
- The reply helper joins both text() and json(). Its loopback test holds an
  incomplete JSON body, checks headers cannot complete the read, and covers text
  and invalid JSON. The old helper fails both engines at the JSON body barrier.
- Windows migration fixture reset uses the atomic writer and memo invalidation;
  SOCKS HTTPS failures assert the CONNECT error, separately from HTTP's 502.
  The searcher cache allocation test compares small/large catalogs rather than
  an OS-dependent fixed allocation floor. Bypassing only the searcher cache makes
  it fail (238 to 677 allocations) while AutoVision still passes.

## Completed Go, SDK and build checks

| Check | Result |
| --- | --- |
| Linux whole repository, `go test -json -count=1 -tags nogui -timeout 90m -p 2 ./...`, after cgroup fix | 69 packages passed; 19 packages have no tests |
| Windows whole repository with the same flags | 68 packages passed; 19 have no tests; gateway interrupted to repair the cgroup infinite loop |
| Windows entire gateway package after both cgroup and cache-test repairs | Passed, 1,074 seconds; completes the interrupted repository sweep |
| Complete Linux gateway race run | 2,281 test entries in four shards, all passed |
| Linux race: redact, wan, access, proc, steady; provider login/usage selection | Passed |
| WAN, hold, external revocation and sandbox regressions, `-race -count=20` | Passed |
| Linux cgroup and all four small-cache tests, `-race -count=20` | Passed |
| Windows cgroup, cache, SOCKS and migration regressions, `-count=20` | Passed |
| Non-root Linux permissions/failure fixtures | Passed |
| Windows native GUI/font/WebView host selection | Passed |
| Pinned Windows Bun plugin/provider/gateway/CLI/GUI supplements | Passed; POSIX-only new-Bun fallback helper skipped on Windows |
| Native plugin stderr/drain and opt-in 100 MiB flow fixtures | Passed |
| Bundled WAN SDK fixtures | 9 passed |
| Site and installer tests | 30 passed |
| Final Windows vet; Linux nogui vet; Windows, Linux nogui and Windows dev builds | All five exited 0 |
| Changed Go files gofmt; owned-source whitespace; changed JS syntax | Passed |

All Go tests use an isolated HOME with pinned caches and cleared agent overrides.
Docker tests run without network or host user-config mounts. The non-root pass
covers permissions that root necessarily skips. No TLS checks were disabled.

## Browser sweep

The original 828-file Chromium/WebKit sweep completed: **11,747 test nodes**,
11,662 passed, 84 failed and one cancelled, with no skips. These include parent
nodes, so the failures represent **33 files**, not 84 independent bugs. Its
nonzero exit and original logs are retained; it is not called a single green run.

A 31-file batch of the changed regressions completed 504 nodes: 501 passed
and three failed across two files (local-address and plugin-panels). Both were
repaired again and rerun in full: local-address plus title-bar-tools passed all
32 nodes; plugin-panels passed all six. Sidebar's complete 14-node pass also ran
with the batch. The added two-engine response-consumption regression was included
in this batch, separately from the original 828-file expansion.

Additional whole-file reruns cover Usage TTFT, live-route navigation and speed.
The TTFT/ledger batch passed all 46 nodes; speed passed all six. Short translated
headings can fit TTFT at widths where longer ones cannot, so the tests exercise
actual overflow and compare the hint with the columns actually hidden. The
original cancellation was a Usage route callback waiting on a page that had
closed; the fixture now owns its Node timer and drains pending callbacks at
teardown. The final whole-file lifecycle rerun passed **34 nodes, zero failed,
zero cancelled and zero skipped**.

The final audit covers 37 repair files and confirms all 33 originally failed
files have complete repaired passes, including the cancelled case. The added
regression brings the total file coverage to 829. The full sweep command was
`node --test --test-concurrency=4 --test-reporter=tap internal/gui/tests/*.test.cjs`;
repair batches ran the complete selected files in both engines. No test failure is accepted merely because
it also occurs on upstream main. Tests run with full assertions in both engines;
fixed geometry tolerances and expected request contents were preserved.

## Windows executable

Built from production-fix commit `415ab4d7` with Go 1.26.8,
`-tags production -trimpath -buildvcs=false`, an explicit version and
`-H windowsgui`. Icon and DPI resources come from the repository's
`build/windows/winres.json` using pinned go-winres 0.3.3. No signing certificate
was supplied. PE machine is x64 (0x8664), GUI subsystem 2; `--version` prints
`magpie wan-gateway-415ab4d7` without starting the app.

Local artifact: `dist/magpie-windows-amd64-wan-gateway-415ab4d7.exe`
(59,159,040 bytes). SHA-256:
`2ab6e2c8df67b4d3e9f27575cbb65d703fa3a9850e9797282701ce98e4f3bbb0`.
Binary and checksum remain local build artifacts, outside the source commit.

## Scope and evidence

Detailed local logs and isolated fixture homes are ignored under
`node_modules/.wan-validation/runs/`. Principal evidence directories:

- `latest-main-acceptance-20261021`: repository Windows sweep, race runs,
  non-root supplement, SDK and site tests.
- `latest-main-cgroup-fixed-20261021`: complete Linux sweep after the repair.
- `latest-main-gateway-final-20261021`: final complete Windows gateway package.
- `latest-main-small-final-linux-20261021`: all four cache tests, race ×20.
- `latest-main-final-verified-static-retry-20261021`: five final vet/build jobs.
- `latest-main-acceptance-20261021-ui-full`: original full browser sweep.
- `latest-main-changed-ui-final-20261021`: final changed-file browser batch.
- `latest-main-acceptance-20261021-repairs*`: full affected-file reruns.
- `latest-main-usage-lifecycle-final-20261021`: final 34-node Usage pass.
- `latest-main-browser-repair-coverage.json`, `latest-main-final-acceptance-audit.json`:
  completed-run audit, including all original failure/cancellation files.
- `latest-main-cgroup-before-20261021`, `latest-main-small-search-negative-20261021`,
  `latest-main-lifecycle-negative-20261021`: actual negative regression checks.

The first final static attempt stalled in the Windows sandbox and was stopped;
the isolated retry completed all five jobs. An incomplete site fixture image
was corrected before the 30-test pass. Neither attempt is counted as passing.

macOS native tests and Linux GTK native tests were not run on this Windows host.
Live/vendor tests and both Pi package-install integration tests were not run;
Linux has no Bun, so available local Bun fixtures were supplemented on Windows.
The historical public WAN/Claude Code smoke test predates this rebase and is not
claimed as a fresh online check: unauthenticated/wrong-key requests returned 401,
a valid key listed models, management paths returned 404, and Claude Code made
one successful turn. No public endpoint or dev server was restarted for this
acceptance; the user-requested stopped state is retained.
