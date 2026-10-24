package gateway

import (
	"net/http/httptest"
	"testing"
)

func TestAffinityNativeSessionHeaderWinsAcrossTurns(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("session-id", "synthetic-native-session")
	r.Header.Set("x-session-id", "synthetic-compatibility-session")
	key, kind := sessionKey(r, "responses", true, []byte(`{"prompt_cache_key":"first"}`), 7, "public")
	raw, _ := rawSessionID(r, true, nil)
	if kind != "session_header" || raw != "synthetic-native-session" {
		t.Fatal("native session header was not preferred")
	}
	a := newAffinity()
	d := domain{channel: 9, model: "upstream", format: "responses"}
	a.record(key, d)
	next, _ := sessionKey(r, "responses", true, []byte(`{"prompt_cache_key":"second","previous_response_id":"response-next"}`), 7, "public")
	if got, ok := a.lookup(next); !ok || got != d {
		t.Fatal("native continuation lost its channel affinity")
	}
	other, _ := sessionKey(r, "responses", true, nil, 8, "public")
	if _, ok := a.lookup(other); ok {
		t.Fatal("client key scopes shared an affinity entry")
	}
}
