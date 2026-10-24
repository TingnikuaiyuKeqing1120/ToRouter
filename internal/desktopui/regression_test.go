package desktopui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResizeRejectsLegacyGET(t *testing.T) {
	opts := newAppOptions(launchConfig{pet: true, port: 17878}, nil)
	rec := httptest.NewRecorder()
	opts.AssetServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/desktop/resize-pet?dock=true", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("legacy GET mutated window or reported success: got %d, want 405", rec.Code)
	}
}

func TestClosePetRequiresManagementHeader(t *testing.T) {
	opts := newAppOptions(launchConfig{pet: true, port: 17878}, nil)
	rec := httptest.NewRecorder()
	opts.AssetServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/desktop/close-pet", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("local close accepted without management header: got %d, want 403", rec.Code)
	}
}
