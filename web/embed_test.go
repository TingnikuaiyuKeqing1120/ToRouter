package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/channels", "/nested/route"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, p, nil))
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `id="root"`) {
			t.Fatalf("SPA %s: code=%d body=%s", p, r.Code, r.Body.String())
		}
	}
	entries, err := fs.ReadDir(FS, "dist/assets")
	if err != nil || len(entries) == 0 {
		t.Fatal("built assets missing", err)
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/assets/"+entries[0].Name(), nil))
	if r.Code != http.StatusOK || r.Body.Len() == 0 {
		t.Fatal("asset not served")
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if r.Code != http.StatusNotFound {
		t.Fatal("missing assets must not return index HTML")
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/", nil))
	if r.Code != http.StatusMethodNotAllowed {
		t.Fatal("mutating methods should be rejected")
	}
}
