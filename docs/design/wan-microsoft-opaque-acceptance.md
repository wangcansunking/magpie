# Microsoft opaque WAN credential acceptance

The previous login accepted Microsoft access tokens only if their JWT payload
contained the expected resource audience and tenant/object fields. New grants
now accept opaque tokens and use client_info requested from the TLS OAuth token
endpoint for stable account identity. Display labels may use the optional ID
token; neither access-token claims nor labels choose the provider or account.
The secure record advances to version 4, with version-3 loading retained.

Provider selection follows the exact active credential snapshot. Microsoft
usage reads never send its access token to GitHub. Renewal retains omitted
account metadata and refuses changed or malformed identities before persistence.
Specific fixed error messages replace the generic unsupported-credential error;
no token contents enter errors or logs.

## Verification

Only the affected subsystem and translation checks were run, not the full repo.

- WAN package: 221 passing cases including subtests, 7.688 seconds.
- Related Microsoft/usage/refresh race run: 47 passing cases including subtests,
  5.877 seconds.
- Translation-key checks: 3 passing tests, all four translations present.
- Windows and Linux go vet for internal/wan passed with nogui tags.
- Regression control: restoring the previous JWT requirement makes
  TestWANMicrosoftOpaqueCredentialReachesOnlyMicrosoft fail with
  "valid opaque Microsoft credential rejected". The fix was restored and
  all positive checks above passed afterward.
- Browser login uses an opaque token fixture, PKCE and a real isolated localhost
  callback; secure persistence, restore and cancellation checks pass.
- New tests cover opaque secure-store round trips, stable usage identity,
  provider-specific request routing, omitted/same/different/malformed refresh
  identity, stale snapshots, legacy records and secret-free errors.

Evidence is under node_modules/.wan-validation/runs/wan-opaque-{go,race,i18n,
negative,vet-windows,vet-linux}-20261023. All OAuth responses are fixtures;
no real organizational grant, credential store or public tunnel was used.
The user's exact rejected token format was not inspected, so this establishes
and fixes a reproducible compatibility bug rather than proving which old
predicate rejected that particular response. The new executable still needs
one real Microsoft sign-in to confirm the organization's end-to-end flow.

## Protocol references

- [Microsoft access-token guidance](https://learn.microsoft.com/en-us/entra/identity-platform/access-tokens)
- [MSAL account cache implementation](https://github.com/AzureAD/microsoft-authentication-library-for-python/blob/dev/msal/token_cache.py)
- [MSAL handling of directly received ID tokens](https://github.com/AzureAD/microsoft-authentication-library-for-python/blob/dev/msal/oauth2cli/oidc.py)

## Windows artifact

Source commit 016792d9 produced dist/microsoft-credential/magpie.exe,
59,420,672 bytes, version magpie wan-gateway-016792d9.
Windows x64 PE machine, GUI subsystem, embedded resources and isolated
--version smoke checks passed. SHA-256:

8239d4fbf86a50bf65f498e8019c117a40021bcae6c59d14eb4e33f01721f207

Build evidence: wan-opaque-exe-20261023/build-status.json.
Exit the old application before starting this separate new executable.

