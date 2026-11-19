# Persistent WAN diagnostics acceptance — 2026-10-28

An explicit WAN Start now appends a separate run to the active settings folder's
wan-diagnostics.jsonl. It records starting/running, relay disconnection category,
bounded transport cause (errno, HTTP status or SSH reason), each retry start,
each retry failure, next schedule, recovery, helper failure and final failed or
stopped outcome. UTC time, local run ID and attempts make runs distinguishable
across process restarts. The final result has a reserved queue slot.

The parent persists only validated private-protocol events and fixed local
failure descriptions. It never saves raw helper stdout/stderr, upstream Error
objects, access/refresh tokens, WAN/provider keys, account IDs or tunnel URLs.
Unknown text is omitted. Invalid helper messages still revoke admission and
leave a fixed protocol_output diagnostic, rather than leaking the bad bytes.

A bounded worker performs all file writes outside protocol/credential/manager
locks. Normal shutdown joins the helper, flushes the queue and final result,
and joins the writer before the run completes. Overflow has an explicit lost
record count. A write error emits one fixed warning and disables logging for
that run without failing WAN. A later Start retries log creation. An abrupt
process termination or filesystem failure can lose recent events.

Each current file is at most 1 MiB; one complete-record backup is retained as
wan-diagnostics.jsonl.1. Rotation/append uses a cross-process file lock and new
files use 0600. Log paths follow custom/portable configuration folders. No log
is uploaded automatically. English and Chinese README guides explain WAN
setup, separate sign-in/keys, tunnels, recovery and diagnostics.

## Related verification

Only changed WAN code and its protocol/recovery/credential lifecycle were
selected, as requested. No full repository or browser suite was run; UI source
is unchanged. Test HOME/config/temp are isolated and caches pinned. No live
account, desktop process, public tunnel or external service was used.

- 74 passing Go race cases: actual disk append/restart/rotation, protocol
  validation and redaction, final failure/normal Stop, write failure that
  leaves WAN running, held writer that cannot delay admission revocation,
  overflow reporting and reserved final outcome, plus affected lifecycle,
  recovery and credential cases.
- All 19 fixed SDK/SSH fixtures pass in isolated Node, production Bun 1.4.3
  and the user's Bun 1.4.2. New diagnostics preserve disconnect and successive
  retry failure categories, round sub-millisecond elapsed time, and omit
  upstream secret/error text. No leaked sockets/servers, external DNS/network
  attempts or unhandled errors were observed.
- Regression: disabling the startSDK journal connection makes
  TestWANDiagnosticsManagerPersistsFinalFailureAndStop/failure fail with
  "helper failure reason was not persisted". The implementation is restored.
- Windows and Linux vet of internal/wan passed; changed Go files are gofmt
  clean, git diff --check is clean. README WAN sections and local reference
  targets were checked.

The pinned SDK bundle SHA-256 is
95334b588dd77d8a2d96f7353e27ebfa6b88ea968abb463590559e77ca1d5e9e.
Ignored evidence lives in node_modules/.wan-validation/runs/*diagnostics*
and bun-fixtures-diagnostics-*-20261028. Production artifact evidence follows.


## Windows artifact

Source commit: 6abaf0aa (persistent diagnostics and English/Chinese README).
Output: dist/wan-diagnostics/magpie.exe (59,488,768 bytes).
Version: magpie wan-gateway-6abaf0aa.
Windows x64 PE, GUI subsystem, resources and isolated --version checks passed.
SHA-256: 5189ab0ea24c6962d6f7782c5492cd717e3f5cbc61c8e0247d265e590b3aa32d.
Build evidence: wan-diagnostics-exe-20261028/build-status.json.
The user's existing desktop executable and running processes were untouched.
