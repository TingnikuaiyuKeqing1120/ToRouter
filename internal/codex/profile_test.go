package codex

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRequestProfileSessionClearedForNonConversationCalls(t *testing.T) {
	h := make(http.Header)
	ApplyRequestHeaders(h, "synthetic-access", "synthetic-account", "caller-scoped-session")
	if got := h.Get("session-id"); got != "caller-scoped-session" {
		t.Fatalf("native session header = %q", got)
	}
	if h.Get("session_id") != "" || h.Get("thread-id") != "" {
		t.Fatal("a session must not invent compatibility or thread headers")
	}
	ApplyRequestHeaders(h, "synthetic-access", "synthetic-account", "")
	if got := h.Get("session-id"); got != "" {
		t.Fatalf("non-conversation call retained previous session %q", got)
	}
}

func TestRequestProfileVersionAndOriginatorStayTogether(t *testing.T) {
	t.Setenv("TOROUTER_CODEX_ORIGINATOR", "codex_cli_rs")
	t.Setenv("TOROUTER_CODEX_CLIENT_VERSION", "0.155.0")
	t.Setenv("TOROUTER_CODEX_USER_AGENT", "")
	h := make(http.Header)
	ApplyRequestHeaders(h, "synthetic-access", "synthetic-account", "")
	if h.Get("originator") != "codex_cli_rs" || !strings.HasPrefix(h.Get("User-Agent"), "codex_cli_rs/0.155.0 (") {
		t.Fatalf("inconsistent client identity: originator=%q UA=%q", h.Get("originator"), h.Get("User-Agent"))
	}
	u, err := url.Parse(modelsURLWithVersion("https://example.test/models"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("client_version") != "0.155.0" {
		t.Fatalf("catalogue version diverged: %s", u)
	}
	// Existing environment controls still resolve both identity consumers;
	// this change introduces no additional profile setting.
	t.Setenv("TOROUTER_CODEX_ORIGINATOR", "synthetic-client")
	t.Setenv("TOROUTER_CODEX_CLIENT_VERSION", "0.155.0-test")
	ApplyRequestHeaders(h, "synthetic-access", "synthetic-account", "")
	u, _ = url.Parse(modelsURLWithVersion("https://example.test/models"))
	if h.Get("originator") != "synthetic-client" || !strings.HasPrefix(h.Get("User-Agent"), "synthetic-client/0.155.0-test (") || u.Query().Get("client_version") != "0.155.0-test" {
		t.Fatalf("overrides diverged: header=%v URL=%s", h, u)
	}
}
