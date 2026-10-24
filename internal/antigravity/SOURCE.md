# Antigravity native driver source and limits

Implemented in Go using the standard HTTP and JSON libraries. OAuth client
binding, project initialization and request envelopes were studied at:

- CLIProxyAPI `e7d4c7316fca3fe0e852866f4e611cb4f2fa0764`:
  `internal/auth/antigravity/constants.go`, `auth.go`,
  `internal/runtime/executor/antigravity_executor.go`,
  `internal/runtime/executor/antigravity_executor_request.go`,
  `internal/misc/antigravity_version.go`.
- CLIProxyAPI `d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683`:
  `internal/misc/antigravity_version.go` (updated complete Hub UA and the
  release-specific changelist; the executor's content is unchanged).
- Magpie `ef655acb40b2b2b5484e8eab41c79ff8b41bb6d3`:
  `internal/provider/google.go` (PKCE, project and session handling).

Both pinned sources use MIT licenses. Complete upstream legal texts and source
URLs are retained in `third_party/references.json` and
`third_party/upstream/CLIProxyAPI-e7d4c731/LICENSE`,
`third_party/upstream/magpie-ef655acb/LICENSE`.

The installed-app client ID and client binding are public protocol constants
from those sources, not a user's private credentials. User access/refresh
tokens and project identity belong to the host's encrypted credential store.
The Google userinfo stable ID supplies Account and Subject; its email is not
needed in ordinary UI or saved as the account label.

`profile.go` pins the CPA Go Hub combination: the UA is
`antigravity/hub/2.24.0 (aidev_client; os_type=darwin; arch=arm64; cl=1000335682)`
from `d946c4fba6fc`, with HTTP/1.1, no TLS ALPN extension and the pooling
strategy studied at `e7d4c7316fca`. Release `2.24.0` and build changelist
`1000335682` are fixed together. CPA states that this changelist was verified
in the official darwin-arm archive's `webm_encoder` build metadata; that is
upstream source evidence, not an archive inspection or real-account test
performed by this project. Onboarding adds the same
`google-api-nodejs-client/10.3.0` and `gl-node/22.21.1` software markers. The
platform is the reference profile's fixed platform, not the gateway host OS.
Versions are not refreshed at runtime. Magpie's runtime OS/architecture and
Antigravity-Manager `9f8e4f24d61b33fa80b8078d104c942752a5fa0b`
(`src-tauri/src/proxy/upstream/client.rs`, `src-tauri/src/constants.rs`,
`src-tauri/src/modules/device.rs`) were cross-checked; the latter's Chrome123
emulation and IDE machine-identity editing are not part of this profile.
This pins a reference implementation, not a claim that it reproduces every
native ClientHello field or is the only profile accepted by Google.

Each driver retains at most 64 least-recently-used connection pools, keyed by
a hash of stable Subject (then Account) plus OAuth client binding and the
original `*http.Transport` pointer. Incomplete, pre-userinfo credentials fall
back to a hashed refresh/access marker; an authorization-code exchange uses
its code as a temporary marker. Known account identity does not change when
either token rotates. Transport identity preserves separate proxy, dial and
certificate policies; net/http also scopes connections by the actual proxy
returned by a dynamic Proxy function. Replace the original transport when
changing configuration after first use, per net/http's concurrency contract.
Control and inference share pools only when their original transports match.

Eviction closes idle connections without interrupting active responses.
Clones retain caller timeouts, cancellation, certificate verification, proxy,
dial, cookie and other HTTP client behavior; existing bearer-safe redirect
rejection still applies. Idle connections expire within 30 seconds (explicit
shorter limits survive), with at most two idle connections per host and eight
per pool. Caller-disabled keep-alives remain disabled. The cache introduces
no background worker or user-facing setting.

Opaque custom RoundTrippers and transports with custom DialTLS/DialTLSContext
remain caller-owned and are passed through, including their connection and
protocol behavior. This preserves custom semantics but does not enforce the
HTTP/1.1/no-ALPN profile or account pool isolation on those injected branches.
It is not evidence that a custom transport used the pinned profile.

No CLI execution, automatic OAuth source-file reads, automatic inference
retries, model entitlement guesses, quota estimates or version-update network
calls are included. The software profile is pinned to the reference snapshot.
Requests keep original Gemini content, tools, thought signatures and cache
fields inside the vendor envelope. Responses are unwrapped without fabricating
successful terminal events; malformed, interrupted and in-band error responses
remain observable.

Local TLS/HTTP servers verify ClientHello ALPN, HTTP version, stable-account
reuse through access/refresh rotation, account/base-transport isolation,
dynamic proxies, HTTPS CONNECT, certificate verification, cancellation,
timeout, idle eviction and custom transport boundaries. Existing synthetic
authorization, project, directory, JSON/SSE and cancellation tests
do not prove real Google account eligibility, Antigravity project onboarding,
real model availability or service acceptance of this software profile.
