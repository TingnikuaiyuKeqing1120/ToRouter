package antigravity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"torouter/internal/store"
)

func TestCompleteImportRefreshOnlyObtainsRealProjectAndIdentity(t *testing.T) {
	var calls []string
	d := localDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-refresh" {
				t.Error("wrong import refresh")
			}
			fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-new-refresh","expires_in":3600}`)
		case "/userinfo":
			fmt.Fprint(w, `{"id":"synthetic-subject","email":"fixture@example.invalid"}`)
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"cloudaicompanionProject":"synthetic-project"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	c, err := d.CompleteImport(context.Background(), store.OAuthCredential{Provider: "antigravity", RefreshToken: "synthetic-refresh"})
	if err != nil || c.AccessToken != "synthetic-access" || c.RefreshToken != "synthetic-new-refresh" || c.Subject != "synthetic-subject" || c.ProjectID != "synthetic-project" || c.ClientID != clientID || strings.Join(calls, ",") != "/token,/userinfo,/v1internal:loadCodeAssist" {
		t.Fatalf("incomplete import %v", err)
	}
}
