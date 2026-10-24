package console

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"torouter/internal/store"
)

func TestClientKeysOneTimeDisclosureAndRevocation(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := New(s, "http://127.0.0.1:9000", Hooks{})
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	for _, body := range []string{`{"name":""}`, `{"name":"test","key":"chosen"}`, `{"name":"test"}{}`} {
		if w := request("POST", "/client-keys", body); w.Code != 400 {
			t.Fatalf("bad input accepted: %d %s", w.Code, w.Body)
		}
	}
	w := request("POST", "/client-keys", `{"name":"Codex"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct {
		ClientKey store.ClientKey `json:"client_key"`
		Key       string          `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key == "" || created.ClientKey.ID == 0 || created.ClientKey.Name != "Codex" {
		t.Fatal("missing creation result")
	}
	if _, err := s.AuthenticateClientKey(created.Key); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/client-keys", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), created.Key) {
		t.Fatal("list exposes secret", w.Code)
	}
	var keys []store.ClientKey
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil || len(keys) != 1 {
		t.Fatal("invalid list", err)
	}
	path := fmt.Sprintf("/client-keys/%d", created.ClientKey.ID)
	w = request("GET", path+"/key", "")
	if w.Code != 200 {
		t.Fatalf("reveal key: %d %s", w.Code, w.Body)
	}
	var revealed struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &revealed); err != nil || revealed.Key != created.Key {
		t.Fatalf("revealed key mismatch: got %q, want %q", revealed.Key, created.Key)
	}
	if w = request("GET", "/client-keys/9999/key", ""); w.Code != 404 {
		t.Fatalf("nonexistent key revealed: %d", w.Code)
	}
	if w = request("DELETE", path, ""); w.Code != 204 {
		t.Fatal(w.Code, w.Body)
	}
	if _, err := s.AuthenticateClientKey(created.Key); err == nil {
		t.Fatal("revoked key still valid")
	}
	if w = request("DELETE", path, ""); w.Code != 404 {
		t.Fatal("missing key deletion status", w.Code)
	}
	w = request("GET", "/connection", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "http://127.0.0.1:9000") {
		t.Fatal("wrong gateway origin", w.Body)
	}
}
