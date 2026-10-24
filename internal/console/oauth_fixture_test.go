package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"torouter/internal/store"
)

var authorizationFixtureMu sync.Mutex

// authorizationFixturePath makes the existing issuer/callback tests explicitly
// start from a stored pool. New target-selection tests pass their own exact IDs.
func authorizationFixturePath(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Has("mode") {
		return path
	} // Invalid retired parameters remain invalid.
	operation := q.Get("operation")
	if operation == "" {
		operation = "add_credential"
		q.Set("operation", operation)
	}
	if operation == "reauth" && q.Get("channel_id") == "" {
		return u.Path + "?" + q.Encode()
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "oauth" || parts[2] != "start" {
		return path
	}
	kind, err := oauthProvider(parts[1])
	if err != nil {
		return path
	}
	if q.Get("method") == "" {
		method := "browser"
		if kind == store.CredentialCopilotOAuth {
			method = "device"
		}
		q.Set("method", method)
	}
	authorizationFixtureMu.Lock()
	defer authorizationFixtureMu.Unlock()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/channels", nil))
	if w.Code != 200 {
		t.Fatalf("read authorization fixture pools: %d", w.Code)
	}
	var rows []store.Channel
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var target *store.Channel
	for i := range rows {
		if q.Get("channel_id") != "" {
			if strconv.FormatInt(rows[i].ID, 10) == q.Get("channel_id") {
				target = &rows[i]
				break
			}
		} else if rows[i].CredentialType == kind {
			target = &rows[i]
			break
		}
	}
	if target == nil && q.Get("channel_id") == "" {
		names := map[string]string{"codex": "Codex", "copilot": "GitHub Copilot", "chatgpt": "ChatGPT"}
		name := names[parts[1]]
		if name == "" {
			name = "Synthetic " + parts[1]
		}
		raw, _ := json.Marshal(map[string]any{"name": name, "credential_type": kind, "enabled": true})
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/channels", strings.NewReader(string(raw))))
		if w.Code != 200 {
			t.Fatalf("create authorization fixture pool: %d %s", w.Code, w.Body.String())
		}
		var created store.Channel
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}

		if c, ok := h.(consoleHandler); ok {
			runtime, _, resolveErr := c.oauth.plugins.Native(context.Background(), kind)
			if resolveErr == nil {
				var st *store.Store
				base := ""
				if runtime.Codex != nil {
					st = runtime.Codex.Store
					base = runtime.Codex.Endpoints.Backend
				}
				if runtime.SIWC != nil {
					st = runtime.SIWC.Store
					base = runtime.SIWC.Endpoints.Inference
				}
				if runtime.Copilot != nil {
					st = runtime.Copilot.Store
					base = runtime.Copilot.Endpoints.ModelAPI
				}
				if st != nil && strings.HasPrefix(base, "http://127.0.0.1:") {
					t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
					for i := range created.Endpoints {
						created.Endpoints[i].BaseURL = base
					}
					if err := st.SaveChannel(&created); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		target = &created
	}
	if target != nil {
		q.Set("channel_id", strconv.FormatInt(target.ID, 10))
		if operation == "reauth" && q.Get("credential_id") == "" && len(target.Credentials) > 0 {
			q.Set("credential_id", target.Credentials[0].ID)
		}
	}
	return u.Path + "?" + q.Encode()
}
