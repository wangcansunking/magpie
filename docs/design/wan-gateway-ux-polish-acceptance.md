# WAN visual follow-up — 2026-10-23

The approved concept's hierarchy now appears in the actual page: a page title,
connection banner, aligned account/bandwidth cards, compact renewal health,
visible credential facts, bordered tunnel metadata and client setup.
WAN-specific actions have explicit boundaries and readable labels. Primary
actions use the accent fill; secondary actions use outlined buttons, and
disabled states retain readable text. Address/key copy controls follow the
same sizing and preserve their success feedback. Keyboard focus has an
explicit outline; narrow-screen buttons have at least 44-pixel targets.

A missing runtime is a prerequisite alert above connection controls, with a
primary Install button. The entire alert disappears when the runtime is
available, including after an external installation is detected by Start.
Installed runtime readiness no longer occupies the account card. Unknown WAN
reads do not assert a missing runtime. Signing in and installation still do
not publish a tunnel.

## Inspected fixture captures

- [Desktop quota state](wan-gateway-polished-desktop-20261023.png).
- [Mobile quota state](wan-gateway-polished-mobile-20261023.png).
- [Required runtime alert](wan-gateway-runtime-required-20261023.png).
- [Running state and visible address copy](wan-gateway-polished-running-20261023.png).

These are browser fixtures with fictional account/tunnel data, not real
public hosting or live credentials. They are native browser screenshots of
the implementation, not generated replacement mockups.

## Verification

Only affected GUI checks were run; no repository-wide suite or backend
authentication change was included.

- Chromium and WebKit in English, Simplified/Traditional Chinese, Japanese
  and German cover setup/install, status and WAN keys, credential validity,
  renewal health, client snippets, quota and tunnel replacement. The focused
  run passed 60 cases; its six premature async assertions were repaired and
  their 16-case subset passed completely. All 66 selected cases have passing
  results across those runs.
- Six final cases cover both engines' externally installed runtime handling,
  light/dark button contrast (at least 4.5:1 for enabled sign-in/install text),
  visible boundaries, focus, desktop card order and mobile copy targets.
- All three translation ownership/duplicate/completeness checks passed.
- All three GUI asset budget/type/usage checks passed. JavaScript syntax and
  `git diff --check` passed.
- Negative control: removing button borders from the asynchronously served
  fixture CSS made the actual new test fail at `wanInstall has a visible
  button boundary`. Repository source was never replaced by that mutation.

WebKit's frame-stability click wait stalled in an initial run. The WAN tests
now use the repository's existing `pointerClick` after reader-driven scrolling;
it checks the live hit target and sends real pointer input without forcing
through an overlay. Copy/open assertions wait for their API response, and
Microsoft sign-in assertions wait for the completed requesting-code state.
No failure was waived as a baseline.

Evidence is under ignored `node_modules/.wan-validation/runs/` in
`wan-ux-verified-20261023`, `wan-ux-async-actions-20261023`,
`wan-ux-copy-20261023`, `wan-ux-i18n-20261023`,
`wan-ux-assets-20261023` and `wan-ux-negative-boundary-20261023`.

The Microsoft organizational-account device-flow compatibility limitation
documented in the preceding acceptance remains unresolved by this visual
change. No real user client or public tunnel was restarted for verification.
