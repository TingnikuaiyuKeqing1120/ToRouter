package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"torouter/internal/buildinfo"
	"torouter/internal/store"
)

func TestConsoleReadyDistinguishesRestartAndOtherInstallation(t *testing.T) {
	dir := t.TempDir()
	read := func(dir string) map[string]any {
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		h, err := consoleReadyHandler(st, 19000)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:19000/_console/ready", nil)
		r.Header.Set("Origin", "http://127.0.0.1:17878")
		w := httptest.NewRecorder()
		LocalOnly(h).ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:17878" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response %d %v", w.Code, w.Header())
		}
		var value map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if len(value) != 5 || value["service"] != "to-router" || value["version"] != buildinfo.Version {
			t.Fatalf("unexpected public fields %v", value)
		}
		return value
	}
	before, after, other := read(dir), read(dir), read(t.TempDir())
	if before["installation_id"] != after["installation_id"] || before["instance_id"] == after["instance_id"] || before["installation_id"] == other["installation_id"] {
		t.Fatal("readiness cannot distinguish process and installation")
	}
}

func TestConsoleReadyCrossOriginExceptionIsReadOnlyAndNarrow(t *testing.T) {
	for _, tc := range []struct {
		method, path, origin string
		status               int
	}{
		{"GET", consoleReadyPath, "http://localhost:17878", 200},
		{"GET", consoleReadyPath, "https://evil.example", 403},
		{"GET", consoleReadyPath, "http://localhost:17878/path", 403},
		{"POST", consoleReadyPath, "http://localhost:17878", 403},
		{"GET", "/api/settings", "http://localhost:17878", 403},
	} {
		r := httptest.NewRequest(tc.method, "http://127.0.0.1:19000"+tc.path, nil)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		LocalOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s origin=%s: %d", tc.method, tc.path, tc.origin, w.Code)
		}
	}
}
