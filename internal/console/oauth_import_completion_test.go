package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"torouter/internal/codex"
	"torouter/internal/store"
)

func TestCompleteCodexImportRefreshesOnlySelectedNativeMaterial(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/oauth/token" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-original-refresh" {
			t.Error("wrong refresh material")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-rotated-refresh","expires_in":3600}`))
	}))
	defer up.Close()
	eps := codex.Endpoints{Token: up.URL + "/oauth/token"}
	got, err := completeCodexImport(context.Background(), up.Client(), eps, store.OAuthCredential{RefreshToken: "synthetic-original-refresh", Account: "synthetic-workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.AccessToken != "synthetic-access" || got.RefreshToken != "synthetic-rotated-refresh" || got.Account != "synthetic-workspace" || got.ExpiresAt.IsZero() {
		t.Fatal("incomplete or changed imported material")
	}
}
