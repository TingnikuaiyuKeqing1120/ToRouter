package console

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestSafeRaw(t *testing.T) {
	raw, truncated := safeRaw([]byte(`{"error":{"message":"bad test\u002dsecret","api_key":"other-value","messages":["private prompt"]}}`), "test-secret")
	for _, private := range []string{"test-secret", "other-value", "private prompt"} {
		if strings.Contains(raw, private) {
			t.Fatalf("sensitive value not redacted: %s", private)
		}
	}
	if truncated || !strings.Contains(raw, "REDACTED") {
		t.Fatal("unexpected redaction result")
	}
	raw, truncated = safeRaw([]byte(strings.Repeat("错", 5000)), "")
	if !truncated || len([]rune(raw)) > 4120 {
		t.Fatal("raw diagnostic not bounded")
	}
}

func TestRedactBareAdminKey(t *testing.T) {
	key := "tr_" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	for _, input := range []string{"failed (" + key + "), retry", `{"detail":"failed (` + key + `), retry"}`} {
		got := Redact(input)
		if strings.Contains(got, key) || !strings.Contains(got, "failed ([REDACTED]), retry") {
			t.Fatalf("bare administrator key leaked: %q", got)
		}
	}
	ordinary := "tr_example tr_" + strings.Repeat("x", 42)
	if got := Redact(ordinary); got != ordinary {
		t.Fatal("ordinary tr_ text was redacted")
	}
}

func TestRedactHomePaths(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"windows", `data=C:\Users\Example User\AppData\Local\to-router`, `data=[HOME]\AppData\Local\to-router`},
		{"escaped windows", `error path C:\\Users\\fixture-user\\to-router\\app.log`, `error path [HOME]\\to-router\\app.log`},
		{"windows slashes", `D:/users/fixture-user/to-router/app.log`, `[HOME]/to-router/app.log`},
		{"unix", `/home/fixture-user/.config/to-router/app.log`, `[HOME]/.config/to-router/app.log`},
		{"unix with native windows separators", `\home\fixture-user\.config\to-router\app.log`, `[HOME]\.config\to-router\app.log`},
		{"mac", `open '/Users/fixture-user/Library/to-router/app.log' failed`, `open '[HOME]/Library/to-router/app.log' failed`},
		{"home only", `"/home/fixture-user"`, `"[HOME]"`},
		{"non home", `http://localhost/home/fixture-user and /srv/to-router/app.log and sk- tr_client_ tr_session_ tr_mcp_`, `http://localhost/home/fixture-user and /srv/to-router/app.log and sk- tr_client_ tr_session_ tr_mcp_`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.input); got != tc.want {
				t.Fatalf("Redact() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedactBareTokensCompletely(t *testing.T) {
	for _, token := range []string{"sk-" + "proj-Synthetic_private-123", "tr_client_Synthetic_private-123", "tr_session_Synthetic_private-123", "tr_mcp_Synthetic_private-123", "ghp_Synthetic123", "github_pat_Synthetic_123", "eyJsynthetic12.synthetic123.signature123"} {
		t.Run(strings.SplitN(token, "_", 2)[0], func(t *testing.T) {
			if got := Redact("failed (" + token + "), retry"); got != "failed ([REDACTED]), retry" {
				t.Fatalf("bare token not fully redacted: %q", got)
			}
		})
	}
}

func TestSafeRawRedactsJSONPathsAndBareTokens(t *testing.T) {
	input := strings.ReplaceAll(`{"error":{"path":"C:\\Users\\fixture-user\\to-router\\app.log","detail":"/home/fixture-user/.config/to-router tr_mcp_Synthetic-private-123","items":["TEST_TOKEN","\/Users\/fixture-user\/Library\/to-router"]}}`, "TEST_TOKEN", "sk-"+"proj-Synthetic-private-456")
	raw, truncated := safeRaw([]byte(input), "")
	if truncated || strings.Contains(raw, "Synthetic") || strings.Contains(raw, "fixture-user") {
		t.Fatalf("JSON contains identity or credential: %s", raw)
	}
	var got struct {
		Error struct {
			Path, Detail string
			Items        []string
		}
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("redaction broke JSON: %v", err)
	}
	if got.Error.Path != `[HOME]\to-router\app.log` || got.Error.Detail != "[HOME]/.config/to-router [REDACTED]" || len(got.Error.Items) != 2 || got.Error.Items[0] != "[REDACTED]" || got.Error.Items[1] != "[HOME]/Library/to-router" {
		t.Fatalf("diagnostic tails or redaction markers missing: %s", raw)
	}
}
