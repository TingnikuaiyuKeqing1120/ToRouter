package claude

import (
	"net/http"

	"github.com/google/uuid"
)

// Keep this software identity together with transport.go and ordered_conn.go.
// Baseline: router-for-me/CLIProxyAPI e7d4c7316fca3fe0e852866f4e611cb4f2fa0764,
// internal/runtime/executor/helps/claude_device_profile.go. See SOURCE.md.
// Version refreshes are reviewed maintenance, never a runtime network lookup.
const (
	profileVersion   = "2.1.280"
	profileControlUA = "axios/1.15.2"
)

func applyProfileHeaders(h http.Header, token, session string) {
	for name, value := range map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json", "Accept": "application/json",
		"Accept-Encoding":   "gzip, deflate, br, zstd",
		"User-Agent":        "claude-cli/" + profileVersion + " (external, cli)",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Dangerous-Direct-Browser-Access": "true", "X-App": "cli",
		"X-Stainless-Retry-Count": "0", "X-Stainless-Runtime": "node",
		"X-Stainless-Lang": "js", "X-Stainless-Timeout": "600",
		"X-Stainless-Package-Version": "0.112.1",
		"X-Stainless-Runtime-Version": "v26.3.0",
		"X-Stainless-OS":              "MacOS", "X-Stainless-Arch": "arm64",
		"X-Claude-Code-Session-Id": session, "X-Client-Request-Id": uuid.NewString(),
		"Connection": "keep-alive",
	} {
		h.Set(name, value)
	}
}
