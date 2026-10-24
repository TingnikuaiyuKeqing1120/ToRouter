package console

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"torouter/internal/store"
)

func TestClientScopeCRUDAndPreview(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := store.Channel{Name: "unknown provider", APIKey: "synthetic", Enabled: true, Models: []string{"real"}, Endpoints: []store.Endpoint{{Format: store.FormatOpenAIChatCompletions, BaseURL: "http://127.0.0.1:1"}}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDownstreamModel(store.DownstreamModel{ID: "public", Enabled: true, Bindings: []store.Binding{{ChannelID: c.ID, Model: "real", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	h := New(s, "http://127.0.0.1:9000", Hooks{})
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	for _, body := range []string{`{"name":"a","scope":null}`, `{"name":"a","scope":{}}`, `{"name":"a","scope":{"model_mode":"selected","model_ids":["missing"]}}`, `{"name":"a","scope":{"model_mode":"all","tag_rules":{"billing":["privacy:zdr"]}}}`} {
		if w := request("POST", "/client-keys", body); w.Code != 400 {
			t.Fatalf("invalid create scope: %d %s", w.Code, w.Body)
		}
	}
	w := request("POST", "/client-keys", `{"name":"scoped","scope":{"model_mode":"selected","model_ids":["public"],"tag_rules":{"billing":["billing:unknown"]}}}`)
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
	path := fmt.Sprintf("/client-keys/%d", created.ClientKey.ID)
	for _, body := range []string{`{"name":"edited"}`, `{"name":"edited","scope":null}`, `{"name":"edited","scope":{}}`} {
		if w := request("PUT", path, body); w.Code != 400 {
			t.Fatalf("invalid edit accepted: %d %s", w.Code, w.Body)
		}
	}
	w = request("PUT", path, `{"name":"edited","scope":{"model_mode":"selected","model_ids":[],"tag_rules":{"billing":[]}}}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), created.Key) {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	key, err := s.AuthenticateClientKey(created.Key)
	if err != nil || key.Name != "edited" || key.Scope.AllowsModel("public") {
		t.Fatalf("identity or scope update failed: %+v %v", key, err)
	}
	if token, err := s.RevealClientKey(key.ID); err != nil || token != created.Key {
		t.Fatal("update replaced original key")
	}
	for _, tc := range []struct {
		body   string
		want   int
		reason string
	}{
		{`{"scope":{"model_mode":"all","tag_rules":{"billing":["billing:unknown"]}}}`, 1, "存在匹配绑定"},
		{`{"scope":{"model_mode":"all","tag_rules":{"billing":[]}}}`, 0, "计费模式不匹配"},
		{`{"scope":{"model_mode":"selected","model_ids":[],"tag_rules":{}}}`, 0, "公开模型不在允许范围内"},
	} {
		w = request("POST", "/client-keys/preview", tc.body)
		var preview struct {
			Allowed int                 `json:"allowed_models"`
			Total   int                 `json:"total_models"`
			Models  []scopePreviewModel `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || preview.Allowed != tc.want || preview.Total != 1 || len(preview.Models) != 1 || !strings.Contains(w.Body.String(), tc.reason) {
			t.Fatalf("preview: %d %s", w.Code, w.Body)
		}
	}
	if w := request("POST", "/client-keys/preview", `{}`); w.Code != 400 {
		t.Fatal("preview requires explicit scope")
	}
	if w := request("PUT", "/client-keys/9999", `{"name":"missing","scope":{"model_mode":"all"}}`); w.Code != 404 {
		t.Fatalf("missing key: %d %s", w.Code, w.Body)
	}
}
