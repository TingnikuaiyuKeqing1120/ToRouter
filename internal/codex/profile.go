package codex

import (
	"fmt"
	"net/http"
	"runtime"
)

// The client identity and session header are one maintained profile. Sources:
// openai/codex@1277bcfddc69800c5e3538dccf3f26bedbd80290:
//
//	codex-rs/login/src/auth/default_client.rs (originator and UA construction)
//	codex-rs/codex-api/src/requests/headers.rs (session-id and separate thread-id)
//	codex-rs/core/src/client.rs (responses_session_id: cache affinity)
//
// Cross-checks:
// router-for-me/CLIProxyAPI@e7d4c7316fca3fe0e852866f4e611cb4f2fa0764,
//
//	internal/runtime/executor/codex_executor_request.go;
//
// qxcnm/Codex-Manager@430eddb0af9ae6277f8133fbac7f8634b7699dc6,
//
//	crates/service/src/gateway/upstream/headers/codex_headers.rs.
//
// The existing release pin is retained; the source checkout is not a claim
// that its development version, every optional header, or server acceptance
// has been reproduced. No browser TLS profile is implied.
const (
	Originator = "codex_cli_rs"
	// ClientVersion is shared by the UA and models client_version query.
	ClientVersion = "0.155.0"
	sessionHeader = "session-id"
)

func OriginatorValue() string { return envOr("TOROUTER_CODEX_ORIGINATOR", Originator) }
func ClientVersionValue() string {
	return envOr("TOROUTER_CODEX_CLIENT_VERSION", ClientVersion)
}

// DefaultUserAgent keeps the existing simplified CLI shape. OS version and
// terminal identity are unknown to this gateway and are not invented. Existing
// environment overrides remain available without adding user configuration.
func DefaultUserAgent() string {
	return fmt.Sprintf("%s/%s (%s; %s)", OriginatorValue(), ClientVersionValue(), runtime.GOOS, runtime.GOARCH)
}

func UserAgentValue() string { return envOr("TOROUTER_CODEX_USER_AGENT", DefaultUserAgent()) }

// ApplyRequestHeaders stamps the common backend profile. The gateway supplies
// a caller-scoped session only for inference with an identifiable source.
// Models, usage and credential probes pass an empty session. Native thread-id,
// installation/window identity and turn-state have distinct scopes; a session
// identifier alone cannot supply them. The request body is never altered here.
func ApplyRequestHeaders(h http.Header, accessToken, accountID, sessionID string) {
	h.Set("Authorization", "Bearer "+accessToken)
	if accountID != "" {
		h.Set("ChatGPT-Account-ID", accountID)
	}
	h.Set("originator", OriginatorValue())
	h.Set("User-Agent", UserAgentValue())
	h.Del(sessionHeader)
	if sessionID != "" {
		h.Set(sessionHeader, sessionID)
	}
}
