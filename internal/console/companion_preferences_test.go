package console

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"torouter/internal/store"
)

func TestCompanionPreferencesSharedPatchAndPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	h := New(s, "http://127.0.0.1:19191", Hooks{})
	call := func(method, body string) map[string]any {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/companion/preferences", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("%s preferences got %d want 200", method, w.Code)
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	initial := call("GET", "")
	if initial["theme"] != "dark" || initial["paused"] != false {
		t.Fatalf("bad defaults: %+v", initial)
	}
	first := call("PUT", `{"theme":"light"}`)
	second := call("PUT", `{"paused":true}`)
	if first["theme"] != "light" || second["theme"] != "light" || second["paused"] != true || second["revision"].(float64) <= first["revision"].(float64) {
		t.Fatal("one preference patch lost another field or revision")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h = New(s, "http://127.0.0.1:19191", Hooks{})
	if saved := call("GET", ""); saved["theme"] != "light" || saved["paused"] != true {
		t.Fatal("preferences did not survive reopen")
	}
	for _, body := range []string{`{"theme":"purple"}`, `{"unknown":1}`, `{"theme":"light"} {}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("PUT", "/companion/preferences", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("invalid patch accepted: %d", w.Code)
		}
	}
}
