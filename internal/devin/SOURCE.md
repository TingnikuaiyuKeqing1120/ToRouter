# Devin native adapter

Adapted 2026-10-20 from:

- [Magpie community Devin plugin 0.1.20](https://github.com/magpie-community/plugins/tree/d34f78b69a303a620307f1202a77e171a13ecf8f/packages/devin),
  `index.mjs`: independent PKCE, exchange, exact model discovery, native client
  metadata and Connect RPC framing. The repository's
  [MIT license](../../third_party/upstream/magpie-plugins-d34f78b6/LICENSE) applies;
  the package declares MIT and has no separate license file at this revision.
- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI/tree/e7d4c7316fca3fe0e852866f4e611cb4f2fa0764),
  `internal/runtime/executor/helps/devin_wire.go` and
  `internal/auth/devin/user_status.go`: protobuf field numbers for prompts,
  thinking/signatures, usage, model terminals and stable account identity.
  The [original MIT license](../../third_party/upstream/CLIProxyAPI-e7d4c731/LICENSE)
  remains in the offline notices.

Implementation is Go and exposes a **stateless Responses projection** of the
private RPC, not a claim that Devin offers the OpenAI Responses API.

Intentional differences:

- Independent authorization only. No CLI process, source login file, local
  credential following, or CLI fallback. A stable upstream account identity is
  required before committing consent. The session declares no verified refresh
  endpoint or expiry; rejection requires reauthorization.
- Use exact model UIDs from the account catalogue, not a static guess, alias
  expansion or fallback to an installed client's model list.
- Keep original system/tool instructions and declared parameters. Do not
  rewrite prompts, insert tools or fabricate missing tool results. Unsupported
  media, strict schemas, structured output, forced tool selection, stored
  Responses and incomplete tool histories fail before inference.
- Preserve ordinary text/tools and signed reasoning material. A namespaced,
  reversible opaque envelope carries signature bytes and type; it neither
  decrypts vendor state nor encrypts plaintext reasoning. Tool call IDs have a
  reversible transport encoding.
- Use one guarded inference request, lazy streaming/backpressure and actual
  Connect plus model terminals. Truncation is failure, never completion or an
  adapter retry. UTF-8 split across RPC frames is reconstructed. Usage comes
  only from explicit upstream counters, not text-length estimates.
- Keep stable account-scoped session/prompt identifiers; no cache-hit promise.
  T119's common semantic/state/cache policies still apply before this adapter.

Local synthetic tests cover the declared contracts. They do not prove real
vendor authorization, subscription eligibility, current private API acceptance,
or arbitrary client compatibility.
