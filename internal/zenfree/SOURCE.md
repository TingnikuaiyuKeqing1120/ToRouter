# OpenCode Zen Free native adapter

Adapted 2026-10-20 from the MIT-licensed
[Magpie community plugin 0.1.11](https://github.com/magpie-community/plugins/tree/d34f78b69a303a620307f1202a77e171a13ecf8f/packages/zen-free),
commit `d34f78b69a303a620307f1202a77e171a13ecf8f`.
The original [license](../../third_party/upstream/magpie-plugins-d34f78b6/LICENSE-ZEN-FREE)
and copyright are retained in the offline notices.

Reference files: `provider.mjs` (public endpoint/profile/session identifiers),
`models.mjs` (catalogue SDK and protocol table), `stream.mjs` (same-protocol
non-stream assembly). Implementation is Go; no JS runtime, CLI execution,
native login reading or account credential migration is involved.

Intentional differences:

- Live listing and explicit zero input/output prices are required. A model name
  ending in `free` alone is not price evidence. Unknown protocols and SystemOne
  are not offered. Failed discovery leaves the saved catalogue intact.
- Per-model protocol metadata is committed atomically with the catalogue; the
  core router selects the real protocol before applying its conversion policy.
- Real requests must declare the five core tools required by the reference
  profile. No undeclared tools are inserted, no returned call is silently
  renamed or dropped, and no system prompt is rewritten. Unsupported requests
  fail before inference. The console's explicit connectivity probe is its own
  minimal test client, declares disabled test tools and requests `tool_choice=none`.
- SSE is forwarded for streaming clients; non-stream clients receive a bounded
  reconstruction in the same protocol with a required terminal event. Errors
  after inference starts are ambiguous/committed failures, not retry hints.
- Public access has no OAuth state or asserted unlimited quota. Channel creation
  is local/idempotent and does not automatically enable any models.

Tests use synthetic local HTTP endpoints. They do not demonstrate that the
current real free tier accepts this pinned software profile or that every
client supplies its required tool set.

Protocol evidence was rechecked on 2026-10-23 against the
[official model endpoint table](https://opencode.ai/v2/docs/console/models/).
The table declares an endpoint and SDK for each listed model. The SDK mapping
is a default-routing inference when the explicit table is unavailable; neither
source proves that an unlisted protocol has been tried and rejected upstream.
A local `gateway/request` declaration error is not an upstream probe result.
