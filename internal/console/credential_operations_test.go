package console

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/codex"
	"torouter/internal/store"
)

func TestCredentialOperationsHTTPContract(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var consumed atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-two" {
			t.Error("wrong pool member selected")
		}
		switch r.URL.Path {
		case "/cards/consume":
			consumed.Add(1)
			w.WriteHeader(503)
		case "/cards":
			fmt.Fprint(w, `{"available_count":1,"credits":[{"id":"card","status":"available"}]}`)
		default:
			t.Error("unexpected external call", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	c := store.Channel{Name: "test", CredentialType: store.CredentialCodexOAuth, Enabled: true, Models: []string{"model"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIResponses, BaseURL: "https://chatgpt.com/backend-api/codex"}}, Credentials: []store.CredentialItem{
		{ID: "one", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: "client", Account: "account-one", AccessToken: "synthetic-one", RefreshToken: "refresh-one", ExpiresAt: time.Now().Add(time.Hour)}},
		{ID: "two/%25", Enabled: true, OAuth: &store.OAuthCredential{Provider: store.OAuthProviderCodex, ClientID: "client", Account: "account-two", AccessToken: "synthetic-two", RefreshToken: "refresh-two", ExpiresAt: time.Now().Add(time.Hour)}},
	}}
	if err = st.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	svc := codex.NewService(st, upstream.Client(), codex.Endpoints{ResetCredits: upstream.URL + "/cards"})
	h := NewWithOAuth(st, "http://127.0.0.1:17878", Hooks{CodexOperations: codex.NewOperations(svc)}, nil, nil, svc)
	base := fmt.Sprintf("/channels/%d/credentials/%s/operations", c.ID, url.PathEscape("two/%25"))
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		for _, secret := range []string{"synthetic-two", "refresh-two", "account-two"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("response exposed credential")
			}
		}
		return w
	}
	var report codex.OperationReport
	if err = json.Unmarshal(request("GET", base, "", 200).Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	identity := report.Identity
	if identity == "" || report.Config.AutoReset.Enabled || report.Config.Warmup.Enabled || consumed.Load() != 0 {
		t.Fatal("GET changed state or enabled defaults")
	}
	request("PUT", base, `{"expected_identity":"stale","warmup":{"mode":"reset","daily_times":["09:00"],"model":"model"}}`, 409)
	request("PUT", base, `{"expected_identity":"`+identity+`","warmup":null}`, 400)
	request("PUT", base, `{"expected_identity":"`+identity+`","warmup":{"mode":"reset","daily_times":["09:00"],"model":"model"}}`, 200)
	request("POST", base+"/refresh-reset-cards", `{"expected_identity":"`+identity+`"}`, 200)
	body := `{"expected_identity":"` + identity + `","request_id":"12345678-1234-4234-8234-123456789abc"}`
	for i := 0; i < 2; i++ {
		w := request("POST", base+"/consume-reset", body, 200)
		if err = json.Unmarshal(w.Body.Bytes(), &report); err != nil || report.LastReset == nil || report.LastReset.State != "unknown" {
			t.Fatal("HTTP response hid uncertain upstream outcome")
		}
	}
	if consumed.Load() != 1 {
		t.Fatal("duplicate HTTP request consumed again")
	}
	request("POST", base+"/consume-reset", strings.Replace(body, "123456789abc", "123456789abd", 1), 409)
	request("POST", base+"/acknowledge", `{"expected_identity":"`+identity+`","kind":"reset"}`, 200)
	request("PUT", fmt.Sprintf("/channels/%d/credentials/one/enabled", c.ID), `{}`, 400)
	request("PUT", fmt.Sprintf("/channels/%d/credentials/one/enabled", c.ID), `{"enabled":false}`, 200)
	request("PUT", fmt.Sprintf("/channels/%d/credential-order", c.ID), `{"credential_ids":["two/%25","one"]}`, 200)
	got, _ := st.Channel(c.ID)
	if got.Credentials[0].ID != "two/%25" || got.Credentials[1].Enabled || got.Credentials[0].Operations == nil {
		t.Fatal("order/toggle did not preserve independent config")
	}
	request("GET", "/channels/no/credentials/two/operations", "", 400)
}
