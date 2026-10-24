# Codex request profile sources

The maintained values and header assembly live in `profile.go`. This is a
source-level comparison and local wire regression, not real-account acceptance.

| Reference | Evidence used |
| --- | --- |
| `openai/codex@1277bcfddc69800c5e3538dccf3f26bedbd80290` | `codex-rs/login/src/auth/default_client.rs`: `DEFAULT_ORIGINATOR`, `get_codex_user_agent`; `codex-rs/codex-api/src/requests/headers.rs`: `build_session_headers`; `codex-rs/core/src/client.rs`: `responses_session_id` |
| `router-for-me/CLIProxyAPI@e7d4c7316fca3fe0e852866f4e611cb4f2fa0764` | `internal/runtime/executor/codex_executor_request.go`: `applyCodexHeadersFromSources` forwards incoming `Session-Id`, with separate SIWC compatibility behavior |
| `qxcnm/Codex-Manager@430eddb0af9ae6277f8133fbac7f8634b7699dc6` | `crates/service/src/gateway/upstream/headers/codex_headers.rs`: `resolve_optional_session_id` chooses an incoming session, otherwise an optional fallback, and emits `session-id` |

The official native header is `session-id`. Its Responses client sends
`thread-id` separately. The model client explicitly uses the Responses session
for cache affinity; root agents can use the prompt cache key while other agents
use the actual session identity. This does not make all thread, installation,
window or turn metadata interchangeable. Compatibility `session_id` and
`conversation_id` headers in other implementations are not extra native fields
to invent for every request.

The gateway maps an explicit incoming session source through its existing
affinity key: authenticated client key ID, downstream model, inbound format,
and source identifier. Native `session-id` takes priority, followed by existing
generic session headers, explicit Responses `prompt_cache_key` and supported
session metadata. The result is stable across turns, retries and gateway
recreation. Original cache keys and Responses/tool/reasoning bodies are not
rewritten. Separate client keys isolate callers; callers sharing a key and the
same source also share this boundary.

Without a source, no session is sent. Request contents and changing
`previous_response_id` cursors cannot identify the original conversation.
Identical unlabelled conversations cannot be distinguished. Catalogue, usage and
credential operations supply an empty session and clear a previously set
profile session header.

The existing `0.155.0` release pin and simplified OS/architecture UA are retained.
The official source snapshot is a development tree, so it is evidence for field
names and scopes, not proof of a complete reproduction of that release. Its
OS version, terminal, installation/window identity and turn-state are not
fabricated. Existing environment overrides remain; no new UI/MCP profile
setting or browser TLS simulation is introduced.

Regression entry points: `profile_test.go` and
`../gateway/codex_profile_test.go`. Local HTTP captures cover actual outbound
headers/body, multiple turns, a 503 followed by gateway retry, caller and source
isolation, a fresh Gateway, explicit caches, missing sources and cursor-only
requests. Real providers, natural token renewal and a full process restart are
outside these new tests.
