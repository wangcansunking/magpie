# Microsoft remembered sign-in: native-vault size regression

## Actual failure and fix

Live dev verification on Windows on 2026-10-23 established the failure after
successful Microsoft authorization. The token endpoint returned HTTP 200 and
valid account metadata, but the remembered credential occupied 4866 serialized
bytes and 3696 compressed bytes. Windows Credential Manager permits only 2560
bytes in one credential blob. Memory sign-in succeeded; remembered sign-in
failed with the fixed safe-storage error. This was reproduced against the
actual service before changing the storage implementation.

Large Microsoft credentials now stay entirely inside the current user's native
credential vault. Bounded parts hold the compressed payload; a small root
identifies a complete slot and records length/count/hash. Save writes the
inactive slot before switching the root, then erases the previous slot. Restore
refuses missing, truncated, mixed or corrupt parts. Logout erases both fixed
slots, including interrupted writes and fragments without a root. The existing
auth commit gate serializes refresh, replacement and logout and handles unknown
write outcomes. Small and previously compressed records remain compatible.
No plaintext credential files or CLI-token imports were introduced.

Development-only OAuth diagnostics emit numeric sizes, booleans, bounded error
codes and fixed callback stages. They never emit token contents, identifiers,
URLs with authorization parameters or vendor error descriptions. Production
builds contain only no-op diagnostic functions.

## Real dev acceptance

The user completed only the Microsoft authorization step. Subsequent checks
were performed by the agent against an isolated actual dev process:

1. Successful real Microsoft system-browser authorization-code/PKCE exchange.
2. A real 3696-byte compressed remembered credential was accepted by Windows
   Credential Manager; auth reached signed_in with storage=system.
3. The dev process was terminated and restarted using the same isolated
   configuration. It restored signed_in/system without another browser login.
4. The official SDK created/started an actual Microsoft-account Dev Tunnel;
   state reached running with a valid HTTPS endpoint.
5. Public HTTPS probes: /v1/models without a WAN key returned 401; with the
   isolated test WAN key it returned 200; /api/wan with that key returned 404.
6. Stop returned HTTP 200 and state stopped. No live tunnel remains from this
   acceptance run. The isolated gateway contained no provider accounts, so the
   models response proves transport/authentication, not provider inference.

The validation parent retained the administrative key and WAN test key in
memory and did not print them. Only redacted schema/status events are recorded
in node_modules/.wan-validation/microsoft-live-dev/live-events.log. Test
configuration is separate from the user's desktop configuration.

The service accepted the Microsoft credential for usage reads but did not
report BandwidthPerUser. That remains explicitly unknown; it is not sign-in
failure or proof of an unlimited quota. Background renewal was scheduled; no
claim is made that an actual hour-long renewal interval elapsed during this
acceptance. Related rotation and live-helper update behaviors are covered by
the focused tests.

## Related checks

- WAN module: 239 passing cases including subtests, 8.990 seconds.
- Related vault/Microsoft/refresh race run: 60 passing cases including subtests,
  7.066 seconds.
- Dev diagnostics/privacy and large browser grant: 2 passing tests, 3.857 seconds.
- Windows and Linux go vet of internal/wan passed; changed Go files are gofmt clean.
- Regression control: restoring rejection above one native blob makes the large
  Microsoft credential round-trip test fail with the previous does-not-fit
  message. The fix was restored before the passing checks above.
- No full-repository suite was run, per the requested scope.

Ignored evidence: node_modules/.wan-validation/runs/wan-native-parts-{go,race,
dev-tests,negative,vet-windows,vet-linux}-20261023.

The native limit is documented by Microsoft's
[CREDENTIAL contract](https://learn.microsoft.com/en-us/windows/win32/api/wincred/ns-wincred-credentiala).

## Windows artifact

The production artifact is recorded after the source commit and build.
