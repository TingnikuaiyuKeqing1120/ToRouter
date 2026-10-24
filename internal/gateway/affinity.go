package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Session affinity: remember which channel served a conversation so its later
// turns keep landing there.
//
// The key comes from the client, not from the request content. Claude Code
// sends metadata.user_id, Codex sends prompt_cache_key, and several agents send
// x-session-id style headers. Deriving a fingerprint from the conversation
// instead works only as a fallback: the carrier set grows every turn, so only
// the oldest value is stable, and a compaction that drops early turns loses it.
// cc-switch and new-api both read the client's own identifiers for the same
// reason (docs/note/good-finds.md G013).
//
// Recording happens on success only, so a channel that failed never becomes the
// pin. The table is memory-only: losing it costs one re-learned session, and a
// restart cannot silently misroute a conversation that was working.
//
// Key and value describe different worlds. The key names the conversation the
// way the client speaks it — client key, downstream model, inbound format, and
// the client's own identifier or the oldest carrier — because two conversations
// that merely look alike must never share a pin. The value names the endpoint
// world the gateway chose for it, and only that world can continue the
// conversation: a Responses carrier is meaningless to the Chat Completions
// endpoint of the same channel.

// domain is one continuation domain: a channel plus the endpoint format that
// served it plus the upstream model served on that endpoint. Two bindings on
// the same channel but different models are different domains, because a
// thinking signature is only valid for the model that produced it; two
// endpoints of one channel (Responses versus Chat Completions) are different
// domains for the same reason — a carrier produced in one endpoint world has no
// meaning in the other. The generation is the channel's configuration
// generation at pin time: a channel re-pointed at another credential, base URL
// or endpoint set keeps its id but is a different state namespace, and only the
// configuration that produced a carrier can continue it.
type domain struct {
	channel    int64
	model      string
	format     string // store.Format* of the endpoint that served the request
	generation int64  // the channel's continuation-domain generation when pinned
	credential string // account identity for server-side history; never a token
}

type affinity struct {
	mu      sync.Mutex
	entries map[string]affinityEntry
	order   []string
	max     int
}

type affinityEntry struct {
	d domain
}

func newAffinity() *affinity {
	return &affinity{
		entries: map[string]affinityEntry{},
		max:     10000,
	}
}

func (a *affinity) lookup(key string) (domain, bool) {
	if key == "" {
		return domain{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[key]
	if !ok {
		return domain{}, false
	}
	return e.d, true
}

func (a *affinity) record(key string, d domain) {
	if key == "" || d.channel <= 0 || d.model == "" || d.format == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.entries[key]; !exists {
		a.order = append(a.order, key)
		if len(a.order) > a.max {
			// Drop the oldest half: a linear scan of the whole map on every
			// insert would cost more than the entries are worth.
			cut := len(a.order) - a.max/2
			for _, k := range a.order[:cut] {
				delete(a.entries, k)
			}
			a.order = append([]string{}, a.order[cut:]...)
		}
	}
	a.entries[key] = affinityEntry{d: d}
}

// sessionKey extracts the client's own conversation identifier and returns it
// namespaced by client, downstream model, and inbound format, so two clients
// that happen to send the same x-session-id do not share a pin, the same
// conversation replayed under a different downstream model gets its own, and
// the same conversation spoken in another inbound format gets its own as well:
// the format decides which endpoint world the request lives in, and the key has
// to describe the client's world for the value — the endpoint world that serves
// it — to be found again. The returned kind describes where the identifier came
// from and is safe to log; the raw identifier never is.
func sessionKey(r *http.Request, format string, responses bool, body []byte, clientKeyID int64, model string) (key, kind string) {
	raw, kind := rawSessionID(r, responses, body)
	if raw == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(clientKeyID, 10) + "|" + model + "|" + format + "|" + raw))
	return "s:" + hex.EncodeToString(sum[:16]), kind
}

func rawSessionID(r *http.Request, responses bool, body []byte) (string, string) {
	if r != nil {
		for _, h := range []string{"session-id", "x-session-id", "session_id", "x-grok-conv-id", "x-grok-session-id"} {
			if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
				return v, "session_header"
			}
		}
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return "", ""
	}
	if responses {
		// Codex sets a stable key per session; previous_response_id is a cursor
		// that changes every turn and must not be used here.
		if v, ok := root["prompt_cache_key"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), "prompt_cache_key"
		}
	}
	if meta, ok := root["metadata"].(map[string]any); ok {
		// Claude Code encodes the session in user_id as user_xxx_session_yyy.
		if v, ok := meta["user_id"].(string); ok {
			if s := parseSessionFromUserID(v); s != "" {
				return s, "user_metadata"
			}
		}
		if v, ok := meta["session_id"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), "session_metadata"
		}
	}
	return "", ""
}

func parseSessionFromUserID(userID string) string {
	if i := strings.Index(userID, "_session_"); i >= 0 {
		if s := strings.TrimSpace(userID[i+len("_session_"):]); s != "" {
			return s
		}
	}
	return ""
}

// carrierFingerprint hashes the oldest continuation carrier found in a request,
// namespaced by client key, downstream model and inbound format — the same
// namespaces the session key uses, and for the same reason: the key describes
// the conversation the way the client speaks it. Only the oldest carrier is
// used: the set of carriers grows every turn, but the first one stays
// byte-identical until the client compacts the history away. The inbound format
// belongs in the namespace because the same bytes arrive as different carriers
// in different endpoint worlds, and only the world that produced one can
// continue it.
func carrierFingerprint(clientKeyID int64, model, format, oldest string) string {
	if oldest == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(clientKeyID, 10) + "|" + model + "|" + format + "|" + oldest))
	return "f:" + hex.EncodeToString(sum[:16])
}
