# Bundled WAN SDK host

`host.ts` is magpie's private, generation-bound stdin/stdout adapter around
Microsoft's dev tunnels SDK. `host.cjs` is the self-contained CommonJS output
embedded by `../sdk.go`; end users do not install npm packages. The SDK owns
relay, SSH and tunnel-management protocols. This directory does not download
or start a tunnel as part of a build.

While running, the parent can send a `credentials` operation containing the
current generation, protocol version 1, a new GitHub access token and the next
positive `revision`. Refresh tokens never enter this protocol. The helper
fetches host credentials and revalidates the pinned tunnel metadata, then
updates the SDK host without reconnecting streams and emits a `credentials`
event carrying that revision. Concurrent, duplicate or out-of-order updates
fail closed. The parent extends its credential lease only after the matching
ACK; Stop and EOF join an outstanding update before disposal.

## Fixed inputs

`package-lock.json` pins every archive, including nested versions. The main
release is `@microsoft/dev-tunnels-{connections,contracts,management}` 1.3.6;
its SSH packages are 3.12.22. Proxy support is `https-proxy-agent` 7.0.6 and
`agent-base` 7.1.4. The bundler is esbuild 0.25.11. The npm manifest is private,
uses exact direct versions and deliberately defines no npm scripts.

The SDK graph and its SHA512 archive hashes come from Microsoft's fixed
[sample host lock](https://raw.githubusercontent.com/microsoft/dev-tunnels/eb4556cbdce39234869d6936584ecde1d39fbfa2/samples/ts/host/package-lock.json).
Every SDK graph archive used here matched that lock's integrity. The proxy
and build-tool archives were SHA512 hashed, and every extracted regular file
independently matched unpkg HTTPS SHA256 metadata for its exact version.
The lock records the resulting archive hashes, not invented placeholders.
Those checks establish the selected release bytes, not a source-to-release
reproducibility or runtime compatibility claim.

**Current rebuild platform: Windows x64.** Only esbuild's Windows x64 binary
was acquired and verified. The build-input manifest is restricted to that
platform; the lock has no unverified hashes for other esbuild binaries.
esbuild's upstream optional-dependency list is retained in its lock entry,
but only the Windows x64 entry is concretely locked. Do not regenerate the
lock on another OS and call it the same verified graph. Extending rebuilds
to another platform requires acquiring that exact 0.25.11 platform archive,
verifying its integrity and adding the corresponding lock entry. This is a
build-tool limitation, not a claim that the JavaScript bundle contains a
Windows native runtime dependency.

## Rebuild without npm lifecycle scripts

Use an existing Node 20+ installation. Run commands from the repository root,
not from an extracted archive or the acquired dependency directory.
Acquisition is a separate step; it can access the registry and must retain
normal TLS validation. Never run package install/postinstall scripts:

```bash
npm --prefix internal/wan/sdk ci --ignore-scripts --no-audit --no-fund
```

Then build explicitly, not through an npm script:

```bash
node internal/wan/sdk/build.mjs
```

`--ignore-scripts` is required: the upstream graph includes esbuild's
postinstall, native accelerator installers and other package lifecycle
hooks. esbuild can use its already-present, exact optional platform binary
without running postinstall. Do not omit optional dependencies or run
`npm rebuild`. Review any dependency or lock change before loading the build
tool; the script's installed-version checks are not substitutes for archive
integrity verification.

For a separately acquired, independently integrity-verified graph, pass the
actual `node_modules` directory explicitly, still from the repository root:

```bash
node internal/wan/sdk/build.mjs --dependency-root PATH_TO_VERIFIED_NODE_MODULES
```

The build reads dependencies as source data; only the pinned esbuild build
tool and its platform binary execute. It does not import or run `host.ts`,
the SDK, the output bundle or a tunnel. It uses `platform: node`, CommonJS,
Node 20 targeting and no browser substitutions. The SDK's required module
initialization order is preserved: `DEVTUNNELS_PROTOCOL_VERSION` is set
before the CommonJS SDK require is evaluated. Type-only imports are erased.

The two native WebSocket accelerators, `bufferutil` and `utf-8-validate`, are
aliased to each package's own shipped `fallback.js`, not their native
loaders. debug's optional `supports-color` probe is replaced by a module
that throws inside its existing try/catch, preserving the absent optional
module behavior without any runtime package search. This affects console
color detection only; the helper disables console diagnostics. The SDK's
undeclared `node-rsa` fallback, selected only when its Node version check is
below 10.12, is replaced with an explicit unsupported-runtime throw. Modern
native `crypto.generateKeyPair` failures are not caught by that branch and
still fail normally; no legacy RSA implementation is shipped.

The build emits `host.cjs` and `host.meta.json` after checking that every
external import is a Node builtin, no native addon or node-gyp loader
entered the graph, and there is exactly one runtime output file. Computed
require warnings fail the build. Dependency source names are canonicalized
to `node_modules/...`, so moving a verified graph to another acquisition
folder does not bake that folder into bundle comments. Keep the generated
metafile as build-review evidence; it is not a runtime requirement. Compare
bundle bytes after a second build before claiming reproducibility. Build
success alone proves neither Bun compatibility nor public relay behavior.

## Licenses

`THIRD_PARTY_NOTICES.txt` retains the supplied license texts, copyright
notices and supplemental notices for the full locked graph (including
build-only inputs conservatively). `build.mjs` embeds it in the output as
line comments so it survives embedding in the Go binary. Do not distribute
a regenerated bundle without its notices.

The Microsoft SDK archives declare MIT and retain Microsoft's copyright
headers but omit a license file. The full Microsoft MIT text is taken from
the fixed dev-tunnels repository revision above, SHA256
`c2cfccb812fe482101a8f04597dfc5a9991a6b2748266c47ac91b6a5aae15383`.
The SSH packages' own release archives supply their MIT declaration and
Microsoft copyright; their version-specific repository license was not
retrieved and has not been compared. The supplied utf-8-validate license
also references Markus Kuhn's short-license URL; that original reference is
retained, not claimed to have been independently retrieved or compared.
These provenance limitations are stated in the notices as well.

This build documentation does not claim that the helper, Bun, a GitHub
login or a public tunnel has been run. Verification of the authenticated
runtime and fail-closed hosting contract belongs to the separate WAN tests
and explicitly approved sandbox relay check in the
[WAN design reference](../../../docs/subsystems/wan-gateway.md).

## Relay recovery

RelayRecovery owns retries for one already-running explicit Start, using the
same fixed SDK host and tunnel. Attempts start immediately, then at least
20 seconds apart (at most three per minute), without overlap. The original
five-minute deadline permits at most 15 attempts and never resets on an error
or intermediate status. One SDK connect call is one attempt; SDK internal retry
loops remain disabled. Initial connection failures still fail fast.

Only network, timeout and transient service failures recover. Authentication,
permission, rate-limit, TLS and another host taking the tunnel remain terminal.
Stop cancels both timers and the shared connection cancellation token; shutdown
joins the in-flight attempt before disposal. Tests use a virtual clock for the
entire five-minute/15-attempt window. An actual fixed SDK V1 fixture replaces
its in-memory relay, fails one SSH network attempt, and verifies a fresh
forwarded request after recovery using an accelerated injected timer.
