package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"torouter/internal/store"
)

func TestCompleteImportObtainsActualAccountWithoutRefreshingValidToken(t *testing.T) {
	calls := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/profile":
			if r.Header.Get("Authorization") != "Bearer synthetic-access" {
				t.Error("missing import bearer")
			}
			io.WriteString(w, `{"account":{"uuid":"synthetic-account"},"organization":{"uuid":"synthetic-org"}}`)
		case "/roles":
			io.WriteString(w, `{}`)
		default:
			t.Error("unexpected token exchange")
			http.Error(w, "invalid", 400)
		}
	}))
	defer srv.Close()
	d := NewDriver(srv.Client(), endpointFixture(srv.URL))
	c, err := d.CompleteImport(context.Background(), store.OAuthCredential{Provider: "claude", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh"})
	if err != nil || c.Account != "synthetic-account" || c.OrganizationID != "synthetic-org" || !validDeviceID(c.DeviceID) || c.ClientID != ClientID || strings.Join(calls, ",") != "/profile,/roles" {
		t.Fatalf("import completion failed %v", err)
	}
}
func TestCompleteImportDoesNotInventMissingAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }))
	defer srv.Close()
	d := NewDriver(srv.Client(), endpointFixture(srv.URL))
	if _, err := d.CompleteImport(context.Background(), store.OAuthCredential{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh"}); err == nil {
		t.Fatal("missing account was fabricated")
	}
}
