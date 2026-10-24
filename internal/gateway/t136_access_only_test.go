package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/store"
)

func TestT136AccessOnly401RequiresExactReauthorization(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"synthetic rejected credential"}}`)
			}))
			defer upstream.Close()
			c := addCodexChannel(t, st, upstream.URL)
			c.Credentials[0].OAuth.RefreshToken = ""
			c.Credentials[0].OAuth.AccessToken = "synthetic-access-only"
			c.Credentials[0].OAuth.ExpiresAt = time.Time{}
			if err := st.SaveChannel(&c); err != nil {
				t.Fatal(err)
			}
			gateway := NewWithOAuth(st, nil, siwcStub(t, st), codexService(t, st, upstream.URL))
			siwcSettings(t, st)
			w := invoke(gateway, "/v1/responses", `{"model":"test","input":"synthetic"}`)
			if hits.Load() != 1 {
				t.Fatalf("request count %d, body %s", hits.Load(), w.Body.String())
			}
			live, err := st.Channel(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := store.OAuthStatusActive
			if status == http.StatusUnauthorized {
				want = store.OAuthStatusReauthRequired
			}
			if got := live.Credentials[0].OAuth.Status; got != want {
				t.Fatalf("status %q want %q after HTTP %d", got, want, status)
			}
			if !live.Enabled || live.Credentials[0].OAuth.AccessToken != "synthetic-access-only" {
				t.Fatal("authorization feedback changed config or token")
			}
		})
	}
}
