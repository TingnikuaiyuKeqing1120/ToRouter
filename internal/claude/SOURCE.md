# Claude subscription driver source and limits

The OAuth wire constants, request identity/profile, CCH normalization scanner,
TLS ClientHello specifications, HTTP/1.1 ordered connection writer and maintained
model names are adapted from `router-for-me/CLIProxyAPI` at commit
`e7d4c7316fca3fe0e852866f4e611cb4f2fa0764` (the repository's pinned CPA
research snapshot, 2026-10-20).

Exact source files:

- `internal/auth/claude/anthropic_auth.go`
- `internal/auth/claude/identity.go`
- `internal/auth/claude/utls_transport.go`
- `internal/runtime/executor/claude_signing.go`
- `internal/runtime/executor/claude_signing_test.go` (known CCH vectors)
- `internal/runtime/executor/claude_executor_cloaking.go` (billing fingerprint)
- `internal/runtime/executor/claude_executor_request.go` (request headers/betas)
- `internal/runtime/executor/helps/claude_device_profile.go`
- `internal/runtime/executor/helps/claude_credential_identity.go`
- `internal/runtime/executor/helps/utls_client.go`
- `internal/httpwire/ordered_conn.go`
- `internal/httpwire/ordered_conn_test.go`
- `internal/registry/models/models.json`

Copyright (c) 2025-2005.9 Luis Pater
Copyright (c) 2025.9-present Router-For.ME

The upstream MIT license, including its original copyright spelling, is retained
in [LICENSE.CPA](LICENSE.CPA). The package also uses uTLS (BSD-3-Clause), xxHash
(BSD-2-Clause), Brotli (MIT), and klauspost/compress (BSD-3-Clause); dependency
notices are maintained by the host's third-party inventory.

This is a native Go adapter, with no CLI execution, source-tool configuration
reads, credential persistence or inference retries. Model names are a pinned
maintenance list, not authenticated model discovery or entitlement evidence.

Request changes: overwrite selected credential identity in `metadata.user_id`
(retaining other metadata and JSON identity extras); derive/reuse a conversation
UUID; prepend/update the billing CCH system block; add the Claude Code identity
system block when absent; apply the pinned software headers and body-derived
feature betas. Existing messages, tools, signed thinking and other system content
remain logical content. CCH signs the final serialized body with the upstream
normalization algorithm. This does not claim equivalence to CPA's complete
cloaking pipeline, diagnostic injection, system-message relocation, MCP tool
aliases, tool-search rewriting or signed-thinking sanitization. Unsupported
vendor semantics surface as real upstream failures.

Software headers are centralized in `profile.go`; incoming native billing
attribution retains its original version/continuity fields, while newly created
billing blocks use that pinned profile. Native Messages callers' complete beta
headers reach this adapter. Feature inference reads actual protocol slots,
not arbitrary names inside tool schemas/inputs. Explicit unknown betas survive;
an explicit nonempty `thinking.display` suppresses the conflicting redaction
beta. These are header decisions, not tool or thinking-content rewrites.
Store close/reopen plus a locally served token rotation verifies the existing
persisted account/device identity, request session, body and CCH remain stable.

Default and standard `*http.Transport` clients receive the custom Node/OpenSSL
uTLS profiles, ordered HTTP/1.1 headers, proxy tunneling, certificate validation,
original request context and client timeout. Explicit nonstandard RoundTrippers
remain caller-owned transport injections. No token-bearing redirects are
followed. Gzip/deflate/Brotli/zstd are decoded; status, response JSON/SSE content,
terminal events and truncation remain observable to the host.

Tests use synthetic tokens, local endpoints and pinned known vectors only.
They do not prove acceptance by Anthropic, subscription access, actual model
availability, or complete Claude Code interoperability.
