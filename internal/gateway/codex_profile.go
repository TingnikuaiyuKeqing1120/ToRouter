package gateway

import (
	"net/http"

	"torouter/internal/auth"
)

// codexSessionID reuses the affinity namespace (authenticated client key,
// downstream model and inbound format) and its explicit client session sources.
// The deterministic mapping survives retries and gateway restarts without an
// identity store or token-derived values. Read the original inbound body, not
// a translated or retry-adjusted body, so cache and metadata semantics survive.
//
// No source means no header: history, previous_response_id and a global default
// cannot identify a conversation. Two unlabelled identical histories cannot be
// distinguished, and sharing a client key also shares this caller boundary.
func codexSessionID(original *http.Request, in requestInfo) string {
	client, authenticated := auth.ClientFromContextOK(original.Context())
	if !authenticated {
		return ""
	}
	session, _ := sessionKey(original, in.format, in.responses, in.body, client.ID, in.model)
	return session
}
