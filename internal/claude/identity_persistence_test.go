package claude

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/nativeoauth"
	"torouter/internal/store"
)

func TestProfileIdentitySurvivesRefreshAndStoreReopen(t *testing.T) {
	t.Setenv("TOROUTER_OAUTH_EXTRA_ALLOWED_HOSTS", "127.0.0.1")
	var refreshes atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh" {
			t.Error("unexpected control request")
		}
		refreshes.Add(1)
		io.WriteString(w, `{"access_token":"synthetic-rotated-access","refresh_token":"synthetic-rotated-refresh","expires_in":3600}`)
	}))
	defer up.Close()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	driver := NewDriver(up.Client(), endpointFixture(up.URL))
	credential := fixtureCredential()
	credential.Status = store.OAuthStatusActive
	credential.ExpiresAt = time.Now().Add(-time.Hour)
	channel := store.Channel{Name: "Synthetic identity", Enabled: true, CredentialType: store.CredentialClaudeOAuth, Endpoints: driver.Definition().Endpoints,
		Credentials: []store.CredentialItem{{ID: "fixture", Enabled: true, OAuth: &credential}}}
	if err := st.SaveChannel(&channel); err != nil {
		t.Fatal(err)
	}
	before, beforeSession, err := prepareBody([]byte(messageBody), make(http.Header), credential)
	if err != nil {
		t.Fatal(err)
	}
	channel, err = st.Channel(channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nativeoauth.NewService(st, driver).Bearer(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	channel, err = st.Channel(channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	reopened := channel.Credentials[0].OAuth
	if reopened.DeviceID != credential.DeviceID || reopened.Account != credential.Account || reopened.OrganizationID != credential.OrganizationID || reopened.RefreshToken != "synthetic-rotated-refresh" {
		t.Fatal("stable identity or rotated credential lost on reopen")
	}
	if _, err := nativeoauth.NewService(st, NewDriver(up.Client(), endpointFixture(up.URL))).Bearer(context.Background(), channel); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 {
		t.Fatal("reopen discarded a valid refreshed token")
	}
	after, afterSession, err := prepareBody([]byte(messageBody), make(http.Header), *reopened)
	if err != nil || beforeSession != afterSession || !bytes.Equal(before, after) {
		t.Fatal("refresh/reopen altered request identity, signed context or CCH")
	}
}
