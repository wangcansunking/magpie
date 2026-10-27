# WAN retry progress integer protocol fix — 2026-10-27

The user reported WAN helper returned unsupported protocol output after a
running session. The desktop binary SHA-256 matches the previous shipped
90998f2c build. No per-attempt diagnostic history was persisted by that build,
so the exact original disconnect cause and retry count cannot be reconstructed.
No live desktop process, sign-in or public tunnel was changed during diagnosis.

A matching protocol failure was reproduced deterministically: after a retry
consumes 0.25 milliseconds, the helper emits retryInMs: 19999.75. Go's strict
integer field rejects this JSON before publishing progress, revokes admission,
and reports the user's exact error. Previous fixtures used whole-millisecond
clocks and missed this production timing case. This is a helper/parent protocol
bug, not evidence that a five-minute retry window or service quota was exhausted.

RelayRecovery now rounds the delay upward to whole milliseconds, using the
same value for the IPC progress and retry scheduler. At the reproduced boundary
it sends 20000 and schedules at 20000.25; it cannot retry earlier than the
20-second minimum. Go continues refusing fractional or malformed messages.

## Related verification

- New fractional-clock regression fails against the previous embedded helper
  with retryInMs:19999.75 and passes against the rebuilt helper with 20000.
- All 18 unchanged-plus-regression SDK fixtures pass under isolated Node,
  Bun 1.4.3 and the exact Bun 1.4.2 runtime used by the user's desktop.
  Actual SDK/SSH recovery still reconnects the same tunnel and streams traffic.
  Fixture transports are in-memory/loopback; no external service is contacted.
- A Go protocol rejection fixture covers fractional retryInMs; related
  SDK/recovery/credential cases passed 51 tests with -race and isolated HOME/caches.
- No UI code changed and no full repository/browser suite was run.
- Windows/Linux vet of internal/wan passed; changed Go files are gofmt clean.
- The helper rebuilt twice with identical SHA-256:
  1da40fc2cd007015fd65a3da9558c3974dce9a491020156fbdf0a16c376d6f8d.
- Windows executable build evidence is recorded below.

Ignored evidence: bun-fixtures-integer-*-20261027 and the matching
node_modules/.wan-validation/runs/*integer*20261027 directories.
